package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/apple"
	"icloud-privacy-mail-v2/internal/auth"
	"icloud-privacy-mail-v2/internal/buildinfo"
	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/mailwatcher"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/scheduler"
	"icloud-privacy-mail-v2/internal/serverchan"
	"icloud-privacy-mail-v2/internal/store"
	"icloud-privacy-mail-v2/internal/updatecheck"
	"icloud-privacy-mail-v2/internal/webui"
)

type contextKey string

const adminContextKey contextKey = "admin"

type appleKeepAliveTarget struct {
	CheckedAt time.Time
	NextAt    time.Time
	Interval  time.Duration
}

type Server struct {
	cfg                 config.Config
	runtimeCtx          context.Context
	store               *store.Store
	auth                *auth.Service
	apple               *apple.Service
	mailbox             *mailboxservice.Service
	scheduler           *scheduler.Service
	watcher             *mailwatcher.Service
	updates             *updatecheck.Service
	serverChan          serverchan.Sender
	log                 *slog.Logger
	mux                 *http.ServeMux
	keepAliveMu         sync.RWMutex
	keepAliveNextAt     time.Time
	keepAliveInterval   time.Duration
	keepAliveState      func(context.Context, domain.LoginState) (domain.LoginState, error)
	keepAliveTargets    map[string]appleKeepAliveTarget
	keepAliveIntervalFn func(time.Duration, int) time.Duration
}

