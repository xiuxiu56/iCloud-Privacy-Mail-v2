package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

type mailboxLeaseActionRequest struct {
	Project        string `json:"project"`
	Note           string `json:"note"`
	Reason         string `json:"reason"`
	TTLSeconds     int    `json:"ttl_seconds"`
	ReconcileBound bool   `json:"reconcile_bound"`
}

func (s *Server) handlePublicMailboxLease(w http.ResponseWriter, r *http.Request) {
	if !s.requirePublicMailboxLeaseAPI(w, r) {
		return
	}
	lease, ok := s.store.FindMailboxLease(r.PathValue("lease_id"))
	if !ok {
		writeMailboxLeaseError(w, store.ErrLeaseNotFound)
		return
	}
	project := mailboxLeaseProject(r.URL.Query().Get("project"), r.Header.Get("X-Project"))
	if project == "" {
		writeMailboxLeaseError(w, store.ErrLeaseProjectRequired)
		return
	}
	if !strings.EqualFold(lease.Project, project) {
		writeMailboxLeaseError(w, store.ErrLeaseProjectMismatch)
		return
	}
	data := map[string]any{"lease": s.publicMailboxLease(lease)}
	if mailbox, found := s.store.FindMailboxByID(lease.MailboxID); found {
		data["mailbox"] = s.publicMailbox(r, mailbox, true)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": data})
}

func (s *Server) handlePublicMailboxLeaseCommit(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseAction(w, r, "commit", r.PathValue("lease_id"))
}

func (s *Server) handlePublicMailboxLeaseRelease(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseAction(w, r, "release", r.PathValue("lease_id"))
}

func (s *Server) handlePublicMailboxLeaseRenew(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseAction(w, r, "renew", r.PathValue("lease_id"))
}

func (s *Server) handlePublicMailboxLeaseNote(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseAction(w, r, "note", r.PathValue("lease_id"))
}

func (s *Server) handlePublicMailboxLeaseDeactivate(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseAction(w, r, "deactivate", r.PathValue("lease_id"))
}

func (s *Server) handlePublicMailboxLeaseCommitCompat(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseActionCompat(w, r, "commit")
}

func (s *Server) handlePublicMailboxLeaseReleaseCompat(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseActionCompat(w, r, "release")
}

func (s *Server) handlePublicMailboxLeaseRenewCompat(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseActionCompat(w, r, "renew")
}

func (s *Server) handlePublicMailboxLeaseDeactivateCompat(w http.ResponseWriter, r *http.Request) {
	s.handlePublicMailboxLeaseActionCompat(w, r, "deactivate")
}

func (s *Server) handlePublicMailboxLeaseActionCompat(w http.ResponseWriter, r *http.Request, action string) {
	if !s.requirePublicMailboxLeaseAPI(w, r) {
		return
	}
	request, ok := decodeMailboxLeaseAction(w, r)
	if !ok {
		return
	}
	email, err := url.PathUnescape(r.PathValue("email"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_email", "邮箱地址格式不正确")
		return
	}
	project := mailboxLeaseProject(request.Project, r.Header.Get("X-Project"))
	lease, found := s.store.LatestMailboxLeaseByEmailProject(email, project)
	if !found {
		if project == "" {
			writeMailboxLeaseError(w, store.ErrLeaseProjectRequired)
		} else {
			writeMailboxLeaseError(w, store.ErrLeaseNotFound)
		}
		return
	}
	s.applyPublicMailboxLeaseAction(w, r, action, lease.ID, project, request)
}

func (s *Server) handlePublicMailboxLeaseAction(w http.ResponseWriter, r *http.Request, action, leaseID string) {
	if !s.requirePublicMailboxLeaseAPI(w, r) {
		return
	}
	request, ok := decodeMailboxLeaseAction(w, r)
	if !ok {
		return
	}
	project := mailboxLeaseProject(request.Project, r.Header.Get("X-Project"))
	s.applyPublicMailboxLeaseAction(w, r, action, leaseID, project, request)
}

func (s *Server) applyPublicMailboxLeaseAction(w http.ResponseWriter, r *http.Request, action, leaseID, project string, request mailboxLeaseActionRequest) {
	note := strings.TrimSpace(request.Note)
	if note == "" {
		note = strings.TrimSpace(request.Reason)
	}
	now := time.Now()
	var (
		mailbox    domain.Mailbox
		lease      domain.MailboxLease
		idempotent bool
		err        error
	)
	switch action {
	case "commit":
		if request.ReconcileBound {
			mailbox, lease, idempotent, err = s.store.ReconcileUsedMailboxLease(leaseID, project, note, now)
		} else {
			mailbox, lease, idempotent, err = s.store.CommitMailboxLease(leaseID, project, note, now)
		}
	case "release":
		mailbox, lease, idempotent, err = s.store.ReleaseMailboxLease(leaseID, project, note, now)
	case "renew":
		mailbox, lease, err = s.store.RenewMailboxLease(leaseID, project, note, s.mailboxLeaseTTL(request.TTLSeconds), now)
	case "note":
		mailbox, lease, err = s.store.SetMailboxLeaseNote(leaseID, project, request.Note, now)
	case "deactivate":
		mailbox, lease, idempotent, err = s.store.DeactivateMailboxLease(leaseID, project, note, now)
	default:
		writeError(w, http.StatusBadRequest, "invalid_lease_action", "邮箱租约动作不正确")
		return
	}
	if err != nil {
		writeMailboxLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"mailbox":    s.publicMailbox(r, mailbox, true),
		"lease":      s.publicMailboxLease(lease),
		"idempotent": idempotent,
		"reconciled": request.ReconcileBound,
	}})
}

