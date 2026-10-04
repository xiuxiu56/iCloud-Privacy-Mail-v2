package mailbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

type remoteMailboxDeleteClientFixture struct {
	remote        protocol.ICloudRemoteMailbox
	operations    []string
	remoteIDs     []string
	listCalls     int
	moveErr       error
	destroyErr    error
	discoveryErr  error
	discoveredIDs map[string][]string
	emptyTrashErr error
	onDelete      func()
	onMove        func()
	onEmptyTrash  func()
}

func (f *remoteMailboxDeleteClientFixture) ListPrivacyMailboxes(context.Context, protocol.ICloudSession) ([]protocol.ICloudRemoteMailbox, error) {
	f.operations = append(f.operations, "查询远端邮箱")
	f.listCalls++
	if f.listCalls == 1 {
		return []protocol.ICloudRemoteMailbox{f.remote}, nil
	}
	return nil, nil
}

func (f *remoteMailboxDeleteClientFixture) DeletePrivacyMailbox(_ context.Context, _ protocol.ICloudSession, anonymousID string) error {
	f.operations = append(f.operations, "删除远端邮箱")
	if anonymousID != f.remote.AnonymousID {
		return errors.New("远端邮箱标识不匹配")
	}
	if f.onDelete != nil {
		f.onDelete()
	}
	return nil
}

func (f *remoteMailboxDeleteClientFixture) FindRemoteMessageIDsByMailbox(_ context.Context, _ protocol.ICloudSession, mailboxes []domain.Mailbox) (protocol.ICloudRemoteMessageDiscoveryResult, error) {
	f.operations = append(f.operations, "扫描远端邮件")
	if f.discoveryErr != nil {
		return protocol.ICloudRemoteMessageDiscoveryResult{}, f.discoveryErr
	}
	result := protocol.ICloudRemoteMessageDiscoveryResult{RemoteIDsByMailbox: make(map[string][]string), ThreadsScanned: len(mailboxes)}
	for index, mailbox := range mailboxes {
		remoteIDs := f.discoveredIDs[mailbox.ID]
		if f.discoveredIDs == nil {
			remoteIDs = []string{fmt.Sprintf("icloud:INBOX:%d", 501+index)}
		}
		result.RemoteIDsByMailbox[mailbox.ID] = append([]string(nil), remoteIDs...)
	}
	return result, nil
}

func (f *remoteMailboxDeleteClientFixture) MoveRemoteMessagesToTrash(_ context.Context, _ protocol.ICloudSession, remoteIDs []string) (protocol.ICloudMailCleanupResult, error) {
	f.operations = append(f.operations, "移动远端邮件")
	f.remoteIDs = append([]string(nil), remoteIDs...)
	if f.onMove != nil {
		f.onMove()
	}
	if f.moveErr != nil {
		return protocol.ICloudMailCleanupResult{}, f.moveErr
	}
	return protocol.ICloudMailCleanupResult{
		MovedToTrash:   len(remoteIDs),
		MovedRemoteIDs: append([]string(nil), remoteIDs...),
	}, nil
}

func (f *remoteMailboxDeleteClientFixture) MoveRemoteMessagesToTrashAndDestroy(_ context.Context, _ protocol.ICloudSession, remoteIDs []string) (protocol.ICloudMailCleanupResult, error) {
	f.operations = append(f.operations, "移入废纸篓并彻底删除")
	f.remoteIDs = append([]string(nil), remoteIDs...)
	if f.onMove != nil {
		f.onMove()
	}
	result := protocol.ICloudMailCleanupResult{
		MovedToTrash:   len(remoteIDs),
		MovedRemoteIDs: append([]string(nil), remoteIDs...),
	}
	if f.moveErr != nil {
		return protocol.ICloudMailCleanupResult{}, f.moveErr
	}
	if f.destroyErr != nil {
		return result, f.destroyErr
	}
	result.Destroyed = len(remoteIDs)
	return result, nil
}