func New(cfg config.Config, state *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:              cfg,
		runtimeCtx:       context.Background(),
		store:            state,
		auth:             auth.NewService(state, time.Duration(cfg.SessionTTLHours)*time.Hour),
		apple:            apple.NewService(cfg, state),
		mailbox:          mailboxservice.NewService(cfg, state),
		updates:          updatecheck.New(cfg.UpdateEnabled, cfg.UpdateRepository),
		serverChan:       serverchan.NewClient(),
		log:              logger,
		mux:              http.NewServeMux(),
		keepAliveTargets: make(map[string]appleKeepAliveTarget),
	}
	state.SetChangeLogLimit(cfg.DatabaseChangeLogLimit)
	s.scheduler = scheduler.NewService(state, s.mailbox)
	s.watcher = mailwatcher.NewService(cfg, state, s.mailbox, logger)
	s.keepAliveState = s.apple.KeepAliveState
	s.keepAliveIntervalFn = randomAppleKeepAliveInterval
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/health", s.handlePublicHealth)
	s.mux.HandleFunc("POST /api/v1/mailboxes/claim", s.handlePublicClaimMailbox)
	s.mux.HandleFunc("POST /api/v1/mailboxes/lookup", s.handlePublicLookupMailboxes)
	s.mux.HandleFunc("GET /api/v1/mailboxes/{email}/code", s.handlePublicMailboxCode)
	s.mux.HandleFunc("GET /api/v1/mailboxes/{email}/messages", s.handlePublicMailboxMessages)
	s.mux.HandleFunc("GET /api/v1/mailboxes/{email}/messages/{messageID}", s.handlePublicMailboxMessage)
	s.mux.HandleFunc("POST /api/v1/mailboxes/{email}/commit", s.handlePublicMailboxLeaseCommitCompat)
	s.mux.HandleFunc("POST /api/v1/mailboxes/{email}/release", s.handlePublicMailboxLeaseReleaseCompat)
	s.mux.HandleFunc("POST /api/v1/mailboxes/{email}/renew", s.handlePublicMailboxLeaseRenewCompat)
	s.mux.HandleFunc("GET /api/v1/mailbox-leases/{lease_id}", s.handlePublicMailboxLease)
	s.mux.HandleFunc("POST /api/v1/mailbox-leases/{lease_id}/commit", s.handlePublicMailboxLeaseCommit)
	s.mux.HandleFunc("POST /api/v1/mailbox-leases/{lease_id}/release", s.handlePublicMailboxLeaseRelease)
	s.mux.HandleFunc("POST /api/v1/mailbox-leases/{lease_id}/renew", s.handlePublicMailboxLeaseRenew)
	s.mux.HandleFunc("POST /api/v1/mailbox-leases/{lease_id}/note", s.handlePublicMailboxLeaseNote)
	s.mux.HandleFunc("GET /api/v1/public-code/status", s.handlePublicCodePageStatus)
	s.mux.HandleFunc("GET /api/v1/public-code", s.handlePublicCodePageLookup)
	s.mux.HandleFunc("GET /api/v1/public-code/messages", s.handlePublicCodePageMessages)
	s.mux.HandleFunc("GET /api/v1/public-code/messages/{messageID}", s.handlePublicCodePageMessage)
	s.mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("POST /api/auth/setup", s.handleAuthSetup)
	s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.protected(s.handleAuthLogout))
	s.mux.HandleFunc("GET /api/dashboard", s.protected(s.handleDashboard))
	s.mux.HandleFunc("GET /api/apple-accounts", s.protected(s.handleAppleAccounts))
	s.mux.HandleFunc("GET /api/apple-accounts/{id}", s.protected(s.handleAppleAccount))
	s.mux.HandleFunc("DELETE /api/apple-accounts/{id}", s.protected(s.handleAppleAccountDelete))
	s.mux.HandleFunc("POST /api/apple-accounts/login/start", s.protected(s.handleAppleLoginStart))
	s.mux.HandleFunc("POST /api/apple-accounts/login/2fa", s.protected(s.handleAppleLogin2FA))
	s.mux.HandleFunc("POST /api/apple-accounts/{id}/check", s.protected(s.handleAppleAccountCheck))
	s.mux.HandleFunc("POST /api/apple-accounts/{id}/imap", s.protected(s.handleAppleAccountSaveIMAP))
	s.mux.HandleFunc("POST /api/apple-accounts/{id}/mailboxes", s.protected(s.handleCreatePrivacyMailbox))
	s.mux.HandleFunc("POST /api/apple-accounts/{id}/mailboxes/sync", s.protected(s.handleSyncPrivacyMailboxes))
	s.mux.HandleFunc("GET /api/apple-mail/cleanup/status", s.protected(s.handleAppleMailCleanupStatus))
	s.mux.HandleFunc("POST /api/apple-mail/cleanup", s.protected(s.handleAppleMailCleanupStart))
	s.mux.HandleFunc("POST /api/apple-mail/cleanup/cancel", s.protected(s.handleAppleMailCleanupCancel))
	s.mux.HandleFunc("GET /api/mailboxes", s.protected(s.handleMailboxes))
	s.mux.HandleFunc("POST /api/mailboxes", s.protected(s.handleImportMailbox))
	s.mux.HandleFunc("POST /api/mailboxes/resolve", s.protected(s.handleMailboxResolve))
	s.mux.HandleFunc("GET /api/mailboxes/sync-messages/status", s.protected(s.handleExistingMailboxMessagesSyncStatus))
	s.mux.HandleFunc("POST /api/mailboxes/sync-messages", s.protected(s.handleExistingMailboxMessagesSync))
	s.mux.HandleFunc("POST /api/mailboxes/remote-clean", s.protected(s.handleMailboxesRemoteClean))
	s.mux.HandleFunc("GET /api/mailboxes/{id}", s.protected(s.handleMailbox))
	s.mux.HandleFunc("POST /api/mailboxes/{id}/status", s.protected(s.handleMailboxStatus))
	s.mux.HandleFunc("POST /api/mailboxes/{id}/sync", s.protected(s.handleMailboxSync))
	s.mux.HandleFunc("POST /api/mailboxes/{id}/remote-clean", s.protected(s.handleMailboxRemoteClean))
	s.mux.HandleFunc("DELETE /api/mailboxes/{id}", s.protected(s.handleMailboxDelete))
	s.mux.HandleFunc("GET /api/mailboxes/{id}/messages", s.protected(s.handleMailboxMessages))
	s.mux.HandleFunc("GET /api/mailboxes/{id}/messages/{messageID}", s.protected(s.handleMailboxMessage))
	s.mux.HandleFunc("GET /api/mailboxes/{id}/code", s.protected(s.handleMailboxCode))
	s.mux.HandleFunc("GET /api/domain-mail/settings", s.protected(s.handleDomainMailSettings))
	s.mux.HandleFunc("PUT /api/domain-mail/settings", s.protected(s.handleSaveDomainMailSettings))
	s.mux.HandleFunc("GET /api/domain-mailboxes", s.protected(s.handleDomainMailboxes))
	s.mux.HandleFunc("POST /api/domain-mailboxes/generate", s.protected(s.handleGenerateDomainMailboxes))
	s.mux.HandleFunc("POST /api/domain-mailboxes/sync", s.protected(s.handleSyncDomainMailboxes))
	s.mux.HandleFunc("POST /api/domain-mailboxes/messages/delete", s.protected(s.handleDeleteDomainMailboxMessages))
	s.mux.HandleFunc("POST /api/domain-mailboxes/remote-clean", s.protected(s.handleCleanDomainMailboxRemoteMessages))
	s.mux.HandleFunc("POST /api/domain-mailboxes/delete", s.protected(s.handleDeleteDomainMailboxes))
	s.mux.HandleFunc("DELETE /api/domain-mailboxes/{id}", s.protected(s.handleDeleteDomainMailbox))
	s.mux.HandleFunc("GET /api/tasks", s.protected(s.handleTasks))
	s.mux.HandleFunc("GET /api/settings", s.protected(s.handleSettings))
	s.mux.HandleFunc("PUT /api/settings", s.protected(s.handleSaveSettings))
	s.mux.HandleFunc("POST /api/server-chan/test", s.protected(s.handleServerChanTest))
	s.mux.HandleFunc("GET /api/update/status", s.protected(s.handleUpdateStatus))
	s.mux.HandleFunc("GET /api/create-settings", s.protected(s.handleCreateSettings))
	s.mux.HandleFunc("PUT /api/create-settings", s.protected(s.handleSaveCreateSettings))
	s.mux.HandleFunc("GET /api/events", s.protected(s.handleEvents))
	s.mux.HandleFunc("GET /api/realtime", s.protected(s.handleRealtime))
	s.mux.HandleFunc("GET /api/database/status", s.protected(s.handleDatabaseStatus))
	s.mux.HandleFunc("POST /api/database/backup", s.protected(s.handleDatabaseBackup))
	s.mux.HandleFunc("POST /api/database/check", s.protected(s.handleDatabaseCheck))
	s.mux.HandleFunc("POST /api/database/optimize", s.protected(s.handleDatabaseOptimize))
	s.mux.HandleFunc("POST /api/events/clear", s.protected(s.handleClearEvents))
	s.mux.HandleFunc("GET /api/runtime/export", s.protected(s.handleExportRuntime))
	s.mux.HandleFunc("GET /api/runtime/export-mailbox-apis", s.protected(s.handleExportMailboxAPIs))
	s.mux.HandleFunc("GET /api/runtime/export-mailbox-emails", s.protected(s.handleExportMailboxEmails))
	s.mux.HandleFunc("GET /api/scheduler/status", s.protected(s.handleSchedulerStatus))
	s.mux.HandleFunc("POST /api/scheduler/start", s.protected(s.handleSchedulerStart))
	s.mux.HandleFunc("POST /api/scheduler/stop", s.protected(s.handleSchedulerStop))
	s.mux.HandleFunc("POST /api/scheduler/logs/clear", s.protected(s.handleSchedulerClearLogs))
	s.mux.HandleFunc("/api/", s.handleUnknownAPI)
	s.mux.HandleFunc("/manage", s.handleRemovedManage)
	s.mux.HandleFunc("/manage/", s.handleRemovedManage)
	s.mux.Handle("/", webui.Handler())
}

