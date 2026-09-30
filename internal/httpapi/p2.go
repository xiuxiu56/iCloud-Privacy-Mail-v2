package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/buildinfo"
	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/scheduler"
	"icloud-privacy-mail-v2/internal/store"
)

const publicCodePageContextKey contextKey = "public-code-page"

func (s *Server) handlePublicHealth(w http.ResponseWriter, r *http.Request) {
	current := buildinfo.Current()
	if s.globalAPIKey() != "" && !s.authorizedGlobalAPI(r) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "API Key 错误")
		return
	}
	active := false
	for _, session := range s.store.ICloudSessions() {
		if session.IsICloudPlus && session.CanCreateHME && len(session.Cookies) > 0 {
			active = true
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"service":                     "icloud-privacy-mail-v2",
			"version":                     current.Version,
			"commit":                      current.Commit,
			"api_active":                  s.store.Settings().EnablePublicMailboxAPI && s.globalAPIKey() != "",
			"icloud_active":               active,
			"lease_api_version":           1,
			"lease_ttl_seconds":           s.cfg.PublicMailboxLeaseTTLMinutes * 60,
			"lease_max_ttl_seconds":       s.cfg.PublicMailboxLeaseMaxTTLMinutes * 60,
			"mailbox_note_api_supported":  true,
			"mailbox_kind_filters":        []string{"any", domain.MailboxKindICloudHME, domain.MailboxKindDomainForward},
			"account_id_filter_supported": true,
			"apple_id_filter_supported":   true,
			"domain_filter_supported":     true,
			"message_api_supported":       true,
			"time":                        time.Now().Format(time.RFC3339),
		},
	})
}

