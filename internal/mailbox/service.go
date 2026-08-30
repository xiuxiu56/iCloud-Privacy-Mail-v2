package mailbox

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

type Service struct {
	cfg                      config.Config
	store                    *store.Store
	client                   *protocol.ICloudClient
	messageBackend           messageSyncBackend
	deleteClient             remoteMailboxDeleteClient
	createMu                 sync.Mutex
	syncMu                   sync.Mutex
	syncs                    map[string]*syncCall
	cleanupMu                sync.Mutex
	cleanupState             AppleMailCleanupJob
	cleanupMailboxes         map[string]int
	cleanupCancel            context.CancelFunc
	cleanupGeneration        uint64
	messageSyncJobMu         sync.Mutex
	messageSyncJob           ExistingMailboxMessageSyncJob
	messageSyncJobGeneration uint64
}

type remoteMailboxDeleteClient interface {
	ListPrivacyMailboxes(context.Context, protocol.ICloudSession) ([]protocol.ICloudRemoteMailbox, error)
	DeletePrivacyMailbox(context.Context, protocol.ICloudSession, string) error
	FindRemoteMessageIDsByMailbox(context.Context, protocol.ICloudSession, []domain.Mailbox) (protocol.ICloudRemoteMessageDiscoveryResult, error)
	MoveRemoteMessagesToTrash(context.Context, protocol.ICloudSession, []string) (protocol.ICloudMailCleanupResult, error)
	MoveRemoteMessagesToTrashAndDestroy(context.Context, protocol.ICloudSession, []string) (protocol.ICloudMailCleanupResult, error)
	EmptyTrash(context.Context, protocol.ICloudSession) (int, error)
}

const mailboxCodeFreshWindow = 5 * time.Minute

type syncCall struct {
	done      chan struct{}
	signature string
	result    AccountMessageSyncResult
	err       error
}

type messageSyncBackend interface {
	SyncIMAP(context.Context, protocol.LoginState, []domain.Mailbox, protocol.MailSyncOptions) (protocol.MailSyncBatchResult, error)
	FetchIMAP(context.Context, protocol.LoginState, []string) (map[string]protocol.ICloudSyncedMessage, error)
	SyncWeb(context.Context, protocol.ICloudSession, []domain.Mailbox, protocol.MailSyncOptions) (protocol.MailSyncBatchResult, error)
}

type defaultMessageSyncBackend struct {
	client *protocol.ICloudClient
}

func (backend defaultMessageSyncBackend) SyncIMAP(ctx context.Context, state protocol.LoginState, mailboxes []domain.Mailbox, options protocol.MailSyncOptions) (protocol.MailSyncBatchResult, error) {
	return protocol.SyncICloudIMAPMessagesWithOptions(ctx, state, mailboxes, options)
}

func (backend defaultMessageSyncBackend) FetchIMAP(ctx context.Context, state protocol.LoginState, uids []string) (map[string]protocol.ICloudSyncedMessage, error) {
	return protocol.FetchICloudIMAPMessagesByUID(ctx, state, uids)
}

func (backend defaultMessageSyncBackend) SyncWeb(ctx context.Context, session protocol.ICloudSession, mailboxes []domain.Mailbox, options protocol.MailSyncOptions) (protocol.MailSyncBatchResult, error) {
	return backend.client.SyncMailboxMessagesBatchWithOptions(ctx, session, mailboxes, options)
}

type CodeResult struct {
	Email      string    `json:"email"`
	Code       string    `json:"code"`
	Subject    string    `json:"subject"`
	From       string    `json:"from"`
	ReceivedAt time.Time `json:"received_at"`
	MessageID  string    `json:"message_id"`
}

type MessageContentBackfillResult struct {
	Total   int
	Updated int
	Failed  int
}

type ExistingMailboxMessageSyncFailure struct {
	AccountID string `json:"account_id"`
	Account   string `json:"account"`
	Mailboxes int    `json:"mailboxes"`
	Error     string `json:"error"`
}

type MessageSyncOptions struct {
	Mode             protocol.MailSyncMode
	Trigger          string
	After            time.Time
	Limit            int
	FullScan         bool
	FullScanOnFirst  bool
	UseCursor        bool
	TouchMailboxIDs  []string
	AllowWebAPI      bool
	AllowFallback    bool
	UseWebComplement bool
	Reconcile        bool
}

type AccountMessageSyncResult struct {
	AccountID         string `json:"account_id"`
	Account           string `json:"account"`
	Mailboxes         int    `json:"mailboxes"`
	Method            string `json:"method,omitempty"`
	FallbackUsed      bool   `json:"fallback_used"`
	FallbackReason    string `json:"fallback_reason,omitempty"`
	Scanned           int    `json:"scanned"`
	Matched           int    `json:"matched"`
	SyncedMessages    int    `json:"synced_messages"`
	HasMore           bool   `json:"has_more"`
	InitializedCursor bool   `json:"initialized_cursor"`
	Error             string `json:"error,omitempty"`
}

type MailboxMessageSyncBatchResult struct {
	Accounts       []AccountMessageSyncResult `json:"accounts,omitempty"`
	SyncedMessages int                        `json:"synced_messages"`
	Scanned        int                        `json:"scanned"`
	Matched        int                        `json:"matched"`
	IMAPAccounts   int                        `json:"imap_accounts"`
	WebAPIAccounts int                        `json:"web_api_accounts"`
	Fallbacks      int                        `json:"fallbacks"`
	HasMore        bool                       `json:"has_more"`
}

type MailSyncState struct {
	AccountID       string    `json:"account_id"`
	Method          string    `json:"method"`
	Folder          string    `json:"folder"`
	UIDValidity     string    `json:"uid_validity,omitempty"`
	LastScannedUID  string    `json:"last_scanned_uid,omitempty"`
	LastSyncAt      time.Time `json:"last_sync_at,omitempty"`
	HistoryComplete bool      `json:"history_complete"`
	LastFullScanAt  time.Time `json:"last_full_scan_at,omitempty"`
	LastReconcileAt time.Time `json:"last_reconcile_at,omitempty"`
	SourceSignature string    `json:"source_signature,omitempty"`
}

const mailReconcileInterval = 24 * time.Hour

// MailReconcileDue 判断某个物理收件箱是否需要执行近期对账扫描。
func (s *Service) MailReconcileDue(sourceKey string, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	var state MailSyncState
	found, err := s.store.LoadRuntimeState(mailSyncStateID(sourceKey, "imap", "source"), &state)
	if err != nil || !found || !state.HistoryComplete {
		return true
	}
	last := state.LastReconcileAt
	if last.IsZero() {
		last = state.LastFullScanAt
	}
	return last.IsZero() || now.Sub(last) >= mailReconcileInterval
}

type ExistingMailboxMessageSyncResult struct {
	TotalAccounts      int                                 `json:"total_accounts"`
	TotalMailboxes     int                                 `json:"total_mailboxes"`
	SkippedMailboxes   int                                 `json:"skipped_mailboxes"`
	SuccessfulAccounts int                                 `json:"successful_accounts"`
	FailedAccounts     int                                 `json:"failed_accounts"`
	SyncedMessages     int                                 `json:"synced_messages"`
	Scanned            int                                 `json:"scanned"`
	Matched            int                                 `json:"matched"`
	IMAPAccounts       int                                 `json:"imap_accounts"`
	WebAPIAccounts     int                                 `json:"web_api_accounts"`
	Fallbacks          int                                 `json:"fallbacks"`
	HasMore            bool                                `json:"has_more"`
	Accounts           []AccountMessageSyncResult          `json:"accounts,omitempty"`
	Failures           []ExistingMailboxMessageSyncFailure `json:"failures,omitempty"`
}

type ExistingMailboxMessageSyncJob struct {
	ID                 string                              `json:"id,omitempty"`
	Running            bool                                `json:"running"`
	Status             string                              `json:"status"`
	Stage              string                              `json:"stage,omitempty"`
	TotalAccounts      int                                 `json:"total_accounts"`
	TotalMailboxes     int                                 `json:"total_mailboxes"`
	SkippedMailboxes   int                                 `json:"skipped_mailboxes"`
	Queued             int                                 `json:"queued"`
	Active             int                                 `json:"active"`
	CompletedAccounts  int                                 `json:"completed_accounts"`
	SuccessfulAccounts int                                 `json:"successful_accounts"`
	FailedAccounts     int                                 `json:"failed_accounts"`
	SyncedMessages     int                                 `json:"synced_messages"`
	Scanned            int                                 `json:"scanned"`
	Matched            int                                 `json:"matched"`
	IMAPAccounts       int                                 `json:"imap_accounts"`
	WebAPIAccounts     int                                 `json:"web_api_accounts"`
	Fallbacks          int                                 `json:"fallbacks"`
	HasMore            bool                                `json:"has_more"`
	LastError          string                              `json:"last_error,omitempty"`
	Failures           []ExistingMailboxMessageSyncFailure `json:"failures,omitempty"`
	StartedAt          time.Time                           `json:"started_at,omitempty"`
	UpdatedAt          time.Time                           `json:"updated_at,omitempty"`
	CompletedAt        time.Time                           `json:"completed_at,omitempty"`
}

type existingMailboxMessageSyncTarget struct {
	index     int
	accountID string
	mailboxes []domain.Mailbox
}

type CodeQuery struct {
	After         time.Time
	Keyword       string
	SkipMessageID string
	IncludeServed bool
	MarkAsServed  bool
}

type RemoteCleanupOptions struct {
	AccountID  string `json:"account_id,omitempty"`
	MoveSynced bool   `json:"move_synced"`
	EmptyTrash bool   `json:"empty_trash"`
	PurgeLocal bool   `json:"purge_local,omitempty"`
}

type RemoteCleanupFailure struct {
	MailboxID string `json:"mailbox_id"`
	Email     string `json:"email"`
	Error     string `json:"error"`
}

type RemoteCleanupBatchResult struct {
	Cleanup         protocol.ICloudMailCleanupResult `json:"cleanup"`
	Mailboxes       int                              `json:"mailboxes"`
	FailedMailboxes int                              `json:"failed_mailboxes"`
	Failures        []RemoteCleanupFailure           `json:"failures,omitempty"`
}

type DomainRemoteCleanupResult struct {
	Accounts           int `json:"accounts"`
	Mailboxes          int `json:"mailboxes"`
	SyncScanned        int `json:"sync_scanned"`
	VerifiedMailboxes  int `json:"verified_mailboxes"`
	FallbackAccounts   int `json:"fallback_accounts"`
	ThreadsScanned     int `json:"threads_scanned"`
	CloudMessagesFound int `json:"cloud_messages_found"`
	MovedToTrash       int `json:"moved_to_trash"`
	Destroyed          int `json:"destroyed"`
	Skipped            int `json:"skipped"`
	LocalRemoved       int `json:"local_removed"`
	DeletedMailboxes   int `json:"deleted_mailboxes"`
}

type AppleMailCleanupRequest struct {
	AccountIDs []string `json:"account_ids,omitempty"`
	Scope      string   `json:"scope,omitempty"`
	Strategy   string   `json:"strategy,omitempty"`
	PurgeLocal bool     `json:"purge_local"`
}

type AppleMailCleanupFailure struct {
	AccountID string `json:"account_id"`
	AppleID   string `json:"apple_id,omitempty"`
	Error     string `json:"error"`
}

type AppleMailCleanupJob struct {
	ID                  string                    `json:"id,omitempty"`
	Running             bool                      `json:"running"`
	Status              string                    `json:"status"`
	Stage               string                    `json:"stage,omitempty"`
	AccountIDs          []string                  `json:"account_ids,omitempty"`
	TotalAccounts       int                       `json:"total_accounts"`
	TotalMailboxes      int                       `json:"total_mailboxes"`
	CompletedMailboxes  int                       `json:"completed_mailboxes"`
	SuccessfulMailboxes int                       `json:"successful_mailboxes"`
	FailedMailboxes     int                       `json:"failed_mailboxes"`
	Queued              int                       `json:"queued"`
	Active              int                       `json:"active"`
	Completed           int                       `json:"completed"`
	Success             int                       `json:"success"`
	Failed              int                       `json:"failed"`
	CurrentAccountID    string                    `json:"current_account_id,omitempty"`
	CurrentAppleID      string                    `json:"current_apple_id,omitempty"`
	CurrentFolder       string                    `json:"current_folder,omitempty"`
	FoldersScanned      int                       `json:"folders_scanned"`
	Discovered          int                       `json:"discovered"`
	MovedToTrash        int                       `json:"moved_to_trash"`
	Destroyed           int                       `json:"destroyed"`
	LocalRemoved        int                       `json:"local_removed"`
	LastError           string                    `json:"last_error,omitempty"`
	Failures            []AppleMailCleanupFailure `json:"failures,omitempty"`
	StartedAt           time.Time                 `json:"started_at,omitempty"`
	UpdatedAt           time.Time                 `json:"updated_at,omitempty"`
	CompletedAt         time.Time                 `json:"completed_at,omitempty"`
}

