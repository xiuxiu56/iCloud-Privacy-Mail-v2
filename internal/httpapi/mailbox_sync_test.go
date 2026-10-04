package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/store"
)

func TestHandleExistingMailboxMessagesSyncStartsBackgroundJob(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建测试数据库失败：%v", err)
	}
	defer state.Close()
	session, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "sync@example.com"})
	if err != nil {
		t.Fatalf("保存测试账号失败：%v", err)
	}

	server := &Server{mailbox: mailboxservice.NewService(config.Default(), state)}
	request := httptest.NewRequest(http.MethodPost, "/api/mailboxes/sync-messages", strings.NewReader(`{"account_ids":["`+session.AccountID+`"]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.handleExistingMailboxMessagesSync(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("同步已有邮箱邮件接口状态不正确：%d，响应=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Job struct {
				ID            string `json:"id"`
				TotalAccounts int    `json:"total_accounts"`
				Running       bool   `json:"running"`
			} `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析同步接口响应失败：%v", err)
	}
	if !payload.Success || payload.Data.Job.ID == "" || payload.Data.Job.TotalAccounts != 0 {
		t.Fatalf("空邮箱同步任务不正确：%+v", payload)
	}
}
