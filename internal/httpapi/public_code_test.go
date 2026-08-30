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
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

func TestPublicCodePageListsAndReadsMailboxMessages(t *testing.T) {
	server, state, mailbox := newPublicCodeTestServer(t, true)
	now := time.Now().UTC().Truncate(time.Second)
	created, err := state.ApplyMailboxSyncBatch([]store.MailboxSyncUpdate{{MailboxID: mailbox.ID, Messages: []store.MailboxSyncMessage{{
		RemoteID: "imap:42", Source: "imap", Subject: "登录验证码", From: "OpenAI <noreply@example.com>",
		Body: "验证码是 123456", HTMLBody: "<p>验证码是 <strong>123456</strong></p>", ContentType: "text/html", ReceivedAt: now,
	}}}})
	if err != nil || created != 1 {
		t.Fatalf("准备测试邮件失败：created=%d err=%v", created, err)
	}

	listPath := "/api/v1/public-code/messages?email=" + url.QueryEscape(mailbox.Email)
	list := publicCodeTestRequest(t, server, listPath)
	if list.Code != http.StatusOK {
		t.Fatalf("邮件列表接口状态码为 %d：%s", list.Code, list.Body.String())
	}
	data := publicCodeTestData(t, list)
	items, ok := data["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("邮件列表不正确：%+v", data)
	}
	summary, _ := items[0].(map[string]any)
	if summary["subject"] != "登录验证码" || summary["body"] != nil || summary["html_body"] != nil {
		t.Fatalf("邮件摘要字段不正确：%+v", summary)
	}

	messageID, _ := summary["id"].(string)
	detailPath := "/api/v1/public-code/messages/" + url.PathEscape(messageID) + "?email=" + url.QueryEscape(mailbox.Email)
	detail := publicCodeTestRequest(t, server, detailPath)
	if detail.Code != http.StatusOK {
		t.Fatalf("邮件详情接口状态码为 %d：%s", detail.Code, detail.Body.String())
	}
	message, _ := publicCodeTestData(t, detail)["message"].(map[string]any)
	if message["body"] != "验证码是 123456" || message["html_body"] == "" || message["mailbox_id"] != nil || message["remote_id"] != nil {
		t.Fatalf("邮件详情字段不正确：%+v", message)
	}
}

func TestPublicCodePageMessagesRequireEnabledSetting(t *testing.T) {
	server, _, mailbox := newPublicCodeTestServer(t, false)
	response := publicCodeTestRequest(t, server, "/api/v1/public-code/messages?email="+url.QueryEscape(mailbox.Email))
	if response.Code != http.StatusForbidden {
		t.Fatalf("关闭公共页面后的状态码为 %d，期望 403：%s", response.Code, response.Body.String())
	}
}

func TestPublicCodePageStatusReturnsConfiguredDomainNames(t *testing.T) {
	server, state, _ := newPublicCodeTestServer(t, true)
	domainSettings := domain.DefaultDomainMailSettings()
	domainSettings.Enabled = true
	_, _, err := state.SaveDomainMailConfig(domainSettings, []domain.DomainMailRoute{
		{
			Domain:         "xiummm.com",
			ReceiverType:   domain.DomainReceiverCustomIMAP,
			ForwardToEmail: "receiver@example.net",
			IMAPHost:       "imap.example.net",
			IMAPPort:       993,
			IMAPUsername:   "receiver@example.net",
			IMAPPassword:   "fixture-password",
			IMAPTLS:        true,
		},
		{
			Domain:         "mail.example.org",
			ReceiverType:   domain.DomainReceiverCustomIMAP,
			ForwardToEmail: "receiver@example.org",
			IMAPHost:       "imap.example.org",
			IMAPPort:       993,
			IMAPUsername:   "receiver@example.org",
			IMAPPassword:   "fixture-password",
			IMAPTLS:        true,
		},
	})
	if err != nil {
		t.Fatalf("保存域名邮箱测试配置失败：%v", err)
	}

	response := publicCodeTestRequest(t, server, "/api/v1/public-code/status")
	if response.Code != http.StatusOK {
		t.Fatalf("公共页面状态接口状态码为 %d：%s", response.Code, response.Body.String())
	}
	data := publicCodeTestData(t, response)
	domainMail, ok := data["domain_mail"].(map[string]any)
	if !ok || domainMail["enabled"] != true {
		t.Fatalf("公共页面状态没有返回已启用的域名邮箱配置：%+v", data)
	}
	domains, ok := domainMail["domains"].([]any)
	if !ok || len(domains) != 2 || domains[0] != "mail.example.org" || domains[1] != "xiummm.com" {
		t.Fatalf("公共页面状态返回的接收域名不正确：%+v", domainMail)
	}
	for _, forbidden := range []string{"receiver@example.net", "imap.example.net", "fixture-password"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("公共页面状态暴露了收件账号配置：%s", forbidden)
		}
	}
}