const appleMailCleanupStateID = "apple-mail-cleanup"
const existingMailboxMessageSyncStateID = "mailbox-message-sync"

func NewService(cfg config.Config, state *store.Store) *Service {
	client := protocol.NewICloudClient()
	service := &Service{cfg: cfg, store: state, client: client, messageBackend: defaultMessageSyncBackend{client: client}, deleteClient: client, syncs: make(map[string]*syncCall), cleanupState: AppleMailCleanupJob{Status: "idle"}, cleanupMailboxes: make(map[string]int), messageSyncJob: ExistingMailboxMessageSyncJob{Status: "idle"}}
	if state != nil {
		var persisted AppleMailCleanupJob
		if found, err := state.LoadRuntimeState(appleMailCleanupStateID, &persisted); err == nil && found {
			service.cleanupState = persisted
			if service.cleanupState.Running {
				service.cleanupState.Running = false
				service.cleanupState.Status = "interrupted"
				service.cleanupState.Stage = "interrupted"
				service.cleanupState.Active = 0
				service.cleanupState.LastError = "服务重启，未完成的邮件清理任务已停止，请确认云端状态后重新执行"
				service.cleanupState.UpdatedAt = time.Now()
				service.cleanupState.CompletedAt = time.Now()
				_ = state.SaveRuntimeState(appleMailCleanupStateID, service.cleanupState, true)
			}
		}
		var persistedSync ExistingMailboxMessageSyncJob
		if found, err := state.LoadRuntimeState(existingMailboxMessageSyncStateID, &persistedSync); err == nil && found {
			service.messageSyncJob = persistedSync
			if service.messageSyncJob.Running {
				service.messageSyncJob.Running = false
				service.messageSyncJob.Status = "interrupted"
				service.messageSyncJob.Stage = "interrupted"
				service.messageSyncJob.Active = 0
				service.messageSyncJob.Queued = 0
				service.messageSyncJob.LastError = "服务重启，未完成的邮件同步任务已停止，请重新执行"
				service.messageSyncJob.UpdatedAt = time.Now()
				service.messageSyncJob.CompletedAt = time.Now()
				_ = state.SaveRuntimeState(existingMailboxMessageSyncStateID, service.messageSyncJob, true)
			}
		}
	}
	return service
}

// ImportLocal 把已有隐私邮箱绑定到 Apple 账号，不调用 Apple 创建接口。
func (s *Service) ImportLocal(accountID, email, label, note string) (domain.Mailbox, bool, error) {
	accountID = strings.TrimSpace(accountID)
	if _, ok := s.store.FindAppleAccount(accountID); !ok {
		return domain.Mailbox{}, false, errors.New("Apple 账号不存在")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(strings.TrimSpace(address.Address), email) {
		return domain.Mailbox{}, false, errors.New("邮箱地址格式不正确")
	}
	mailbox, created, err := s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Email: email, Label: strings.TrimSpace(label), IsActive: true, Origin: "manual",
	}, firstNonEmpty(note, "手动导入本地邮箱"))
	return mailbox, created, err
}

func (s *Service) Create(ctx context.Context, accountID, label, note, channel string) (domain.Mailbox, error) {
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		return domain.Mailbox{}, errors.New("Apple 账号登录态不存在")
	}
	s.createMu.Lock()
	defer s.createMu.Unlock()
	label = s.store.NextMailboxLabel(label)
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		channel = "auto"
	}
	var remote protocol.ICloudRemoteMailbox
	var err error
	if channel == "auto" || channel == "apple_account" {
		if _, saved := protocol.LoginStateForKind(session, domain.LoginStateAppleAccount); saved {
			var updated domain.ICloudSession
			remote, updated, err = s.client.CreatePrivacyMailboxWithAppleAccount(ctx, session, s.cfg.AppleAccountAPIKey, label, note)
			if err == nil {
				_, _ = s.store.SaveICloudSession(updated)
			}
		} else if channel == "apple_account" {
			err = errors.New("该账号没有 Apple Account 新接口登录态")
		}
	}
	if remote.Email == "" && (channel == "auto" || channel == "icloud_web") {
		remote, err = s.client.CreatePrivacyMailbox(ctx, session, label, note)
	}
	if err != nil {
		return domain.Mailbox{}, err
	}
	mailbox, _, err := s.store.UpsertMailboxFromRemote(accountID, remoteMailbox(remote), note)
	return mailbox, err
}

func (s *Service) SyncRemote(ctx context.Context, accountID string) ([]domain.Mailbox, error) {
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		return nil, errors.New("Apple 账号登录态不存在")
	}
	remotes, err := s.client.ListPrivacyMailboxes(ctx, session)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Mailbox, 0, len(remotes))
	for _, remote := range remotes {
		mailbox, _, saveErr := s.store.UpsertMailboxFromRemote(accountID, remoteMailbox(remote), "从 Apple 服务器同步")
		if saveErr != nil {
			return out, saveErr
		}
		out = append(out, mailbox)
	}
	return out, nil
}

// DeleteCompletely 使用统一流程清理已同步的远端邮件、删除 Apple 隐私邮箱并清理本地数据。
func (s *Service) DeleteCompletely(ctx context.Context, mailboxID string) error {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return errors.New("邮箱不存在")
	}
	if mailboxKind(mailbox) == domain.MailboxKindDomainForward {
		_, err := s.DeleteDomainMailboxesWithRemoteMessages(ctx, []string{mailboxID})
		return err
	}
	if !s.store.Settings().EnableWebRemoteMailCleanup {
		return errors.New("iCloud Web API 远端邮件操作已关闭，请在系统设置中开启后再彻底删除邮箱")
	}
	session, ok := s.store.ICloudSessionByAccountID(mailbox.AccountID)
	if !ok {
		return errors.New("对应 Apple 账号登录态不存在")
	}
	client := s.deleteClient
	if client == nil {
		client = s.client
	}
	if _, err := s.cleanRemoteMessages(ctx, client, mailbox, session, RemoteCleanupOptions{
		MoveSynced: true,
		EmptyTrash: true,
		PurgeLocal: true,
	}); err != nil {
		return fmt.Errorf("删除隐私邮箱前清理已同步的 Apple 远端邮件失败：%w", err)
	}
	remotes, err := client.ListPrivacyMailboxes(ctx, session)
	if err != nil {
		return err
	}
	remote, exists := matchRemote(remotes, mailbox.AnonymousID, mailbox.Email)
	if exists {
		if err := client.DeletePrivacyMailbox(ctx, session, remote.AnonymousID); err != nil {
			return err
		}
	}
	confirmed, err := client.ListPrivacyMailboxes(ctx, session)
	if err != nil {
		return fmt.Errorf("Apple 删除后确认失败：%w", err)
	}
	if _, stillExists := matchRemote(confirmed, firstNonEmpty(remote.AnonymousID, mailbox.AnonymousID), mailbox.Email); stillExists {
		return errors.New("Apple 服务器仍然存在该隐私邮箱，本地记录已保留")
	}
	return s.store.DeleteMailbox(mailboxID)
}

func (s *Service) DeleteLocal(mailboxID string) error {
	if _, err := s.store.DeleteMailboxMessages(mailboxID); err != nil {
		return fmt.Errorf("删除邮箱前清理本地邮件失败：%w", err)
	}
	return s.store.DeleteMailbox(mailboxID)
}

func (s *Service) CleanRemoteMessages(ctx context.Context, mailboxID string, options RemoteCleanupOptions) (protocol.ICloudMailCleanupResult, error) {
	if !s.store.Settings().EnableWebRemoteMailCleanup {
		return protocol.ICloudMailCleanupResult{}, errors.New("iCloud Web API 远端邮件操作已关闭")
	}
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return protocol.ICloudMailCleanupResult{}, errors.New("邮箱不存在")
	}
	session, ok := s.store.ICloudSessionByAccountID(mailbox.AccountID)
	if !ok {
		return protocol.ICloudMailCleanupResult{}, errors.New("对应 Apple 账号登录态不存在")
	}
	options = normalizeRemoteCleanupOptions(options)
	client := s.deleteClient
	if client == nil {
		client = s.client
	}
	return s.cleanRemoteMessages(ctx, client, mailbox, session, options)
}

// cleanRemoteMessages 是详情清理和彻底删除共同使用的已同步邮件清理实现。
func (s *Service) cleanRemoteMessages(ctx context.Context, client remoteMailboxDeleteClient, mailbox domain.Mailbox, session protocol.ICloudSession, options RemoteCleanupOptions) (protocol.ICloudMailCleanupResult, error) {
	result := protocol.ICloudMailCleanupResult{}
	indexedRemoteIDs := remoteMessageIDs(s.store.MessagesForMailbox(mailbox.ID))
	if options.MoveSynced {
		moved, err := client.MoveRemoteMessagesToTrash(ctx, session, indexedRemoteIDs)
		result.MovedToTrash += moved.MovedToTrash
		result.Skipped += moved.Skipped
		if err != nil {
			return result, err
		}
		result.MovedRemoteIDs = append(result.MovedRemoteIDs, moved.MovedRemoteIDs...)
		result.AbsentRemoteIDs = append(result.AbsentRemoteIDs, moved.AbsentRemoteIDs...)
	}
	if options.EmptyTrash {
		destroyed, err := client.EmptyTrash(ctx, session)
		result.Destroyed += destroyed
		if err != nil {
			return result, err
		}
	}
	if options.MoveSynced {
		if err := s.store.DeleteMailIndexByRemoteIDs(firstNonEmpty(session.AccountID, mailbox.AccountID), indexedRemoteIDs); err != nil {
			return result, err
		}
	}
	if options.PurgeLocal {
		removed, err := s.store.DeleteMailboxMessages(mailbox.ID)
		if err != nil {
			return result, err
		}
		result.LocalRemoved += removed
	} else if options.MoveSynced {
		localIDs := append(append([]string(nil), result.MovedRemoteIDs...), result.AbsentRemoteIDs...)
		removed, err := s.store.DeleteMailboxMessagesByRemoteIDs(mailbox.ID, localIDs)
		if err != nil {
			return result, err
		}
		result.LocalRemoved += removed
	}
	return result, nil
}

func (s *Service) CleanRemoteMailboxes(ctx context.Context, options RemoteCleanupOptions) (RemoteCleanupBatchResult, error) {
	if !s.store.Settings().EnableWebRemoteMailCleanup {
		return RemoteCleanupBatchResult{}, errors.New("iCloud Web API 远端邮件操作已关闭")
	}
	options = normalizeRemoteCleanupOptions(options)
	options.AccountID = strings.TrimSpace(options.AccountID)
	client := s.deleteClient
	if client == nil {
		client = s.client
	}
	result := RemoteCleanupBatchResult{}
	cleanedTrash := make(map[string]bool)
	for _, mailbox := range s.store.AllMailboxes() {
		if ctx.Err() != nil {
			break
		}
		if options.AccountID != "" && mailbox.AccountID != options.AccountID {
			continue
		}
		if !mailbox.ICloudActive || mailbox.Status == domain.StatusDisabled {
			result.Cleanup.Skipped++
			if options.PurgeLocal {
				removed, err := s.store.DeleteMailboxMessages(mailbox.ID)
				if err != nil {
					result.FailedMailboxes++
					result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				} else {
					result.Cleanup.LocalRemoved += removed
				}
			}
			continue
		}
		session, ok := s.store.ICloudSessionByAccountID(mailbox.AccountID)
		if !ok {
			result.Cleanup.Skipped++
			if options.PurgeLocal {
				removed, err := s.store.DeleteMailboxMessages(mailbox.ID)
				if err != nil {
					result.FailedMailboxes++
					result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				} else {
					result.Cleanup.LocalRemoved += removed
				}
			}
			continue
		}
		if options.MoveSynced {
			moved, err := client.MoveRemoteMessagesToTrash(ctx, session, remoteMessageIDs(s.store.MessagesForMailbox(mailbox.ID)))
			result.Cleanup.MovedToTrash += moved.MovedToTrash
			result.Cleanup.Skipped += moved.Skipped
			if err != nil {
				result.FailedMailboxes++
				result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				if options.PurgeLocal {
					removed, localErr := s.store.DeleteMailboxMessages(mailbox.ID)
					if localErr == nil {
						result.Cleanup.LocalRemoved += removed
					}
				}
				continue
			}
			var removed int
			if options.PurgeLocal {
				removed, err = s.store.DeleteMailboxMessages(mailbox.ID)
			} else {
				localIDs := append(append([]string(nil), moved.MovedRemoteIDs...), moved.AbsentRemoteIDs...)
				removed, err = s.store.DeleteMailboxMessagesByRemoteIDs(mailbox.ID, localIDs)
			}
			if err != nil {
				result.FailedMailboxes++
				result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				continue
			}
			result.Cleanup.LocalRemoved += removed
		} else if options.PurgeLocal {
			removed, err := s.store.DeleteMailboxMessages(mailbox.ID)
			if err != nil {
				result.FailedMailboxes++
				result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				continue
			}
			result.Cleanup.LocalRemoved += removed
		}
		result.Mailboxes++
		sessionKey := firstNonEmpty(session.AccountID, session.DSID, session.AppleID, mailbox.AccountID)
		if options.EmptyTrash && !cleanedTrash[sessionKey] {
			destroyed, err := client.EmptyTrash(ctx, session)
			result.Cleanup.Destroyed += destroyed
			if err != nil {
				result.FailedMailboxes++
				result.Failures = append(result.Failures, RemoteCleanupFailure{MailboxID: mailbox.ID, Email: mailbox.Email, Error: err.Error()})
				continue
			}
			cleanedTrash[sessionKey] = true
		}
	}
	return result, nil
}

