package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestSaveICloudSessionWithPasswordEncryptsAndReturnsOnlyInDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	state, err := Open(path)
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}

	const password = "Apple-Password-123"
	session, err := state.SaveICloudSessionWithPassword(domain.ICloudSession{AppleID: "saved@icloud.com"}, password)
	if err != nil {
		t.Fatalf("保存 Apple ID 密码失败：%v", err)
	}
	var raw []byte
	if err := state.db.QueryRow(`SELECT data_json FROM apple_accounts WHERE id = ?`, session.AccountID).Scan(&raw); err != nil {
		t.Fatalf("读取 Apple 账号密文失败：%v", err)
	}
	if bytes.Contains(raw, []byte(password)) || !bytes.Contains(raw, []byte(secretPrefix)) {
		t.Fatalf("Apple ID 密码未加密保存：%s", raw)
	}
	detail, ok := state.FindAppleAccount(session.AccountID)
	if !ok || detail.Password != password {
		t.Fatalf("账号详情未返回已解密密码：%+v", detail)
	}
	items := state.AppleAccounts()
	if len(items) != 1 || items[0].Password != "" {
		t.Fatalf("账号列表不应返回 Apple ID 密码：%+v", items)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开数据库失败：%v", err)
	}
	defer reopened.Close()
	detail, ok = reopened.FindAppleAccount(session.AccountID)
	if !ok || detail.Password != password {
		t.Fatalf("重启后 Apple ID 密码解密结果不正确：%+v", detail)
	}
}

func TestSetAppleAccountEnabledPersistsStatus(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer state.Close()
	session, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "toggle@icloud.com"})
	if err != nil {
		t.Fatalf("保存测试账号失败：%v", err)
	}
	account, err := state.SetAppleAccountEnabled(session.AccountID, false)
	if err != nil || account.Status != domain.StatusDisabled {
		t.Fatalf("停用账号失败：账号=%+v 错误=%v", account, err)
	}
	account, err = state.SetAppleAccountEnabled(session.AccountID, true)
	if err != nil || account.Status != domain.StatusActive {
		t.Fatalf("开启账号失败：账号=%+v 错误=%v", account, err)
	}
	loaded, found := state.FindAppleAccount(session.AccountID)
	if !found || loaded.Status != domain.StatusActive {
		t.Fatalf("账号状态没有持久化：找到=%t 账号=%+v", found, loaded)
	}
	if _, err := state.SetAppleAccountEnabled(session.AccountID, false); err != nil {
		t.Fatalf("再次停用账号失败：%v", err)
	}
	if _, err := state.SaveICloudSession(domain.ICloudSession{AccountID: session.AccountID, AppleID: session.AppleID}); err != nil {
		t.Fatalf("更新停用账号登录态失败：%v", err)
	}
	loaded, found = state.FindAppleAccount(session.AccountID)
	if !found || loaded.Status != domain.StatusDisabled {
		t.Fatalf("更新登录态时不应自动开启停用账号：找到=%t 账号=%+v", found, loaded)
	}
}

func TestDisabledAppleAccountIsSkippedWhenClaimingMailbox(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer state.Close()
	session, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "claim-toggle@icloud.com"})
	if err != nil {
		t.Fatalf("保存测试账号失败：%v", err)
	}
	if _, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "available@icloud.com", IsActive: true}, ""); err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	if _, err := state.SetAppleAccountEnabled(session.AccountID, false); err != nil {
		t.Fatalf("停用账号失败：%v", err)
	}
	_, _, _, err = state.ClaimMailboxLease("claim-disabled", "测试", "", "", time.Hour, time.Now())
	if !errors.Is(err, ErrNoAvailableMailbox) {
		t.Fatalf("停用账号的邮箱不应被领取：%v", err)
	}
	if _, err := state.SetAppleAccountEnabled(session.AccountID, true); err != nil {
		t.Fatalf("开启账号失败：%v", err)
	}
	mailbox, _, _, err := state.ClaimMailboxLease("claim-enabled", "测试", "", "", time.Hour, time.Now())
	if err != nil || mailbox.Email != "available@icloud.com" {
		t.Fatalf("重新开启后应可领取邮箱：邮箱=%+v 错误=%v", mailbox, err)
	}
}

func TestSaveICloudSessionUpdatesExistingAppleAccountByAppleID(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer state.Close()

	first, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "Example@iCloud.com",
		LoginStates: []domain.LoginState{{
			Kind:    domain.LoginStateAppleAccount,
			SavedAt: time.Now(),
		}},
	})
	if err != nil {
		t.Fatalf("首次保存 Apple 登录态失败：%v", err)
	}

	second, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "  example@icloud.com  ",
		DSID:    "123456789",
		LoginStates: []domain.LoginState{{
			Kind:    domain.LoginStateICloudWeb,
			SavedAt: time.Now().Add(time.Minute),
		}},
	})
	if err != nil {
		t.Fatalf("重新登录后更新 Apple 登录态失败：%v", err)
	}

	if second.AccountID != first.AccountID {
		t.Fatalf("相同 Apple ID 生成了新账号：首次=%s 重新登录=%s", first.AccountID, second.AccountID)
	}
	if accounts := state.AppleAccounts(); len(accounts) != 1 {
		t.Fatalf("相同 Apple ID 应只保留一条账号记录，实际=%d", len(accounts))
	}
	if sessions := state.ICloudSessions(); len(sessions) != 1 {
		t.Fatalf("相同 Apple ID 应只保留一条会话记录，实际=%d", len(sessions))
	}
	if _, ok := loginStateByKind(second.LoginStates, domain.LoginStateAppleAccount); !ok {
		t.Fatal("更新时丢失了 Apple Account 新接口登录态")
	}
	if _, ok := loginStateByKind(second.LoginStates, domain.LoginStateICloudWeb); !ok {
		t.Fatal("更新时未合并 iCloud Web 旧接口登录态")
	}
}