func (s *Server) handlePublicClaimMailbox(w http.ResponseWriter, r *http.Request) {
	if !s.store.Settings().EnablePublicMailboxAPI {
		writeError(w, http.StatusForbidden, "public_api_disabled", "公共取号 API 尚未开启")
		return
	}
	if !s.authorizedGlobalAPI(r) {
		writeError(w, http.StatusUnauthorized, "global_api_key_required", "自动取号需要提交全局 API Key")
		return
	}
	var body struct {
		Project     string `json:"project"`
		Purpose     string `json:"purpose"`
		RequestID   string `json:"request_id"`
		Note        string `json:"note"`
		TTLSeconds  int    `json:"ttl_seconds"`
		MailboxKind string `json:"mailbox_kind"`
		Domain      string `json:"domain"`
		AccountID   string `json:"account_id"`
		AppleID     string `json:"apple_id"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	body.MailboxKind = strings.ToLower(strings.TrimSpace(body.MailboxKind))
	body.AccountID = strings.TrimSpace(body.AccountID)
	body.AppleID = strings.TrimSpace(body.AppleID)
	if body.MailboxKind == "any" {
		body.MailboxKind = ""
	}
	if body.MailboxKind != "" && body.MailboxKind != domain.MailboxKindICloudHME && body.MailboxKind != domain.MailboxKindDomainForward {
		writeError(w, http.StatusBadRequest, "invalid_mailbox_kind", "mailbox_kind 只支持 icloud_hme、domain_forward 或留空")
		return
	}
	if strings.TrimSpace(body.Domain) != "" && body.MailboxKind != domain.MailboxKindDomainForward {
		writeError(w, http.StatusBadRequest, "invalid_mailbox_domain", "domain 仅用于领取 domain_forward 域名邮箱")
		return
	}
	if body.AppleID != "" {
		body.AccountID = body.AppleID
	}
	if body.AccountID != "" {
		resolvedAccountID, found := s.store.ResolveAppleAccountID(body.AccountID)
		if strings.Contains(body.AccountID, "@") && !found {
			writeError(w, http.StatusBadRequest, "apple_account_not_found", "Apple 账号不存在")
			return
		}
		if found {
			body.AccountID = resolvedAccountID
		}
	}
	mailbox, lease, created, err := s.store.ClaimMailboxLeaseFiltered(
		body.Project,
		body.Purpose,
		body.RequestID,
		body.Note,
		s.mailboxLeaseTTL(body.TTLSeconds),
		time.Now(),
		store.MailboxClaimFilter{MailboxKind: body.MailboxKind, Domain: body.Domain, AccountID: body.AccountID},
	)
	if err != nil {
		if errors.Is(err, store.ErrNoAvailableMailbox) {
			writeError(w, http.StatusOK, "no_available_mailbox", err.Error())
			return
		}
		writeMailboxLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"mailbox":    s.publicMailbox(r, mailbox, true),
		"lease":      s.publicMailboxLease(lease),
		"created":    created,
		"idempotent": !created,
	}})
}

func (s *Server) handlePublicLookupMailboxes(w http.ResponseWriter, r *http.Request) {
	if !s.store.Settings().EnablePublicMailboxAPI {
		writeError(w, http.StatusForbidden, "public_api_disabled", "公共取号 API 尚未开启")
		return
	}
	if !s.authorizedGlobalAPI(r) {
		writeError(w, http.StatusUnauthorized, "global_api_key_required", "查询邮箱需要提交全局 API Key")
		return
	}
	var body struct {
		Emails []string `json:"emails"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if len(body.Emails) == 0 || len(body.Emails) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_email_count", "邮箱数量必须是 1-500")
		return
	}
	seen := map[string]bool{}
	items := make([]map[string]any, 0, len(body.Emails))
	missing := make([]string, 0)
	for _, raw := range body.Emails {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		mailbox, ok := s.store.FindMailboxByEmail(email)
		if !ok {
			missing = append(missing, email)
			continue
		}
		items = append(items, s.publicMailbox(r, mailbox, true))
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": items, "missing": missing}})
}

func (s *Server) handlePublicMailboxCode(w http.ResponseWriter, r *http.Request) {
	fromPublicPage, _ := r.Context().Value(publicCodePageContextKey).(bool)
	settings := s.store.Settings()
	if fromPublicPage {
		if !settings.EnablePublicCodePage {
			writeError(w, http.StatusForbidden, "public_code_page_disabled", "公共验证码页面尚未开启")
			return
		}
	} else if !settings.EnablePublicMailboxAPI {
		writeError(w, http.StatusForbidden, "public_api_disabled", "公共取号 API 尚未开启")
		return
	}
	email, err := url.PathUnescape(r.PathValue("email"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_email", "邮箱地址格式不正确")
		return
	}
	mailbox, ok := s.store.FindMailboxByEmail(email)
	if !ok {
		writeError(w, http.StatusNotFound, "mailbox_not_found", "邮箱不存在")
		return
	}
	if !fromPublicPage && !s.authorizedMailboxAPI(r, mailbox) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "API Key 错误")
		return
	}
	if !mailbox.APIActive || mailbox.Status == domain.StatusDisabled {
		writeError(w, http.StatusForbidden, "api_disabled", "邮箱 API 已停用")
		return
	}
	if !mailbox.ICloudActive {
		writeError(w, http.StatusForbidden, "icloud_inactive", "邮箱在 iCloud 中已停用")
		return
	}
	after, err := parseRFC3339(r.URL.Query().Get("after"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_after", err.Error())
		return
	}
	waitMS := s.cfg.PublicFastSyncWaitMS
	if raw := strings.TrimSpace(r.URL.Query().Get("wait_ms")); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil {
			waitMS = parsed
		}
	}
	if waitMS < 0 {
		waitMS = 0
	}
	if waitMS > 30000 {
		waitMS = 30000
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	if keyword == "" {
		keyword = "OpenAI"
	}
	allowStale := parseBool(r.URL.Query().Get("allow_stale"))
	cacheOnly := parseBool(r.URL.Query().Get("cache"))
	peekOnly := parseBool(r.URL.Query().Get("peek")) || parseBool(r.URL.Query().Get("preview"))
	query := mailboxservice.CodeQuery{
		After:         after,
		Keyword:       keyword,
		SkipMessageID: mailbox.LastCodeMessageID,
		IncludeServed: cacheOnly || peekOnly,
		MarkAsServed:  !cacheOnly && !peekOnly,
	}
	if result, found, lookupErr := s.mailbox.CachedCodeWithQuery(mailbox.ID, query); lookupErr != nil {
		writeServiceError(w, lookupErr)
		return
	} else if found {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
		return
	}
	if cacheOnly {
		writeError(w, http.StatusOK, "no_code", "本地缓存中暂无验证码")
		return
	}
	if waitMS == 0 {
		writeError(w, http.StatusOK, "no_code", "暂未收到验证码")
		return
	}

	deadline := time.NewTimer(time.Duration(waitMS) * time.Millisecond)
	defer deadline.Stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	syncDone := make(chan error, 1)
	minInterval := time.Duration(s.cfg.PublicSyncMinIntervalMS) * time.Millisecond
	if minInterval < 0 {
		minInterval = 0
	}
	if mailbox.LastSyncAt.IsZero() || time.Since(mailbox.LastSyncAt) >= minInterval {
		go func() {
			syncCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 120*time.Second)
			defer cancel()
			_, syncErr := s.mailbox.SyncMessages(syncCtx, mailbox.ID)
			syncDone <- syncErr
		}()
	} else {
		syncDone <- nil
	}
	var syncErr error
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			if syncErr != nil && !allowStale {
				writeError(w, http.StatusBadGateway, "mail_sync_failed", "同步验证码邮件失败，请检查 IMAP 或 iCloud 登录态后重试")
				return
			}
			if syncErr != nil && allowStale {
				if result, found, lookupErr := s.staleCachedCode(mailbox.ID, query); lookupErr != nil {
					writeServiceError(w, lookupErr)
					return
				} else if found {
					writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": staleCodeData(result)})
					return
				}
			}
			writeError(w, http.StatusOK, "no_code", "暂未收到验证码")
			return
		case syncErr = <-syncDone:
			syncDone = nil
			if result, found, lookupErr := s.mailbox.CachedCodeWithQuery(mailbox.ID, query); lookupErr != nil {
				writeServiceError(w, lookupErr)
				return
			} else if found {
				writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
				return
			}
			if syncErr != nil && !allowStale {
				writeError(w, http.StatusBadGateway, "mail_sync_failed", "同步验证码邮件失败，请检查 IMAP 或 iCloud 登录态后重试")
				return
			}
			if syncErr != nil && allowStale {
				if result, found, lookupErr := s.staleCachedCode(mailbox.ID, query); lookupErr != nil {
					writeServiceError(w, lookupErr)
					return
				} else if found {
					writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": staleCodeData(result)})
					return
				}
			}
		case <-poll.C:
			if result, found, lookupErr := s.mailbox.CachedCodeWithQuery(mailbox.ID, query); lookupErr != nil {
				writeServiceError(w, lookupErr)
				return
			} else if found {
				writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
				return
			}
		}
	}
}