// CleanDomainRemoteMessages 先完成所有 iCloud 主号邮件的云端移除，再清理本地邮件。
func (s *Service) CleanDomainRemoteMessages(ctx context.Context, mailboxIDs []string, purgeLocal bool) (DomainRemoteCleanupResult, error) {
	return s.cleanDomainRemoteMessages(ctx, mailboxIDs, purgeLocal, false)
}

// DeleteDomainMailboxesWithRemoteMessages 在云端邮件处理成功后，删除本地邮件和域名邮箱登记。
func (s *Service) DeleteDomainMailboxesWithRemoteMessages(ctx context.Context, mailboxIDs []string) (DomainRemoteCleanupResult, error) {
	return s.cleanDomainRemoteMessages(ctx, mailboxIDs, true, true)
}

func (s *Service) cleanDomainRemoteMessages(ctx context.Context, mailboxIDs []string, purgeLocal, deleteMailboxes bool) (DomainRemoteCleanupResult, error) {
	if !s.store.Settings().EnableWebRemoteMailCleanup {
		return DomainRemoteCleanupResult{}, errors.New("iCloud Web API 远端邮件操作已关闭")
	}
	wanted := make(map[string]bool, len(mailboxIDs))
	for _, id := range mailboxIDs {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}
	if len(wanted) == 0 {
		return DomainRemoteCleanupResult{}, errors.New("请至少选择一个域名邮箱")
	}
	type cleanupGroup struct {
		session   protocol.ICloudSession
		mailboxes []domain.Mailbox
		remoteIDs []string
		movedIDs  []string
		absentIDs []string
	}
	groups := make(map[string]*cleanupGroup)
	groupOrder := make([]string, 0)
	orderedIDs := make([]string, 0, len(wanted))
	for id := range wanted {
		orderedIDs = append(orderedIDs, id)
	}
	sort.Strings(orderedIDs)
	mailboxesByID := make(map[string]domain.Mailbox, len(orderedIDs))
	mailboxGroupKeys := make(map[string]string, len(orderedIDs))
	for _, id := range orderedIDs {
		mailbox, found := s.store.FindMailboxByID(id)
		if !found || mailboxKind(mailbox) != domain.MailboxKindDomainForward {
			return DomainRemoteCleanupResult{}, errors.New("选中项中包含不存在的域名邮箱")
		}
		route, found := s.store.DomainMailRouteForSync(mailbox.DomainRouteID)
		if !found || route.ReceiverType != domain.DomainReceiverAppleAccount {
			return DomainRemoteCleanupResult{}, fmt.Errorf("%s 使用标准 IMAP，云端删除暂只支持 iCloud Web API；本地数据已保留", mailbox.Email)
		}
		session, found := s.store.ICloudSessionByAccountID(mailbox.AccountID)
		if !found || !protocol.CanUseICloudWebMail(session) {
			return DomainRemoteCleanupResult{}, fmt.Errorf("%s 对应的 iCloud Web 邮件登录态不可用", mailbox.Email)
		}
		key := firstNonEmpty(session.AccountID, mailbox.AccountID, session.DSID)
		group := groups[key]
		if group == nil {
			group = &cleanupGroup{session: session}
			groups[key] = group
			groupOrder = append(groupOrder, key)
		}
		group.mailboxes = append(group.mailboxes, mailbox)
		mailboxesByID[id] = mailbox
		mailboxGroupKeys[id] = key
	}
	client := s.deleteClient
	if client == nil {
		client = s.client
	}
	result := DomainRemoteCleanupResult{Accounts: len(groups), Mailboxes: len(wanted)}
	// 先扫描每个 iCloud 主号，按域名收件地址查找真实云端邮件。
	// 这里不依赖本地邮件缓存，因为未同步到本地的邮件也需要删除。
	for _, key := range groupOrder {
		group := groups[key]
		remoteIDs, syncScanned, verified, prepareErr := s.prepareDomainDeleteGroup(ctx, key, group.session, group.mailboxes)
		result.SyncScanned += syncScanned
		if verified {
			result.VerifiedMailboxes += len(group.mailboxes)
			group.remoteIDs = append(group.remoteIDs, remoteIDs...)
		} else {
			result.FallbackAccounts++
			discovered, err := client.FindRemoteMessageIDsByMailbox(ctx, group.session, group.mailboxes)
			if err != nil {
				if prepareErr != nil {
					return result, fmt.Errorf("补齐 %s 的 IMAP 索引失败：%v；iCloud Web 定位也失败：%w", firstNonEmpty(group.session.AppleID, group.session.AccountID, key), prepareErr, err)
				}
				return result, fmt.Errorf("扫描 %s 的 iCloud 云端邮件失败：%w", firstNonEmpty(group.session.AppleID, group.session.AccountID, key), err)
			}
			result.ThreadsScanned += discovered.ThreadsScanned
			for _, mailbox := range group.mailboxes {
				group.remoteIDs = append(group.remoteIDs, discovered.RemoteIDsByMailbox[mailbox.ID]...)
			}
		}
		group.remoteIDs = uniqueStrings(group.remoteIDs)
		result.CloudMessagesFound += len(group.remoteIDs)
	}

	// 云端阶段：任一主号失败时，不删除任何本地邮件或邮箱登记。
	for _, key := range groupOrder {
		group := groups[key]
		if len(group.remoteIDs) == 0 {
			continue
		}
		moved, err := client.MoveRemoteMessagesToTrashAndDestroy(ctx, group.session, group.remoteIDs)
		result.MovedToTrash += moved.MovedToTrash
		result.Destroyed += moved.Destroyed
		result.Skipped += moved.Skipped
		if err != nil {
			return result, err
		}
		group.movedIDs = append(group.movedIDs, moved.MovedRemoteIDs...)
		group.absentIDs = append(group.absentIDs, moved.AbsentRemoteIDs...)
	}
	for _, key := range groupOrder {
		group := groups[key]
		emails := make([]string, 0, len(group.mailboxes))
		for _, mailbox := range group.mailboxes {
			emails = append(emails, mailbox.Email)
		}
		if err := s.store.DeleteMailIndexForAddresses(key, emails); err != nil {
			return result, err
		}
	}

	// 本地阶段：只有全部云端主号都完成后才开始执行。
	for _, id := range orderedIDs {
		mailbox := mailboxesByID[id]
		if deleteMailboxes {
			result.LocalRemoved += len(s.store.MessagesForMailbox(mailbox.ID))
			if err := s.DeleteLocal(mailbox.ID); err != nil {
				return result, err
			}
			result.DeletedMailboxes++
			continue
		}
		var removed int
		var err error
		if purgeLocal {
			removed, err = s.store.DeleteMailboxMessages(mailbox.ID)
		} else {
			group := groups[mailboxGroupKeys[id]]
			confirmedIDs := append(append([]string(nil), group.movedIDs...), group.absentIDs...)
			removed, err = s.store.DeleteMailboxMessagesByRemoteIDs(mailbox.ID, confirmedIDs)
		}
		if err != nil {
			return result, err
		}
		result.LocalRemoved += removed
	}
	return result, nil
}

// prepareDomainDeleteGroup 在删除前把共享 IMAP 索引推进到当前收件箱快照。
// 只有邮箱历史覆盖完整，且共享索引与本地邮件都能还原远端标识时，才跳过 Web 全量定位。
func (s *Service) prepareDomainDeleteGroup(ctx context.Context, sourceKey string, session protocol.ICloudSession, mailboxes []domain.Mailbox) ([]string, int, bool, error) {
	imapState, saved := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP)
	if !saved || strings.TrimSpace(imapState.IMAPEmail) == "" || strings.TrimSpace(imapState.IMAPAppPassword) == "" {
		return nil, 0, false, nil
	}

	scanned := 0
	const maxSyncBatches = 100
	for batch := 0; batch < maxSyncBatches; batch++ {
		result, err := s.SyncMailboxBatchWithOptions(ctx, mailboxes, MessageSyncOptions{
			Mode: protocol.MailSyncModeAllRecent, Trigger: "domain-delete-prepare",
			FullScanOnFirst: true, UseCursor: true,
		})
		scanned += result.Scanned
		if err != nil {
			return nil, scanned, false, err
		}
		if !result.HasMore {
			break
		}
		if batch == maxSyncBatches-1 {
			return nil, scanned, false, errors.New("IMAP 增量邮件过多，本次未能推进到当前 UID 快照")
		}
	}

	var cursor MailSyncState
	found, err := s.store.LoadRuntimeState(mailSyncStateID(sourceKey, "imap", "source"), &cursor)
	if err != nil {
		return nil, scanned, false, err
	}
	if !found || !cursor.HistoryComplete || cursor.SourceSignature != imapSourceSignature(imapState) {
		return nil, scanned, false, nil
	}

	mailboxIDs := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		mailboxIDs = append(mailboxIDs, mailbox.ID)
	}
	histories := s.store.MailboxHistoryStates(mailboxIDs)
	indexed, err := s.store.IndexedMailForMailboxes(sourceKey, cursor.UIDValidity, mailboxes)
	if err != nil {
		return nil, scanned, false, err
	}
	remoteIDs := make([]string, 0)
	for _, mailbox := range mailboxes {
		history, ok := histories[mailbox.ID]
		if !ok || !history.Complete || history.SourceKey != sourceKey ||
			!strings.EqualFold(history.Email, mailbox.Email) ||
			history.UIDValidity != cursor.UIDValidity ||
			imapUIDNumber(history.HistoryThroughUID) < imapUIDNumber(cursor.LastScannedUID) {
			return nil, scanned, false, nil
		}
		ids, complete := verifiedIndexedRemoteMessageIDs(indexed[mailbox.ID])
		if !complete {
			return nil, scanned, false, nil
		}
		remoteIDs = append(remoteIDs, ids...)
		ids, complete = verifiedRemoteMessageIDs(s.store.MessagesForMailbox(mailbox.ID))
		if !complete {
			return nil, scanned, false, nil
		}
		remoteIDs = append(remoteIDs, ids...)
	}
	return uniqueStrings(remoteIDs), scanned, true, nil
}

func verifiedIndexedRemoteMessageIDs(entries []store.MailIndexEntry) ([]string, bool) {
	remoteIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		recognized := false
		candidates := append(append([]string(nil), entry.RemoteIDs...), entry.RemoteID)
		if uid := strings.TrimSpace(entry.UID); uid != "" {
			candidates = append(candidates, "imap:"+strings.TrimPrefix(uid, "imap:"))
		}
		for _, remoteID := range candidates {
			remoteID = strings.TrimSpace(remoteID)
			if !recognizedDomainRemoteID(remoteID) {
				continue
			}
			recognized = true
			remoteIDs = append(remoteIDs, remoteID)
		}
		if !recognized {
			return nil, false
		}
	}
	return uniqueStrings(remoteIDs), true
}

func verifiedRemoteMessageIDs(messages []domain.Message) ([]string, bool) {
	remoteIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		recognized := false
		for _, remoteID := range append(append([]string(nil), message.RemoteIDs...), message.RemoteID) {
			remoteID = strings.TrimSpace(remoteID)
			if !recognizedDomainRemoteID(remoteID) {
				continue
			}
			recognized = true
			remoteIDs = append(remoteIDs, remoteID)
		}
		if !recognized {
			return nil, false
		}
	}
	return uniqueStrings(remoteIDs), true
}