func TestSaveICloudSessionUpdatesExistingAppleAccountByDSID(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer state.Close()

	first, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "old@icloud.com", DSID: "987654321"})
	if err != nil {
		t.Fatalf("首次保存 Apple 登录态失败：%v", err)
	}
	second, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "new@icloud.com", DSID: "987654321"})
	if err != nil {
		t.Fatalf("按 DSID 更新 Apple 登录态失败：%v", err)
	}

	if second.AccountID != first.AccountID {
		t.Fatalf("相同 DSID 生成了新账号：首次=%s 重新登录=%s", first.AccountID, second.AccountID)
	}
	accounts := state.AppleAccounts()
	if len(accounts) != 1 || accounts[0].AppleID != "new@icloud.com" {
		t.Fatalf("按 DSID 更新后的账号数据不正确：%+v", accounts)
	}
}

func TestSaveICloudSessionCreatesDifferentAppleAccounts(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时数据库失败：%v", err)
	}
	defer state.Close()

	first, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "first@icloud.com", DSID: "111"})
	if err != nil {
		t.Fatalf("保存第一个 Apple 账号失败：%v", err)
	}
	second, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "second@icloud.com", DSID: "222"})
	if err != nil {
		t.Fatalf("保存第二个 Apple 账号失败：%v", err)
	}

	if second.AccountID == first.AccountID {
		t.Fatalf("不同 Apple 账号被错误合并：%s", first.AccountID)
	}
	if accounts := state.AppleAccounts(); len(accounts) != 2 {
		t.Fatalf("不同 Apple 账号应生成两条记录，实际=%d", len(accounts))
	}
}

func TestICloudStatusFromSessionUsesLoginStateHealth(t *testing.T) {
	checkedAt := time.Now()
	tests := []struct {
		name   string
		states []domain.LoginState
		want   string
	}{
		{
			name: "所有通道正常",
			states: []domain.LoginState{
				{Kind: domain.LoginStateAppleAccount, LastCheckedAt: checkedAt, LastCheckOK: true},
				{Kind: domain.LoginStateICloudWeb, LastCheckedAt: checkedAt, LastCheckOK: true},
			},
			want: domain.ICloudStatusActive,
		},
		{
			name: "部分通道正常",
			states: []domain.LoginState{
				{Kind: domain.LoginStateAppleAccount, LastCheckedAt: checkedAt, LastCheckOK: true},
				{Kind: domain.LoginStateICloudWeb, LastCheckedAt: checkedAt, LastCheckOK: false},
			},
			want: domain.ICloudStatusPartial,
		},
		{
			name: "所有已检测通道失败",
			states: []domain.LoginState{
				{Kind: domain.LoginStateAppleAccount, LastCheckedAt: checkedAt, LastCheckOK: false},
				{Kind: domain.LoginStateICloudWeb, LastCheckedAt: checkedAt, LastCheckOK: false},
			},
			want: domain.ICloudStatusFailed,
		},
		{
			name: "一个通道正常其余待检测",
			states: []domain.LoginState{
				{Kind: domain.LoginStateAppleAccount, LastCheckedAt: checkedAt, LastCheckOK: true},
				{Kind: domain.LoginStateICloudWeb},
			},
			want: domain.ICloudStatusActive,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := iCloudStatusFromSession(domain.ICloudSession{LoginStates: test.states})
			if got != test.want {
				t.Fatalf("账号总状态=%s，期望=%s", got, test.want)
			}
		})
	}
}

func TestMergeICloudSessionKeepsCheckFieldsAsAGroup(t *testing.T) {
	checkedAt := time.Now()
	existing := domain.ICloudSession{
		LastCheckedAt:     checkedAt,
		LastCheckOK:       true,
		LastStatusMessage: "登录态正常",
	}

	withoutCheck := mergeICloudSession(existing, domain.ICloudSession{AppleID: "same@icloud.com"})
	if !withoutCheck.LastCheckedAt.Equal(checkedAt) || !withoutCheck.LastCheckOK || withoutCheck.LastStatusMessage != "登录态正常" {
		t.Fatalf("无检测结果的会话未成组保留旧结果：%+v", withoutCheck)
	}

	failedAt := checkedAt.Add(time.Minute)
	withFailure := mergeICloudSession(existing, domain.ICloudSession{
		AppleID:           "same@icloud.com",
		LastCheckedAt:     failedAt,
		LastCheckOK:       false,
		LastStatusMessage: "登录态异常",
	})
	if !withFailure.LastCheckedAt.Equal(failedAt) || withFailure.LastCheckOK || withFailure.LastStatusMessage != "登录态异常" {
		t.Fatalf("真实失败的检测结果被旧结果覆盖：%+v", withFailure)
	}
}

func loginStateByKind(states []domain.LoginState, kind string) (domain.LoginState, bool) {
	for _, state := range states {
		if state.Kind == kind {
			return state, true
		}
	}
	return domain.LoginState{}, false
}
