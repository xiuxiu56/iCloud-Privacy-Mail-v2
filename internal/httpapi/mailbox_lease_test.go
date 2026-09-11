package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

func TestPublicMailboxLeaseLifecycle(t *testing.T) {
	server, state := newMailboxLeaseTestServer(t)
	claim := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailboxes/claim", `{
		"project":"gpt-register-next",
		"purpose":"注册任务 #1",
		"request_id":"job-1",
		"note":"等待注册结果"
	}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("领取接口状态码为 %d：%s", claim.Code, claim.Body.String())
	}
	claimPayload := decodeLeaseTestPayload(t, claim)
	data := payloadData(t, claimPayload)
	mailbox := payloadObject(t, data, "mailbox")
	lease := payloadObject(t, data, "lease")
	if mailbox["status"] != domain.StatusReserved {
		t.Fatalf("领取后邮箱状态为 %v，期望 reserved", mailbox["status"])
	}
	if lease["state"] != domain.MailboxLeaseClaimed || strings.TrimSpace(stringValue(lease["id"])) == "" {
		t.Fatalf("领取后租约不正确：%+v", lease)
	}
	leaseID := stringValue(lease["id"])

	repeated := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailboxes/claim", `{
		"project":"gpt-register-next",
		"purpose":"注册任务 #1",
		"request_id":"job-1"
	}`)
	repeatedData := payloadData(t, decodeLeaseTestPayload(t, repeated))
	if repeatedData["idempotent"] != true || stringValue(payloadObject(t, repeatedData, "lease")["id"]) != leaseID {
		t.Fatalf("重复领取没有返回原租约：%+v", repeatedData)
	}

	commit := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(leaseID)+"/commit", `{
		"project":"gpt-register-next",
		"note":"ChatGPT 注册成功"
	}`)
	if commit.Code != http.StatusOK {
		t.Fatalf("提交接口状态码为 %d：%s", commit.Code, commit.Body.String())
	}
	commitData := payloadData(t, decodeLeaseTestPayload(t, commit))
	if payloadObject(t, commitData, "mailbox")["status"] != domain.StatusUsed || payloadObject(t, commitData, "lease")["state"] != domain.MailboxLeaseCommitted {
		t.Fatalf("提交后的状态不正确：%+v", commitData)
	}
	stored, ok := state.FindMailboxByEmail("lease-api-fixture@icloud.com")
	if !ok || stored.Status != domain.StatusUsed || stored.Note != "ChatGPT 注册成功" {
		t.Fatalf("提交结果没有持久化：%+v，存在=%t", stored, ok)
	}

	release := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(leaseID)+"/release", `{"project":"gpt-register-next"}`)
	if release.Code != http.StatusConflict {
		t.Fatalf("已提交租约释放状态码为 %d，期望 409：%s", release.Code, release.Body.String())
	}
	if code := stringValue(decodeLeaseTestPayload(t, release)["code"]); code != "lease_committed" {
		t.Fatalf("已提交租约释放错误码为 %q", code)
	}
}

func TestPublicMailboxLeaseDeactivate(t *testing.T) {
	server, state := newMailboxLeaseTestServer(t)
	claim := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailboxes/claim", `{
		"project":"gpt-register-next",
		"purpose":"删除账号",
		"request_id":"delete-account-1"
	}`)
	claimData := payloadData(t, decodeLeaseTestPayload(t, claim))
	leaseID := stringValue(payloadObject(t, claimData, "lease")["id"])

	commit := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(leaseID)+"/commit", `{
		"project":"gpt-register-next",
		"note":"ChatGPT 注册成功"
	}`)
	if commit.Code != http.StatusOK {
		t.Fatalf("准备停用邮箱时提交租约失败：%d %s", commit.Code, commit.Body.String())
	}

	deactivate := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(leaseID)+"/deactivate", `{
		"project":"gpt-register-next",
		"note":"ChatGPT 状态：已停用｜套餐：Free"
	}`)
	if deactivate.Code != http.StatusOK {
		t.Fatalf("停用邮箱接口状态码为 %d：%s", deactivate.Code, deactivate.Body.String())
	}
	data := payloadData(t, decodeLeaseTestPayload(t, deactivate))
	if payloadObject(t, data, "mailbox")["status"] != domain.StatusDisabled {
		t.Fatalf("停用接口没有返回 disabled：%+v", data)
	}
	stored, ok := state.FindMailboxByEmail("lease-api-fixture@icloud.com")
	if !ok || stored.Status != domain.StatusDisabled || stored.Note != "ChatGPT 状态：已停用｜套餐：Free" {
		t.Fatalf("停用结果没有持久化：%+v，存在=%t", stored, ok)
	}

	repeated := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(leaseID)+"/deactivate", `{
		"project":"gpt-register-next",
		"note":"ChatGPT 状态：已停用｜套餐：Free"
	}`)
	if repeated.Code != http.StatusOK || payloadData(t, decodeLeaseTestPayload(t, repeated))["idempotent"] != true {
		t.Fatalf("重复停用没有保持幂等：%d %s", repeated.Code, repeated.Body.String())
	}
}

func TestPublicMailboxLeaseDeactivateFromHistoricalLease(t *testing.T) {
	server, state := newMailboxLeaseTestServer(t)
	claim := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailboxes/claim", `{
		"project":"gpt-register-next",
		"purpose":"旧账号",
		"request_id":"old-account-1"
	}`)
	claimData := payloadData(t, decodeLeaseTestPayload(t, claim))
	oldLeaseID := stringValue(payloadObject(t, claimData, "lease")["id"])

	release := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(oldLeaseID)+"/release", `{"project":"gpt-register-next"}`)
	if release.Code != http.StatusOK {
		t.Fatalf("释放旧租约失败：%d %s", release.Code, release.Body.String())
	}
	newClaim := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailboxes/claim", `{
		"project":"gpt-register-next",
		"purpose":"新账号",
		"request_id":"new-account-1"
	}`)
	if newClaim.Code != http.StatusOK {
		t.Fatalf("重新领取邮箱失败：%d %s", newClaim.Code, newClaim.Body.String())
	}

	deactivate := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(oldLeaseID)+"/deactivate", `{
		"project":"gpt-register-next",
		"note":"旧账号已删除"
	}`)
	if deactivate.Code != http.StatusOK {
		t.Fatalf("历史租约停用邮箱失败：%d %s", deactivate.Code, deactivate.Body.String())
	}
	data := payloadData(t, decodeLeaseTestPayload(t, deactivate))
	if payloadObject(t, data, "mailbox")["status"] != domain.StatusDisabled {
		t.Fatalf("历史租约停用接口没有返回 disabled：%+v", data)
	}
	stored, ok := state.FindMailboxByEmail("lease-api-fixture@icloud.com")
	if !ok || stored.Status != domain.StatusDisabled || stored.ActiveLeaseID != "" || stored.Note != "旧账号已删除" {
		t.Fatalf("历史租约没有将邮箱标记为已停用：%+v，存在=%t", stored, ok)
	}
}

func TestPublicMailboxLeaseEmailCompatibilityAndNote(t *testing.T) {
	server, state := newMailboxLeaseTestServer(t)
	claim := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailboxes/claim", `{"project":"fixture","purpose":"兼容接口","request_id":"compat-1"}`)
	claimData := payloadData(t, decodeLeaseTestPayload(t, claim))
	leaseID := stringValue(payloadObject(t, claimData, "lease")["id"])

	note := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(leaseID)+"/note", `{"project":"fixture","note":"密码已接受，等待人工确认"}`)
	if note.Code != http.StatusOK {
		t.Fatalf("备注接口状态码为 %d：%s", note.Code, note.Body.String())
	}
	if got := stringValue(payloadObject(t, payloadData(t, decodeLeaseTestPayload(t, note)), "lease")["note"]); got != "密码已接受，等待人工确认" {
		t.Fatalf("租约备注为 %q", got)
	}

	emailPath := url.PathEscape("lease-api-fixture@icloud.com")
	release := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailboxes/"+emailPath+"/release", `{"project":"fixture","reason":"注册失败，归还邮箱"}`)
	if release.Code != http.StatusOK {
		t.Fatalf("按邮箱兼容释放状态码为 %d：%s", release.Code, release.Body.String())
	}
	stored, ok := state.FindMailboxByEmail("lease-api-fixture@icloud.com")
	if !ok || stored.Status != domain.StatusAvailable || stored.Note != "注册失败，归还邮箱" {
		t.Fatalf("兼容释放结果不正确：%+v，存在=%t", stored, ok)
	}

	regularCommit := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(leaseID)+"/commit", `{"project":"fixture","note":"账号已绑定"}`)
	if regularCommit.Code != http.StatusConflict || stringValue(decodeLeaseTestPayload(t, regularCommit)["code"]) != "lease_released" {
		t.Fatalf("普通提交应继续拒绝已释放租约：status=%d body=%s", regularCommit.Code, regularCommit.Body.String())
	}
	reconcile := leaseTestRequest(t, server, http.MethodPost, "/api/v1/mailbox-leases/"+url.PathEscape(leaseID)+"/commit", `{
		"project":"fixture",
		"note":"注册未完成，但邮箱已绑定",
		"reconcile_bound":true
	}`)
	if reconcile.Code != http.StatusOK {
		t.Fatalf("已绑定纠偏接口状态码为 %d：%s", reconcile.Code, reconcile.Body.String())
	}
	reconcileData := payloadData(t, decodeLeaseTestPayload(t, reconcile))
	if reconcileData["reconciled"] != true || payloadObject(t, reconcileData, "mailbox")["status"] != domain.StatusUsed || payloadObject(t, reconcileData, "lease")["state"] != domain.MailboxLeaseReleased {
		t.Fatalf("已绑定纠偏结果不正确：%+v", reconcileData)
	}
}

func newMailboxLeaseTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建测试状态失败：%v", err)
	}
	settings := state.Settings()
	settings.EnablePublicMailboxAPI = true
	settings.PublicAPIKey = "lease-test-key"
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatalf("保存测试公共 API 设置失败：%v", err)
	}
	if _, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email: "lease-api-fixture@icloud.com", Label: "api_1", IsActive: true,
	}, "API 测试邮箱"); err != nil {
		t.Fatalf("创建 API 测试邮箱失败：%v", err)
	}
	cfg := config.Default()
	return New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil))), state
}

func leaseTestRequest(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", "lease-test-key")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func decodeLeaseTestPayload(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析接口响应失败：%v；响应=%s", err, recorder.Body.String())
	}
	return payload
}

func payloadData(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data 对象：%+v", payload)
	}
	return data
}

func payloadObject(t *testing.T, payload map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 %s 对象：%+v", key, payload)
	}
	return value
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