// StartBackground 启动与 HTTP 服务共用生命周期的后台任务。
func (s *Server) StartBackground(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.runtimeCtx = ctx
	s.scheduler.Resume(ctx)
	go s.runMailboxLeaseReaper(ctx)
	go s.runDatabaseMaintenance(ctx)
	go s.runMessageContentBackfill(ctx)
	s.startAccountLoginStateNotifications(ctx)
	if s.cfg.AppleAccountKeepAliveEnabled {
		go s.runAppleKeepAlive(ctx)
	}
	if s.cfg.MailWatcherEnabled {
		go s.watcher.Run(ctx)
	}
}

func (s *Server) runMessageContentBackfill(ctx context.Context) {
	result, err := s.mailbox.BackfillMessageContent(ctx)
	if result.Total == 0 {
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("旧邮件完整正文补全未全部完成", "总数", result.Total, "已补全", result.Updated, "失败", result.Failed, "错误", err)
		}
		return
	}
	s.log.Info("旧邮件完整正文补全完成", "总数", result.Total, "已补全", result.Updated)
}

func (s *Server) runMailboxLeaseReaper(ctx context.Context) {
	interval := time.Duration(s.cfg.PublicMailboxLeaseSweepSeconds) * time.Second
	if interval < 5*time.Second {
		interval = 30 * time.Second
	}
	reap := func() {
		count, err := s.store.ExpireMailboxLeases(time.Now())
		if err != nil {
			s.log.Warn("回收过期邮箱租约失败", "错误", err)
			return
		}
		if count > 0 {
			s.log.Info("已回收过期邮箱租约", "数量", count)
		}
	}
	reap()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}

func (s *Server) runAppleKeepAlive(ctx context.Context) {
	baseInterval := time.Duration(s.cfg.AppleAccountKeepAliveMS) * time.Millisecond
	if baseInterval <= 0 {
		baseInterval = 3 * time.Minute
	}
	scanInterval := appleKeepAliveScanInterval(baseInterval)
	defer s.setAppleKeepAliveSchedule(time.Time{}, 0)
	s.log.Info("Apple 登录态保活已启动", "基础间隔", baseInterval, "扫描间隔", scanInterval, "每轮随机", fmt.Sprintf("±%d%%", s.cfg.AppleAccountKeepAliveJitterPercent))
	s.keepAliveAppleRound(ctx, baseInterval)
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		s.setAppleKeepAliveSchedule(time.Now().Add(scanInterval), baseInterval)
		select {
		case <-ctx.Done():
			s.log.Info("Apple 登录态保活已停止")
			return
		case <-ticker.C:
			s.keepAliveAppleRound(ctx, baseInterval)
		}
	}
}

func appleKeepAliveScanInterval(base time.Duration) time.Duration {
	if base <= 0 {
		base = 3 * time.Minute
	}
	interval := base / 4
	if interval > 30*time.Second {
		return 30 * time.Second
	}
	if interval < 5*time.Second {
		return 5 * time.Second
	}
	return interval
}

