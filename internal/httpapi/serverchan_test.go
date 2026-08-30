package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/serverchan"
	"icloud-privacy-mail-v2/internal/store"
)

type serverChanCall struct {
	Options serverchan.Options
	Message serverchan.Message
}

type fakeServerChanSender struct {
	calls chan serverChanCall
	err   error
}

func (f *fakeServerChanSender) Send(_ context.Context, options serverchan.Options, message serverchan.Message) (serverchan.Result, error) {
	if f.calls != nil {
		f.calls <- serverChanCall{Options: options, Message: message}
	}
	return serverchan.Result{PushID: "push-test"}, f.err
}

func TestSettingsAPIReturnsStoredServerChanSendKey(t *testing.T) {
	server, state := newServerChanTestFixture(t)
	settings := state.Settings()
	settings.ServerChanSendKey = "SCT-secret-for-test"
	settings.NotifyAdminLogin = true
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"enable_mail_watcher":true,"notify_admin_login":true}`))
	recorder := httptest.NewRecorder()
	server.handleSaveSettings(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("保存设置失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), settings.ServerChanSendKey) {
		t.Fatal("保存响应未返回 Server 酱 SendKey")
	}
	persisted := state.Settings()
	if persisted.ServerChanSendKey != settings.ServerChanSendKey || !persisted.NotifyAdminLogin || !persisted.EnableMailWatcher {
		t.Fatalf("保存后设置不正确：%+v", persisted)
	}

	recorder = httptest.NewRecorder()
	server.handleSettings(recorder, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), settings.ServerChanSendKey) {
		t.Fatalf("读取设置响应不正确：%d %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data struct {
			Settings domain.Settings `json:"settings"`
			Runtime  map[string]any  `json:"runtime"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Settings.ServerChanSendKey != settings.ServerChanSendKey || response.Data.Runtime["server_chan_configured"] != true {
		t.Fatalf("Server 酱读取状态不正确：%+v %+v", response.Data.Settings, response.Data.Runtime)
	}
	changes, err := state.ChangesAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if strings.Contains(string(change.Payload), settings.ServerChanSendKey) {
			t.Fatalf("变更日志泄露了 Server 酱 SendKey：%s", change.Payload)
		}
	}
}

func TestServerChanTestUsesStoredKeyAndOptions(t *testing.T) {
	server, state := newServerChanTestFixture(t)
	settings := state.Settings()
	settings.ServerChanSendKey = "SCT-stored-key"
	settings.ServerChanHideIP = true
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	fake := &fakeServerChanSender{calls: make(chan serverChanCall, 1)}
	server.serverChan = fake
	recorder := httptest.NewRecorder()
	server.handleServerChanTest(recorder, httptest.NewRequest(http.MethodPost, "/api/server-chan/test", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("测试推送失败：%d %s", recorder.Code, recorder.Body.String())
	}
	call := <-fake.calls
	if call.Options.SendKey != settings.ServerChanSendKey || !call.Options.HideIP || !strings.Contains(call.Message.Title, "测试") || !strings.HasPrefix(call.Message.Short, "第1次通知｜") || !strings.Contains(call.Message.Desp, "通知序号：第1次通知") {
		t.Fatalf("测试推送参数不正确：%+v", call)
	}
}

func TestServerChanConfigFallbackMigratesOnSettingsSave(t *testing.T) {
	server, state := newServerChanTestFixture(t)
	server.cfg.ServerChanSendKey = "SCT-config-fallback"
	server.cfg.ServerChanHideIP = true
	server.cfg.ServerChanNotifyAdminLogin = true
	server.cfg.ServerChanNotifyLoginStateOffline = true

	effective := server.serverChanSettings()
	if effective.ServerChanSendKey != "SCT-config-fallback" || !effective.NotifyAdminLogin || !effective.NotifyAccountLoginStateOffline || !effective.ServerChanHideIP {
		t.Fatalf("配置文件回退未生效：%+v", effective)
	}
	recorder := httptest.NewRecorder()
	server.handleSaveSettings(recorder, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"notify_admin_login":false,"notify_account_login_state_offline":true,"server_chan_hide_ip":true}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("保存回退配置失败：%d %s", recorder.Code, recorder.Body.String())
	}
	persisted := state.Settings()
	if persisted.ServerChanSendKey != "SCT-config-fallback" || persisted.NotifyAdminLogin || !persisted.NotifyAccountLoginStateOffline {
		t.Fatalf("回退 SendKey 未迁移到数据库：%+v", persisted)
	}
}

