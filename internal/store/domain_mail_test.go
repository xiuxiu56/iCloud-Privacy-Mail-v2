package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestDomainMailConfigCreatesTypedMailboxesAndFiltersClaims(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer state.Close()
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "receiver@icloud.com",
		LoginStates: []domain.LoginState{{
			Kind: domain.LoginStateICloudIMAP, IMAPEmail: "receiver@icloud.com", IMAPUsername: "receiver@icloud.com",
			IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPAppPassword: "fixture-password",
		}},
	})
	if err != nil {
		t.Fatalf("保存测试账号失败：%v", err)
	}
	settings := domain.DefaultDomainMailSettings()
	settings.Enabled = true
	settings, routes, err := state.SaveDomainMailConfig(settings, []domain.DomainMailRoute{{
		Domain: "xiummm.com", ReceiverType: domain.DomainReceiverAppleAccount,
		AccountID: session.AccountID, ForwardToEmail: "receiver@icloud.com",
	}})
	if err != nil || len(routes) != 1 || !settings.Enabled {
		t.Fatalf("保存域名邮箱配置失败：settings=%+v routes=%+v err=%v", settings, routes, err)
	}
	created, err := state.CreateDomainMailboxes(routes[0].ID, []string{"mrhuang1@xiummm.com"}, "域名邮箱", "", true)
	if err != nil || len(created) != 1 {
		t.Fatalf("生成域名邮箱失败：items=%+v err=%v", created, err)
	}
	if created[0].MailboxKind != domain.MailboxKindDomainForward || created[0].AccountID != session.AccountID {
		t.Fatalf("域名邮箱类型或账号绑定不正确：%+v", created[0])
	}
	if _, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "privacy@icloud.com", IsActive: true}, ""); err != nil {
		t.Fatalf("创建 iCloud 隐私邮箱失败：%v", err)
	}
	now := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	domainMailbox, _, _, err := state.ClaimMailboxLeaseFiltered("domain-project", "域名任务", "domain-1", "", time.Hour, now, MailboxClaimFilter{
		MailboxKind: domain.MailboxKindDomainForward, Domain: "xiummm.com",
	})
	if err != nil || domainMailbox.Email != "mrhuang1@xiummm.com" {
		t.Fatalf("按域名领取邮箱失败：mailbox=%+v err=%v", domainMailbox, err)
	}
	icloudMailbox, _, _, err := state.ClaimMailboxLeaseFiltered("icloud-project", "iCloud 任务", "icloud-1", "", time.Hour, now, MailboxClaimFilter{
		MailboxKind: domain.MailboxKindICloudHME,
	})
	if err != nil || icloudMailbox.Email != "privacy@icloud.com" {
		t.Fatalf("按类型领取 iCloud 邮箱失败：mailbox=%+v err=%v", icloudMailbox, err)
	}
	mailboxPool := state.Mailboxes("", "", "", 1, 20)
	if mailboxPool.Total != 1 || len(mailboxPool.Items) != 1 || mailboxPool.Items[0].MailboxKind != domain.MailboxKindICloudHME {
		t.Fatalf("邮箱池不应显示域名邮箱：%+v", mailboxPool)
	}
}

func TestDomainMailCustomIMAPPasswordIsEncrypted(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer state.Close()
	const password = "domain-imap-secret"
	_, routes, err := state.SaveDomainMailConfig(domain.DefaultDomainMailSettings(), []domain.DomainMailRoute{{
		Domain: "example.net", ReceiverType: domain.DomainReceiverCustomIMAP, ForwardToEmail: "receiver@example.net",
		IMAPHost: "imap.example.net", IMAPPort: 993, IMAPUsername: "receiver@example.net", IMAPPassword: password, IMAPTLS: true,
	}})
	if err != nil || len(routes) != 1 || !routes[0].IMAPPasswordConfigured || routes[0].IMAPPassword != "" {
		t.Fatalf("保存标准 IMAP 路由失败：routes=%+v err=%v", routes, err)
	}
	var raw []byte
	if err := state.db.QueryRow(`SELECT data_json FROM domain_mail_routes WHERE id = ?`, routes[0].ID).Scan(&raw); err != nil {
		t.Fatalf("读取路由密文失败：%v", err)
	}
	if bytes.Contains(raw, []byte(password)) || !bytes.Contains(raw, []byte(secretPrefix)) {
		t.Fatalf("标准 IMAP 密码未加密保存：%s", raw)
	}
	secretRoute, ok := state.DomainMailRouteForSync(routes[0].ID)
	if !ok || secretRoute.IMAPPassword != password {
		t.Fatalf("后端同步未读取到解密密码：%+v", secretRoute)
	}
}

func TestDomainMailboxesListsNewestIDFirstWhenCreatedTogether(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer state.Close()
	_, routes, err := state.SaveDomainMailConfig(domain.DefaultDomainMailSettings(), []domain.DomainMailRoute{{
		Domain: "example.net", ReceiverType: domain.DomainReceiverCustomIMAP, ForwardToEmail: "receiver@example.net",
		IMAPHost: "imap.example.net", IMAPPort: 993, IMAPUsername: "receiver@example.net", IMAPPassword: "fixture-password", IMAPTLS: true,
	}})
	if err != nil || len(routes) != 1 {
		t.Fatalf("保存域名收件路由失败：routes=%+v err=%v", routes, err)
	}
	created, err := state.CreateDomainMailboxes(routes[0].ID, []string{"old@example.net", "new@example.net"}, "域名邮箱", "", true)
	if err != nil || len(created) != 2 {
		t.Fatalf("生成域名邮箱失败：items=%+v err=%v", created, err)
	}
	items := state.DomainMailboxes()
	if len(items) != 2 || items[0].ID != created[1].ID || items[1].ID != created[0].ID {
		t.Fatalf("同一时间生成的邮箱应按最新 ID 在前排列：items=%+v", items)
	}
}