func TestCleanDomainRemoteMessagesDeletesCloudMailBeforeLocalMail(t *testing.T) {
	state, mailboxes := newDomainDeleteServiceFixture(t, 2)
	client := &remoteMailboxDeleteClientFixture{}
	client.onMove = func() {
		for _, mailbox := range mailboxes {
			if len(state.MessagesForMailbox(mailbox.ID)) != 1 {
				t.Errorf("云端删除完成前本地邮件应保留：%s", mailbox.Email)
			}
			if _, found := state.FindMailboxByID(mailbox.ID); !found {
				t.Errorf("云端删除完成前域名邮箱登记应保留：%s", mailbox.Email)
			}
		}
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client
	ids := []string{mailboxes[0].ID, mailboxes[1].ID}

	result, err := service.CleanDomainRemoteMessages(context.Background(), ids, true)
	if err != nil {
		t.Fatalf("清理域名邮箱邮件失败：%v", err)
	}
	if !reflect.DeepEqual(client.operations, []string{"扫描远端邮件", "移入废纸篓并彻底删除"}) {
		t.Fatalf("域名邮箱只应删除主号邮件，不应删除 iCloud 邮箱对象：%v", client.operations)
	}
	if result.CloudMessagesFound != 2 || result.MovedToTrash != 2 || result.Destroyed != 2 || result.LocalRemoved != 2 || result.DeletedMailboxes != 0 {
		t.Fatalf("域名邮件清理统计不正确：%+v", result)
	}
	for _, mailbox := range mailboxes {
		if len(state.MessagesForMailbox(mailbox.ID)) != 0 {
			t.Fatalf("云端完成后本地邮件未清理：%s", mailbox.Email)
		}
		if _, found := state.FindMailboxByID(mailbox.ID); !found {
			t.Fatalf("“全部删除邮件”应保留地址登记：%s", mailbox.Email)
		}
	}
}

func TestDeleteDomainMailboxesRemovesLocalRecordsAfterCloudMail(t *testing.T) {
	state, mailboxes := newDomainDeleteServiceFixture(t, 2)
	client := &remoteMailboxDeleteClientFixture{}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	result, err := service.DeleteDomainMailboxesWithRemoteMessages(context.Background(), []string{mailboxes[0].ID, mailboxes[1].ID})
	if err != nil {
		t.Fatalf("删除域名邮箱失败：%v", err)
	}
	if !reflect.DeepEqual(client.operations, []string{"扫描远端邮件", "移入废纸篓并彻底删除"}) || result.MovedToTrash != 2 || result.Destroyed != 2 || result.LocalRemoved != 2 || result.DeletedMailboxes != 2 {
		t.Fatalf("域名邮箱删除流程不正确：operations=%v result=%+v", client.operations, result)
	}
	for _, mailbox := range mailboxes {
		if _, found := state.FindMailboxByID(mailbox.ID); found {
			t.Fatalf("云端邮件删除完成后本地地址登记仍存在：%s", mailbox.Email)
		}
	}
}

func TestDeleteDomainMailboxesKeepsLocalDataWhenCloudDeletionFails(t *testing.T) {
	state, mailboxes := newDomainDeleteServiceFixture(t, 1)
	client := &remoteMailboxDeleteClientFixture{moveErr: errors.New("云端删除测试失败")}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	_, err := service.DeleteDomainMailboxesWithRemoteMessages(context.Background(), []string{mailboxes[0].ID})
	if err == nil || !strings.Contains(err.Error(), "云端删除测试失败") {
		t.Fatalf("未返回云端删除错误：%v", err)
	}
	if _, found := state.FindMailboxByID(mailboxes[0].ID); !found || len(state.MessagesForMailbox(mailboxes[0].ID)) != 1 {
		t.Fatal("云端删除失败时应保留本地邮件和地址登记")
	}
}

func TestDeleteDomainMailboxesKeepsLocalDataWhenTrashDestroyFails(t *testing.T) {
	state, mailboxes := newDomainDeleteServiceFixture(t, 1)
	client := &remoteMailboxDeleteClientFixture{destroyErr: errors.New("废纸篓彻底删除测试失败")}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	result, err := service.DeleteDomainMailboxesWithRemoteMessages(context.Background(), []string{mailboxes[0].ID})
	if err == nil || !strings.Contains(err.Error(), "废纸篓彻底删除测试失败") {
		t.Fatalf("未返回废纸篓彻底删除错误：%v", err)
	}
	if result.MovedToTrash != 1 || result.Destroyed != 0 {
		t.Fatalf("云端删除失败统计不正确：%+v", result)
	}
	if _, found := state.FindMailboxByID(mailboxes[0].ID); !found || len(state.MessagesForMailbox(mailboxes[0].ID)) != 1 {
		t.Fatal("废纸篓彻底删除失败时应保留本地邮件和地址登记")
	}
}

func TestDeleteDomainMailboxUsesCloudRecipientScanInsteadOfLocalMessageCache(t *testing.T) {
	state, mailboxes := newDomainDeleteServiceFixture(t, 1)
	if _, err := state.DeleteMailboxMessages(mailboxes[0].ID); err != nil {
		t.Fatalf("清理本地测试邮件失败：%v", err)
	}
	client := &remoteMailboxDeleteClientFixture{discoveredIDs: map[string][]string{
		mailboxes[0].ID: {"icloud:INBOX:909"},
	}}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	result, err := service.DeleteDomainMailboxesWithRemoteMessages(context.Background(), []string{mailboxes[0].ID})
	if err != nil {
		t.Fatalf("删除域名邮箱失败：%v", err)
	}
	if !reflect.DeepEqual(client.remoteIDs, []string{"icloud:INBOX:909"}) || result.CloudMessagesFound != 1 || result.MovedToTrash != 1 || result.Destroyed != 1 {
		t.Fatalf("未按云端收件人扫描结果删除邮件：remoteIDs=%v result=%+v", client.remoteIDs, result)
	}
	if _, found := state.FindMailboxByID(mailboxes[0].ID); found {
		t.Fatal("云端邮件完成后本地域名邮箱登记仍存在")
	}
}

func TestDeleteDomainMailboxUsesCompleteIMAPIndexWhenLocalMessagesAreEmpty(t *testing.T) {
	state, mailboxes := newDomainDeleteServiceFixture(t, 1)
	mailbox := mailboxes[0]
	primeDomainDeleteIMAPHistory(t, state, mailbox, true)
	if _, err := state.DeleteMailboxMessages(mailbox.ID); err != nil {
		t.Fatalf("清理本地测试邮件失败：%v", err)
	}
	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{
		MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}, UIDValidity: "delete-validity", LastUID: "101",
	}}
	client := &remoteMailboxDeleteClientFixture{}
	service := NewService(config.Default(), state)
	service.messageBackend = backend
	service.deleteClient = client

	result, err := service.DeleteDomainMailboxesWithRemoteMessages(context.Background(), []string{mailbox.ID})
	if err != nil {
		t.Fatalf("使用完整 IMAP 索引删除域名邮箱失败：%v", err)
	}
	if !reflect.DeepEqual(client.operations, []string{"移入废纸篓并彻底删除"}) ||
		!reflect.DeepEqual(client.remoteIDs, []string{"imap:101"}) || result.FallbackAccounts != 0 || result.VerifiedMailboxes != 1 {
		t.Fatalf("本地邮件为空时未使用完整 IMAP 索引：operations=%v remoteIDs=%v result=%+v", client.operations, client.remoteIDs, result)
	}
	if _, found := state.FindMailboxByID(mailbox.ID); found {
		t.Fatal("云端索引邮件删除完成后域名邮箱登记仍存在")
	}
}