func (s *Server) keepAliveAppleRound(ctx context.Context, baseInterval time.Duration) {
	if ctx.Err() != nil || !s.store.Settings().EnableAppleKeepAlive {
		return
	}
	keepAliveState := s.keepAliveState
	if keepAliveState == nil {
		keepAliveState = s.apple.KeepAliveState
	}
	now := time.Now()
	for _, session := range s.store.ICloudSessions() {
		if ctx.Err() != nil {
			return
		}
		state, ok := protocol.LoginStateForKind(session, domain.LoginStateAppleAccount)
		if !ok || !appleKeepAliveEligible(state) {
			continue
		}
		nextAt, _ := s.appleKeepAliveTargetForSession(session, state, baseInterval)
		if now.Before(nextAt) {
			continue
		}
		accountLabel := strings.TrimSpace(session.AppleID)
		if accountLabel == "" {
			accountLabel = session.AccountID
		}
		s.recordAppleKeepAliveEvent("info", fmt.Sprintf("开始 Apple 登录态保活：%s", accountLabel))
		callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		next, err := keepAliveState(callCtx, state)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			code, _, _ := protocol.ErrorDetails(err)
			if code == "apple_account_auth_failed" {
				s.clearAppleKeepAliveTarget(session)
				state.LastCheckedAt = time.Now()
				state.LastCheckOK = false
				state.LastStatusMessage = "Apple Account 登录态已失效：" + err.Error()
				session = protocol.WithLoginState(session, state)
				message := fmt.Sprintf("Apple 登录态保活失败：%s；登录态已失效，需要重新登录；%s", accountLabel, err.Error())
				if _, saveErr := s.store.SaveICloudSessionWithEvent(session, "error", message); saveErr != nil {
					s.log.Warn("保存 Apple 登录态失效结果失败", "账号", session.AccountID, "错误", saveErr)
				}
			} else {
				s.recordAppleKeepAliveEvent("warning", fmt.Sprintf("Apple 登录态保活临时失败：%s；%s", accountLabel, err.Error()))
			}
			s.log.Warn("Apple 登录态保活失败", "账号", session.AccountID, "Apple ID", session.AppleID, "错误", err)
			continue
		}
		next.Kind = domain.LoginStateAppleAccount
		next.LastCheckedAt = time.Now()
		next.LastCheckOK = true
		next.LastStatusMessage = "Apple Account 登录态保活成功"
		session = protocol.WithLoginState(session, next)
		nextInterval := s.resetAppleKeepAliveTarget(session, next, baseInterval)
		message := fmt.Sprintf("Apple 登录态保活成功：%s；下次目标间隔 %s", accountLabel, nextInterval.Round(time.Second))
		if _, err := s.store.SaveICloudSessionWithEvent(session, "info", message); err != nil {
			s.log.Warn("保存 Apple 登录态保活结果失败", "账号", session.AccountID, "错误", err)
			continue
		}
		s.log.Info("Apple 登录态保活成功", "账号", session.AccountID, "Apple ID", session.AppleID, "下次目标间隔", nextInterval)
	}
}

func (s *Server) recordAppleKeepAliveEvent(level, message string) {
	if err := s.store.RecordEvent(level, "apple", message); err != nil {
		s.log.Warn("保存 Apple 登录态保活运行记录失败", "错误", err)
	}
}

func appleKeepAliveEligible(state domain.LoginState) bool {
	if strings.TrimSpace(state.Scnt) == "" || strings.TrimSpace(state.APIKey) == "" || len(state.Cookies) == 0 {
		return false
	}
	return state.LastCheckedAt.IsZero() || state.LastCheckOK
}

func randomAppleKeepAliveInterval(base time.Duration, jitterPercent int) time.Duration {
	if base <= 0 {
		base = 3 * time.Minute
	}
	if jitterPercent <= 0 {
		return base
	}
	if jitterPercent > 50 {
		jitterPercent = 50
	}
	spread := int64(base) * int64(jitterPercent) / 100
	if spread <= 0 {
		return base
	}
	offset := rand.Int64N(spread*2+1) - spread
	interval := base + time.Duration(offset)
	if interval < 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

func (s *Server) appleKeepAliveTargetForSession(session domain.ICloudSession, state domain.LoginState, baseInterval time.Duration) (time.Time, time.Duration) {
	key := appleKeepAliveSessionKey(session)
	s.keepAliveMu.Lock()
	defer s.keepAliveMu.Unlock()
	target, ok := s.keepAliveTargets[key]
	if ok && target.CheckedAt.Equal(state.LastCheckedAt) {
		return target.NextAt, target.Interval
	}
	interval := s.nextAppleKeepAliveInterval(baseInterval)
	nextAt := time.Now()
	if !state.LastCheckedAt.IsZero() {
		nextAt = state.LastCheckedAt.Add(interval)
	}
	target = appleKeepAliveTarget{CheckedAt: state.LastCheckedAt, NextAt: nextAt, Interval: interval}
	s.keepAliveTargets[key] = target
	return target.NextAt, target.Interval
}

func (s *Server) resetAppleKeepAliveTarget(session domain.ICloudSession, state domain.LoginState, baseInterval time.Duration) time.Duration {
	interval := s.nextAppleKeepAliveInterval(baseInterval)
	s.keepAliveMu.Lock()
	s.keepAliveTargets[appleKeepAliveSessionKey(session)] = appleKeepAliveTarget{
		CheckedAt: state.LastCheckedAt,
		NextAt:    state.LastCheckedAt.Add(interval),
		Interval:  interval,
	}
	s.keepAliveMu.Unlock()
	return interval
}

func (s *Server) clearAppleKeepAliveTarget(session domain.ICloudSession) {
	s.keepAliveMu.Lock()
	delete(s.keepAliveTargets, appleKeepAliveSessionKey(session))
	s.keepAliveMu.Unlock()
}

func (s *Server) nextAppleKeepAliveInterval(baseInterval time.Duration) time.Duration {
	intervalFn := s.keepAliveIntervalFn
	if intervalFn == nil {
		intervalFn = randomAppleKeepAliveInterval
	}
	return intervalFn(baseInterval, s.cfg.AppleAccountKeepAliveJitterPercent)
}

func appleKeepAliveSessionKey(session domain.ICloudSession) string {
	if accountID := strings.TrimSpace(session.AccountID); accountID != "" {
		return accountID
	}
	if appleID := strings.ToLower(strings.TrimSpace(session.AppleID)); appleID != "" {
		return appleID
	}
	return "default"
}

func (s *Server) setAppleKeepAliveSchedule(nextAt time.Time, interval time.Duration) {
	s.keepAliveMu.Lock()
	s.keepAliveNextAt = nextAt
	s.keepAliveInterval = interval
	s.keepAliveMu.Unlock()
}

func (s *Server) appleKeepAliveSchedule() (time.Time, time.Duration) {
	s.keepAliveMu.RLock()
	defer s.keepAliveMu.RUnlock()
	return s.keepAliveNextAt, s.keepAliveInterval
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.CookieName)
		if err != nil || strings.TrimSpace(cookie.Value) == "" {
			writeError(w, http.StatusUnauthorized, "auth_required", "请先登录")
			return
		}
		admin, ok := s.auth.Authenticate(cookie.Value)
		if !ok {
			writeError(w, http.StatusUnauthorized, "auth_required", "登录态已失效，请重新登录")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), adminContextKey, admin)))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	current := buildinfo.Current()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"status":  "ok",
			"version": current.Version,
			"commit":  current.Commit,
		},
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"setup_required": false, "authenticated": false}
	if _, ok := s.store.Admin(); !ok {
		data["setup_required"] = true
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
		return
	}
	if cookie, err := r.Cookie(auth.CookieName); err == nil {
		if admin, ok := s.auth.Authenticate(cookie.Value); ok {
			data["authenticated"] = true
			data["admin"] = publicAdmin(admin)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var body credentialPayload
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	result, err := s.auth.Setup(body.Username, body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "setup_failed", err.Error())
		return
	}
	s.setSessionCookie(w, result)
	s.notifyAdminLogin(r, result.Admin)
	writeJSON(w, http.StatusCreated, map[string]any{"success": true, "data": map[string]any{"admin": publicAdmin(result.Admin)}})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var body credentialPayload
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	result, err := s.auth.Login(body.Username, body.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "login_failed", err.Error())
		return
	}
	s.setSessionCookie(w, result)
	s.notifyAdminLogin(r, result.Admin)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"admin": publicAdmin(result.Admin)}})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.CookieName); err == nil {
		if err := s.auth.Logout(cookie.Value); err != nil {
			s.log.Warn("退出登录态清理失败", "错误", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": s.store.Dashboard()})
}

func (s *Server) handleAppleAccounts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"items":        s.apple.Accounts(),
			"module_ready": true,
		},
	})
}

