package apple

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

func TestCheckKindOnlyChecksRequestedChannel(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer database.Close()
	session, err := database.SaveICloudSession(domain.ICloudSession{
		AppleID: "check-kind@icloud.com",
		Cookies: []domain.SessionCookie{{Name: "session", Value: "fixture"}},
		LoginStates: []domain.LoginState{
			{Kind: domain.LoginStateAppleAccount, Scnt: "scnt", APIKey: "api", Cookies: []domain.SessionCookie{{Name: "session", Value: "fixture"}}},
			{Kind: domain.LoginStateICloudWeb, Cookies: []domain.SessionCookie{{Name: "session", Value: "fixture"}}},
			{Kind: domain.LoginStateICloudIMAP, IMAPEmail: "check-kind@icloud.com", IMAPAppPassword: "app-password"},
		},
	})
	if err != nil {
		t.Fatalf("保存测试登录态失败：%v", err)
	}
	service := NewService(config.Config{}, database)
	appleCalls, webCalls, imapCalls := 0, 0, 0
	service.checkApple = func(_ context.Context, current domain.ICloudSession) (domain.ICloudSession, error) {
		appleCalls++
		return current, nil
	}
	service.checkWeb = func(_ context.Context, current domain.ICloudSession, _ string) (domain.ICloudSession, error) {
		webCalls++
		return current, nil
	}
	service.checkIMAP = func(context.Context, string, string) error {
		imapCalls++
		return nil
	}
	if _, err := service.CheckKind(context.Background(), session.AccountID, domain.LoginStateICloudIMAP); err != nil {
		t.Fatalf("单独检测 IMAP 失败：%v", err)
	}
	if appleCalls != 0 || webCalls != 0 || imapCalls != 1 {
		t.Fatalf("单独检测 IMAP 错误调用了其他通道：Apple=%d Web=%d IMAP=%d", appleCalls, webCalls, imapCalls)
	}
	checked, found := database.ICloudSessionByAccountID(session.AccountID)
	if !found {
		t.Fatal("检测后登录态不存在")
	}
	for _, state := range checked.LoginStates {
		if state.Kind == domain.LoginStateICloudIMAP && (state.LastCheckedAt.IsZero() || !state.LastCheckOK) {
			t.Fatalf("IMAP 检测结果未保存：%+v", state)
		}
		if state.Kind != domain.LoginStateICloudIMAP && !state.LastCheckedAt.IsZero() {
			t.Fatalf("单独检测 IMAP 改动了其他通道：%+v", state)
		}
	}
	if _, err := service.CheckKind(context.Background(), session.AccountID, domain.LoginStateAppleAccount); err != nil {
		t.Fatalf("单独检测 Apple Account 失败：%v", err)
	}
	if appleCalls != 1 || webCalls != 0 || imapCalls != 1 {
		t.Fatalf("单独检测 Apple Account 错误调用了其他通道：Apple=%d Web=%d IMAP=%d", appleCalls, webCalls, imapCalls)
	}
	if _, err := service.CheckKind(context.Background(), session.AccountID, domain.LoginStateICloudWeb); err != nil {
		t.Fatalf("单独检测 iCloud Web 失败：%v", err)
	}
	if appleCalls != 1 || webCalls != 1 || imapCalls != 1 {
		t.Fatalf("单独检测 iCloud Web 错误调用了其他通道：Apple=%d Web=%d IMAP=%d", appleCalls, webCalls, imapCalls)
	}
}

func TestAccountReturnsIMAPCredentialsOnlyInDetail(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer database.Close()

	const appPassword = "abcd-efgh-ijkl-mnop"
	session, err := database.SaveICloudSession(domain.ICloudSession{
		AppleID: "imap@icloud.com",
		LoginStates: []domain.LoginState{{
			Kind:            domain.LoginStateICloudIMAP,
			SavedAt:         time.Now(),
			IMAPEmail:       "mail@icloud.com",
			IMAPAppPassword: appPassword,
		}},
	})
	if err != nil {
		t.Fatalf("保存 IMAP 登录态失败：%v", err)
	}

	service := NewService(config.Config{}, database)
	items := service.Accounts()
	if len(items) != 1 {
		t.Fatalf("账号列表数量不正确：%d", len(items))
	}
	if items[0].IMAPEmail != "" || items[0].IMAPAppPassword != "" {
		t.Fatalf("账号列表不应返回 IMAP 凭据：%+v", items[0])
	}

	detail, err := service.Account(session.AccountID)
	if err != nil {
		t.Fatalf("读取账号详情失败：%v", err)
	}
	if detail.IMAPEmail != "mail@icloud.com" || detail.IMAPAppPassword != appPassword {
		t.Fatalf("账号详情中的 IMAP 凭据不正确：%+v", detail)
	}
}