func TestDeleteDomainMailboxTreatsZeroAsEmptyOnlyAfterIMAPHistoryIsComplete(t *testing.T) {
	state, mailboxes := newDomainDeleteServiceFixture(t, 1)
	mailbox := mailboxes[0]
	primeDomainDeleteIMAPHistory(t, state, mailbox, false)
	if _, err := state.DeleteMailboxMessages(mailbox.ID); err != nil {
		t.Fatalf("清理本地测试邮件失败：%v", err)
	}
	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{
		MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}, UIDValidity: "delete-validity", LastUID: "101",
	}}
	client := &remoteMailboxDeleteClientFixture{}
	service := NewService(config.Default(), state)
	service.messageBackend = backend
	service.deleteClient = client

	result, err := service.DeleteDomainMailboxesWithRemoteMessages(context.Background(), []string{mailbox.ID})
	if err != nil {
		t.Fatalf("删除已确认无邮件的域名邮箱失败：%v", err)
	}
	if len(client.operations) != 0 || result.CloudMessagesFound != 0 || result.FallbackAccounts != 0 || result.VerifiedMailboxes != 1 {
		t.Fatalf("完整历史中的零匹配不应再扫描 Web：operations=%v result=%+v", client.operations, result)
	}
	if _, found := state.FindMailboxByID(mailbox.ID); found {
		t.Fatal("确认云端无邮件后域名邮箱登记仍存在")
	}
}

