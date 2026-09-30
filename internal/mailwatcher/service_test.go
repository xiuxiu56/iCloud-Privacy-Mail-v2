package mailwatcher

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/store"
)

func TestWatcherStatusPersistsAndPublishes(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()
	changes, unsubscribe := database.SubscribeChanges(8)
	defer unsubscribe()
	service := NewService(config.Default(), database, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.updateStatus(func(status *Status) {
		status.Running = true
		status.Enabled = true
		status.GroupCount = 2
	})
	var persisted Status
	if found, err := database.LoadRuntimeState("mailwatcher", &persisted); err != nil || !found {
		t.Fatalf("读取监听状态失败：found=%t err=%v", found, err)
	}
	if !persisted.Running || persisted.GroupCount != 2 {
		t.Fatalf("监听状态持久化不正确：%+v", persisted)
	}
	select {
	case change := <-changes:
		if change.Resource != "mailwatcher" {
			t.Fatalf("实时资源不正确：%+v", change)
		}
	default:
		t.Fatal("监听状态变化未发布 SSE")
	}
}

func TestGroupsIncludeIMAPAndWebOnlyAccounts(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()
	imapSession, err := database.SaveICloudSession(domain.ICloudSession{
		AppleID: "imap@icloud.com", DSID: "imap-dsid", MailGatewayBaseURL: "https://mail.example.test",
		Cookies:     []domain.SessionCookie{{Name: "token", Value: "imap-token"}},
		LoginStates: []domain.LoginState{{Kind: domain.LoginStateICloudIMAP, IMAPEmail: "imap@icloud.com", IMAPUsername: "imap@icloud.com", IMAPAppPassword: "app-password"}},
	})
	if err != nil {
		t.Fatalf("创建 IMAP 测试账号失败：%v", err)
	}
	webSession, err := database.SaveICloudSession(domain.ICloudSession{
		AppleID: "web@icloud.com", DSID: "web-dsid", MailGatewayBaseURL: "https://mail.example.test",
		Cookies: []domain.SessionCookie{{Name: "token", Value: "web-token"}},
	})
	if err != nil {
		t.Fatalf("创建 Web 测试账号失败：%v", err)
	}
	imapMailbox, _, _ := database.UpsertMailboxFromRemote(imapSession.AccountID, domain.RemoteMailbox{Email: "imap-alias@icloud.com", IsActive: true}, "")
	_, _, _ = database.UpsertMailboxFromRemote(webSession.AccountID, domain.RemoteMailbox{Email: "web-alias@icloud.com", IsActive: true}, "")
	service := NewService(config.Default(), database, mailboxservice.NewService(config.Default(), database), slog.New(slog.NewTextHandler(io.Discard, nil)))

	groups := service.groups(true)
	imapGroups, webGroups := groupModeCounts(groups)
	if len(groups) != 2 || imapGroups != 1 || webGroups != 1 {
		t.Fatalf("双路径监听分组不正确：groups=%+v，IMAP=%d，Web=%d", groups, imapGroups, webGroups)
	}
	imapOnlyGroups := service.groups(false)
	imapOnlyCount, webOnlyCount := groupModeCounts(imapOnlyGroups)
	if len(imapOnlyGroups) != 1 || imapOnlyCount != 1 || webOnlyCount != 0 || imapOnlyGroups[0].hasWeb {
		t.Fatalf("关闭 Web API 后监听分组不正确：groups=%+v，IMAP=%d，Web=%d", imapOnlyGroups, imapOnlyCount, webOnlyCount)
	}
	service.Wake(imapMailbox.ID)
	select {
	case <-service.wake:
		accountIDs := service.takeWakeAccounts()
		if len(accountIDs) != 1 || accountIDs[0] != imapSession.AccountID {
			t.Fatalf("主动唤醒账号错误：得到 %v，期望 %q", accountIDs, imapSession.AccountID)
		}
	case <-time.After(time.Second):
		t.Fatal("主动唤醒没有投递目标账号")
	}
}

func TestWakeDeduplicatesSameAccount(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()
	session, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "wake@icloud.com"})
	if err != nil {
		t.Fatalf("创建测试账号失败：%v", err)
	}
	first, _, _ := database.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "first-wake@icloud.com", IsActive: true}, "")
	second, _, _ := database.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "second-wake@icloud.com", IsActive: true}, "")
	service := NewService(config.Default(), database, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.Wake(first.ID)
	service.Wake(first.ID)
	service.Wake(second.ID)
	accountIDs := service.takeWakeAccounts()
	if len(accountIDs) != 1 || accountIDs[0] != session.AccountID {
		t.Fatalf("同账号主动唤醒未去重：%v", accountIDs)
	}
}