func (s *Server) handleAppleAccount(w http.ResponseWriter, r *http.Request) {
	account, err := s.apple.Account(r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"account": account}})
}

func (s *Server) handleAppleAccountDelete(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	state := s.scheduler.Snapshot()
	if state.Running {
		for _, runningAccountID := range state.AccountIDs {
			if runningAccountID == accountID {
				writeError(w, http.StatusConflict, "scheduler_account_in_use", "该账号正在参与自动创建，请先停止任务再删除")
				return
			}
		}
	}
	deleted, err := s.store.DeleteAppleAccount(accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"deleted": deleted}})
}

func (s *Server) handleAppleLoginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Flow            protocol.AuthFlow `json:"flow"`
		AppleID         string            `json:"apple_id"`
		Password        string            `json:"password"`
		TwoFactorMethod string            `json:"two_factor_method"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	result, err := s.apple.StartLogin(r.Context(), apple.LoginRequest{
		Flow: body.Flow, AppleID: body.AppleID, Password: body.Password, TwoFactorMethod: body.TwoFactorMethod,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

func (s *Server) handleAppleLogin2FA(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PendingID   string          `json:"pending_id"`
		Code        string          `json:"code"`
		PhoneNumber json.RawMessage `json:"phone_number,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	result, err := s.apple.Submit2FA(r.Context(), body.PendingID, body.Code, body.PhoneNumber)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

func (s *Server) handleAppleAccountCheck(w http.ResponseWriter, r *http.Request) {
	account, err := s.apple.Check(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "code": "session_check_failed", "message": err.Error(), "data": map[string]any{"account": account}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"account": account}})
}

func (s *Server) handleAppleAccountSaveIMAP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email       string `json:"email"`
		AppPassword string `json:"app_password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	account, err := s.apple.SaveIMAP(r.Context(), r.PathValue("id"), body.Email, body.AppPassword)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.watcher.Wake("")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"account": account}})
}

func (s *Server) handleCreatePrivacyMailbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label   string `json:"label"`
		Note    string `json:"note"`
		Channel string `json:"channel"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	accountID := r.PathValue("id")
	mailbox, err := s.mailbox.Create(r.Context(), accountID, body.Label, body.Note, body.Channel)
	if err != nil {
		s.scheduler.RecordManualFailure(accountID, body.Label, err)
		writeServiceError(w, err)
		return
	}
	schedulerState := s.scheduler.RecordManualSuccess(accountID, mailbox)
	mailbox.APIToken = ""
	writeJSON(w, http.StatusCreated, map[string]any{"success": true, "data": map[string]any{"mailbox": mailbox, "scheduler": schedulerState}})
}