func (s *Server) handlePublicMailboxMessages(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := s.publicAPIMailbox(w, r)
	if !ok {
		return
	}
	var syncErr error
	if parseBool(r.URL.Query().Get("sync")) {
		minInterval := time.Duration(s.cfg.PublicSyncMinIntervalMS) * time.Millisecond
		if minInterval < 0 {
			minInterval = 0
		}
		if mailbox.LastSyncAt.IsZero() || time.Since(mailbox.LastSyncAt) >= minInterval {
			syncCtx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
			_, syncErr = s.mailbox.SyncPublicMessages(syncCtx, mailbox.ID)
			cancel()
		}
	}
	items := s.store.MessagesForMailbox(mailbox.ID)
	total := len(items)
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 100 {
		limit = 100
	}
	if len(items) > limit {
		items = items[:limit]
	}
	summaries := make([]map[string]any, 0, len(items))
	for _, message := range items {
		summaries = append(summaries, publicMessageSummary(message))
	}
	data := map[string]any{"email": mailbox.Email, "mailbox_kind": publicMailboxKind(mailbox), "items": summaries, "total": total}
	if refreshed, found := s.store.FindMailboxByID(mailbox.ID); found && !refreshed.LastSyncAt.IsZero() {
		data["last_sync_at"] = refreshed.LastSyncAt
	}
	if syncErr != nil {
		data["sync_error"] = "同步邮件失败，当前显示本地已保存的邮件"
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}

func (s *Server) handlePublicMailboxMessage(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := s.publicAPIMailbox(w, r)
	if !ok {
		return
	}
	message, err := s.mailbox.MessageContent(r.Context(), mailbox.ID, r.PathValue("messageID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"email": mailbox.Email, "mailbox_kind": publicMailboxKind(mailbox), "message": publicMessageContent(message),
	}})
}

