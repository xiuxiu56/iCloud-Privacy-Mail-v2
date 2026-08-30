package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestSetMailboxStatusHandlesOptionalNote(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email: "fixture@icloud.com",
		Label: "x_1",
		Note:  "原备注",
	}, "原备注")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}

	newNote := "新备注"
	updated, err := state.SetMailboxStatus(mailbox.ID, nil, nil, domain.StatusUsed, &newNote)
	if err != nil {
		t.Fatalf("更新邮箱状态和备注失败：%v", err)
	}
	if updated.Note != "新备注" {
		t.Fatalf("邮箱备注未更新：%q", updated.Note)
	}

	updated, err = state.SetMailboxStatus(mailbox.ID, nil, nil, domain.StatusAvailable, nil)
	if err != nil {
		t.Fatalf("仅更新邮箱状态失败：%v", err)
	}
	if updated.Note != "新备注" {
		t.Fatalf("仅更新状态时备注发生变化：%q", updated.Note)
	}

	updated, err = state.SetMailboxStatus(mailbox.ID, nil, nil, domain.StatusReserved, nil)
	if err != nil {
		t.Fatalf("手动设置已预留状态失败：%v", err)
	}
	if updated.Status != domain.StatusReserved || updated.ActiveLeaseID != "" {
		t.Fatalf("手动设置的已预留状态不正确：%+v", updated)
	}

	emptyNote := ""
	updated, err = state.SetMailboxStatus(mailbox.ID, nil, nil, "", &emptyNote)
	if err != nil {
		t.Fatalf("清空邮箱备注失败：%v", err)
	}
	if updated.Note != "" {
		t.Fatalf("邮箱备注未清空：%q", updated.Note)
	}
}

func TestMailboxLeaseCommitMarksUsedOnlyAfterSuccess(t *testing.T) {
	state, mailbox := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	claimed, lease, created, err := state.ClaimMailboxLease("gpt-register-next", "注册任务 #1", "request-1", "等待注册", 30*time.Minute, now)
	if err != nil {
		t.Fatalf("领取邮箱租约失败：%v", err)
	}
	if !created || lease.State != domain.MailboxLeaseClaimed {
		t.Fatalf("新租约状态不正确：created=%t lease=%+v", created, lease)
	}
	if claimed.Status != domain.StatusReserved || claimed.ActiveLeaseID != lease.ID {
		t.Fatalf("领取后邮箱应为已预留：%+v", claimed)
	}
	if claimed.Status == domain.StatusUsed {
		t.Fatal("注册成功前邮箱不应标记为已使用")
	}

	committed, committedLease, idempotent, err := state.CommitMailboxLease(lease.ID, lease.Project, "注册成功", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("提交邮箱租约失败：%v", err)
	}
	if idempotent || committed.Status != domain.StatusUsed || committed.ActiveLeaseID != "" || committedLease.State != domain.MailboxLeaseCommitted {
		t.Fatalf("提交后的邮箱或租约状态不正确：mailbox=%+v lease=%+v idempotent=%t", committed, committedLease, idempotent)
	}
	if committed.Note != "注册成功" || committedLease.Note != "注册成功" {
		t.Fatalf("提交备注没有同步：mailbox=%q lease=%q", committed.Note, committedLease.Note)
	}

	_, repeated, idempotent, err := state.CommitMailboxLease(lease.ID, lease.Project, "重复请求不覆盖备注", now.Add(2*time.Minute))
	if err != nil || !idempotent || repeated.Note != "注册成功" {
		t.Fatalf("重复提交没有返回幂等结果：lease=%+v idempotent=%t err=%v", repeated, idempotent, err)
	}
	if _, _, _, err := state.ReleaseMailboxLease(lease.ID, lease.Project, "错误释放", now.Add(3*time.Minute)); !errors.Is(err, ErrLeaseCommitted) {
		t.Fatalf("已提交租约应拒绝释放，实际错误：%v", err)
	}

	stored, ok := state.FindMailboxByID(mailbox.ID)
	if !ok || stored.Status != domain.StatusUsed {
		t.Fatalf("持久化邮箱状态不正确：%+v，存在=%t", stored, ok)
	}
}