func TestWebPollDelayUsesBackoffAndBoundedJitter(t *testing.T) {
	cfg := config.Default()
	cfg.MailWatcherWebPollMS = 60000
	service := &Service{cfg: cfg}
	base := service.webPollDelay("account-1", 0)
	retry := service.webPollDelay("account-1", 2)
	if base < time.Minute || base > 72*time.Second {
		t.Fatalf("Web 首次轮询间隔超出预期：%s", base)
	}
	if retry < 4*time.Minute || retry > 5*time.Minute {
		t.Fatalf("Web 失败退避间隔超出预期：%s", retry)
	}
}

func TestIdleCircuitDisablesAfterMoreThanThreeReconnectFailures(t *testing.T) {
	service := &Service{}
	const accountID = "account-idle-failure"
	const signature = "imap-signature"
	for attempt := 1; attempt <= idleReconnectLimit; attempt++ {
		failures, disabled := service.recordIdleFailure(accountID, signature)
		if failures != attempt || disabled {
			t.Fatalf("第 %d 次失败不应停用 IDLE：失败次数=%d disabled=%t", attempt, failures, disabled)
		}
	}
	failures, disabled := service.recordIdleFailure(accountID, signature)
	if failures != idleReconnectLimit+1 || !disabled {
		t.Fatalf("超过 3 次重连失败后应停用 IDLE：失败次数=%d disabled=%t", failures, disabled)
	}
	if !service.idleCircuitDisabled(accountID, signature) {
		t.Fatal("已熔断的账号仍被判断为可用")
	}
	service.markIdleConnected(accountID, signature)
	if service.idleCircuitDisabled(accountID, signature) {
		t.Fatal("成功连接后没有清除 IDLE 熔断状态")
	}
}

func TestDisabledIdleAccountFallsBackToWebPolling(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()
	session, err := database.SaveICloudSession(domain.ICloudSession{
		AppleID: "web-fallback@icloud.com", DSID: "web-fallback-dsid", MailGatewayBaseURL: "https://mail.example.test",
		Cookies:     []domain.SessionCookie{{Name: "session", Value: "web-cookie"}},
		LoginStates: []domain.LoginState{{Kind: domain.LoginStateICloudIMAP, IMAPEmail: "web-fallback@icloud.com", IMAPUsername: "web-fallback@icloud.com", IMAPAppPassword: "bad-password"}},
	})
	if err != nil {
		t.Fatalf("创建测试账号失败：%v", err)
	}
	if _, _, err := database.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "web-fallback-alias@icloud.com", IsActive: true}, ""); err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	service := NewService(config.Default(), database, mailboxservice.NewService(config.Default(), database), slog.New(slog.NewTextHandler(io.Discard, nil)))
	groups := service.groups(true)
	if len(groups) != 1 || !groups[0].hasIMAP || !groups[0].hasWeb {
		t.Fatalf("初始监听分组未同时启用 IMAP 和 Web：%+v", groups)
	}
	for attempt := 0; attempt <= idleReconnectLimit; attempt++ {
		service.recordIdleFailure(groups[0].key, groups[0].idleSignature)
	}
	groups = service.groups(true)
	if len(groups) != 1 || groups[0].hasIMAP || !groups[0].hasWeb {
		t.Fatalf("IMAP 熔断后没有切换到 Web 轮询：%+v", groups)
	}
}