func TestPublicMailboxMessageAPIUsesMailboxTokenAndReturnsKind(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	settings := state.Settings()
	settings.EnablePublicMailboxAPI = true
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatalf("保存公共 API 设置失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{Email: "external-api@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	created, err := state.ApplyMailboxSyncBatch([]store.MailboxSyncUpdate{{MailboxID: mailbox.ID, Messages: []store.MailboxSyncMessage{{
		RemoteID: "imap:88", Source: "imap", Subject: "外部 API 邮件", From: "sender@example.com", Body: "正文", ReceivedAt: time.Now(),
	}}}})
	if err != nil || created != 1 {
		t.Fatalf("准备测试邮件失败：created=%d err=%v", created, err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	path := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/messages?key=" + url.QueryEscape(mailbox.APIToken)
	response := publicCodeTestRequest(t, server, path)
	if response.Code != http.StatusOK {
		t.Fatalf("外部邮件列表接口状态码为 %d：%s", response.Code, response.Body.String())
	}
	data := publicCodeTestData(t, response)
	if data["mailbox_kind"] != domain.MailboxKindICloudHME {
		t.Fatalf("外部邮件列表没有返回邮箱类型：%+v", data)
	}
	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("外部邮件列表不正确：%+v", data)
	}
	messageID, _ := items[0].(map[string]any)["id"].(string)
	detailPath := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/messages/" + url.PathEscape(messageID) + "?key=" + url.QueryEscape(mailbox.APIToken)
	detail := publicCodeTestRequest(t, server, detailPath)
	if detail.Code != http.StatusOK || publicCodeTestData(t, detail)["mailbox_kind"] != domain.MailboxKindICloudHME {
		t.Fatalf("外部邮件详情接口不正确：status=%d body=%s", detail.Code, detail.Body.String())
	}
}

func TestDomainMailboxExternalAPISupportsClaimCodeAndMessages(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	settings := state.Settings()
	settings.EnablePublicMailboxAPI = true
	settings.PublicAPIKey = "domain-api-key"
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatalf("保存公共 API 设置失败：%v", err)
	}
	domainSettings := domain.DefaultDomainMailSettings()
	domainSettings.Enabled = true
	_, routes, err := state.SaveDomainMailConfig(domainSettings, []domain.DomainMailRoute{{
		Domain: "api.example.net", ReceiverType: domain.DomainReceiverCustomIMAP, ForwardToEmail: "receiver@example.net",
		IMAPHost: "imap.example.net", IMAPPort: 993, IMAPUsername: "receiver@example.net", IMAPPassword: "fixture-password", IMAPTLS: true,
	}})
	if err != nil || len(routes) != 1 {
		t.Fatalf("创建域名邮箱路由失败：routes=%+v err=%v", routes, err)
	}
	created, err := state.CreateDomainMailboxes(routes[0].ID, []string{"external@api.example.net"}, "外部 API 域名邮箱", "", true)
	if err != nil || len(created) != 1 {
		t.Fatalf("创建域名邮箱失败：items=%+v err=%v", created, err)
	}
	mailbox, found := state.FindMailboxByID(created[0].ID)
	if !found {
		t.Fatal("未找到已创建的域名邮箱")
	}
	now := time.Now().UTC().Truncate(time.Second)
	createdMessages, err := state.ApplyMailboxSyncBatch([]store.MailboxSyncUpdate{{MailboxID: mailbox.ID, Messages: []store.MailboxSyncMessage{{
		RemoteID: "imap:188", Source: "imap", Subject: "OpenAI 登录验证码", From: "OpenAI <noreply@example.com>",
		Body: "验证码是 654321", ContentType: "text/plain", ReceivedAt: now,
	}}}})
	if err != nil || createdMessages != 1 {
		t.Fatalf("保存域名邮箱测试邮件失败：created=%d err=%v", createdMessages, err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))

	claimRequest := httptest.NewRequest(http.MethodPost, "/api/v1/mailboxes/claim", strings.NewReader(`{
		"project":"domain-api-test","purpose":"领取域名邮箱","request_id":"domain-claim-1",
		"mailbox_kind":"domain_forward","domain":"api.example.net"
	}`))
	claimRequest.Header.Set("Content-Type", "application/json")
	claimRequest.Header.Set("X-API-Key", "domain-api-key")
	claim := httptest.NewRecorder()
	server.ServeHTTP(claim, claimRequest)
	if claim.Code != http.StatusOK {
		t.Fatalf("域名邮箱领取接口状态码为 %d：%s", claim.Code, claim.Body.String())
	}
	claimedMailbox, _ := publicCodeTestData(t, claim)["mailbox"].(map[string]any)
	if claimedMailbox["mailbox_kind"] != domain.MailboxKindDomainForward || claimedMailbox["email"] != mailbox.Email {
		t.Fatalf("外部 API 领取的域名邮箱不正确：%+v", claimedMailbox)
	}

	codePath := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/code?cache=1&key=domain-api-key"
	code := publicCodeTestRequest(t, server, codePath)
	if code.Code != http.StatusOK || publicCodeTestData(t, code)["code"] != "654321" {
		t.Fatalf("域名邮箱外部取码接口不正确：status=%d body=%s", code.Code, code.Body.String())
	}
	listPath := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Email) + "/messages?key=domain-api-key"
	list := publicCodeTestRequest(t, server, listPath)
	listData := publicCodeTestData(t, list)
	items, _ := listData["items"].([]any)
	if list.Code != http.StatusOK || listData["mailbox_kind"] != domain.MailboxKindDomainForward || len(items) != 1 {
		t.Fatalf("域名邮箱外部邮件接口不正确：status=%d data=%+v", list.Code, listData)
	}
}

func newPublicCodeTestServer(t *testing.T, enabled bool) (*Server, *store.Store, domain.Mailbox) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	settings := state.Settings()
	settings.EnablePublicCodePage = enabled
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatalf("保存公共页面设置失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{Email: "public-code@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return server, state, mailbox
}

func publicCodeTestRequest(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func publicCodeTestData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析接口响应失败：%v；响应=%s", err, recorder.Body.String())
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("接口响应缺少 data：%+v", payload)
	}
	return data
}