func (f *remoteMailboxDeleteClientFixture) EmptyTrash(context.Context, protocol.ICloudSession) (int, error) {
	f.operations = append(f.operations, "清空远端废纸篓")
	if f.onEmptyTrash != nil {
		f.onEmptyTrash()
	}
	if f.emptyTrashErr != nil {
		return 0, f.emptyTrashErr
	}
	return 1, nil
}

func TestDeleteCompletelyUsesSyncedMessageCleanupBeforeDeletingMailbox(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{
		remote: protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
	}
	client.onDelete = func() {
		if _, ok := state.FindMailboxByID(mailbox.ID); !ok {
			t.Error("删除 Apple 远端邮箱时，本地邮箱记录应仍然存在")
		}
		if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 0 {
			t.Errorf("删除 Apple 远端邮箱前本地邮件尚未清空：%d 封", len(messages))
		}
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	if err := service.DeleteCompletely(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("彻底删除邮箱失败：%v", err)
	}
	expected := []string{"移动远端邮件", "清空远端废纸篓", "查询远端邮箱", "删除远端邮箱", "查询远端邮箱"}
	if !reflect.DeepEqual(client.operations, expected) {
		t.Fatalf("删除执行顺序不正确：得到 %v，期望 %v", client.operations, expected)
	}
	if !reflect.DeepEqual(client.remoteIDs, []string{"icloud:Inbox:101"}) {
		t.Fatalf("远端邮件标识不正确：%v", client.remoteIDs)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); ok {
		t.Fatal("完成邮件清理和 Apple 删除后，本地邮箱记录仍然存在")
	}
}

func TestDeleteCompletelyStopsWhenMovingRemoteMessagesFails(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{
		remote:  protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
		moveErr: errors.New("远端邮件清理测试失败"),
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	err := service.DeleteCompletely(context.Background(), mailbox.ID)
	if err == nil || !strings.Contains(err.Error(), "删除隐私邮箱前清理已同步的 Apple 远端邮件失败") {
		t.Fatalf("未返回明确的邮件清理错误：%v", err)
	}
	if !reflect.DeepEqual(client.operations, []string{"移动远端邮件"}) {
		t.Fatalf("清理失败后仍执行了后续步骤：%v", client.operations)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); !ok {
		t.Fatal("远端邮件清理失败时不应删除邮箱")
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 2 {
		t.Fatalf("远端邮件清理失败时不应清空本地邮件：剩余 %d 封", len(messages))
	}
}

func TestDeleteCompletelyKeepsLocalDataWhenEmptyTrashFails(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{
		remote:        protocol.ICloudRemoteMailbox{AnonymousID: mailbox.AnonymousID, Email: mailbox.Email},
		emptyTrashErr: errors.New("清空废纸篓测试失败"),
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	err := service.DeleteCompletely(context.Background(), mailbox.ID)
	if err == nil || !strings.Contains(err.Error(), "清空废纸篓测试失败") {
		t.Fatalf("未返回清空废纸篓错误：%v", err)
	}
	if !reflect.DeepEqual(client.operations, []string{"移动远端邮件", "清空远端废纸篓"}) {
		t.Fatalf("清空废纸篓失败后仍执行了后续步骤：%v", client.operations)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); !ok {
		t.Fatal("清空废纸篓失败时本地邮箱记录应保留")
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 2 {
		t.Fatalf("清空废纸篓失败时本地邮件应保留：剩余 %d 封", len(messages))
	}
}

func TestCleanRemoteMessagesUsesSharedSyncedMessageCleanup(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	result, err := service.CleanRemoteMessages(context.Background(), mailbox.ID, RemoteCleanupOptions{
		MoveSynced: true,
		EmptyTrash: true,
	})
	if err != nil {
		t.Fatalf("清理 Apple 远端邮件失败：%v", err)
	}
	if !reflect.DeepEqual(client.operations, []string{"移动远端邮件", "清空远端废纸篓"}) {
		t.Fatalf("详情清理执行顺序不正确：%v", client.operations)
	}
	if result.MovedToTrash != 1 || result.Destroyed != 1 || result.LocalRemoved != 1 {
		t.Fatalf("详情清理结果不正确：%+v", result)
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); !ok {
		t.Fatal("详情清理后隐私邮箱记录应保留")
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 1 || messages[0].RemoteID != "" {
		t.Fatalf("详情清理后应仅保留缺少远端标识的本地邮件：%+v", messages)
	}
}

func TestRemoteMailCleanupStopsWhenWebAPIDisabled(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	settings := state.Settings()
	settings.EnableWebRemoteMailCleanup = false
	if _, err := state.SaveSettings(settings); err != nil {
		t.Fatalf("关闭 Web API 远端邮件操作失败：%v", err)
	}
	client := &remoteMailboxDeleteClientFixture{}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	_, err := service.CleanRemoteMessages(context.Background(), mailbox.ID, RemoteCleanupOptions{MoveSynced: true})
	if err == nil || !strings.Contains(err.Error(), "远端邮件操作已关闭") {
		t.Fatalf("关闭 Web API 后的远端清理提示不正确：%v", err)
	}
	if len(client.operations) != 0 {
		t.Fatalf("关闭 Web API 后仍执行了远端操作：%v", client.operations)
	}
}

func TestCleanRemoteMailboxesPurgesAllLocalMessages(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	result, err := service.CleanRemoteMailboxes(context.Background(), RemoteCleanupOptions{
		MoveSynced: true,
		EmptyTrash: true,
		PurgeLocal: true,
	})
	if err != nil {
		t.Fatalf("批量清理邮件失败：%v", err)
	}

	if result.FailedMailboxes != 0 {
		t.Fatalf("全部清理出现失败：%+v", result.Failures)
	}
	if result.Mailboxes != 1 {
		t.Fatalf("处理邮箱数量不正确：得到 %d，期望 1", result.Mailboxes)
	}
	if result.Cleanup.LocalRemoved != 2 {
		t.Fatalf("本地邮件清理数量不正确：得到 %d，期望 2", result.Cleanup.LocalRemoved)
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 0 {
		t.Fatalf("全部清理后仍有本地邮件：%d 封", len(messages))
	}
	if !reflect.DeepEqual(client.remoteIDs, []string{"icloud:Inbox:101"}) {
		t.Fatalf("远端邮件标识不正确：%v", client.remoteIDs)
	}
}

func TestAppleMailCleanupUsesMailboxPoolIndexesAndKeepsMailbox(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	secondMailbox, _, err := state.UpsertMailboxFromRemote(mailbox.AccountID, domain.RemoteMailbox{
		AnonymousID: "anonymous-cleanup-second", Email: "cleanup-second@icloud.com", Label: "cleanup_second", IsActive: true,
	}, "邮箱池清理合并测试")
	if err != nil {
		t.Fatalf("创建第二个邮箱池清理测试邮箱失败：%v", err)
	}
	if _, _, err := state.UpsertMessage(secondMailbox.ID, "imap:102", "imap", "验证码 102", "sender@example.com", "102", time.Now()); err != nil {
		t.Fatalf("保存第二个邮箱池清理测试邮件失败：%v", err)
	}
	_, routes, err := state.SaveDomainMailConfig(domain.DefaultDomainMailSettings(), []domain.DomainMailRoute{{
		Domain: "cleanup.example", ReceiverType: domain.DomainReceiverAppleAccount,
		AccountID: mailbox.AccountID, ForwardToEmail: "delete-fixture@icloud.com",
	}})
	if err != nil || len(routes) != 1 {
		t.Fatalf("创建域名邮箱清理隔离测试路由失败：routes=%+v err=%v", routes, err)
	}
	domainMailboxes, err := state.CreateDomainMailboxes(routes[0].ID, []string{"keep@cleanup.example"}, "域名邮箱", "", true)
	if err != nil || len(domainMailboxes) != 1 {
		t.Fatalf("创建域名邮箱清理隔离测试数据失败：mailboxes=%+v err=%v", domainMailboxes, err)
	}
	if _, _, err := state.UpsertMessage(domainMailboxes[0].ID, "icloud:Inbox:999", "icloud", "域名邮件", "sender@example.com", "保留", time.Now()); err != nil {
		t.Fatalf("保存域名邮箱清理隔离测试邮件失败：%v", err)
	}
	client := &remoteMailboxDeleteClientFixture{}
	client.onMove = func() {
		if firstCount, secondCount := len(state.MessagesForMailbox(mailbox.ID)), len(state.MessagesForMailbox(secondMailbox.ID)); firstCount != 2 || secondCount != 1 {
			t.Errorf("Apple 云端邮件移动完成前，本地邮件应保留：first=%d second=%d", firstCount, secondCount)
		}
	}
	client.onEmptyTrash = func() {
		if firstCount, secondCount := len(state.MessagesForMailbox(mailbox.ID)), len(state.MessagesForMailbox(secondMailbox.ID)); firstCount != 2 || secondCount != 1 {
			t.Errorf("Apple 废纸篓清理完成前，本地邮件应保留：first=%d second=%d", firstCount, secondCount)
		}
	}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	job, err := service.StartAppleMailCleanup(context.Background(), AppleMailCleanupRequest{PurgeLocal: true})
	if err != nil {
		t.Fatalf("启动邮箱池邮件清理失败：%v", err)
	}
	job = waitAppleMailCleanup(t, service, job)

	if !reflect.DeepEqual(client.operations, []string{"移动远端邮件", "清空远端废纸篓"}) {
		t.Fatalf("邮箱池清理不应扫描 Apple 文件夹：%v", client.operations)
	}
	remoteIDs := append([]string(nil), client.remoteIDs...)
	sort.Strings(remoteIDs)
	if !reflect.DeepEqual(remoteIDs, []string{"icloud:Inbox:101", "imap:102"}) {
		t.Fatalf("邮箱池清理未使用已同步的远端标识：%v", client.remoteIDs)
	}
	if job.Status != "completed" || job.TotalMailboxes != 2 || job.SuccessfulMailboxes != 2 || job.Discovered != 2 || job.MovedToTrash != 2 || job.Destroyed != 1 || job.LocalRemoved != 3 {
		t.Fatalf("邮箱池清理统计不正确：%+v", job)
	}
	if _, found := state.FindMailboxByID(mailbox.ID); !found {
		t.Fatal("全部清理邮件后应保留隐私邮箱地址")
	}
	if _, found := state.FindMailboxByID(secondMailbox.ID); !found {
		t.Fatal("全部清理邮件后应保留第二个隐私邮箱地址")
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 0 {
		t.Fatalf("Apple 云端清理成功后，本地邮件仍然存在：%d", len(messages))
	}
	if messages := state.MessagesForMailbox(secondMailbox.ID); len(messages) != 0 {
		t.Fatalf("Apple 云端清理成功后，第二个邮箱的本地邮件仍然存在：%d", len(messages))
	}
	if messages := state.MessagesForMailbox(domainMailboxes[0].ID); len(messages) != 1 {
		t.Fatalf("邮箱池清理不应处理域名邮箱邮件：%d", len(messages))
	}
}

func TestAppleMailCleanupKeepsLocalMessagesWhenTrashCleanupFails(t *testing.T) {
	state, mailbox := newDeleteServiceFixture(t)
	client := &remoteMailboxDeleteClientFixture{emptyTrashErr: errors.New("清理废纸篓测试失败")}
	service := NewService(config.Config{}, state)
	service.deleteClient = client

	job, err := service.StartAppleMailCleanup(context.Background(), AppleMailCleanupRequest{PurgeLocal: true})
	if err != nil {
		t.Fatalf("启动邮箱池邮件清理失败：%v", err)
	}
	job = waitAppleMailCleanup(t, service, job)

	if job.Status != "partial" || job.FailedMailboxes != 1 || !strings.Contains(job.LastError, "清理废纸篓测试失败") {
		t.Fatalf("远端清理失败状态不正确：%+v", job)
	}
	if _, found := state.FindMailboxByID(mailbox.ID); !found {
		t.Fatal("远端清理失败时应保留隐私邮箱地址")
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 2 {
		t.Fatalf("远端清理失败时应保留本地邮件：%d", len(messages))
	}
}

func TestAppleMailCleanupRejectsWhenWebSessionUnavailable(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID:            "web-unavailable@icloud.com",
		DSID:               "web-unavailable-dsid",
		MailGatewayBaseURL: "https://p1-mccgateway.icloud.com",
	})
	if err != nil {
		t.Fatalf("创建未登录态测试账号失败：%v", err)
	}
	if _, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{
		AnonymousID: "web-unavailable-mailbox",
		Email:       "web-unavailable@icloud.com",
		Label:       "web_unavailable",
		IsActive:    true,
	}, "未登录态清理测试"); err != nil {
		t.Fatalf("创建未登录态测试邮箱失败：%v", err)
	}

	service := NewService(config.Config{}, state)
	_, err = service.StartAppleMailCleanup(context.Background(), AppleMailCleanupRequest{AccountIDs: []string{session.AccountID}, PurgeLocal: true})
	if err == nil || !strings.Contains(err.Error(), "iCloud Web 邮件登录态不可用") {
		t.Fatalf("未登录态清理应在启动前明确提示：%v", err)
	}
	if job := service.AppleMailCleanupStatus(); job.Running || job.Status != "idle" {
		t.Fatalf("未登录态清理不应创建后台任务：%+v", job)
	}
}

func waitAppleMailCleanup(t *testing.T, service *Service, job AppleMailCleanupJob) AppleMailCleanupJob {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for job.Running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		job = service.AppleMailCleanupStatus()
	}
	if job.Running {
		t.Fatal("等待邮箱池邮件清理任务完成超时")
	}
	return job
}

func newDomainDeleteServiceFixture(t *testing.T, count int) (*store.Store, []domain.Mailbox) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建域名删除测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID: "domain-delete@icloud.com", DSID: "domain-delete-dsid", MailGatewayBaseURL: "https://mail.example.test",
		Cookies: []domain.SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-token", Domain: ".icloud.com", Path: "/"}},
	})
	if err != nil {
		t.Fatalf("创建域名删除测试账号失败：%v", err)
	}
	settings := domain.DefaultDomainMailSettings()
	settings.Enabled = true
	_, routes, err := state.SaveDomainMailConfig(settings, []domain.DomainMailRoute{{
		Domain: "delete.example", ReceiverType: domain.DomainReceiverAppleAccount,
		AccountID: session.AccountID, ForwardToEmail: session.AppleID,
	}})
	if err != nil || len(routes) != 1 {
		t.Fatalf("创建域名删除测试路由失败：routes=%+v err=%v", routes, err)
	}
	emails := make([]string, 0, count)
	for index := 0; index < count; index++ {
		emails = append(emails, fmt.Sprintf("delete-%d@delete.example", index+1))
	}
	mailboxes, err := state.CreateDomainMailboxes(routes[0].ID, emails, "域名删除测试", "", true)
	if err != nil {
		t.Fatalf("创建域名删除测试邮箱失败：%v", err)
	}
	for index, mailbox := range mailboxes {
		remoteID := fmt.Sprintf("imap:%d", 101+index)
		if _, _, err := state.UpsertMessage(mailbox.ID, remoteID, "imap", fmt.Sprintf("域名邮件 %d", index+1), "sender@example.com", "正文", time.Now()); err != nil {
			t.Fatalf("创建域名删除测试邮件失败：%v", err)
		}
	}
	return state, mailboxes
}

func primeDomainDeleteIMAPHistory(t *testing.T, state *store.Store, mailbox domain.Mailbox, includeMessage bool) {
	t.Helper()
	session, found := state.ICloudSessionByAccountID(mailbox.AccountID)
	if !found {
		t.Fatal("域名删除测试账号登录态不存在")
	}
	imapState := domain.LoginState{
		Kind: domain.LoginStateICloudIMAP, IMAPEmail: session.AppleID, IMAPUsername: session.AppleID,
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPAppPassword: "delete-app-password",
	}
	session.LoginStates = append(session.LoginStates, imapState)
	if _, err := state.SaveICloudSession(session); err != nil {
		t.Fatalf("保存域名删除测试 IMAP 登录态失败：%v", err)
	}
	commit := store.MailIndexCommit{SourceKey: mailbox.AccountID, Folder: "INBOX", UIDValidity: "delete-validity"}
	if includeMessage {
		commit.Entries = []store.MailIndexEntry{{
			UID: "101", RemoteID: "imap:101", RemoteIDs: []string{"imap:101"}, Recipients: []string{mailbox.Email},
			Source: "imap", Subject: "域名邮件", From: "sender@example.com", BodyComplete: true, IndexedAt: time.Now(),
		}}
	}
	completedAt := time.Now()
	commit.Histories = mailboxHistoryCommits([]domain.Mailbox{mailbox}, mailbox.AccountID, "delete-validity", "101", completedAt)
	stateID := mailSyncStateID(mailbox.AccountID, "imap", "source")
	cursor := MailSyncState{
		AccountID: mailbox.AccountID, Method: "imap", Folder: "INBOX", UIDValidity: "delete-validity", LastScannedUID: "101",
		LastSyncAt: completedAt, HistoryComplete: true, LastFullScanAt: completedAt, SourceSignature: imapSourceSignature(imapState),
	}
	updates := []store.MailboxSyncUpdate{{MailboxID: mailbox.ID, LastUID: "101", SyncedAt: completedAt}}
	if _, err := state.ApplyMailboxSyncBatchWithIndex(updates, stateID, cursor, commit); err != nil {
		t.Fatalf("建立域名删除测试 IMAP 历史失败：%v", err)
	}
}

func newDeleteServiceFixture(t *testing.T) (*store.Store, domain.Mailbox) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{
		AppleID:            "delete-fixture@icloud.com",
		DSID:               "fixture-dsid",
		MailGatewayBaseURL: "https://mail.example.test",
		Cookies:            []domain.SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-token"}},
	})
	if err != nil {
		t.Fatalf("创建测试 Apple 登录态失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{
		AnonymousID: "anonymous-delete-fixture",
		Email:       "delete-fixture@icloud.com",
		Label:       "delete_fixture",
		IsActive:    true,
	}, "删除流程测试")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	if _, _, err := state.UpsertMessage(mailbox.ID, "icloud:Inbox:101", "icloud", "验证码 101", "sender@example.com", "101", time.Now()); err != nil {
		t.Fatalf("保存远端测试邮件失败：%v", err)
	}
	if _, _, err := state.UpsertMessage(mailbox.ID, "", "local", "本地缓存", "sender@example.com", "local", time.Now()); err != nil {
		t.Fatalf("保存本地测试邮件失败：%v", err)
	}
	return state, mailbox
}