func recognizedDomainRemoteID(remoteID string) bool {
	remoteID = strings.TrimSpace(remoteID)
	if strings.HasPrefix(remoteID, "imap:") {
		return imapUIDNumber(remoteID) > 0
	}
	if !strings.HasPrefix(remoteID, "icloud:") {
		return false
	}
	rest := strings.TrimPrefix(remoteID, "icloud:")
	folder, uid, found := strings.Cut(rest, ":")
	return found && strings.TrimSpace(folder) != "" && strings.TrimSpace(uid) != ""
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (s *Service) StartAppleMailCleanup(parent context.Context, request AppleMailCleanupRequest) (AppleMailCleanupJob, error) {
	if !s.store.Settings().EnableWebRemoteMailCleanup {
		return AppleMailCleanupJob{}, errors.New("iCloud Web API 远端邮件操作已关闭")
	}
	request.Scope = strings.ToLower(strings.TrimSpace(request.Scope))
	if request.Scope == "" {
		request.Scope = "all"
	}
	if request.Scope != "all" {
		return AppleMailCleanupJob{}, errors.New("当前只支持清理全部 Apple 云端邮件")
	}
	request.Strategy = strings.ToLower(strings.TrimSpace(request.Strategy))
	if request.Strategy == "" {
		request.Strategy = "move_then_destroy"
	}
	if request.Strategy != "move_then_destroy" {
		return AppleMailCleanupJob{}, errors.New("当前只支持先移入废纸篓再彻底删除")
	}
	accountIDs, err := s.cleanupAccountIDs(request.AccountIDs)
	if err != nil {
		return AppleMailCleanupJob{}, err
	}
	mailboxCounts, totalMailboxes := s.cleanupMailboxCounts(accountIDs)
	if parent == nil {
		parent = context.Background()
	}

	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if s.cleanupState.Running {
		return AppleMailCleanupJob{}, errors.New("全部 Apple 邮件清理任务正在运行")
	}
	ctx, cancel := context.WithCancel(parent)
	s.cleanupCancel = cancel
	s.cleanupMailboxes = mailboxCounts
	s.cleanupGeneration++
	generation := s.cleanupGeneration
	now := time.Now()
	s.cleanupState = AppleMailCleanupJob{
		ID:             fmt.Sprintf("apple_mail_cleanup_%d", now.UnixNano()),
		Running:        true,
		Status:         "queued",
		Stage:          "queued",
		AccountIDs:     append([]string(nil), accountIDs...),
		TotalAccounts:  len(accountIDs),
		TotalMailboxes: totalMailboxes,
		Queued:         len(accountIDs),
		Failures:       []AppleMailCleanupFailure{},
		StartedAt:      now,
		UpdatedAt:      now,
	}
	s.publishAppleMailCleanupLocked()
	out := s.appleMailCleanupSnapshotLocked()
	go s.runAppleMailCleanup(ctx, request, accountIDs, generation)
	return out, nil
}

func (s *Service) AppleMailCleanupStatus() AppleMailCleanupJob {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	return s.appleMailCleanupSnapshotLocked()
}

func (s *Service) CancelAppleMailCleanup(message string) AppleMailCleanupJob {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if !s.cleanupState.Running {
		return s.appleMailCleanupSnapshotLocked()
	}
	if s.cleanupCancel != nil {
		s.cleanupCancel()
		s.cleanupCancel = nil
	}
	s.cleanupGeneration++
	now := time.Now()
	s.cleanupState.Running = false
	s.cleanupState.Status = "cancelled"
	s.cleanupState.Stage = "cancelled"
	s.cleanupState.Active = 0
	s.cleanupState.Queued = 0
	s.cleanupState.CurrentFolder = ""
	s.cleanupState.UpdatedAt = now
	s.cleanupState.CompletedAt = now
	if strings.TrimSpace(message) == "" {
		message = "全部 Apple 邮件清理任务已取消"
	}
	s.cleanupState.LastError = strings.TrimSpace(message)
	s.publishAppleMailCleanupLocked()
	return s.appleMailCleanupSnapshotLocked()
}

func (s *Service) runAppleMailCleanup(ctx context.Context, request AppleMailCleanupRequest, accountIDs []string, generation uint64) {
	for index, accountID := range accountIDs {
		if ctx.Err() != nil || !s.beginAppleMailCleanupAccount(generation, accountID, len(accountIDs)-index-1) {
			return
		}
		account, _ := s.store.FindAppleAccount(accountID)
		session, ok := s.store.ICloudSessionByAccountID(accountID)
		if !ok {
			s.finishAppleMailCleanupAccount(generation, accountID, account.AppleID, 0, errors.New("该账号没有可用的 iCloud Web 旧接口登录态"))
			continue
		}
		base := s.appleMailCleanupTotals(generation)
		_, cleanupErr := s.client.CleanAllRemoteMail(ctx, session, func(progress protocol.ICloudAllMailCleanupProgress) {
			s.updateAppleMailCleanupProgress(generation, base, progress)
		})
		localRemoved := 0
		if cleanupErr == nil && request.PurgeLocal {
			localRemoved, cleanupErr = s.store.DeleteAccountMessages(accountID)
			if cleanupErr != nil {
				cleanupErr = fmt.Errorf("Apple 云端邮件已清理，但本地邮件清理失败：%w", cleanupErr)
			}
		}
		if ctx.Err() != nil {
			return
		}
		s.finishAppleMailCleanupAccount(generation, accountID, account.AppleID, localRemoved, cleanupErr)
	}
	s.finishAppleMailCleanupJob(generation)
}

type appleMailCleanupTotals struct {
	FoldersScanned int
	Discovered     int
	MovedToTrash   int
	Destroyed      int
}

func (s *Service) appleMailCleanupTotals(generation uint64) appleMailCleanupTotals {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if generation != s.cleanupGeneration {
		return appleMailCleanupTotals{}
	}
	return appleMailCleanupTotals{
		FoldersScanned: s.cleanupState.FoldersScanned,
		Discovered:     s.cleanupState.Discovered,
		MovedToTrash:   s.cleanupState.MovedToTrash,
		Destroyed:      s.cleanupState.Destroyed,
	}
}

func (s *Service) beginAppleMailCleanupAccount(generation uint64, accountID string, queued int) bool {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if generation != s.cleanupGeneration || !s.cleanupState.Running {
		return false
	}
	account, _ := s.store.FindAppleAccount(accountID)
	s.cleanupState.Status = "running"
	s.cleanupState.Stage = "scanning"
	s.cleanupState.Active = 1
	s.cleanupState.Queued = queued
	s.cleanupState.CurrentAccountID = accountID
	s.cleanupState.CurrentAppleID = account.AppleID
	s.cleanupState.CurrentFolder = ""
	s.cleanupState.UpdatedAt = time.Now()
	s.publishAppleMailCleanupLocked()
	return true
}

func (s *Service) updateAppleMailCleanupProgress(generation uint64, base appleMailCleanupTotals, progress protocol.ICloudAllMailCleanupProgress) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if generation != s.cleanupGeneration || !s.cleanupState.Running {
		return
	}
	s.cleanupState.Stage = progress.Stage
	s.cleanupState.CurrentFolder = progress.Folder
	s.cleanupState.FoldersScanned = base.FoldersScanned + progress.Result.FoldersScanned
	s.cleanupState.Discovered = base.Discovered + progress.Result.Discovered
	s.cleanupState.MovedToTrash = base.MovedToTrash + progress.Result.MovedToTrash
	s.cleanupState.Destroyed = base.Destroyed + progress.Result.Destroyed
	s.cleanupState.UpdatedAt = time.Now()
	s.publishAppleMailCleanupLocked()
}

func (s *Service) finishAppleMailCleanupAccount(generation uint64, accountID, appleID string, localRemoved int, cleanupErr error) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if generation != s.cleanupGeneration || !s.cleanupState.Running {
		return
	}
	s.cleanupState.Completed++
	mailboxCount := s.cleanupMailboxes[accountID]
	s.cleanupState.CompletedMailboxes += mailboxCount
	s.cleanupState.Active = 0
	s.cleanupState.LocalRemoved += localRemoved
	s.cleanupState.CurrentFolder = ""
	s.cleanupState.UpdatedAt = time.Now()
	if cleanupErr != nil {
		s.cleanupState.Failed++
		s.cleanupState.FailedMailboxes += mailboxCount
		s.cleanupState.LastError = cleanupErr.Error()
		s.cleanupState.Failures = append(s.cleanupState.Failures, AppleMailCleanupFailure{AccountID: accountID, AppleID: appleID, Error: cleanupErr.Error()})
		s.cleanupState.Stage = "account-failed"
	} else {
		s.cleanupState.Success++
		s.cleanupState.SuccessfulMailboxes += mailboxCount
		s.cleanupState.Stage = "account-completed"
	}
	s.publishAppleMailCleanupLocked()
}

func (s *Service) finishAppleMailCleanupJob(generation uint64) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if generation != s.cleanupGeneration || !s.cleanupState.Running {
		return
	}
	now := time.Now()
	s.cleanupState.Running = false
	s.cleanupState.Active = 0
	s.cleanupState.Queued = 0
	s.cleanupState.CurrentAccountID = ""
	s.cleanupState.CurrentAppleID = ""
	s.cleanupState.CurrentFolder = ""
	s.cleanupState.CompletedAt = now
	s.cleanupState.UpdatedAt = now
	s.cleanupCancel = nil
	if s.cleanupState.Failed > 0 {
		s.cleanupState.Status = "partial"
		s.cleanupState.Stage = "partial"
	} else {
		s.cleanupState.Status = "completed"
		s.cleanupState.Stage = "completed"
		s.cleanupState.LastError = ""
	}
	s.publishAppleMailCleanupLocked()
}

func (s *Service) cleanupAccountIDs(requested []string) ([]string, error) {
	seen := make(map[string]bool)
	accountIDs := make([]string, 0)
	if len(requested) == 0 {
		for _, account := range s.store.AppleAccounts() {
			if account.ID != "" && !seen[account.ID] {
				seen[account.ID] = true
				accountIDs = append(accountIDs, account.ID)
			}
		}
	} else {
		for _, accountID := range requested {
			accountID = strings.TrimSpace(accountID)
			if accountID == "" || seen[accountID] {
				continue
			}
			if _, ok := s.store.FindAppleAccount(accountID); !ok {
				return nil, fmt.Errorf("Apple 账号不存在：%s", accountID)
			}
			seen[accountID] = true
			accountIDs = append(accountIDs, accountID)
		}
	}
	if len(accountIDs) == 0 {
		return nil, errors.New("没有可清理的 Apple 账号")
	}
	return accountIDs, nil
}

func (s *Service) cleanupMailboxCounts(accountIDs []string) (map[string]int, int) {
	selected := make(map[string]bool, len(accountIDs))
	for _, accountID := range accountIDs {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			selected[accountID] = true
		}
	}
	counts := make(map[string]int, len(selected))
	total := 0
	for _, mailbox := range s.store.AllMailboxes() {
		if selected[mailbox.AccountID] {
			counts[mailbox.AccountID]++
			total++
		}
	}
	return counts, total
}

func (s *Service) publishAppleMailCleanupLocked() {
	if s.store != nil {
		_ = s.store.SaveRuntimeState(appleMailCleanupStateID, s.cleanupState, true)
	}
}

func (s *Service) appleMailCleanupSnapshotLocked() AppleMailCleanupJob {
	out := s.cleanupState
	out.AccountIDs = append([]string(nil), s.cleanupState.AccountIDs...)
	out.Failures = append([]AppleMailCleanupFailure(nil), s.cleanupState.Failures...)
	if out.TotalMailboxes == 0 && len(out.AccountIDs) > 0 {
		_, out.TotalMailboxes = s.cleanupMailboxCounts(out.AccountIDs)
	}
	return out
}

func (s *Service) SyncMessages(ctx context.Context, mailboxID string) (int, error) {
	allowWebAPI := s.store.Settings().EnableWebCodeSync
	result, err := s.syncMailboxMessagesWithOptions(ctx, mailboxID, MessageSyncOptions{
		Mode: protocol.MailSyncModeVerification, Trigger: "verification", Limit: s.cfg.MailWatcherFetchLimit,
		UseCursor: true, AllowWebAPI: allowWebAPI, AllowFallback: allowWebAPI, UseWebComplement: allowWebAPI,
	})
	return result.SyncedMessages, err
}

// SyncPublicMessages 同步公共页面与外部 API 请求的邮件列表，并沿用“Web API 取码与邮件刷新”开关。
func (s *Service) SyncPublicMessages(ctx context.Context, mailboxID string) (MailboxMessageSyncBatchResult, error) {
	allowWebAPI := s.store.Settings().EnableWebCodeSync
	return s.syncMailboxMessagesWithOptions(ctx, mailboxID, MessageSyncOptions{
		Mode: protocol.MailSyncModeAllRecent, Trigger: "public-messages", Limit: s.cfg.MailWatcherFetchLimit,
		UseCursor: true, AllowWebAPI: allowWebAPI, AllowFallback: allowWebAPI, UseWebComplement: allowWebAPI,
	})
}

// SyncMailboxMessages 同步指定邮箱所属 Apple 主号的新邮件，并把结果分发到该主号的全部隐私邮箱。
func (s *Service) SyncMailboxMessages(ctx context.Context, mailboxID string) (MailboxMessageSyncBatchResult, error) {
	allowWebAPI := s.store.Settings().EnableWebManualMailSync
	return s.syncMailboxMessagesWithOptions(ctx, mailboxID, MessageSyncOptions{
		Mode: protocol.MailSyncModeAllRecent, Trigger: "manual", Limit: s.cfg.MailWatcherFetchLimit,
		FullScanOnFirst: true, UseCursor: true, TouchMailboxIDs: []string{mailboxID},
		AllowWebAPI: allowWebAPI, AllowFallback: allowWebAPI, UseWebComplement: allowWebAPI,
	})
}