func (s *Server) publicAPIMailbox(w http.ResponseWriter, r *http.Request) (domain.Mailbox, bool) {
	if !s.store.Settings().EnablePublicMailboxAPI {
		writeError(w, http.StatusForbidden, "public_api_disabled", "公共取号 API 尚未开启")
		return domain.Mailbox{}, false
	}
	email, err := url.PathUnescape(r.PathValue("email"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_email", "邮箱地址格式不正确")
		return domain.Mailbox{}, false
	}
	mailbox, ok := s.store.FindMailboxByEmail(email)
	if !ok {
		writeError(w, http.StatusNotFound, "mailbox_not_found", "邮箱不存在")
		return domain.Mailbox{}, false
	}
	if !s.authorizedMailboxAPI(r, mailbox) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "API Key 错误")
		return domain.Mailbox{}, false
	}
	if !mailbox.APIActive || mailbox.Status == domain.StatusDisabled {
		writeError(w, http.StatusForbidden, "api_disabled", "邮箱 API 已停用")
		return domain.Mailbox{}, false
	}
	if !mailbox.ICloudActive {
		writeError(w, http.StatusForbidden, "mailbox_inactive", "邮箱收件已停用")
		return domain.Mailbox{}, false
	}
	if publicMailboxKind(mailbox) == domain.MailboxKindDomainForward && !s.store.DomainMailSettings().Enabled {
		writeError(w, http.StatusForbidden, "domain_mail_disabled", "域名邮箱接收尚未开启")
		return domain.Mailbox{}, false
	}
	return mailbox, true
}

func (s *Server) handlePublicCodePageStatus(w http.ResponseWriter, _ *http.Request) {
	settings := s.store.Settings()
	domainSettings := s.store.DomainMailSettings()
	domains := make([]string, 0)
	if domainSettings.Enabled {
		for _, route := range s.store.DomainMailRoutes() {
			if route.Enabled && strings.TrimSpace(route.Domain) != "" {
				domains = append(domains, strings.ToLower(strings.TrimSpace(route.Domain)))
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"enabled": settings.EnablePublicCodePage,
			"route":   "/email-code",
			"domain_mail": map[string]any{
				"enabled": domainSettings.Enabled,
				"domains": domains,
			},
		},
	})
}

func (s *Server) handlePublicCodePageLookup(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if email == "" || !strings.Contains(email, "@") {
		writeError(w, http.StatusBadRequest, "invalid_email", "请输入完整的邮箱地址")
		return
	}
	request := r.Clone(context.WithValue(r.Context(), publicCodePageContextKey, true))
	request.SetPathValue("email", email)
	s.handlePublicMailboxCode(w, request)
}

