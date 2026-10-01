package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

type failingMailboxCreator struct {
	calls atomic.Int32
}

type selectiveMailboxCreator struct {
	mu     sync.Mutex
	calls  []string
	failID string
}

func (c *selectiveMailboxCreator) Create(_ context.Context, accountID, _, _, _ string) (domain.Mailbox, error) {
	c.mu.Lock()
	c.calls = append(c.calls, accountID)
	c.mu.Unlock()
	if accountID == c.failID {
		return domain.Mailbox{}, errors.New("模拟账号创建失败")
	}
	return domain.Mailbox{ID: "mailbox_1", Email: "created@icloud.com"}, nil
}

func (c *selectiveMailboxCreator) snapshotCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *failingMailboxCreator) Create(context.Context, string, string, string, string) (domain.Mailbox, error) {
	c.calls.Add(1)
	return domain.Mailbox{}, errors.New("模拟创建失败")
}

func TestRecordManualSuccessIncludesMailboxLabel(t *testing.T) {
	service := NewService(nil, nil)
	state := service.RecordManualSuccess("account_1", domain.Mailbox{ID: "mailbox_1", Email: "sample@icloud.com", Label: "project_12"})

	if len(state.Events) != 1 {
		t.Fatalf("事件数量 = %d，期望为 1", len(state.Events))
	}
	if state.Events[0].Label != "project_12" {
		t.Fatalf("事件标签 = %q，期望为 %q", state.Events[0].Label, "project_12")
	}
}

func TestSchedulerStatePersistsInSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	service := NewService(database, nil)
	service.RecordManualSuccess("account_1", domain.Mailbox{ID: "mailbox_1", Email: "persist@icloud.com", Label: "persist_1"})
	if err := database.Close(); err != nil {
		t.Fatalf("关闭数据库失败：%v", err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("重新打开 SQLite 数据库失败：%v", err)
	}
	defer reopened.Close()
	restored := NewService(reopened, nil).Snapshot()
	if restored.Success != 1 || len(restored.Events) != 1 || restored.Events[0].Email != "persist@icloud.com" {
		t.Fatalf("调度状态恢复不正确：%+v", restored)
	}
}

func TestRecordManualFailureIncludesTrimmedLabel(t *testing.T) {
	service := NewService(nil, nil)
	state := service.RecordManualFailure("account_1", "  project  ", errors.New("创建失败"))

	if len(state.Events) != 1 {
		t.Fatalf("事件数量 = %d，期望为 1", len(state.Events))
	}
	if state.Events[0].Label != "project" {
		t.Fatalf("事件标签 = %q，期望为 %q", state.Events[0].Label, "project")
	}
}

func TestSchedulerStopsAfterAllAccountsFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()

	firstSession, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "first@example.com"})
	if err != nil {
		t.Fatalf("保存第一个测试登录态失败：%v", err)
	}
	secondSession, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "second@example.com"})
	if err != nil {
		t.Fatalf("保存第二个测试登录态失败：%v", err)
	}
	creator := &failingMailboxCreator{}
	service := NewService(database, creator)
	_, err = service.Start(context.Background(), Config{
		AccountIDs:                []string{firstSession.AccountID, secondSession.AccountID},
		IntervalMinSeconds:        60,
		IntervalMaxSeconds:        60,
		AccountIntervalMinSeconds: 1,
		AccountIntervalMaxSeconds: 1,
	})
	if err != nil {
		t.Fatalf("启动自动创建失败：%v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var state State
	for time.Now().Before(deadline) {
		state = service.Snapshot()
		if !state.Running && state.Failed == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state.Running || state.Status != "stopped" {
		t.Fatalf("创建失败后调度器仍未停止：%+v", state)
	}
	if creator.calls.Load() != 2 {
		t.Fatalf("创建调用次数 = %d，期望两个账号分别失败后停止", creator.calls.Load())
	}
	if state.Failed != 2 || len(state.FailedAccountIDs) != 2 || state.LastError != "模拟创建失败" {
		t.Fatalf("失败状态记录不正确：%+v", state)
	}
	if !state.NextRunAt.IsZero() {
		t.Fatalf("创建失败后仍保留下一轮时间：%s", state.NextRunAt)
	}
	if len(state.Events) < 3 {
		t.Fatalf("调度事件数量不足：%+v", state.Events)
	}
	last := state.Events[len(state.Events)-1]
	if last.Type != "stopped" || last.Message != "所有参与账号创建失败，自动创建已停止" {
		t.Fatalf("停止事件不正确：%+v", last)
	}
	if state.StoppedAt.IsZero() || state.StoppedAt.Before(state.StartedAt) {
		t.Fatalf("停止时间不正确：%+v", state)
	}
}

func TestSchedulerSkipsFailedAccountInLaterRounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()
	first, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "first@example.com"})
	if err != nil {
		t.Fatalf("保存第一个测试登录态失败：%v", err)
	}
	second, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "second@example.com"})
	if err != nil {
		t.Fatalf("保存第二个测试登录态失败：%v", err)
	}
	creator := &selectiveMailboxCreator{failID: first.AccountID}
	service := NewService(database, creator)
	_, err = service.Start(context.Background(), Config{
		AccountIDs:         []string{first.AccountID, second.AccountID},
		IntervalMinSeconds: 1, IntervalMaxSeconds: 1,
		AccountIntervalMinSeconds: 1, AccountIntervalMaxSeconds: 1,
	})
	if err != nil {
		t.Fatalf("启动自动创建失败：%v", err)
	}
	defer service.Stop("")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(creator.snapshotCalls()) >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := creator.snapshotCalls()
	if len(calls) < 3 || calls[0] != first.AccountID || calls[1] != second.AccountID || calls[2] != second.AccountID {
		t.Fatalf("后续轮次调用顺序不正确：%v", calls)
	}
	state := service.Snapshot()
	if !state.Running || state.Failed != 1 || len(state.FailedAccountIDs) != 1 || state.FailedAccountIDs[0] != first.AccountID {
		t.Fatalf("单账号失败后任务状态不正确：%+v", state)
	}
	persisted := NewService(database, nil).Snapshot()
	if len(persisted.FailedAccountIDs) != 1 || persisted.FailedAccountIDs[0] != first.AccountID {
		t.Fatalf("失败账号未持久化：%+v", persisted)
	}
}

func TestSchedulerResumeSkipsFailedAccount(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()
	first, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "first@example.com"})
	if err != nil {
		t.Fatalf("保存第一个测试登录态失败：%v", err)
	}
	second, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "second@example.com"})
	if err != nil {
		t.Fatalf("保存第二个测试登录态失败：%v", err)
	}
	state := State{
		Running: true, Status: "waiting",
		AccountIDs:         []string{first.AccountID, second.AccountID},
		FailedAccountIDs:   []string{first.AccountID},
		IntervalMinSeconds: 60, IntervalMaxSeconds: 60,
		AccountIntervalMinSeconds: 1, AccountIntervalMaxSeconds: 1,
		StartedAt: time.Now().Add(-time.Minute),
	}
	if err := database.SaveRuntimeState("scheduler", state, false); err != nil {
		t.Fatalf("保存恢复状态失败：%v", err)
	}
	creator := &selectiveMailboxCreator{failID: first.AccountID}
	service := NewService(database, creator)
	service.Resume(context.Background())
	defer service.Stop("")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(creator.snapshotCalls()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := creator.snapshotCalls()
	if len(calls) != 1 || calls[0] != second.AccountID {
		t.Fatalf("恢复后调用的账号不正确：%v", calls)
	}
	if !service.Snapshot().Running {
		t.Fatalf("恢复后任务意外停止：%+v", service.Snapshot())
	}
}

func TestSchedulerMissingAccountDoesNotBlockOtherAccounts(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	defer database.Close()
	valid, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "valid@example.com"})
	if err != nil {
		t.Fatalf("保存测试登录态失败：%v", err)
	}
	creator := &selectiveMailboxCreator{failID: "missing_account"}
	service := NewService(database, creator)
	_, err = service.Start(context.Background(), Config{
		AccountIDs:         []string{"missing_account", valid.AccountID},
		IntervalMinSeconds: 60, IntervalMaxSeconds: 60,
		AccountIntervalMinSeconds: 1, AccountIntervalMaxSeconds: 1,
	})
	if err != nil {
		t.Fatalf("一个账号无登录态时应允许其他账号运行：%v", err)
	}
	defer service.Stop("")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(creator.snapshotCalls()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := creator.snapshotCalls()
	if len(calls) < 2 || calls[0] != "missing_account" || calls[1] != valid.AccountID {
		t.Fatalf("有效账号未继续创建：%v", calls)
	}
	if state := service.Snapshot(); !state.Running || state.Failed != 1 {
		t.Fatalf("单账号失败后任务状态不正确：%+v", state)
	}
}