// SyncDomainMailboxes 按接收账号分组同步域名邮箱；首次全量，后续从 LastUID 增量扫描。
func (s *Service) SyncDomainMailboxes(ctx context.Context, mailboxIDs []string) (MailboxMessageSyncBatchResult, error) {
	if !s.store.DomainMailSettings().Enabled {
		return MailboxMessageSyncBatchResult{}, errors.New("域名邮箱接收尚未开启")
	}
	wanted := make(map[string]bool, len(mailboxIDs))
	for _, id := range mailboxIDs {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}
	mailboxes := make([]domain.Mailbox, 0)
	for _, mailbox := range s.store.DomainMailboxes() {
		if len(wanted) > 0 && !wanted[mailbox.ID] {
			continue
		}
		if mailbox.Status == domain.StatusDisabled || !mailbox.ICloudActive {
			continue
		}
		mailboxes = append(mailboxes, mailbox)
	}
	if len(mailboxes) == 0 {
		return MailboxMessageSyncBatchResult{}, errors.New("没有可同步的域名邮箱")
	}
	allowWebAPI := s.store.Settings().EnableWebManualMailSync
	return s.SyncMailboxBatchWithOptions(ctx, mailboxes, MessageSyncOptions{
		Mode: protocol.MailSyncModeAllRecent, Trigger: "domain-bulk-manual",
		FullScanOnFirst: true, UseCursor: true,
		AllowWebAPI: allowWebAPI, AllowFallback: allowWebAPI, UseWebComplement: allowWebAPI,
	})
}

func (s *Service) syncMailboxMessagesWithOptions(ctx context.Context, mailboxID string, options MessageSyncOptions) (MailboxMessageSyncBatchResult, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return MailboxMessageSyncBatchResult{}, errors.New("邮箱不存在")
	}
	mailboxes := s.mailboxesForAccountKind(mailbox.AccountID, mailboxKind(mailbox))
	if mailboxKind(mailbox) == domain.MailboxKindDomainForward && strings.TrimSpace(mailbox.DomainRouteID) != "" {
		if route, found := s.store.DomainMailRouteForSync(mailbox.DomainRouteID); found && route.ReceiverType == domain.DomainReceiverCustomIMAP {
			mailboxes = mailboxes[:0]
			for _, item := range s.store.DomainMailboxes() {
				if item.DomainRouteID == mailbox.DomainRouteID && item.ICloudActive && item.Status != domain.StatusDisabled {
					mailboxes = append(mailboxes, item)
				}
			}
		}
	}
	found := false
	for _, item := range mailboxes {
		if item.ID == mailbox.ID {
			found = true
			break
		}
	}
	if !found {
		mailboxes = append(mailboxes, mailbox)
	}
	return s.SyncMailboxBatchWithOptions(ctx, mailboxes, options)
}

// SyncExistingMailboxMessages 每个 Apple 主账号只拉取一次收件箱，再在本地把邮件分发到对应隐私邮箱。
func (s *Service) SyncExistingMailboxMessages(ctx context.Context) (ExistingMailboxMessageSyncResult, error) {
	return s.syncExistingMailboxMessages(ctx, nil, nil)
}

// StartExistingMailboxMessageSync 在后台启动全部已有邮箱的邮件同步任务。
func (s *Service) StartExistingMailboxMessageSync(parent context.Context) (ExistingMailboxMessageSyncJob, error) {
	targets, totalMailboxes, skippedMailboxes := s.existingMailboxMessageSyncTargets()
	if parent == nil {
		parent = context.Background()
	}

	s.messageSyncJobMu.Lock()
	defer s.messageSyncJobMu.Unlock()
	if s.messageSyncJob.Running {
		return ExistingMailboxMessageSyncJob{}, errors.New("邮件同步任务正在运行")
	}
	s.messageSyncJobGeneration++
	generation := s.messageSyncJobGeneration
	now := time.Now()
	s.messageSyncJob = ExistingMailboxMessageSyncJob{
		ID:               fmt.Sprintf("mailbox_message_sync_%d", now.UnixNano()),
		Running:          true,
		Status:           "queued",
		Stage:            "queued",
		TotalAccounts:    len(targets),
		TotalMailboxes:   totalMailboxes,
		SkippedMailboxes: skippedMailboxes,
		Queued:           len(targets),
		Failures:         []ExistingMailboxMessageSyncFailure{},
		StartedAt:        now,
		UpdatedAt:        now,
	}
	s.publishExistingMailboxMessageSyncLocked()
	out := s.existingMailboxMessageSyncSnapshotLocked()
	go s.runExistingMailboxMessageSync(parent, generation, targets, totalMailboxes, skippedMailboxes)
	return out, nil
}

// ExistingMailboxMessageSyncStatus 返回后台邮件同步任务的当前快照。
func (s *Service) ExistingMailboxMessageSyncStatus() ExistingMailboxMessageSyncJob {
	s.messageSyncJobMu.Lock()
	defer s.messageSyncJobMu.Unlock()
	return s.existingMailboxMessageSyncSnapshotLocked()
}

func (s *Service) runExistingMailboxMessageSync(ctx context.Context, generation uint64, targets []existingMailboxMessageSyncTarget, totalMailboxes, skippedMailboxes int) {
	result, err := s.syncExistingMailboxMessageTargets(ctx, targets, totalMailboxes, skippedMailboxes,
		func(accountID string) { s.beginExistingMailboxMessageSyncAccount(generation, accountID) },
		func(account AccountMessageSyncResult) { s.finishExistingMailboxMessageSyncAccount(generation, account) },
	)
	s.finishExistingMailboxMessageSyncJob(generation, result, err)
}

func (s *Service) existingMailboxMessageSyncTargets() ([]existingMailboxMessageSyncTarget, int, int) {
	allMailboxes := s.store.AllMailboxes()
	groups := make(map[string][]domain.Mailbox)
	order := make([]string, 0)
	totalMailboxes := 0
	skippedMailboxes := 0
	for _, mailbox := range allMailboxes {
		if mailboxKind(mailbox) == domain.MailboxKindDomainForward {
			continue
		}
		accountID := s.mailboxSyncGroupKey(mailbox)
		if strings.TrimSpace(mailbox.Email) == "" || accountID == "__legacy__" {
			skippedMailboxes++
			continue
		}
		if _, exists := groups[accountID]; !exists {
			order = append(order, accountID)
		}
		groups[accountID] = append(groups[accountID], mailbox)
		totalMailboxes++
	}
	targets := make([]existingMailboxMessageSyncTarget, 0, len(order))
	for index, accountID := range order {
		targets = append(targets, existingMailboxMessageSyncTarget{index: index, accountID: accountID, mailboxes: groups[accountID]})
	}
	return targets, totalMailboxes, skippedMailboxes
}

func (s *Service) syncExistingMailboxMessages(ctx context.Context, onStart func(string), onFinish func(AccountMessageSyncResult)) (ExistingMailboxMessageSyncResult, error) {
	targets, totalMailboxes, skippedMailboxes := s.existingMailboxMessageSyncTargets()
	return s.syncExistingMailboxMessageTargets(ctx, targets, totalMailboxes, skippedMailboxes, onStart, onFinish)
}

func (s *Service) syncExistingMailboxMessageTargets(ctx context.Context, targets []existingMailboxMessageSyncTarget, totalMailboxes, skippedMailboxes int, onStart func(string), onFinish func(AccountMessageSyncResult)) (ExistingMailboxMessageSyncResult, error) {
	result := ExistingMailboxMessageSyncResult{TotalAccounts: len(targets), TotalMailboxes: totalMailboxes, SkippedMailboxes: skippedMailboxes}
	allowWebAPI := s.store.Settings().EnableWebManualMailSync
	accountResults := make([]AccountMessageSyncResult, len(targets))
	jobs := make(chan existingMailboxMessageSyncTarget)
	workerCount := 3
	if len(targets) < workerCount {
		workerCount = len(targets)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				if onStart != nil {
					onStart(job.accountID)
				}
				accountResult, syncErr := s.syncGroup(ctx, job.accountID, job.mailboxes, MessageSyncOptions{
					Mode: protocol.MailSyncModeAllRecent, Trigger: "bulk-manual",
					FullScanOnFirst: true, UseCursor: true,
					AllowWebAPI: allowWebAPI, AllowFallback: allowWebAPI, UseWebComplement: allowWebAPI,
				})
				if syncErr != nil {
					accountResult.Error = syncErr.Error()
				}
				accountResults[job.index] = accountResult
				if onFinish != nil {
					onFinish(accountResult)
				}
			}
		}()
	}
	for _, job := range targets {
		select {
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return result, ctx.Err()
		case jobs <- job:
		}
	}
	close(jobs)
	workers.Wait()
	for _, accountResult := range accountResults {
		if accountResult.AccountID == "" {
			continue
		}
		result.Accounts = append(result.Accounts, accountResult)
		result.SyncedMessages += accountResult.SyncedMessages
		result.Scanned += accountResult.Scanned
		result.Matched += accountResult.Matched
		result.HasMore = result.HasMore || accountResult.HasMore
		if accountResult.FallbackUsed {
			result.Fallbacks++
		}
		switch accountResult.Method {
		case "imap":
			result.IMAPAccounts++
		case "web_api":
			result.WebAPIAccounts++
		case "imap_web":
			result.IMAPAccounts++
			result.WebAPIAccounts++
		}
		if accountResult.Error == "" {
			result.SuccessfulAccounts++
			continue
		}
		result.FailedAccounts++
		result.Failures = append(result.Failures, ExistingMailboxMessageSyncFailure{
			AccountID: accountResult.AccountID, Account: accountResult.Account, Mailboxes: accountResult.Mailboxes, Error: accountResult.Error,
		})
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Service) beginExistingMailboxMessageSyncAccount(generation uint64, _ string) {
	s.messageSyncJobMu.Lock()
	defer s.messageSyncJobMu.Unlock()
	if generation != s.messageSyncJobGeneration || !s.messageSyncJob.Running {
		return
	}
	s.messageSyncJob.Status = "running"
	s.messageSyncJob.Stage = "syncing"
	s.messageSyncJob.Active++
	if s.messageSyncJob.Queued > 0 {
		s.messageSyncJob.Queued--
	}
	s.messageSyncJob.UpdatedAt = time.Now()
	s.publishExistingMailboxMessageSyncLocked()
}

func (s *Service) finishExistingMailboxMessageSyncAccount(generation uint64, account AccountMessageSyncResult) {
	s.messageSyncJobMu.Lock()
	defer s.messageSyncJobMu.Unlock()
	if generation != s.messageSyncJobGeneration || !s.messageSyncJob.Running {
		return
	}
	if s.messageSyncJob.Active > 0 {
		s.messageSyncJob.Active--
	}
	s.messageSyncJob.CompletedAccounts++
	s.messageSyncJob.SyncedMessages += account.SyncedMessages
	s.messageSyncJob.Scanned += account.Scanned
	s.messageSyncJob.Matched += account.Matched
	s.messageSyncJob.HasMore = s.messageSyncJob.HasMore || account.HasMore
	if account.FallbackUsed {
		s.messageSyncJob.Fallbacks++
	}
	s.addExistingMailboxMessageSyncMethodLocked(account.Method)
	if account.Error == "" {
		s.messageSyncJob.SuccessfulAccounts++
		s.messageSyncJob.Stage = "account-completed"
	} else {
		s.messageSyncJob.FailedAccounts++
		summary := summarizeExistingMailboxMessageSyncError(account.Error)
		s.messageSyncJob.LastError = summary
		s.messageSyncJob.Failures = append(s.messageSyncJob.Failures, ExistingMailboxMessageSyncFailure{
			AccountID: account.AccountID, Account: account.Account, Mailboxes: account.Mailboxes, Error: summary,
		})
		s.messageSyncJob.Stage = "account-failed"
	}
	s.messageSyncJob.UpdatedAt = time.Now()
	s.publishExistingMailboxMessageSyncLocked()
}

func (s *Service) addExistingMailboxMessageSyncMethodLocked(method string) {
	switch method {
	case "imap":
		s.messageSyncJob.IMAPAccounts++
	case "web_api":
		s.messageSyncJob.WebAPIAccounts++
	case "imap_web":
		s.messageSyncJob.IMAPAccounts++
		s.messageSyncJob.WebAPIAccounts++
	}
}