func (s *Server) publicCodePageMailbox(w http.ResponseWriter, r *http.Request) (domain.Mailbox, bool) {
	if !s.store.Settings().EnablePublicCodePage {
		writeError(w, http.StatusForbidden, "public_code_page_disabled", "公共验证码页面尚未开启")
		return domain.Mailbox{}, false
	}
	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if email == "" || !strings.Contains(email, "@") {
		writeError(w, http.StatusBadRequest, "invalid_email", "请输入完整的邮箱地址")
		return domain.Mailbox{}, false
	}
	mailbox, ok := s.store.FindMailboxByEmail(email)
	if !ok {
		writeError(w, http.StatusNotFound, "mailbox_not_found", "邮箱不存在")
		return domain.Mailbox{}, false
	}
	if !mailbox.APIActive || mailbox.Status == domain.StatusDisabled {
		writeError(w, http.StatusForbidden, "api_disabled", "邮箱 API 已停用")
		return domain.Mailbox{}, false
	}
	if !mailbox.ICloudActive {
		writeError(w, http.StatusForbidden, "icloud_inactive", "邮箱在 iCloud 中已停用")
		return domain.Mailbox{}, false
	}
	return mailbox, true
}

func (s *Server) handlePublicCodePageMessages(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := s.publicCodePageMailbox(w, r)
	if !ok {
		return
	}
	var syncErr error
	if parseBool(r.URL.Query().Get("sync")) {
		minInterval := time.Duration(s.cfg.PublicSyncMinIntervalMS) * time.Millisecond
		if minInterval < 0 {
			minInterval = 0
		}
		if mailbox.LastSyncAt.IsZero() || time.Since(mailbox.LastSyncAt) >= minInterval {
			syncCtx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
			_, syncErr = s.mailbox.SyncPublicMessages(syncCtx, mailbox.ID)
			cancel()
		}
	}
	items := s.store.MessagesForMailbox(mailbox.ID)
	total := len(items)
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 100 {
		limit = 100
	}
	if len(items) > limit {
		items = items[:limit]
	}
	summaries := make([]map[string]any, 0, len(items))
	for _, message := range items {
		summaries = append(summaries, publicMessageSummary(message))
	}
	refreshed, _ := s.store.FindMailboxByID(mailbox.ID)
	data := map[string]any{"email": mailbox.Email, "items": summaries, "total": total}
	if !refreshed.LastSyncAt.IsZero() {
		data["last_sync_at"] = refreshed.LastSyncAt
	}
	if syncErr != nil {
		data["sync_error"] = "同步邮件失败，当前显示本地已保存的邮件"
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}

func (s *Server) handlePublicCodePageMessage(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := s.publicCodePageMailbox(w, r)
	if !ok {
		return
	}
	message, err := s.mailbox.MessageContent(r.Context(), mailbox.ID, r.PathValue("messageID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"message": publicMessageContent(message),
	}})
}

func publicMessageSummary(message domain.Message) map[string]any {
	return map[string]any{
		"id":           message.ID,
		"source":       message.Source,
		"subject":      message.Subject,
		"from":         message.From,
		"content_type": message.ContentType,
		"has_html":     strings.TrimSpace(message.HTMLBody) != "",
		"received_at":  message.ReceivedAt,
	}
}

func publicMessageContent(message domain.Message) map[string]any {
	return map[string]any{
		"id":           message.ID,
		"source":       message.Source,
		"subject":      message.Subject,
		"from":         message.From,
		"body":         message.Body,
		"html_body":    message.HTMLBody,
		"content_type": message.ContentType,
		"received_at":  message.ReceivedAt,
	}
}

func (s *Server) staleCachedCode(mailboxID string, query mailboxservice.CodeQuery) (mailboxservice.CodeResult, bool, error) {
	query.IncludeServed = false
	query.MarkAsServed = false
	return s.mailbox.CachedCodeWithQuery(mailboxID, query)
}