func (s *Server) handleSyncPrivacyMailboxes(w http.ResponseWriter, r *http.Request) {
	items, err := s.mailbox.SyncRemote(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	for i := range items {
		items[i].APIToken = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": items, "count": len(items)}})
}

func (s *Server) handleMailboxes(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page := parsePositiveInt(query.Get("page"), 1)
	pageSize := parsePositiveInt(query.Get("page_size"), 0)
	result := s.store.Mailboxes(query.Get("q"), query.Get("status"), query.Get("account_id"), page, pageSize)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

func (s *Server) handleMailboxResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Emails []string `json:"emails"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	type resolvedMailbox struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		AccountID string `json:"account_id"`
	}
	items := make([]resolvedMailbox, 0, len(body.Emails))
	missing := make([]string, 0)
	seen := make(map[string]struct{}, len(body.Emails))
	for _, value := range body.Emails {
		email := strings.ToLower(strings.TrimSpace(value))
		if email == "" {
			continue
		}
		if _, exists := seen[email]; exists {
			continue
		}
		seen[email] = struct{}{}
		mailbox, ok := s.store.FindMailboxByEmail(email)
		if !ok {
			missing = append(missing, email)
			continue
		}
		items = append(items, resolvedMailbox{ID: mailbox.ID, Email: mailbox.Email, AccountID: mailbox.AccountID})
	}
	if len(seen) == 0 {
		writeError(w, http.StatusBadRequest, "email_list_empty", "请至少输入一个邮箱地址")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": items, "missing": missing}})
}

func (s *Server) handleMailbox(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := s.store.FindMailboxByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "mailbox_not_found", "邮箱不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"mailbox": mailbox}})
}

func (s *Server) handleMailboxStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIActive    *bool   `json:"api_active"`
		ICloudActive *bool   `json:"icloud_active"`
		Status       string  `json:"status"`
		Note         *string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if body.Status != "" && !validMailboxStatus(body.Status) {
		writeError(w, http.StatusBadRequest, "invalid_status", "邮箱状态不正确")
		return
	}
	mailbox, err := s.store.SetMailboxStatus(r.PathValue("id"), body.APIActive, body.ICloudActive, body.Status, body.Note)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	mailbox.APIToken = ""
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"mailbox": mailbox}})
}

func (s *Server) handleMailboxSync(w http.ResponseWriter, r *http.Request) {
	result, err := s.mailbox.SyncMailboxMessages(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data := struct {
		mailboxservice.MailboxMessageSyncBatchResult
		Synced int `json:"synced"`
	}{MailboxMessageSyncBatchResult: result, Synced: result.SyncedMessages}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}

func (s *Server) handleExistingMailboxMessagesSync(w http.ResponseWriter, r *http.Request) {
	job, err := s.mailbox.StartExistingMailboxMessageSync(s.runtimeCtx)
	if err != nil {
		if strings.Contains(err.Error(), "正在运行") {
			writeError(w, http.StatusConflict, "mailbox_message_sync_running", err.Error())
			return
		}
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "data": map[string]any{"job": job}})
}

func (s *Server) handleExistingMailboxMessagesSyncStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"job": s.mailbox.ExistingMailboxMessageSyncStatus()}})
}

func (s *Server) handleImportMailbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID string `json:"account_id"`
		Email     string `json:"email"`
		Label     string `json:"label"`
		Note      string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	mailbox, created, err := s.mailbox.ImportLocal(body.AccountID, body.Email, body.Label, body.Note)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"success": true, "data": map[string]any{"mailbox": mailbox, "created": created}})
}

func (s *Server) handleMailboxRemoteClean(w http.ResponseWriter, r *http.Request) {
	options := mailboxservice.RemoteCleanupOptions{MoveSynced: true, EmptyTrash: true}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &options); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	result, err := s.mailbox.CleanRemoteMessages(r.Context(), r.PathValue("id"), options)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"cleanup": result}})
}

func (s *Server) handleAppleMailCleanupStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"job": s.mailbox.AppleMailCleanupStatus()}})
}

func (s *Server) handleAppleMailCleanupStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountIDs []string `json:"account_ids"`
		Scope      string   `json:"scope"`
		Strategy   string   `json:"strategy"`
		PurgeLocal *bool    `json:"purge_local"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	purgeLocal := true
	if body.PurgeLocal != nil {
		purgeLocal = *body.PurgeLocal
	}
	job, err := s.mailbox.StartAppleMailCleanup(s.runtimeCtx, mailboxservice.AppleMailCleanupRequest{
		AccountIDs: body.AccountIDs,
		Scope:      body.Scope,
		Strategy:   body.Strategy,
		PurgeLocal: purgeLocal,
	})
	if err != nil {
		if strings.Contains(err.Error(), "正在运行") {
			writeError(w, http.StatusConflict, "apple_mail_cleanup_running", err.Error())
			return
		}
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "data": map[string]any{"job": job}})
}

func (s *Server) handleAppleMailCleanupCancel(w http.ResponseWriter, _ *http.Request) {
	job := s.mailbox.CancelAppleMailCleanup("已手动取消全部 Apple 邮件清理任务")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"job": job}})
}

func (s *Server) handleMailboxesRemoteClean(w http.ResponseWriter, r *http.Request) {
	options := mailboxservice.RemoteCleanupOptions{MoveSynced: true, EmptyTrash: true}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &options); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	if strings.TrimSpace(options.AccountID) != "" {
		if _, ok := s.store.FindAppleAccount(options.AccountID); !ok {
			writeError(w, http.StatusNotFound, "account_not_found", "Apple 账号不存在")
			return
		}
	}
	result, err := s.mailbox.CleanRemoteMailboxes(r.Context(), options)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