func TestMailboxLeaseReleaseAndRequestIDAreIdempotent(t *testing.T) {
	state, _ := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC)
	_, lease, created, err := state.ClaimMailboxLease("Fixture", "注册", "same-request", "初始备注", time.Hour, now)
	if err != nil || !created {
		t.Fatalf("首次领取失败：created=%t err=%v", created, err)
	}
	mailbox, repeatedLease, created, err := state.ClaimMailboxLease("fixture", "注册", "same-request", "不同备注", time.Hour, now.Add(time.Minute))
	if err != nil || created || repeatedLease.ID != lease.ID || mailbox.Status != domain.StatusReserved {
		t.Fatalf("重复领取没有返回原租约：mailbox=%+v lease=%+v created=%t err=%v", mailbox, repeatedLease, created, err)
	}
	if _, _, _, err := state.ClaimMailboxLease("fixture", "另一个任务", "same-request", "", time.Hour, now); !errors.Is(err, ErrLeaseRequestConflict) {
		t.Fatalf("复用 request_id 到不同任务应冲突，实际错误：%v", err)
	}

	released, releasedLease, idempotent, err := state.ReleaseMailboxLease(lease.ID, "fixture", "注册失败，释放邮箱", now.Add(2*time.Minute))
	if err != nil || idempotent || released.Status != domain.StatusAvailable || releasedLease.State != domain.MailboxLeaseReleased {
		t.Fatalf("释放结果不正确：mailbox=%+v lease=%+v idempotent=%t err=%v", released, releasedLease, idempotent, err)
	}
	_, repeatedLease, idempotent, err = state.ReleaseMailboxLease(lease.ID, "fixture", "重复释放", now.Add(3*time.Minute))
	if err != nil || !idempotent || repeatedLease.Note != "注册失败，释放邮箱" {
		t.Fatalf("重复释放没有返回幂等结果：lease=%+v idempotent=%t err=%v", repeatedLease, idempotent, err)
	}

	reconciled, reconciledLease, idempotent, err := state.ReconcileUsedMailboxLease(
		lease.ID,
		"fixture",
		"注册未完成，但邮箱已绑定",
		now.Add(4*time.Minute),
	)
	if err != nil || idempotent || reconciled.Status != domain.StatusUsed || reconciledLease.State != domain.MailboxLeaseReleased {
		t.Fatalf("已释放租约纠偏失败：mailbox=%+v lease=%+v idempotent=%t err=%v", reconciled, reconciledLease, idempotent, err)
	}
	if reconciled.Note != "注册未完成，但邮箱已绑定" {
		t.Fatalf("纠偏备注没有同步到邮箱：%q", reconciled.Note)
	}
	repeated, _, idempotent, err := state.ReconcileUsedMailboxLease(lease.ID, "fixture", "重复纠偏", now.Add(5*time.Minute))
	if err != nil || !idempotent || repeated.Status != domain.StatusUsed || repeated.Note != "注册未完成，但邮箱已绑定" {
		t.Fatalf("重复纠偏没有返回幂等结果：mailbox=%+v idempotent=%t err=%v", repeated, idempotent, err)
	}
}

func TestReleasedLeaseReconciliationRejectsMailboxWithNewerLease(t *testing.T) {
	state, _ := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 11, 30, 0, 0, time.UTC)
	_, oldLease, _, err := state.ClaimMailboxLease("fixture", "旧任务", "old-request", "", time.Hour, now)
	if err != nil {
		t.Fatalf("领取旧租约失败：%v", err)
	}
	if _, _, _, err := state.ReleaseMailboxLease(oldLease.ID, "fixture", "旧任务释放", now.Add(time.Minute)); err != nil {
		t.Fatalf("释放旧租约失败：%v", err)
	}
	_, newLease, _, err := state.ClaimMailboxLease("fixture", "新任务", "new-request", "", time.Hour, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("领取新租约失败：%v", err)
	}
	if _, _, _, err := state.ReleaseMailboxLease(newLease.ID, "fixture", "新任务释放", now.Add(3*time.Minute)); err != nil {
		t.Fatalf("释放新租约失败：%v", err)
	}

	if _, _, _, err := state.ReconcileUsedMailboxLease(oldLease.ID, "fixture", "旧任务账号已绑定", now.Add(4*time.Minute)); !errors.Is(err, ErrLeaseBindingConflict) {
		t.Fatalf("旧租约不应覆盖更新的租约历史，实际错误：%v", err)
	}
}

func TestMailboxLeaseExpiryReturnsMailboxToPool(t *testing.T) {
	state, mailbox := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	_, lease, _, err := state.ClaimMailboxLease("fixture", "过期测试", "expiry-request", "等待处理", time.Minute, now)
	if err != nil {
		t.Fatalf("领取测试租约失败：%v", err)
	}
	count, err := state.ExpireMailboxLeases(now.Add(2 * time.Minute))
	if err != nil || count != 1 {
		t.Fatalf("回收过期租约失败：count=%d err=%v", count, err)
	}
	stored, ok := state.FindMailboxByID(mailbox.ID)
	if !ok || stored.Status != domain.StatusAvailable || stored.ActiveLeaseID != "" {
		t.Fatalf("过期后邮箱没有恢复可用：%+v，存在=%t", stored, ok)
	}
	expired, ok := state.FindMailboxLease(lease.ID)
	if !ok || expired.State != domain.MailboxLeaseExpired || expired.ExpiredAt.IsZero() {
		t.Fatalf("租约过期状态不正确：%+v，存在=%t", expired, ok)
	}
	_, _, idempotent, err := state.ReleaseMailboxLease(lease.ID, "fixture", "过期后释放", now.Add(3*time.Minute))
	if err != nil || !idempotent {
		t.Fatalf("过期租约释放应按已回收返回幂等成功：idempotent=%t err=%v", idempotent, err)
	}
}

func TestMailboxLeaseNoteCanBeUpdated(t *testing.T) {
	state, _ := newLeaseTestStore(t)
	now := time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC)
	_, lease, _, err := state.ClaimMailboxLease("fixture", "备注测试", "note-request", "原备注", time.Hour, now)
	if err != nil {
		t.Fatalf("领取测试租约失败：%v", err)
	}
	mailbox, updated, err := state.SetMailboxLeaseNote(lease.ID, "fixture", "人工审核中", now.Add(time.Minute))
	if err != nil || mailbox.Note != "人工审核中" || updated.Note != "人工审核中" {
		t.Fatalf("租约备注更新失败：mailbox=%+v lease=%+v err=%v", mailbox, updated, err)
	}
	if _, _, err := state.SetMailboxLeaseNote(lease.ID, "another-project", "越权备注", now); !errors.Is(err, ErrLeaseProjectMismatch) {
		t.Fatalf("不同项目不应更新租约备注，实际错误：%v", err)
	}
}

func newLeaseTestStore(t *testing.T) (*Store, domain.Mailbox) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote("account_fixture", domain.RemoteMailbox{
		Email:    "lease-fixture@icloud.com",
		Label:    "lease_1",
		IsActive: true,
	}, "测试邮箱")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	return state, mailbox
}