func staleCodeData(result mailboxservice.CodeResult) map[string]any {
	return map[string]any{
		"email": result.Email, "code": result.Code, "subject": result.Subject, "from": result.From,
		"received_at": result.ReceivedAt, "message_id": result.MessageID, "stale_cache": true,
		"sync_error": "同步验证码邮件失败，当前验证码来自本地缓存",
	}
}

func (s *Server) handleSchedulerStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"scheduler": s.scheduler.Snapshot(),
		"defaults":  scheduler.DefaultConfig(s.store),
	}})
}

func (s *Server) handleSchedulerStart(w http.ResponseWriter, r *http.Request) {
	var cfg scheduler.Config
	if err := decodeJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	state, err := s.scheduler.Start(s.runtimeCtx, cfg)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": state}})
}

func (s *Server) handleSchedulerStop(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": s.scheduler.Stop("已手动停止定时创建")}})
}

func (s *Server) handleSchedulerClearLogs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"scheduler": s.scheduler.ClearEvents()}})
}

func (s *Server) handleExportRuntime(w http.ResponseWriter, r *http.Request) {
	state := s.store.Snapshot()
	payload := struct {
		SchemaVersion  int                    `json:"schema_version"`
		ExportedAt     time.Time              `json:"exported_at"`
		AppleAccounts  []domain.AppleAccount  `json:"apple_accounts"`
		Mailboxes      []domain.Mailbox       `json:"mailboxes"`
		MailboxLeases  []domain.MailboxLease  `json:"mailbox_leases"`
		ICloudSessions []domain.ICloudSession `json:"icloud_sessions"`
		CreateSettings domain.CreateSettings  `json:"create_settings"`
		Settings       domain.Settings        `json:"settings"`
		MessageCount   int                    `json:"message_count"`
		Messages       []domain.Message       `json:"messages,omitempty"`
	}{
		SchemaVersion: state.SchemaVersion, ExportedAt: time.Now(), AppleAccounts: state.AppleAccounts,
		Mailboxes: state.Mailboxes, MailboxLeases: state.MailboxLeases, ICloudSessions: state.ICloudSessions, CreateSettings: state.CreateSettings,
		Settings: state.Settings, MessageCount: len(state.Messages),
	}
	if parseBool(r.URL.Query().Get("include_messages")) {
		payload.Messages = state.Messages
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "export_failed", err.Error())
		return
	}
	writeDownload(w, "application/json; charset=utf-8", "icloud-privacy-mail-state-"+time.Now().Format("20060102-150405")+".json", append(data, '\n'))
}

func (s *Server) handleExportMailboxAPIs(w http.ResponseWriter, r *http.Request) {
	s.writeMailboxExport(w, r, true)
}

func (s *Server) handleExportMailboxEmails(w http.ResponseWriter, r *http.Request) {
	s.writeMailboxExport(w, r, false)
}

func (s *Server) writeMailboxExport(w http.ResponseWriter, r *http.Request, includeAPI bool) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "txt"
	}
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	rows := make([][]string, 0)
	for _, mailbox := range s.store.AllMailboxes() {
		if accountID != "" && mailbox.AccountID != accountID {
			continue
		}
		row := []string{mailbox.Email}
		if includeAPI {
			row = append(row, s.mailboxAPIURL(r, mailbox))
		}
		rows = append(rows, row)
	}
	var body strings.Builder
	ext := "txt"
	contentType := "text/plain; charset=utf-8"
	switch format {
	case "txt":
		separator := "\n"
		if includeAPI {
			separator = "----"
		}
		for _, row := range rows {
			body.WriteString(strings.Join(row, separator))
			body.WriteByte('\n')
		}
	case "csv", "tsv":
		ext = format
		contentType = "text/csv; charset=utf-8"
		writer := csv.NewWriter(&body)
		if format == "tsv" {
			writer.Comma = '\t'
			contentType = "text/tab-separated-values; charset=utf-8"
		}
		for _, row := range rows {
			_ = writer.Write(row)
		}
		writer.Flush()
	case "jsonl":
		ext = "jsonl"
		contentType = "application/x-ndjson; charset=utf-8"
		for _, row := range rows {
			record := map[string]string{"email": row[0]}
			if includeAPI {
				record["api"] = row[1]
			}
			data, _ := json.Marshal(record)
			body.Write(data)
			body.WriteByte('\n')
		}
	default:
		writeError(w, http.StatusBadRequest, "invalid_export_format", "导出格式只支持 txt、csv、tsv、jsonl")
		return
	}
	prefix := "icloud-mailbox-emails"
	if includeAPI {
		prefix = "icloud-mailbox-apis"
	}
	writeDownload(w, contentType, prefix+"-"+time.Now().Format("20060102-150405")+"."+ext, []byte(body.String()))
}