func (s *Server) handleMailboxDelete(w http.ResponseWriter, r *http.Request) {
	var err error
	if parseBool(r.URL.Query().Get("local_only")) {
		err = s.mailbox.DeleteLocal(r.PathValue("id"))
	} else {
		err = s.mailbox.DeleteCompletely(r.Context(), r.PathValue("id"))
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleMailboxMessages(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.store.FindMailboxByID(r.PathValue("id")); !ok {
		writeError(w, http.StatusNotFound, "mailbox_not_found", "邮箱不存在")
		return
	}
	items := s.store.MessagesForMailbox(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": items}})
}

func (s *Server) handleMailboxMessage(w http.ResponseWriter, r *http.Request) {
	message, err := s.mailbox.MessageContent(r.Context(), r.PathValue("id"), r.PathValue("messageID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"message": message}})
}

func (s *Server) handleMailboxCode(w http.ResponseWriter, r *http.Request) {
	after, err := parseRFC3339(r.URL.Query().Get("after"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_after", err.Error())
		return
	}
	result, err := s.mailbox.Code(r.Context(), r.PathValue("id"), after, r.URL.Query().Get("keyword"), parseBool(r.URL.Query().Get("allow_stale")))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

func (s *Server) handleTasks(w http.ResponseWriter, _ *http.Request) {
	settings := s.store.Settings()
	schedulerState := s.scheduler.Snapshot()
	watcherSnapshot := s.watcher.Snapshot()
	watcherStatus := "idle"
	watcherDescription := "后台监听尚未开启"
	if !s.cfg.MailWatcherEnabled {
		watcherDescription = "config.json 已关闭后台邮件监听能力"
	} else if settings.EnableMailWatcher {
		switch {
		case !watcherSnapshot.Running:
			watcherStatus = "starting"
			watcherDescription = "后台监听正在启动"
		case watcherSnapshot.GroupCount == 0:
			watcherStatus = "waiting"
			watcherDescription = "已开启，但没有找到同时具备 IMAP 登录态和可用邮箱的账号"
		case watcherSnapshot.LastError != "":
			watcherStatus = "failed"
			watcherDescription = "IMAP 同步异常：" + watcherSnapshot.LastError
		case watcherSnapshot.ConnectedWorkerCount == 0 && watcherSnapshot.LastIdleError != "":
			watcherStatus = "failed"
			watcherDescription = "IMAP IDLE 连接异常：" + watcherSnapshot.LastIdleError
		default:
			watcherStatus = "running"
			watcherDescription = fmt.Sprintf("正在监听 %d 个账号分组，IDLE 已连接 %d/%d，已同步 %d 封邮件", watcherSnapshot.GroupCount, watcherSnapshot.ConnectedWorkerCount, watcherSnapshot.WorkerCount, watcherSnapshot.SyncedMessages)
		}
	}
	keepAliveStatus := "idle"
	if s.cfg.AppleAccountKeepAliveEnabled && settings.EnableAppleKeepAlive {
		keepAliveStatus = "running"
	}
	keepAliveNextAt, keepAliveInterval := s.appleKeepAliveSchedule()
	var keepAliveNextAtPointer *time.Time
	if keepAliveStatus == "running" && !keepAliveNextAt.IsZero() {
		nextAt := keepAliveNextAt
		keepAliveNextAtPointer = &nextAt
	}
	items := []domain.Task{
		{ID: "data-store", Name: "本地数据存储", Description: "保存账号、邮箱、设置和 Apple 登录态", Status: "completed", Progress: 100, Module: "data"},
		{ID: "apple-account", Name: "Apple 账号管理", Description: "Apple Account、iCloud Web、2FA 和登录态检测", Status: "completed", Progress: 100, Module: "apple"},
		{ID: "mailbox-create", Name: "隐私邮箱闭环", Description: "创建、同步、收信、取码、停用和 Apple 远程删除", Status: "completed", Progress: 100, Module: "mailbox"},
		{ID: "imap-watcher", Name: "后台邮件监听", Description: watcherDescription, Status: watcherStatus, Progress: 100, Module: "imap"},
		{ID: "apple-keepalive", Name: "Apple 登录态保活", Description: "周期刷新 Apple Account 管理态", Status: keepAliveStatus, Progress: 100, Module: "apple", NextRunAt: keepAliveNextAtPointer, ScheduledIntervalSeconds: int(keepAliveInterval.Seconds()), JitterPercent: s.cfg.AppleAccountKeepAliveJitterPercent},
		{ID: "scheduler", Name: "定时创建", Description: "按所选账号周期创建隐私邮箱", Status: schedulerState.Status, Progress: 100, Module: "scheduler"},
		{ID: "public-api", Name: "公共取号 API", Description: "健康检查、取号、查询、邮箱取码和 API Key", Status: enabledTaskStatus(settings.EnablePublicMailboxAPI), Progress: 100, Module: "api"},
		{ID: "export", Name: "本地数据导出", Description: "运行数据、邮箱地址和取码 API 导出", Status: "completed", Progress: 100, Module: "export"},
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": items, "scheduler": schedulerState}})
}

func enabledTaskStatus(enabled bool) string {
	if enabled {
		return "running"
	}
	return "idle"
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	settings := s.serverChanSettings()
	apiKeySource := ""
	if strings.TrimSpace(settings.PublicAPIKey) != "" {
		apiKeySource = "system_settings"
	} else if strings.TrimSpace(s.cfg.APIKey) != "" {
		apiKeySource = "config"
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"settings":   settings,
		"data_path":  s.store.Path(),
		"local_only": true,
		"runtime": map[string]any{
			"database_status":                  s.store.DatabaseStatus(),
			"database_backup_dir":              s.cfg.DatabaseBackupDir,
			"database_backup_retention_count":  s.cfg.DatabaseBackupRetentionCount,
			"database_message_retention_days":  s.cfg.DatabaseMessageRetentionDays,
			"mail_watcher_available":           s.cfg.MailWatcherEnabled,
			"mail_watcher_poll_ms":             s.cfg.MailWatcherPollMS,
			"mail_watcher_web_poll_ms":         s.cfg.MailWatcherWebPollMS,
			"mail_watcher_fetch_limit":         s.cfg.MailWatcherFetchLimit,
			"mail_watcher_initial_fetch_limit": s.cfg.MailWatcherInitialFetchLimit,
			"mail_watcher_lookback_hours":      s.cfg.MailWatcherLookbackHours,
			"mail_watcher_status":              s.watcher.Snapshot(),
			"apple_keep_alive_available":       s.cfg.AppleAccountKeepAliveEnabled,
			"apple_keep_alive_ms":              s.cfg.AppleAccountKeepAliveMS,
			"apple_keep_alive_jitter_percent":  s.cfg.AppleAccountKeepAliveJitterPercent,
			"api_configured":                   s.globalAPIKey() != "",
			"api_key_source":                   apiKeySource,
			"config_api_key_configured":        strings.TrimSpace(s.cfg.APIKey) != "",
			"public_base_url":                  s.cfg.PublicBaseURL,
			"server_chan_configured":           strings.TrimSpace(settings.ServerChanSendKey) != "",
			"server_chan_send_key_masked":      maskServerChanSendKey(settings.ServerChanSendKey),
		},
	}})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		domain.Settings
		ClearServerChanSendKey bool `json:"clear_server_chan_send_key"`
	}
	body.Settings = s.store.Settings()
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	settings := body.Settings
	if strings.TrimSpace(settings.ServerChanSendKey) == "" && !body.ClearServerChanSendKey {
		settings.ServerChanSendKey = s.serverChanSettings().ServerChanSendKey
	}
	if err := validateServerChanSettings(settings); err != nil {
		writeError(w, http.StatusBadRequest, "server_chan_settings_invalid", err.Error())
		return
	}
	saved, err := s.store.SaveSettings(settings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_save_failed", err.Error())
		return
	}
	s.watcher.Wake("")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"settings": saved,
		"runtime": map[string]any{
			"server_chan_configured":      strings.TrimSpace(saved.ServerChanSendKey) != "",
			"server_chan_send_key_masked": maskServerChanSendKey(saved.ServerChanSendKey),
		},
	}})
}

func (s *Server) handleCreateSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"settings": s.store.CreateSettings()}})
}