func decodeMailboxLeaseAction(w http.ResponseWriter, r *http.Request) (mailboxLeaseActionRequest, bool) {
	request := mailboxLeaseActionRequest{}
	if r.ContentLength == 0 {
		return request, true
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return mailboxLeaseActionRequest{}, false
	}
	return request, true
}

func (s *Server) requirePublicMailboxLeaseAPI(w http.ResponseWriter, r *http.Request) bool {
	if !s.store.Settings().EnablePublicMailboxAPI {
		writeError(w, http.StatusForbidden, "public_api_disabled", "公共取号 API 尚未开启")
		return false
	}
	if !s.authorizedGlobalAPI(r) {
		writeError(w, http.StatusUnauthorized, "global_api_key_required", "邮箱租约操作需要提交全局 API Key")
		return false
	}
	return true
}

func (s *Server) mailboxLeaseTTL(requestedSeconds int) time.Duration {
	seconds := requestedSeconds
	if seconds <= 0 {
		seconds = s.cfg.PublicMailboxLeaseTTLMinutes * 60
	}
	if seconds < 60 {
		seconds = 60
	}
	maxSeconds := s.cfg.PublicMailboxLeaseMaxTTLMinutes * 60
	if maxSeconds <= 0 {
		maxSeconds = 7 * 24 * 60 * 60
	}
	if seconds > maxSeconds {
		seconds = maxSeconds
	}
	return time.Duration(seconds) * time.Second
}

func mailboxLeaseProject(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return strings.ToLower(value)
		}
	}
	return ""
}

func (s *Server) publicMailboxLease(lease domain.MailboxLease) map[string]any {
	return map[string]any{
		"id":           lease.ID,
		"mailbox_id":   lease.MailboxID,
		"email":        lease.Email,
		"project":      lease.Project,
		"purpose":      lease.Purpose,
		"request_id":   lease.RequestID,
		"state":        lease.State,
		"note":         lease.Note,
		"expires_at":   formatTime(lease.ExpiresAt),
		"created_at":   formatTime(lease.CreatedAt),
		"updated_at":   formatTime(lease.UpdatedAt),
		"committed_at": formatTime(lease.CommittedAt),
		"released_at":  formatTime(lease.ReleasedAt),
		"expired_at":   formatTime(lease.ExpiredAt),
	}
}

func writeMailboxLeaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrLeaseProjectRequired):
		writeError(w, http.StatusBadRequest, "lease_project_required", err.Error())
	case errors.Is(err, store.ErrLeaseNotFound):
		writeError(w, http.StatusNotFound, "lease_not_found", err.Error())
	case errors.Is(err, store.ErrLeaseProjectMismatch):
		writeError(w, http.StatusForbidden, "lease_project_mismatch", err.Error())
	case errors.Is(err, store.ErrLeaseRequestConflict):
		writeError(w, http.StatusConflict, "lease_request_conflict", err.Error())
	case errors.Is(err, store.ErrLeaseCommitted):
		writeError(w, http.StatusConflict, "lease_committed", err.Error())
	case errors.Is(err, store.ErrLeaseReleased):
		writeError(w, http.StatusConflict, "lease_released", err.Error())
	case errors.Is(err, store.ErrLeaseExpired):
		writeError(w, http.StatusConflict, "lease_expired", err.Error())
	case errors.Is(err, store.ErrLeaseBindingConflict):
		writeError(w, http.StatusConflict, "lease_binding_conflict", err.Error())
	case errors.Is(err, store.ErrLeaseReconcileState):
		writeError(w, http.StatusConflict, "lease_reconcile_state", err.Error())
	case errors.Is(err, store.ErrNoAvailableMailbox):
		writeError(w, http.StatusOK, "no_available_mailbox", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "lease_operation_failed", err.Error())
	}
}