func (s *Service) finishExistingMailboxMessageSyncJob(generation uint64, result ExistingMailboxMessageSyncResult, runErr error) {
	s.messageSyncJobMu.Lock()
	defer s.messageSyncJobMu.Unlock()
	if generation != s.messageSyncJobGeneration || !s.messageSyncJob.Running {
		return
	}
	now := time.Now()
	s.messageSyncJob.Running = false
	s.messageSyncJob.Active = 0
	s.messageSyncJob.Queued = 0
	s.messageSyncJob.TotalAccounts = result.TotalAccounts
	s.messageSyncJob.TotalMailboxes = result.TotalMailboxes
	s.messageSyncJob.SkippedMailboxes = result.SkippedMailboxes
	s.messageSyncJob.CompletedAt = now
	s.messageSyncJob.UpdatedAt = now
	if runErr != nil {
		s.messageSyncJob.Status = "interrupted"
		s.messageSyncJob.Stage = "interrupted"
		s.messageSyncJob.LastError = summarizeExistingMailboxMessageSyncError(runErr.Error())
	} else if s.messageSyncJob.FailedAccounts > 0 {
		s.messageSyncJob.Status = "partial"
		s.messageSyncJob.Stage = "partial"
	} else {
		s.messageSyncJob.Status = "completed"
		s.messageSyncJob.Stage = "completed"
		s.messageSyncJob.LastError = ""
	}
	s.publishExistingMailboxMessageSyncLocked()
}

func (s *Service) publishExistingMailboxMessageSyncLocked() {
	if s.store != nil {
		_ = s.store.SaveRuntimeState(existingMailboxMessageSyncStateID, s.messageSyncJob, true)
	}
}

func (s *Service) existingMailboxMessageSyncSnapshotLocked() ExistingMailboxMessageSyncJob {
	out := s.messageSyncJob
	out.Failures = append([]ExistingMailboxMessageSyncFailure(nil), s.messageSyncJob.Failures...)
	return out
}

func summarizeExistingMailboxMessageSyncError(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "邮件同步失败"
	}
	if strings.Contains(message, "/mailws2/v1/thread/search HTTP 400") {
		return "iCloud Web 补查请求参数被拒绝（HTTP 400）"
	}
	for _, marker := range []string{"：{", ": {", "\n"} {
		if index := strings.Index(message, marker); index > 0 {
			message = strings.TrimSpace(message[:index])
			break
		}
	}
	runes := []rune(message)
	if len(runes) > 160 {
		message = string(runes[:160]) + "…"
	}
	return message
}

// SyncMailboxBatch 按 Apple 账号批量拉取一次收件箱，再把邮件分发给对应隐私邮箱。
func (s *Service) SyncMailboxBatch(ctx context.Context, mailboxes []domain.Mailbox, after time.Time, keyword string, maxMessages int) (int, error) {
	allowWebAPI := s.store.Settings().EnableWebBackgroundMail
	result, err := s.SyncMailboxBatchWithOptions(ctx, mailboxes, MessageSyncOptions{
		Mode: protocol.MailSyncModeVerification, Trigger: "watcher", After: after, Limit: maxMessages,
		UseCursor: true, AllowWebAPI: allowWebAPI, AllowFallback: allowWebAPI, UseWebComplement: allowWebAPI,
	})
	return result.SyncedMessages, err
}

func (s *Service) SyncMailboxBatchWithOptions(ctx context.Context, mailboxes []domain.Mailbox, options MessageSyncOptions) (MailboxMessageSyncBatchResult, error) {
	var result MailboxMessageSyncBatchResult
	if len(mailboxes) == 0 {
		return result, nil
	}
	if options.Limit <= 0 && !options.FullScan {
		options.Limit = s.cfg.MailWatcherFetchLimit
	}
	if options.Mode == "" {
		options.Mode = protocol.MailSyncModeVerification
	}
	if options.UseCursor {
		// 后台监听、公共取码和手动同步共用同一个初始化游标，第一个到达的请求负责扫描当前全部邮件。
		options.FullScanOnFirst = true
	}
	groups := make(map[string][]domain.Mailbox)
	order := make([]string, 0)
	for _, mailbox := range mailboxes {
		key := s.mailboxSyncGroupKey(mailbox)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], mailbox)
	}
	for _, key := range order {
		accountResult, err := s.syncGroup(ctx, key, groups[key], options)
		result.Accounts = append(result.Accounts, accountResult)
		result.SyncedMessages += accountResult.SyncedMessages
		result.Scanned += accountResult.Scanned
		result.Matched += accountResult.Matched
		result.HasMore = result.HasMore || accountResult.HasMore
		if accountResult.FallbackUsed {
			result.Fallbacks++
		}
		switch accountResult.Method {
		case "imap":
			result.IMAPAccounts++
		case "web_api":
			result.WebAPIAccounts++
		case "imap_web":
			result.IMAPAccounts++
			result.WebAPIAccounts++
		}
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) syncGroup(ctx context.Context, key string, mailboxes []domain.Mailbox, options MessageSyncOptions) (AccountMessageSyncResult, error) {
	signature := messageSyncRequestSignature(mailboxes, options)
	s.syncMu.Lock()
	if running := s.syncs[key]; running != nil {
		s.syncMu.Unlock()
		select {
		case <-ctx.Done():
			return AccountMessageSyncResult{}, ctx.Err()
		case <-running.done:
			if running.signature == signature {
				return running.result, running.err
			}
			return s.syncGroup(ctx, key, mailboxes, options)
		}
	}
	call := &syncCall{done: make(chan struct{}), signature: signature}
	s.syncs[key] = call
	s.syncMu.Unlock()

	call.result, call.err = s.syncGroupNow(ctx, mailboxes, options)
	s.syncMu.Lock()
	delete(s.syncs, key)
	close(call.done)
	s.syncMu.Unlock()
	return call.result, call.err
}

func (s *Service) syncGroupNow(ctx context.Context, mailboxes []domain.Mailbox, options MessageSyncOptions) (AccountMessageSyncResult, error) {
	requestedMailboxIDs := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		requestedMailboxIDs = append(requestedMailboxIDs, mailbox.ID)
	}
	if len(options.TouchMailboxIDs) == 0 && options.Trigger != "watcher" && options.Trigger != "imap-idle" && options.Trigger != "reconcile" {
		options.TouchMailboxIDs = requestedMailboxIDs
	}
	refreshed := make([]domain.Mailbox, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		if current, ok := s.store.FindMailboxByID(mailbox.ID); ok {
			refreshed = append(refreshed, current)
		}
	}
	if len(refreshed) == 0 {
		return AccountMessageSyncResult{}, nil
	}
	accountID := strings.TrimSpace(refreshed[0].AccountID)
	accountName := accountID
	var customRoute domain.DomainMailRoute
	customIMAP := false
	if mailboxKind(refreshed[0]) == domain.MailboxKindDomainForward && strings.TrimSpace(refreshed[0].DomainRouteID) != "" {
		if route, found := s.store.DomainMailRouteForSync(refreshed[0].DomainRouteID); found && route.ReceiverType == domain.DomainReceiverCustomIMAP {
			customRoute = route
			customIMAP = true
			accountID = "domain-route:" + route.ID
			accountName = firstNonEmpty(route.ReceiverLabel, route.ForwardToEmail, route.Domain)
		}
	}
	if shared := s.mailboxesForSharedSource(refreshed, customRoute); len(shared) > 0 {
		if options.Trigger == "domain-delete-prepare" {
			sharedIDs := make(map[string]bool, len(shared))
			for _, mailbox := range shared {
				sharedIDs[mailbox.ID] = true
			}
			for _, mailbox := range refreshed {
				if !sharedIDs[mailbox.ID] {
					shared = append(shared, mailbox)
				}
			}
		}
		refreshed = shared
	}
	if account, found := s.store.FindAppleAccount(accountID); found {
		accountName = firstNonEmpty(account.Label, account.AppleID, accountID)
	}
	accountResult := AccountMessageSyncResult{AccountID: accountID, Account: accountName, Mailboxes: len(refreshed)}
	session, ok := s.store.ICloudSessionByAccountID(strings.TrimSpace(refreshed[0].AccountID))
	if !ok && !customIMAP {
		return accountResult, errors.New("对应 Apple 账号登录态不存在")
	}
	backend := s.messageBackend
	if backend == nil {
		backend = defaultMessageSyncBackend{client: s.client}
	}
	protocolOptions := protocol.MailSyncOptions{Mode: options.Mode, After: options.After, Limit: options.Limit, FullScan: options.FullScan, UseCursor: options.UseCursor, Reconcile: options.Reconcile}
	var syncResult protocol.MailSyncBatchResult
	var syncErr error
	var imapErr error
	var complementErr error
	source := "icloud"
	imapState, imapSaved := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP)
	if customIMAP {
		imapState = protocol.LoginState{
			Kind: domain.LoginStateICloudIMAP, IMAPEmail: customRoute.ForwardToEmail, IMAPUsername: customRoute.IMAPUsername,
			IMAPHost: customRoute.IMAPHost, IMAPPort: customRoute.IMAPPort, IMAPAppPassword: customRoute.IMAPPassword,
		}
		imapSaved = true
		options.AllowWebAPI, options.AllowFallback, options.UseWebComplement = false, false, false
	}
	protocolOptions.DiscoveryDomains = s.discoveryDomainsForSync(refreshed, customRoute)
	imapConfigured := imapSaved && strings.TrimSpace(imapState.IMAPEmail) != "" && strings.TrimSpace(imapState.IMAPAppPassword) != ""
	sourceSignature := imapSourceSignature(imapState)
	cursorStateID := mailSyncStateID(accountID, "imap", "source")
	cursorFound := false
	var cursor MailSyncState
	if imapConfigured && options.UseCursor {
		if found, _ := s.store.LoadRuntimeState(cursorStateID, &cursor); found {
			if cursor.SourceSignature == sourceSignature {
				cursorFound = cursor.HistoryComplete
				protocolOptions.CursorUID = cursor.LastScannedUID
				protocolOptions.CursorUIDValidity = cursor.UIDValidity
			} else {
				cursor = MailSyncState{}
			}
		}
	}
	if options.FullScanOnFirst && imapConfigured && !cursorFound {
		// 新版账号索引首次必须完整扫描；旧版分类游标不代表历史索引已建立。
		protocolOptions.Mode = protocol.MailSyncModeAllRecent
		protocolOptions.FullScan = true
		protocolOptions.Limit = 0
		protocolOptions.After = time.Time{}
		protocolOptions.CursorUID = ""
		protocolOptions.CursorUIDValidity = ""
	}
	pendingHistory := pendingHistoryMailboxes(s.store, refreshed, accountID, protocolOptions.CursorUIDValidity, protocolOptions.CursorUID)
	indexedResult := protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}}
	var indexUpdates []store.MailIndexEntry
	webOptions := protocolOptions
	if webOptions.FullScan {
		// Web API 首次全量不使用邮箱时间游标，确保 maxResults=1000 的结果都会参与匹配。
		webOptions.UseCursor = false
	}

	type messagePathResult struct {
		result protocol.MailSyncBatchResult
		err    error
	}
	var imapResult protocol.MailSyncBatchResult
	var webResult protocol.MailSyncBatchResult
	var webErr error
	webAttempted := false
	webAvailable := options.AllowWebAPI && protocol.CanUseICloudWebMail(session)
	concurrentPaths := imapConfigured && webAvailable && options.UseWebComplement
	if concurrentPaths {
		imapResults := make(chan messagePathResult, 1)
		webResults := make(chan messagePathResult, 1)
		go func() {
			result, err := backend.SyncIMAP(ctx, imapState, refreshed, protocolOptions)
			imapResults <- messagePathResult{result: result, err: err}
		}()
		go func() {
			result, err := backend.SyncWeb(ctx, session, refreshed, webOptions)
			webResults <- messagePathResult{result: result, err: err}
		}()
		imapPath := <-imapResults
		webPath := <-webResults
		imapResult, imapErr = imapPath.result, imapPath.err
		webResult, webErr = webPath.result, webPath.err
		webAttempted = true
	} else if imapConfigured {
		imapResult, imapErr = backend.SyncIMAP(ctx, imapState, refreshed, protocolOptions)
	}

	if imapConfigured && imapErr == nil {
		syncResult = imapResult
		accountResult.Method = "imap"
		source = "imap"
		if webAttempted {
			if webErr == nil {
				syncResult = mergeMailSyncBatchResults(syncResult, webResult)
				accountResult.Method = "imap_web"
				source = ""
			} else {
				accountResult.FallbackReason = "IMAP 已完成，但 iCloud Web 补查失败：" + webErr.Error()
				// 单行手动同步需要明确告知用户补查失败，批量和后台同步保留 IMAP 成功结果。
				if options.Trigger == "manual" {
					complementErr = errors.New(accountResult.FallbackReason)
				}
			}
		}
	}
	if accountResult.Method == "" {
		if ctx.Err() != nil {
			return accountResult, ctx.Err()
		}
		if !options.AllowWebAPI {
			switch {
			case imapErr != nil:
				return accountResult, fmt.Errorf("IMAP 读信失败，且当前功能的 iCloud Web API 已关闭：%w", imapErr)
			case imapConfigured:
				return accountResult, errors.New("IMAP 未完成读信，且当前功能的 iCloud Web API 已关闭")
			default:
				return accountResult, errors.New("未配置 IMAP，且当前功能的 iCloud Web API 已关闭")
			}
		}
		if imapErr != nil && !options.AllowFallback {
			return accountResult, imapErr
		}
		if !protocol.CanUseICloudWebMail(session) {
			switch {
			case imapErr != nil:
				return accountResult, fmt.Errorf("IMAP 读信失败，且 iCloud Web 邮件登录态不可用：%w", imapErr)
			case imapConfigured:
				return accountResult, errors.New("iCloud Web 邮件登录态不可用")
			default:
				return accountResult, errors.New("没有可用的读信方式，请配置主号 IMAP 或重新登录 iCloud Web")
			}
		}
		if !webAttempted {
			webResult, webErr = backend.SyncWeb(ctx, session, refreshed, webOptions)
			webAttempted = true
		}
		if webErr != nil {
			if imapErr != nil {
				return accountResult, fmt.Errorf("IMAP 读信失败：%v；iCloud Web 回退也失败：%w", imapErr, webErr)
			}
			return accountResult, webErr
		}
		syncResult = webResult
		accountResult.Method = "web_api"
		if imapErr != nil {
			accountResult.FallbackUsed = true
			accountResult.FallbackReason = imapErr.Error()
		}
	}
	if strings.HasPrefix(accountResult.Method, "imap") && cursorFound && len(pendingHistory) > 0 && !options.Reconcile &&
		strings.TrimSpace(imapResult.UIDValidity) == strings.TrimSpace(cursor.UIDValidity) && !protocolOptions.FullScan {
		var indexErr error
		indexedResult, indexUpdates, indexErr = s.materializeIndexedMail(ctx, backend, imapState, accountID, cursor.UIDValidity, pendingHistory)
		if indexErr != nil {
			return accountResult, indexErr
		}
	}
	if strings.HasPrefix(accountResult.Method, "imap") && countProtocolMessages(indexedResult.MessagesByMailbox) > 0 {
		syncResult = mergeMailSyncBatchResults(syncResult, indexedResult)
	}

	refreshed, syncResult, syncErr = s.materializeDiscoveredDomainMailboxes(refreshed, syncResult)
	if syncErr != nil {
		return accountResult, syncErr
	}
	accountResult.Mailboxes = len(refreshed)
	syncedAt := time.Now()
	updates := make([]store.MailboxSyncUpdate, 0, len(refreshed))
	touchAll := len(options.TouchMailboxIDs) == 0
	touchMailboxIDs := make(map[string]bool, len(options.TouchMailboxIDs))
	for _, mailboxID := range options.TouchMailboxIDs {
		if mailboxID = strings.TrimSpace(mailboxID); mailboxID != "" {
			touchMailboxIDs[mailboxID] = true
		}
	}
	for _, mailbox := range refreshed {
		messages := syncResult.MessagesByMailbox[mailbox.ID]
		if !touchAll && !touchMailboxIDs[mailbox.ID] && len(messages) == 0 {
			continue
		}
		update := store.MailboxSyncUpdate{MailboxID: mailbox.ID, SyncedAt: syncedAt}
		if touchAll || touchMailboxIDs[mailbox.ID] {
			update.LastUID = syncResult.LastUID
		}
		for _, message := range messages {
			remoteID := firstNonEmpty(message.RemoteID, message.UID)
			update.Messages = append(update.Messages, store.MailboxSyncMessage{
				RemoteID: remoteID, RemoteIDs: message.RemoteIDs, CanonicalID: message.CanonicalID, Source: firstNonEmpty(message.Source, source), Subject: message.Subject, From: message.From,
				Body: message.Body, HTMLBody: message.HTMLBody, ContentType: message.ContentType, ReceivedAt: message.ReceivedAt,
			})
		}
		updates = append(updates, update)
	}
	var created int
	imapCompleted := strings.HasPrefix(accountResult.Method, "imap")
	cursorCompleted := imapCompleted && options.UseCursor
	uidValidityChanged := cursor.HistoryComplete && strings.TrimSpace(cursor.UIDValidity) != "" && strings.TrimSpace(syncResult.UIDValidity) != "" && cursor.UIDValidity != syncResult.UIDValidity
	fullHistoryScan := (protocolOptions.FullScan && protocolOptions.After.IsZero()) || uidValidityChanged
	historyComplete := cursor.HistoryComplete || (fullHistoryScan && !imapResult.HasMore)
	state := cursor
	state.AccountID, state.Method, state.Folder = accountID, "imap", "INBOX"
	state.SourceSignature = sourceSignature
	if cursorCompleted {
		state.UIDValidity, state.LastScannedUID, state.LastSyncAt = syncResult.UIDValidity, syncResult.LastUID, syncedAt
		state.HistoryComplete = historyComplete
		if fullHistoryScan && historyComplete {
			state.LastFullScanAt = syncedAt
			state.LastReconcileAt = syncedAt
		}
	}
	if options.Reconcile && imapCompleted {
		state.LastReconcileAt = syncedAt
	}
	indexEntries := append(storeIndexEntries(accountID, syncResult.UIDValidity, syncResult.IndexEntries), indexUpdates...)
	indexCommit := store.MailIndexCommit{SourceKey: accountID, Folder: "INBOX", UIDValidity: syncResult.UIDValidity, Replace: fullHistoryScan, Entries: indexEntries}
	if imapCompleted && historyComplete {
		historyTargets := refreshed
		if options.Reconcile && len(pendingHistory) > 0 {
			pendingIDs := make(map[string]bool, len(pendingHistory))
			for _, mailbox := range pendingHistory {
				pendingIDs[mailbox.ID] = true
			}
			historyTargets = make([]domain.Mailbox, 0, len(refreshed)-len(pendingHistory))
			for _, mailbox := range refreshed {
				if !pendingIDs[mailbox.ID] {
					historyTargets = append(historyTargets, mailbox)
				}
			}
		}
		indexCommit.Histories = mailboxHistoryCommits(historyTargets, accountID, syncResult.UIDValidity, syncResult.LastUID, syncedAt)
	}
	if imapCompleted {
		stateID := ""
		var runtimeState any
		if cursorCompleted || options.Reconcile {
			stateID, runtimeState = cursorStateID, state
		}
		created, syncErr = s.store.ApplyMailboxSyncBatchWithIndex(updates, stateID, runtimeState, indexCommit)
	} else {
		created, syncErr = s.store.ApplyMailboxSyncBatch(updates)
	}
	if syncErr != nil {
		return accountResult, syncErr
	}
	accountResult.Scanned = syncResult.Scanned
	accountResult.Matched = syncResult.Matched
	accountResult.SyncedMessages = created
	accountResult.HasMore = syncResult.HasMore
	accountResult.InitializedCursor = syncResult.InitializedCursor
	return accountResult, complementErr
}