func (s *Server) handleSaveCreateSettings(w http.ResponseWriter, r *http.Request) {
	var settings domain.CreateSettings
	if err := decodeJSON(r, &settings); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	saved, err := s.store.SaveCreateSettings(settings)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"settings": saved}})
}

func (s *Server) handleEvents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": s.store.Dashboard().Events}})
}

func (s *Server) handleClearEvents(w http.ResponseWriter, _ *http.Request) {
	if err := s.store.ClearEvents(); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": []domain.Event{}}})
}

func (s *Server) handleUnknownAPI(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "api_not_found", "接口不存在")
}

func (s *Server) handleRemovedManage(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "manage_removed", "/manage 页面已移除，请使用默认控制台")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, result auth.LoginResult) {
	maxAge := int(time.Until(result.ExpiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    result.Token,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  result.ExpiresAt,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode,
	})
}

type credentialPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func publicAdmin(admin domain.Admin) map[string]any {
	return map[string]any{
		"id":            admin.ID,
		"username":      admin.Username,
		"created_at":    formatTime(admin.CreatedAt),
		"last_login_at": formatTime(admin.LastLoginAt),
	}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

func decodeJSON(r *http.Request, target any) error {
	if r.Body == nil {
		return errors.New("请求体为空")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, (1<<20)+1))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("请求体只能包含一个 JSON 对象")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"success": false, "code": code, "message": message})
}

func writeServiceError(w http.ResponseWriter, err error) {
	code, message, retryable := protocol.ErrorDetails(err)
	status := http.StatusBadRequest
	if retryable {
		status = http.StatusBadGateway
	}
	if strings.Contains(strings.ToLower(code), "not_found") || strings.Contains(message, "不存在") {
		status = http.StatusNotFound
	}
	writeJSON(w, status, map[string]any{"success": false, "code": code, "message": message, "retryable": retryable})
}

func validMailboxStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case domain.StatusActive, domain.StatusAvailable, domain.StatusReserved, domain.StatusUsed, domain.StatusFailed, domain.StatusDisabled:
		return true
	default:
		return false
	}
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseRFC3339(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("after 必须是 RFC3339 时间")
	}
	return parsed, nil
}

func parsePositiveInt(value string, fallback int) int {
	var parsed int
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &parsed); err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
