package mailwatcher

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

const (
	mailWatcherSyncTimeout = 90 * time.Second
	mailWatcherActiveTTL   = 20 * time.Minute
	mailReconcileTimeout   = 5 * time.Minute
	mailReconcileLookback  = 30 * 24 * time.Hour
	mailReconcileRetry     = 10 * time.Minute
)

type Service struct {
	cfg             config.Config
	store           *store.Store
	mailbox         *mailboxservice.Service
	log             *slog.Logger
	wake            chan struct{}
	wakeMu          sync.Mutex
	wakeAccounts    map[string]bool
	activeMu        sync.Mutex
	activeUntil     map[string]time.Time
	statusMu        sync.RWMutex
	status          Status
	readyWorkers    map[string]bool
	lastPublishedAt time.Time
}

// Status 是后台监听的真实运行快照，区分 IMAP IDLE 和 Web API 低频轮询。
type Status struct {
	Running              bool      `json:"running"`
	Enabled              bool      `json:"enabled"`
	GroupCount           int       `json:"group_count"`
	IMAPGroupCount       int       `json:"imap_group_count"`
	WebPollingGroupCount int       `json:"web_polling_group_count"`
	WorkerCount          int       `json:"worker_count"`
	ConnectedWorkerCount int       `json:"connected_worker_count"`
	SyncedMessages       int       `json:"synced_messages"`
	IdleEvents           int       `json:"idle_events"`
	WebPolls             int       `json:"web_polls"`
	WebFallbacks         int       `json:"web_fallbacks"`
	StartedAt            time.Time `json:"started_at,omitempty"`
	LastCycleAt          time.Time `json:"last_cycle_at,omitempty"`
	LastSuccessAt        time.Time `json:"last_success_at,omitempty"`
	LastIdleConnectedAt  time.Time `json:"last_idle_connected_at,omitempty"`
	LastIdleEventAt      time.Time `json:"last_idle_event_at,omitempty"`
	LastErrorAt          time.Time `json:"last_error_at,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
	LastIdleErrorAt      time.Time `json:"last_idle_error_at,omitempty"`
	LastIdleError        string    `json:"last_idle_error,omitempty"`
}

type watchGroup struct {
	key       string
	session   domain.ICloudSession
	state     domain.LoginState
	mailboxes []domain.Mailbox
	hasIMAP   bool
	hasWeb    bool
	signature string
}

type idleWorker struct {
	cancel    context.CancelFunc
	signature string
}

func NewService(cfg config.Config, state *store.Store, mailbox *mailboxservice.Service, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	service := &Service{
		cfg:          cfg,
		store:        state,
		mailbox:      mailbox,
		log:          logger,
		wake:         make(chan struct{}, 1),
		wakeAccounts: make(map[string]bool),
		activeUntil:  make(map[string]time.Time),
		readyWorkers: make(map[string]bool),
	}
	if state != nil {
		var persisted Status
		if found, err := state.LoadRuntimeState("mailwatcher", &persisted); err == nil && found {
			persisted.Running = false
			persisted.WorkerCount = 0
			persisted.ConnectedWorkerCount = 0
			service.status = persisted
		}
	}
	return service
}

func (s *Service) Run(ctx context.Context) {
	reconcileInterval := time.Duration(s.cfg.MailWatcherPollMS) * time.Millisecond
	if reconcileInterval < time.Second {
		reconcileInterval = 3 * time.Second
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	workers := make(map[string]idleWorker)
	knownGroups := make(map[string]string)
	webNextAt := make(map[string]time.Time)
	webFailures := make(map[string]int)
	reconcileRetryAt := make(map[string]time.Time)
	defer stopIdleWorkers(workers)
	defer s.resetReadyWorkers()
	s.log.Info("后台邮件监听已启动", "分组重检间隔", reconcileInterval, "Web 轮询间隔", s.webPollInterval())
	s.updateStatus(func(status *Status) {
		status.Running = true
		status.StartedAt = time.Now()
	})
	defer s.updateStatus(func(status *Status) {
		status.Running = false
		status.WorkerCount = 0
		status.ConnectedWorkerCount = 0
	})

	enabledLastCycle := false
	cycle := func(targetAccountID string) {
		settings := s.store.Settings()
		enabled := s.cfg.MailWatcherEnabled && settings.EnableMailWatcher
		allowWebAPI := settings.EnableWebBackgroundMail
		groups := s.groups(allowWebAPI)
		imapGroups, webPollingGroups := groupModeCounts(groups)
		s.updateStatus(func(status *Status) {
			status.Enabled = enabled
			status.GroupCount = len(groups)
			status.IMAPGroupCount = imapGroups
			status.WebPollingGroupCount = webPollingGroups
			status.LastCycleAt = time.Now()
		})
		if !enabled {
			if enabledLastCycle {
				stopIdleWorkers(workers)
			}
			enabledLastCycle = false
			clear(knownGroups)
			clear(webNextAt)
			clear(webFailures)
			s.resetReadyWorkers()
			s.updateStatus(func(status *Status) { status.WorkerCount = 0 })
			return
		}

		s.ensureIdleWorkers(ctx, workers, groups)
		s.updateStatus(func(status *Status) { status.WorkerCount = len(workers) })

		now := time.Now()
		currentGroups := make(map[string]bool, len(groups))
		initialKeys := make(map[string]bool)
		for _, group := range groups {
			currentGroups[group.key] = true
			if !enabledLastCycle || knownGroups[group.key] != group.signature {
				initialKeys[group.key] = true
				knownGroups[group.key] = group.signature
			}
		}
		for key := range knownGroups {
			if currentGroups[key] {
				continue
			}
			delete(knownGroups, key)
			delete(webNextAt, key)
			delete(webFailures, key)
			delete(reconcileRetryAt, key)
		}
		enabledLastCycle = true

		if targetAccountID != "" {
			if group, found := findWatchGroup(groups, targetAccountID); found {
				s.syncWatchGroup(ctx, group, false, false)
				if group.hasWeb {
					webFailures[group.key] = 0
					webNextAt[group.key] = now.Add(s.webPollDelay(group.key, 0))
				}
			}
			return
		}

		for _, group := range groups {
			if !initialKeys[group.key] {
				continue
			}
			err := s.syncWatchGroup(ctx, group, true, !group.hasIMAP)
			if group.hasWeb {
				if err != nil {
					webFailures[group.key]++
				} else {
					webFailures[group.key] = 0
				}
				webNextAt[group.key] = now.Add(s.webPollDelay(group.key, webFailures[group.key]))
			}
		}
		for _, group := range groups {
			shouldWebPoll := group.hasWeb && (!group.hasIMAP || !s.workerReady(group.key))
			if !shouldWebPoll || initialKeys[group.key] || now.Before(webNextAt[group.key]) {
				continue
			}
			err := s.syncWatchGroup(ctx, group, false, true)
			if err != nil {
				webFailures[group.key]++
			} else {
				webFailures[group.key] = 0
			}
			webNextAt[group.key] = now.Add(s.webPollDelay(group.key, webFailures[group.key]))
		}
		for _, group := range groups {
			if initialKeys[group.key] || !group.hasIMAP || now.Before(reconcileRetryAt[group.key]) || !s.mailbox.MailReconcileDue(group.key, now) {
				continue
			}
			if err := s.reconcileWatchGroup(ctx, group); err != nil {
				reconcileRetryAt[group.key] = now.Add(mailReconcileRetry)
			} else {
				delete(reconcileRetryAt, group.key)
			}
		}
	}

	cycle("")
	for {
		select {
		case <-ctx.Done():
			s.log.Info("后台邮件监听已停止")
			return
		case <-s.wake:
			accountIDs := s.takeWakeAccounts()
			if len(accountIDs) == 0 {
				cycle("")
				continue
			}
			for _, accountID := range accountIDs {
				cycle(accountID)
			}
		case <-ticker.C:
			cycle("")
		}
	}
}

// Wake 只唤醒目标邮箱所属 Apple 主账号，不触发其他账号同步。
func (s *Service) Wake(mailboxID string) {
	mailboxID = strings.TrimSpace(mailboxID)
	accountID := ""
	if mailboxID != "" {
		s.activeMu.Lock()
		s.activeUntil[mailboxID] = time.Now().Add(mailWatcherActiveTTL)
		s.activeMu.Unlock()
		if mailbox, found := s.store.FindMailboxByID(mailboxID); found {
			accountID = strings.TrimSpace(mailbox.AccountID)
		}
	}
	if accountID != "" {
		s.wakeMu.Lock()
		s.wakeAccounts[accountID] = true
		s.wakeMu.Unlock()
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) takeWakeAccounts() []string {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	accountIDs := make([]string, 0, len(s.wakeAccounts))
	for accountID := range s.wakeAccounts {
		accountIDs = append(accountIDs, accountID)
	}
	clear(s.wakeAccounts)
	sort.Strings(accountIDs)
	return accountIDs
}

func (s *Service) syncWatchGroup(ctx context.Context, group watchGroup, initial, webPoll bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	after := time.Time{}
	limit := s.cfg.MailWatcherFetchLimit
	if initial {
		limit = s.cfg.MailWatcherInitialFetchLimit
		if s.cfg.MailWatcherLookbackHours > 0 {
			after = time.Now().Add(-time.Duration(s.cfg.MailWatcherLookbackHours) * time.Hour)
		}
	}
	syncCtx, cancel := context.WithTimeout(ctx, mailWatcherSyncTimeout)
	result, err := s.mailbox.SyncMailboxBatchWithOptions(syncCtx, group.mailboxes, mailboxservice.MessageSyncOptions{
		Mode: protocol.MailSyncModeVerification, Trigger: "watcher", After: after, Limit: limit,
		UseCursor: true, AllowWebAPI: group.hasWeb, AllowFallback: group.hasWeb, UseWebComplement: group.hasIMAP && group.hasWeb,
	})
	cancel()
	s.recordSyncResult(result, err, webPoll)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("后台账号级同步邮箱失败", "账号", group.session.AppleID, "邮箱数", len(group.mailboxes), "首次同步", initial, "Web 轮询", webPoll, "错误", err)
	}
	return err
}

func (s *Service) reconcileWatchGroup(ctx context.Context, group watchGroup) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	syncCtx, cancel := context.WithTimeout(ctx, mailReconcileTimeout)
	result, err := s.mailbox.SyncMailboxBatchWithOptions(syncCtx, group.mailboxes, mailboxservice.MessageSyncOptions{
		Mode: protocol.MailSyncModeAllRecent, Trigger: "reconcile", After: time.Now().Add(-mailReconcileLookback),
		FullScan: true, UseCursor: true, Reconcile: true,
		AllowWebAPI: group.hasWeb, AllowFallback: group.hasWeb, UseWebComplement: group.hasWeb,
	})
	cancel()
	s.recordSyncResult(result, err, false)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("邮件近期对账失败", "账号", group.session.AppleID, "邮箱数", len(group.mailboxes), "错误", err)
	}
	return err
}

func (s *Service) ensureIdleWorkers(ctx context.Context, workers map[string]idleWorker, groups []watchGroup) {
	seen := make(map[string]bool, len(groups))
	for _, group := range groups {
		if !group.hasIMAP {
			continue
		}
		seen[group.key] = true
		if worker, ok := workers[group.key]; ok && worker.signature == group.signature {
			continue
		}
		if worker, ok := workers[group.key]; ok {
			worker.cancel()
			s.markWorkerReady(group.key, false)
			delete(workers, group.key)
		}
		workerCtx, cancel := context.WithCancel(ctx)
		workers[group.key] = idleWorker{cancel: cancel, signature: group.signature}
		go s.runIdleWorker(workerCtx, group)
	}
	for key, worker := range workers {
		if seen[key] {
			continue
		}
		worker.cancel()
		s.markWorkerReady(key, false)
		delete(workers, key)
	}
}

func (s *Service) runIdleWorker(ctx context.Context, group watchGroup) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := protocol.WatchICloudIMAPExists(ctx, group.state, func() {
			s.markWorkerReady(group.key, true)
			s.updateStatus(func(status *Status) {
				status.LastIdleConnectedAt = time.Now()
				status.LastIdleError = ""
			})
		}, func() {
			if ctx.Err() != nil {
				return
			}
			s.updateStatus(func(status *Status) {
				status.IdleEvents++
				status.LastIdleEventAt = time.Now()
			})
			syncCtx, cancel := context.WithTimeout(ctx, mailWatcherSyncTimeout)
			result, syncErr := s.mailbox.SyncMailboxBatchWithOptions(syncCtx, group.mailboxes, mailboxservice.MessageSyncOptions{
				Mode: protocol.MailSyncModeVerification, Trigger: "imap-idle", Limit: s.cfg.MailWatcherFetchLimit,
				UseCursor: true, AllowWebAPI: group.hasWeb, AllowFallback: group.hasWeb, UseWebComplement: group.hasWeb,
			})
			cancel()
			s.recordSyncResult(result, syncErr, false)
			if syncErr != nil && ctx.Err() == nil {
				s.log.Warn("IMAP IDLE 触发同步失败", "账号", group.session.AppleID, "邮箱数", len(group.mailboxes), "错误", syncErr)
			}
		})
		s.markWorkerReady(group.key, false)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = time.Second
			continue
		}
		s.recordWatcherError(err)
		s.log.Warn("IMAP IDLE 已断开，准备重连", "账号", group.session.AppleID, "邮箱数", len(group.mailboxes), "等待", backoff, "错误", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// Snapshot 返回并发安全的后台监听运行快照。
func (s *Service) Snapshot() Status {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.status
}

func (s *Service) updateStatus(update func(*Status)) {
	s.statusMu.Lock()
	before := materialStatus(s.status)
	update(&s.status)
	after := materialStatus(s.status)
	now := time.Now()
	shouldPublish := before != after || s.lastPublishedAt.IsZero() || now.Sub(s.lastPublishedAt) >= 30*time.Second
	snapshot := s.status
	if shouldPublish {
		s.lastPublishedAt = now
	}
	s.statusMu.Unlock()
	if shouldPublish && s.store != nil {
		_ = s.store.SaveRuntimeState("mailwatcher", snapshot, true)
	}
}

type statusMaterial struct {
	Running              bool
	Enabled              bool
	GroupCount           int
	IMAPGroupCount       int
	WebPollingGroupCount int
	WorkerCount          int
	ConnectedWorkerCount int
	SyncedMessages       int
	IdleEvents           int
	WebPolls             int
	WebFallbacks         int
	LastError            string
	LastIdleError        string
}

func materialStatus(status Status) statusMaterial {
	return statusMaterial{
		Running: status.Running, Enabled: status.Enabled, GroupCount: status.GroupCount,
		IMAPGroupCount: status.IMAPGroupCount, WebPollingGroupCount: status.WebPollingGroupCount,
		WorkerCount: status.WorkerCount, ConnectedWorkerCount: status.ConnectedWorkerCount,
		SyncedMessages: status.SyncedMessages, IdleEvents: status.IdleEvents,
		WebPolls: status.WebPolls, WebFallbacks: status.WebFallbacks,
		LastError: status.LastError, LastIdleError: status.LastIdleError,
	}
}

func (s *Service) recordSyncResult(result mailboxservice.MailboxMessageSyncBatchResult, err error, webPoll bool) {
	now := time.Now()
	s.updateStatus(func(status *Status) {
		if result.SyncedMessages > 0 {
			status.SyncedMessages += result.SyncedMessages
		}
		if result.Fallbacks > 0 {
			status.WebFallbacks += result.Fallbacks
		}
		if webPoll {
			status.WebPolls++
		}
		if err != nil {
			status.LastError = err.Error()
			status.LastErrorAt = now
			return
		}
		status.LastSuccessAt = now
		status.LastError = ""
	})
}

func (s *Service) recordWatcherError(err error) {
	if err == nil {
		return
	}
	s.updateStatus(func(status *Status) {
		status.LastIdleError = err.Error()
		status.LastIdleErrorAt = time.Now()
	})
}

func (s *Service) markWorkerReady(key string, ready bool) {
	s.updateStatus(func(status *Status) {
		if ready {
			s.readyWorkers[key] = true
		} else {
			delete(s.readyWorkers, key)
		}
		status.ConnectedWorkerCount = len(s.readyWorkers)
	})
}

func (s *Service) workerReady(key string) bool {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.readyWorkers[key]
}

func (s *Service) resetReadyWorkers() {
	s.updateStatus(func(status *Status) {
		s.readyWorkers = make(map[string]bool)
		status.ConnectedWorkerCount = 0
	})
}

func (s *Service) groups(allowWebAPI bool) []watchGroup {
	active := s.activeMailboxIDs(time.Now())
	domainSettings := s.store.DomainMailSettings()
	type bucket struct {
		session   domain.ICloudSession
		state     domain.LoginState
		mailboxes []domain.Mailbox
		hasIMAP   bool
		hasWeb    bool
	}
	buckets := make(map[string]*bucket)
	type sessionEntry struct {
		session domain.ICloudSession
		found   bool
	}
	sessions := make(map[string]sessionEntry)
	for _, mailbox := range s.store.AllMailboxes() {
		if !mailbox.APIActive || !mailbox.ICloudActive || mailbox.Status == domain.StatusDisabled {
			continue
		}
		if mailbox.MailboxKind == domain.MailboxKindDomainForward && !domainSettings.Enabled {
			continue
		}
		accountID := strings.TrimSpace(mailbox.AccountID)
		if mailbox.MailboxKind == domain.MailboxKindDomainForward && strings.TrimSpace(mailbox.DomainRouteID) != "" {
			if route, found := s.store.DomainMailRouteForSync(mailbox.DomainRouteID); found && route.Enabled && route.ReceiverType == domain.DomainReceiverCustomIMAP {
				state := domain.LoginState{
					Kind: domain.LoginStateICloudIMAP, IMAPEmail: route.ForwardToEmail, IMAPUsername: route.IMAPUsername,
					IMAPHost: route.IMAPHost, IMAPPort: route.IMAPPort, IMAPAppPassword: route.IMAPPassword,
				}
				hasIMAP := strings.TrimSpace(state.IMAPUsername) != "" && strings.TrimSpace(state.IMAPAppPassword) != ""
				if !hasIMAP {
					continue
				}
				key := "domain-route:" + route.ID
				item := buckets[key]
				if item == nil {
					item = &bucket{session: domain.ICloudSession{AccountID: key, AppleID: route.ForwardToEmail}, state: state, hasIMAP: true}
					buckets[key] = item
				}
				item.mailboxes = append(item.mailboxes, mailbox)
				continue
			}
		}
		entry, cached := sessions[accountID]
		if !cached {
			entry.session, entry.found = s.store.ICloudSessionByAccountID(accountID)
			sessions[accountID] = entry
		}
		if !entry.found {
			continue
		}
		session := entry.session
		imapState, imapSaved := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP)
		hasIMAP := imapSaved && strings.TrimSpace(imapState.IMAPEmail) != "" && strings.TrimSpace(imapState.IMAPAppPassword) != ""
		hasWeb := allowWebAPI && protocol.CanUseICloudWebMail(session)
		if !hasIMAP && !hasWeb {
			continue
		}
		key := firstNonEmpty(session.AccountID, mailbox.AccountID, mailbox.OwnerID, "__mail__")
		item := buckets[key]
		if item == nil {
			item = &bucket{session: session, state: imapState, hasIMAP: hasIMAP, hasWeb: hasWeb}
			buckets[key] = item
		}
		item.mailboxes = append(item.mailboxes, mailbox)
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	groups := make([]watchGroup, 0, len(keys))
	for _, key := range keys {
		item := buckets[key]
		sort.Slice(item.mailboxes, func(i, j int) bool {
			leftActive := active[item.mailboxes[i].ID]
			rightActive := active[item.mailboxes[j].ID]
			if leftActive != rightActive {
				return leftActive
			}
			return item.mailboxes[i].Email < item.mailboxes[j].Email
		})
		groups = append(groups, watchGroup{
			key: key, session: item.session, state: item.state, mailboxes: item.mailboxes,
			hasIMAP: item.hasIMAP, hasWeb: item.hasWeb,
			signature: groupSignature(item.session, item.state, item.mailboxes),
		})
	}
	return groups
}

func (s *Service) activeMailboxIDs(now time.Time) map[string]bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	active := make(map[string]bool)
	for mailboxID, until := range s.activeUntil {
		if until.After(now) {
			active[mailboxID] = true
			continue
		}
		delete(s.activeUntil, mailboxID)
	}
	return active
}

func groupSignature(session domain.ICloudSession, state domain.LoginState, mailboxes []domain.Mailbox) string {
	parts := []string{
		session.AccountID, session.DSID, session.MailGatewayBaseURL, session.MailBaseURL, session.PremiumMailBaseURL,
		state.IMAPEmail, state.IMAPUsername, state.IMAPHost, fmt.Sprint(state.IMAPPort), state.IMAPAppPassword,
	}
	for _, cookie := range session.Cookies {
		parts = append(parts, cookie.Name, cookie.Value, cookie.Domain, cookie.Path, fmt.Sprint(cookie.Expires))
	}
	for _, mailbox := range mailboxes {
		parts = append(parts, mailbox.ID, mailbox.Email)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "|"))))
}

func groupModeCounts(groups []watchGroup) (imap, webPolling int) {
	for _, group := range groups {
		if group.hasIMAP {
			imap++
		} else if group.hasWeb {
			webPolling++
		}
	}
	return imap, webPolling
}

func findWatchGroup(groups []watchGroup, accountID string) (watchGroup, bool) {
	accountID = strings.TrimSpace(accountID)
	for _, group := range groups {
		if group.key == accountID || strings.TrimSpace(group.session.AccountID) == accountID {
			return group, true
		}
	}
	return watchGroup{}, false
}

func (s *Service) webPollInterval() time.Duration {
	interval := time.Duration(s.cfg.MailWatcherWebPollMS) * time.Millisecond
	if interval < 15*time.Second {
		return time.Minute
	}
	return interval
}

func (s *Service) webPollDelay(key string, failures int) time.Duration {
	delay := s.webPollInterval()
	for attempt := 0; attempt < failures && delay < 15*time.Minute; attempt++ {
		delay *= 2
	}
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	jitter := time.Duration(int(sum[0])%21) * delay / 100
	return delay + jitter
}

func stopIdleWorkers(workers map[string]idleWorker) {
	for key, worker := range workers {
		worker.cancel()
		delete(workers, key)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