func mergeMailSyncBatchResults(primary, complement protocol.MailSyncBatchResult) protocol.MailSyncBatchResult {
	if primary.MessagesByMailbox == nil {
		primary.MessagesByMailbox = make(map[string][]protocol.ICloudSyncedMessage)
	}
	for mailboxID, messages := range complement.MessagesByMailbox {
		merged := primary.MessagesByMailbox[mailboxID]
		byKey := make(map[string]int, len(merged))
		for index, message := range merged {
			if key := syncedMessageKey(message); key != "" {
				byKey[key] = index
			}
		}
		for _, message := range messages {
			key := syncedMessageKey(message)
			index, found := byKey[key]
			if key == "" || !found {
				merged = append(merged, message)
				if key != "" {
					byKey[key] = len(merged) - 1
				}
				continue
			}
			existing := &merged[index]
			existing.RemoteIDs = mergeRemoteIDs(existing.RemoteIDs, message.RemoteIDs, []string{existing.RemoteID, message.RemoteID})
			if existing.CanonicalID == "" {
				existing.CanonicalID = message.CanonicalID
			}
			if strings.TrimSpace(existing.HTMLBody) == "" && strings.TrimSpace(message.HTMLBody) != "" {
				existing.HTMLBody = message.HTMLBody
				existing.ContentType = message.ContentType
			}
			if len(strings.TrimSpace(message.Body)) > len(strings.TrimSpace(existing.Body)) {
				existing.Body = message.Body
			}
		}
		primary.MessagesByMailbox[mailboxID] = merged
	}
	if primary.DiscoveredByEmail == nil {
		primary.DiscoveredByEmail = make(map[string][]protocol.ICloudSyncedMessage)
	}
	for email, messages := range complement.DiscoveredByEmail {
		primary.DiscoveredByEmail[email] = mergeSyncedMessages(primary.DiscoveredByEmail[email], messages)
	}
	primary.Scanned += complement.Scanned
	primary.Matched = 0
	for _, messages := range primary.MessagesByMailbox {
		primary.Matched += len(messages)
	}
	for _, messages := range primary.DiscoveredByEmail {
		primary.Matched += len(messages)
	}
	primary.HasMore = primary.HasMore || complement.HasMore
	indexByUID := make(map[string]int, len(primary.IndexEntries))
	for index, entry := range primary.IndexEntries {
		if strings.TrimSpace(entry.UID) != "" {
			indexByUID[entry.UID] = index
		}
	}
	for _, entry := range complement.IndexEntries {
		if index, found := indexByUID[entry.UID]; found {
			if entry.BodyComplete || !primary.IndexEntries[index].BodyComplete {
				primary.IndexEntries[index] = entry
			}
			continue
		}
		primary.IndexEntries = append(primary.IndexEntries, entry)
		if strings.TrimSpace(entry.UID) != "" {
			indexByUID[entry.UID] = len(primary.IndexEntries) - 1
		}
	}
	return primary
}

func mergeSyncedMessages(primary, complement []protocol.ICloudSyncedMessage) []protocol.ICloudSyncedMessage {
	merged := append([]protocol.ICloudSyncedMessage(nil), primary...)
	seen := make(map[string]bool, len(merged))
	for _, message := range merged {
		seen[syncedMessageKey(message)] = true
	}
	for _, message := range complement {
		key := syncedMessageKey(message)
		if key != "" && seen[key] {
			continue
		}
		if key != "" {
			seen[key] = true
		}
		merged = append(merged, message)
	}
	return merged
}