func TestAdminLoginAndOfflineTransitionsSendNotifications(t *testing.T) {
	server, state := newServerChanTestFixture(t)
	settings := state.Settings()
	settings.ServerChanSendKey = "SCT-notify-key"
	settings.NotifyAdminLogin = true
	settings.NotifyAccountLoginStateOffline = true
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	fake := &fakeServerChanSender{calls: make(chan serverChanCall, 2)}
	server.serverChan = fake

	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("User-Agent", "测试浏览器")
	server.notifyAdminLogin(request, domain.Admin{Username: "admin"})
	loginCall := waitForServerChanCall(t, fake.calls)
	if !strings.Contains(loginCall.Message.Title, "登录") || !strings.HasPrefix(loginCall.Message.Short, "第1次通知｜") || !strings.Contains(loginCall.Message.Desp, "127.0.0.1") || !strings.Contains(loginCall.Message.Desp, "测试浏览器") {
		t.Fatalf("登录通知不正确：%+v", loginCall.Message)
	}

	key := "acc-1\x00" + domain.LoginStateICloudIMAP
	server.notifyOfflineTransitions(settings,
		map[string]loginStateHealth{key: {CheckedAt: time.Now().Add(-time.Minute), OK: true}},
		map[string]loginStateHealth{key: {CheckedAt: time.Now(), OK: false, Message: "应用专用密码已失效"}},
		[]domain.ICloudSession{{AccountID: "acc-1", AppleID: "notify@example.com"}},
	)
	offlineCall := waitForServerChanCall(t, fake.calls)
	if offlineCall.Message.Title != "notify@example.com｜Apple 登录态掉线" || strings.Contains(offlineCall.Message.Title, "/") || !strings.HasPrefix(offlineCall.Message.Short, "第2次通知｜") || !strings.Contains(offlineCall.Message.Desp, "掉线账号：notify@example.com") || !strings.Contains(offlineCall.Message.Desp, "IMAP 取码") {
		t.Fatalf("掉线通知不正确：%+v", offlineCall.Message)
	}
}

func TestOfflineTransitionsMergeMultipleAccountsWithoutTotalCount(t *testing.T) {
	server, state := newServerChanTestFixture(t)
	settings := state.Settings()
	settings.ServerChanSendKey = "SCT-paid-or-free-key"
	settings.NotifyAccountLoginStateOffline = true
	fake := &fakeServerChanSender{calls: make(chan serverChanCall, 2)}
	server.serverChan = fake

	checkedAt := time.Now()
	imapKey := "acc-1\x00" + domain.LoginStateICloudIMAP
	appleKey := "acc-2\x00" + domain.LoginStateAppleAccount
	server.notifyOfflineTransitions(settings,
		map[string]loginStateHealth{
			imapKey:  {CheckedAt: checkedAt.Add(-time.Minute), OK: true},
			appleKey: {CheckedAt: checkedAt.Add(-time.Minute), OK: true},
		},
		map[string]loginStateHealth{
			imapKey:  {CheckedAt: checkedAt, OK: false, Message: "IMAP 密码失效"},
			appleKey: {CheckedAt: checkedAt, OK: false, Message: "Apple 会话失效"},
		},
		[]domain.ICloudSession{
			{AccountID: "acc-1", AppleID: "first@example.com"},
			{AccountID: "acc-2", AppleID: "second@example.com"},
		},
	)

	call := waitForServerChanCall(t, fake.calls)
	if call.Options.SendKey != "SCT-paid-or-free-key" || strings.Contains(call.Message.Title, "/") || !strings.HasPrefix(call.Message.Short, "第1次通知｜") || !strings.Contains(call.Message.Title, "first@example.com") || !strings.Contains(call.Message.Desp, "掉线账号：first@example.com") || !strings.Contains(call.Message.Desp, "掉线账号：second@example.com") {
		t.Fatalf("多账号掉线合并通知不正确：%+v", call)
	}
	select {
	case extra := <-fake.calls:
		t.Fatalf("同一轮多账号掉线不应拆成多条推送：%+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func newServerChanTestFixture(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	server := New(config.Default(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return server, state
}

func waitForServerChanCall(t *testing.T, calls <-chan serverChanCall) serverChanCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("等待 Server 酱推送超时")
		return serverChanCall{}
	}
}