func writeDownload(w http.ResponseWriter, contentType, filename string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) publicMailbox(r *http.Request, mailbox domain.Mailbox, includeAPI bool) map[string]any {
	kind := publicMailboxKind(mailbox)
	out := map[string]any{
		"id": mailbox.ID, "account_id": mailbox.AccountID, "label": mailbox.Label, "email": mailbox.Email,
		"mailbox_kind": kind,
		"api_active":   mailbox.APIActive, "icloud_active": mailbox.ICloudActive, "status": mailbox.Status,
		"note": mailbox.Note, "active_lease_id": mailbox.ActiveLeaseID, "receive_count": mailbox.ReceiveCount, "last_sync_at": mailbox.LastSyncAt,
	}
	if kind == domain.MailboxKindDomainForward {
		if separator := strings.LastIndexByte(mailbox.Email, '@'); separator >= 0 {
			out["domain"] = strings.ToLower(mailbox.Email[separator+1:])
		}
		if route, found := s.store.FindDomainMailRoute(mailbox.DomainRouteID); found {
			out["receiver_type"] = route.ReceiverType
		}
	}
	if includeAPI {
		out["api_url"] = s.mailboxAPIURL(r, mailbox)
		out["api_token_mask"] = maskSecret(mailbox.APIToken)
	}
	return out
}

func publicMailboxKind(mailbox domain.Mailbox) string {
	if strings.TrimSpace(mailbox.MailboxKind) == "" {
		return domain.MailboxKindICloudHME
	}
	return mailbox.MailboxKind
}

func (s *Server) mailboxAPIURL(r *http.Request, mailbox domain.Mailbox) string {
	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/")
	if baseURL == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	return fmt.Sprintf("%s/api/v1/mailboxes/%s/code?key=%s", baseURL, url.PathEscape(mailbox.Email), url.QueryEscape(mailbox.APIToken))
}

func (s *Server) authorizedGlobalAPI(r *http.Request) bool {
	want := s.globalAPIKey()
	if want == "" {
		return false
	}
	return anySecretEqual(want, r.URL.Query().Get("key"), r.Header.Get("X-API-Key"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func (s *Server) authorizedMailboxAPI(r *http.Request, mailbox domain.Mailbox) bool {
	candidates := []string{r.URL.Query().Get("key"), r.Header.Get("X-API-Key"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	if anySecretEqual(mailbox.APIToken, candidates...) {
		return true
	}
	globalKey := s.globalAPIKey()
	return globalKey != "" && anySecretEqual(globalKey, candidates...)
}

func (s *Server) globalAPIKey() string {
	if value := strings.TrimSpace(s.store.Settings().PublicAPIKey); value != "" {
		return value
	}
	return strings.TrimSpace(s.cfg.APIKey)
}

func anySecretEqual(want string, candidates ...string) bool {
	want = strings.TrimSpace(want)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if want != "" && len(candidate) == len(want) && subtle.ConstantTimeCompare([]byte(candidate), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

func maskSecret(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return strings.Repeat("*", len(value))
	}
	return value[:4] + strings.Repeat("*", len(value)-8) + value[len(value)-4:]
}