func (s *Service) discoveryDomainsForSync(mailboxes []domain.Mailbox, customRoute domain.DomainMailRoute) []string {
	settings := s.store.DomainMailSettings()
	if !settings.Enabled || settings.MatchMode != domain.DomainMatchCatchAll || !settings.AutoDiscover || len(mailboxes) == 0 {
		return nil
	}
	allowed := make(map[string]bool)
	accountID := strings.TrimSpace(mailboxes[0].AccountID)
	for _, route := range s.store.DomainMailRoutes() {
		if !route.Enabled {
			continue
		}
		if customRoute.ID != "" {
			if route.ID == customRoute.ID {
				allowed[route.Domain] = true
			}
			continue
		}
		if route.ReceiverType == domain.DomainReceiverAppleAccount && route.AccountID == accountID {
			allowed[route.Domain] = true
		}
	}
	out := make([]string, 0, len(allowed))
	for value := range allowed {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Service) materializeDiscoveredDomainMailboxes(mailboxes []domain.Mailbox, result protocol.MailSyncBatchResult) ([]domain.Mailbox, protocol.MailSyncBatchResult, error) {
	if len(result.DiscoveredByEmail) == 0 {
		return mailboxes, result, nil
	}
	settings := s.store.DomainMailSettings()
	byID := make(map[string]bool, len(mailboxes))
	for _, mailbox := range mailboxes {
		byID[mailbox.ID] = true
	}
	for email, messages := range result.DiscoveredByEmail {
		mailbox, found := s.store.FindMailboxByEmail(email)
		if !found {
			separator := strings.LastIndexByte(email, '@')
			if separator <= 0 {
				continue
			}
			route, routeFound := s.store.FindDomainMailRouteByDomain(email[separator+1:])
			if !routeFound || !route.Enabled {
				continue
			}
			created, err := s.store.CreateDomainMailboxes(route.ID, []string{email}, "自动发现", "由收件人地址自动发现", settings.DefaultAPIActive)
			if err != nil {
				if current, currentFound := s.store.FindMailboxByEmail(email); currentFound {
					mailbox, found = current, true
				} else {
					return mailboxes, result, err
				}
			} else if len(created) > 0 {
				mailbox, found = created[0], true
			}
		}
		if !found || mailboxKind(mailbox) != domain.MailboxKindDomainForward {
			continue
		}
		result.MessagesByMailbox[mailbox.ID] = mergeSyncedMessages(result.MessagesByMailbox[mailbox.ID], messages)
		if !byID[mailbox.ID] {
			mailboxes = append(mailboxes, mailbox)
			byID[mailbox.ID] = true
		}
	}
	result.DiscoveredByEmail = nil
	return mailboxes, result, nil
}

func syncedMessageKey(message protocol.ICloudSyncedMessage) string {
	if canonicalID := strings.TrimSpace(message.CanonicalID); canonicalID != "" {
		return "canonical:" + canonicalID
	}
	if remoteID := strings.TrimSpace(message.RemoteID); remoteID != "" {
		return "remote:" + remoteID
	}
	return ""
}

func mergeRemoteIDs(groups ...[]string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, values := range groups {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func mailSyncStateID(accountID, method string, scopes ...string) string {
	parts := []string{"mail-sync", strings.TrimSpace(accountID), strings.TrimSpace(method)}
	if len(scopes) > 0 {
		if scope := strings.TrimSpace(scopes[0]); scope != "" {
			parts = append(parts, scope)
		}
	}
	return strings.Join(append(parts, "inbox"), ":")
}

func (s *Service) mailboxSyncGroupKey(mailbox domain.Mailbox) string {
	if mailboxKind(mailbox) == domain.MailboxKindDomainForward && strings.TrimSpace(mailbox.DomainRouteID) != "" {
		if route, found := s.store.DomainMailRouteForSync(mailbox.DomainRouteID); found && route.ReceiverType == domain.DomainReceiverCustomIMAP {
			return "domain-route:" + route.ID
		}
	}
	accountID := firstNonEmpty(mailbox.AccountID, mailbox.OwnerID, "__legacy__")
	if accountID == "__legacy__" {
		return accountID
	}
	return accountID
}

func messageSyncRequestSignature(mailboxes []domain.Mailbox, options MessageSyncOptions) string {
	parts := []string{
		string(options.Mode), options.Trigger, options.After.UTC().Format(time.RFC3339Nano), fmt.Sprint(options.Limit),
		fmt.Sprint(options.FullScan), fmt.Sprint(options.FullScanOnFirst), fmt.Sprint(options.UseCursor),
		fmt.Sprint(options.AllowWebAPI), fmt.Sprint(options.AllowFallback), fmt.Sprint(options.UseWebComplement), fmt.Sprint(options.Reconcile),
	}
	ids := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		ids = append(ids, mailbox.ID)
	}
	sort.Strings(ids)
	touchIDs := append([]string(nil), options.TouchMailboxIDs...)
	sort.Strings(touchIDs)
	parts = append(parts, ids...)
	parts = append(parts, "touch")
	return strings.Join(append(parts, touchIDs...), "|")
}

func mailboxKind(mailbox domain.Mailbox) string {
	if strings.TrimSpace(mailbox.MailboxKind) == "" {
		return domain.MailboxKindICloudHME
	}
	return mailbox.MailboxKind
}

// MessageContent 返回本地完整邮件；旧 IMAP 缓存缺少 HTML 时会按 UID 自动补全。
func (s *Service) MessageContent(ctx context.Context, mailboxID, messageID string) (domain.Message, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return domain.Message{}, errors.New("邮箱不存在")
	}
	message, ok := s.store.FindMessageForMailbox(mailbox.ID, messageID)
	if !ok {
		return domain.Message{}, errors.New("邮件不存在")
	}
	if strings.TrimSpace(message.HTMLBody) != "" || strings.TrimSpace(message.ContentType) != "" {
		return message, nil
	}
	uid := imapUIDForMessage(message)
	if uid == "" {
		return message, nil
	}
	session, ok := s.store.ICloudSessionByAccountID(mailbox.AccountID)
	if !ok {
		return domain.Message{}, errors.New("对应 Apple 账号登录态不存在")
	}
	imapState, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP)
	if !ok {
		return message, nil
	}
	fetched, err := protocol.FetchICloudIMAPMessageByUID(ctx, imapState, uid)
	if err != nil {
		return domain.Message{}, err
	}
	return s.store.UpdateMessageContent(mailbox.ID, message.ID, fetched.Body, fetched.HTMLBody, fetched.ContentType)
}

// BackfillMessageContent 批量补全旧 IMAP 邮件的 HTML 与纯文本正文。
func (s *Service) BackfillMessageContent(ctx context.Context) (MessageContentBackfillResult, error) {
	missing := s.store.MessagesMissingContent(0)
	result := MessageContentBackfillResult{Total: len(missing)}
	if len(missing) == 0 {
		return result, nil
	}
	mailboxes := make(map[string]domain.Mailbox)
	for _, mailbox := range s.store.AllMailboxes() {
		mailboxes[mailbox.ID] = mailbox
	}
	type target struct {
		message domain.Message
		mailbox domain.Mailbox
	}
	groups := make(map[string][]target)
	order := make([]string, 0)
	for _, message := range missing {
		mailbox, ok := mailboxes[message.MailboxID]
		if !ok || strings.TrimSpace(mailbox.AccountID) == "" {
			continue
		}
		if _, exists := groups[mailbox.AccountID]; !exists {
			order = append(order, mailbox.AccountID)
		}
		groups[mailbox.AccountID] = append(groups[mailbox.AccountID], target{message: message, mailbox: mailbox})
	}
	var failures []string
	for _, accountID := range order {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		session, ok := s.store.ICloudSessionByAccountID(accountID)
		if !ok {
			failures = append(failures, accountID+"：Apple 账号登录态不存在")
			continue
		}
		imapState, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP)
		if !ok {
			failures = append(failures, accountID+"：IMAP 取码登录不存在")
			continue
		}
		uidTargets := make(map[string][]target)
		uidOrder := make([]string, 0)
		for _, item := range groups[accountID] {
			uid := imapUIDForMessage(item.message)
			if uid == "" {
				continue
			}
			if _, exists := uidTargets[uid]; !exists {
				uidOrder = append(uidOrder, uid)
			}
			uidTargets[uid] = append(uidTargets[uid], item)
		}
		fetched, fetchErr := protocol.FetchICloudIMAPMessagesByUID(ctx, imapState, uidOrder)
		updates := make([]store.MessageContentUpdate, 0, len(groups[accountID]))
		for uid, items := range uidTargets {
			message, ok := fetched[uid]
			if !ok {
				continue
			}
			for _, item := range items {
				updates = append(updates, store.MessageContentUpdate{MailboxID: item.mailbox.ID, MessageID: item.message.ID, Body: message.Body, HTMLBody: message.HTMLBody, ContentType: message.ContentType})
			}
		}
		updated, updateErr := s.store.ApplyMessageContentUpdates(updates)
		result.Updated += updated
		if fetchErr != nil {
			failures = append(failures, accountID+"："+fetchErr.Error())
		}
		if updateErr != nil {
			failures = append(failures, accountID+"："+updateErr.Error())
		}
	}
	result.Failed = len(s.store.MessagesMissingContent(0))
	if result.Failed > result.Total {
		result.Failed = result.Total
	}
	result.Updated = result.Total - result.Failed
	if len(failures) > 0 {
		return result, errors.New(strings.Join(failures, "；"))
	}
	return result, nil
}

func imapUIDForMessage(message domain.Message) string {
	ids := append(append([]string(nil), message.RemoteIDs...), message.RemoteID)
	for _, remoteID := range ids {
		remoteID = strings.TrimSpace(remoteID)
		if !strings.HasPrefix(strings.ToLower(remoteID), "imap:") {
			continue
		}
		if uid := strings.TrimSpace(remoteID[len("imap:"):]); uid != "" {
			return uid
		}
	}
	return ""
}

func (s *Service) Code(ctx context.Context, mailboxID string, after time.Time, keyword string, allowStale bool) (CodeResult, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return CodeResult{}, errors.New("邮箱不存在")
	}
	query := CodeQuery{After: after, Keyword: keyword, SkipMessageID: mailbox.LastCodeMessageID, MarkAsServed: true}
	if result, found, err := s.findCode(mailbox, query); found || err != nil {
		return result, err
	}
	if _, err := s.SyncMessages(ctx, mailboxID); err != nil && !allowStale {
		return CodeResult{}, err
	}
	mailbox, _ = s.store.FindMailboxByID(mailboxID)
	if result, found, err := s.findCode(mailbox, query); found || err != nil {
		return result, err
	}
	return CodeResult{}, errors.New("暂未收到验证码")
}

// CachedCode 只读取本地邮件缓存，不触发 Apple 或 IMAP 网络请求。
func (s *Service) CachedCode(mailboxID string, after time.Time, keyword string, allowStale bool) (CodeResult, bool, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return CodeResult{}, false, errors.New("邮箱不存在")
	}
	query := CodeQuery{After: after, Keyword: keyword, SkipMessageID: mailbox.LastCodeMessageID, MarkAsServed: true}
	return s.findCode(mailbox, query)
}

// CachedCodeWithQuery 支持预览、缓存读取和固定请求起点，便于合并并发取码请求。
func (s *Service) CachedCodeWithQuery(mailboxID string, query CodeQuery) (CodeResult, bool, error) {
	mailbox, ok := s.store.FindMailboxByID(mailboxID)
	if !ok {
		return CodeResult{}, false, errors.New("邮箱不存在")
	}
	return s.findCode(mailbox, query)
}

func (s *Service) findCode(mailbox domain.Mailbox, query CodeQuery) (CodeResult, bool, error) {
	keyword := strings.TrimSpace(query.Keyword)
	if keyword == "" {
		keyword = "OpenAI"
	}
	skipMessageID := strings.TrimSpace(query.SkipMessageID)
	if query.IncludeServed {
		skipMessageID = ""
	}
	after := query.After
	if skipMessageID != "" {
		if servedMessage, ok := s.store.FindMessageForMailbox(mailbox.ID, skipMessageID); ok {
			servedAt := servedMessage.ReceivedAt
			if servedAt.IsZero() {
				servedAt = servedMessage.CreatedAt
			}
			if !servedAt.IsZero() && !servedAt.Before(after) {
				after = servedAt.Add(time.Nanosecond)
			}
		}
	}
	after = codeAfter(after, time.Now())
	for _, message := range s.store.MessagesForMailbox(mailbox.ID) {
		messageTime := message.ReceivedAt
		if messageTime.IsZero() {
			messageTime = message.CreatedAt
		}
		if messageTime.IsZero() || messageTime.Before(after) {
			continue
		}
		text := message.Subject + " " + message.From + " " + message.Body
		if !strings.EqualFold(keyword, "OpenAI") && !strings.Contains(strings.ToLower(text), strings.ToLower(keyword)) {
			continue
		}
		if skipMessageID != "" && message.ID == skipMessageID {
			continue
		}
		code := protocol.ExtractOTP(message.Subject + "\n" + message.Body)
		if code == "" {
			continue
		}
		if query.MarkAsServed {
			if err := s.store.SetMailboxLastCode(mailbox.ID, message.ID, time.Now()); err != nil {
				return CodeResult{}, false, err
			}
		}
		return CodeResult{Email: mailbox.Email, Code: code, Subject: message.Subject, From: message.From, ReceivedAt: messageTime, MessageID: message.ID}, true, nil
	}
	return CodeResult{}, false, nil
}

func codeAfter(after, now time.Time) time.Time {
	cutoff := now.Add(-mailboxCodeFreshWindow)
	if after.After(cutoff) {
		return after
	}
	return cutoff
}

func (s *Service) mailboxesForAccount(accountID string) []domain.Mailbox {
	accountID = strings.TrimSpace(accountID)
	out := make([]domain.Mailbox, 0)
	for _, mailbox := range s.store.AllMailboxes() {
		if mailbox.AccountID != accountID || !mailbox.ICloudActive || mailbox.Status == domain.StatusDisabled {
			continue
		}
		out = append(out, mailbox)
	}
	return out
}

func (s *Service) mailboxesForAccountKind(accountID, kind string) []domain.Mailbox {
	kind = strings.TrimSpace(kind)
	out := make([]domain.Mailbox, 0)
	for _, mailbox := range s.mailboxesForAccount(accountID) {
		if mailboxKind(mailbox) != kind {
			continue
		}
		out = append(out, mailbox)
	}
	return out
}

func normalizeRemoteCleanupOptions(options RemoteCleanupOptions) RemoteCleanupOptions {
	if !options.MoveSynced && !options.EmptyTrash {
		options.MoveSynced = true
		options.EmptyTrash = true
	}
	return options
}

func remoteMessageIDs(messages []domain.Message) []string {
	out := make([]string, 0, len(messages))
	seen := make(map[string]bool)
	for _, message := range messages {
		ids := append(append([]string(nil), message.RemoteIDs...), message.RemoteID)
		for _, remoteID := range ids {
			remoteID = strings.TrimSpace(remoteID)
			if remoteID == "" || seen[remoteID] {
				continue
			}
			seen[remoteID] = true
			out = append(out, remoteID)
		}
	}
	return out
}

func remoteMailbox(remote protocol.ICloudRemoteMailbox) domain.RemoteMailbox {
	return domain.RemoteMailbox{AnonymousID: remote.AnonymousID, Email: remote.Email, ForwardToEmail: remote.ForwardToEmail, Label: remote.Label, Note: remote.Note, IsActive: remote.IsActive, Origin: remote.Origin}
}

func matchRemote(remotes []protocol.ICloudRemoteMailbox, anonymousID, email string) (protocol.ICloudRemoteMailbox, bool) {
	for _, remote := range remotes {
		if strings.TrimSpace(anonymousID) != "" && remote.AnonymousID == strings.TrimSpace(anonymousID) {
			return remote, true
		}
		if strings.EqualFold(remote.Email, strings.TrimSpace(email)) {
			return remote, true
		}
	}
	return protocol.ICloudRemoteMailbox{}, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
