package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

var (
	ErrNoAvailableMailbox   = errors.New("没有可用隐私邮箱")
	ErrLeaseProjectRequired = errors.New("project 不能为空")
	ErrLeaseNotFound        = errors.New("邮箱租约不存在")
	ErrLeaseProjectMismatch = errors.New("邮箱租约不属于当前项目")
	ErrLeaseRequestConflict = errors.New("request_id 已被其他领取请求使用")
	ErrLeaseCommitted       = errors.New("邮箱租约已经提交")
	ErrLeaseReleased        = errors.New("邮箱租约已经释放")
	ErrLeaseExpired         = errors.New("邮箱租约已经过期")
	ErrLeaseBindingConflict = errors.New("邮箱与租约绑定关系不一致")
	ErrLeaseReconcileState  = errors.New("只有已释放或已过期的邮箱租约可以执行已绑定纠偏")
)

type MailboxClaimFilter struct {
	MailboxKind string
	Domain      string
}

// ClaimMailboxLease 在单个 SQLite 事务中原子执行 available -> reserved。
func (s *Store) ClaimMailboxLease(project, purpose, requestID, note string, ttl time.Duration, now time.Time) (domain.Mailbox, domain.MailboxLease, bool, error) {
	return s.ClaimMailboxLeaseFiltered(project, purpose, requestID, note, ttl, now, MailboxClaimFilter{})
}

// ClaimMailboxLeaseFiltered 按邮箱类型和接收域名原子领取邮箱；空筛选保持原接口行为。
func (s *Store) ClaimMailboxLeaseFiltered(project, purpose, requestID, note string, ttl time.Duration, now time.Time, filter MailboxClaimFilter) (domain.Mailbox, domain.MailboxLease, bool, error) {
	project = normalizeLeaseProject(project)
	if project == "" {
		return domain.Mailbox{}, domain.MailboxLease{}, false, ErrLeaseProjectRequired
	}
	purpose, requestID, note = strings.TrimSpace(purpose), strings.TrimSpace(requestID), strings.TrimSpace(note)
	filter.MailboxKind = strings.ToLower(strings.TrimSpace(filter.MailboxKind))
	if filter.MailboxKind == "any" {
		filter.MailboxKind = ""
	}
	filter.Domain = normalizeDomainName(filter.Domain)
	if filter.MailboxKind != "" && filter.MailboxKind != domain.MailboxKindICloudHME && filter.MailboxKind != domain.MailboxKindDomainForward {
		return domain.Mailbox{}, domain.MailboxLease{}, false, errors.New("mailbox_kind 只支持 icloud_hme、domain_forward 或留空")
	}
	if filter.Domain != "" && filter.MailboxKind != domain.MailboxKindDomainForward {
		return domain.Mailbox{}, domain.MailboxLease{}, false, errors.New("domain 仅用于领取 domain_forward 域名邮箱")
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	expiryChanges, _, err := s.expireMailboxLeasesTx(tx, now)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	if requestID != "" {
		var data []byte
		err := tx.QueryRow(`SELECT data_json FROM mailbox_leases WHERE json_extract(data_json, '$.project') = ? AND json_extract(data_json, '$.request_id') = ? ORDER BY json_extract(data_json, '$.created_at') DESC LIMIT 1`, project, requestID).Scan(&data)
		if err == nil {
			var lease domain.MailboxLease
			if err := s.decodeEntity("mailbox_leases", data, &lease); err != nil {
				_ = tx.Rollback()
				return domain.Mailbox{}, lease, false, err
			}
			if lease.Purpose != purpose {
				_ = tx.Rollback()
				return domain.Mailbox{}, lease, false, ErrLeaseRequestConflict
			}
			var mailbox domain.Mailbox
			found, err := s.readEntityTx(tx, "mailboxes", lease.MailboxID, &mailbox)
			if err != nil || !found {
				_ = tx.Rollback()
				return domain.Mailbox{}, lease, false, ErrLeaseBindingConflict
			}
			mailbox = sanitizeMailboxKind(mailbox)
			if !mailboxMatchesClaimFilter(mailbox, filter) {
				_ = tx.Rollback()
				return domain.Mailbox{}, lease, false, ErrLeaseRequestConflict
			}
			if len(expiryChanges) > 0 {
				if err := s.commitTx(tx, expiryChanges); err != nil {
					return domain.Mailbox{}, lease, false, err
				}
			} else {
				_ = tx.Rollback()
			}
			return mailbox, lease, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return domain.Mailbox{}, domain.MailboxLease{}, false, err
		}
	}
	var mailboxData []byte
	query := `SELECT data_json FROM mailboxes
		WHERE json_extract(data_json, '$.api_active') = 1
		AND json_extract(data_json, '$.icloud_active') = 1
		AND json_extract(data_json, '$.status') = ?
		AND COALESCE(json_extract(data_json, '$.active_lease_id'), '') = ''
		AND (
			COALESCE(json_extract(data_json, '$.mailbox_kind'), 'icloud_hme') <> 'domain_forward'
			OR COALESCE((SELECT json_extract(data_json, '$.enabled') FROM domain_mail_settings WHERE id = 'system'), 0) = 1
		)`
	args := []any{domain.StatusAvailable}
	if filter.MailboxKind != "" {
		query += ` AND COALESCE(json_extract(data_json, '$.mailbox_kind'), 'icloud_hme') = ?`
		args = append(args, filter.MailboxKind)
	}
	if filter.Domain != "" {
		query += ` AND lower(substr(json_extract(data_json, '$.email'), instr(json_extract(data_json, '$.email'), '@') + 1)) = ?`
		args = append(args, filter.Domain)
	}
	query += ` ORDER BY json_extract(data_json, '$.created_at') ASC LIMIT 1`
	err = tx.QueryRow(query, args...).Scan(&mailboxData)
	if errors.Is(err, sql.ErrNoRows) {
		if len(expiryChanges) > 0 {
			_ = s.commitTx(tx, expiryChanges)
		} else {
			_ = tx.Rollback()
		}
		return domain.Mailbox{}, domain.MailboxLease{}, false, ErrNoAvailableMailbox
	}
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	var mailbox domain.Mailbox
	if err := s.decodeEntity("mailboxes", mailboxData, &mailbox); err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	mailbox = sanitizeMailboxKind(mailbox)
	id, err := s.nextIDTx(tx, "lease")
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	lease := domain.MailboxLease{ID: id, MailboxID: mailbox.ID, Email: strings.ToLower(strings.TrimSpace(mailbox.Email)), Project: project, Purpose: purpose, RequestID: requestID, State: domain.MailboxLeaseClaimed, Note: note, ExpiresAt: now.Add(ttl), CreatedAt: now, UpdatedAt: now}
	mailbox.Status, mailbox.ActiveLeaseID, mailbox.UpdatedAt = domain.StatusReserved, lease.ID, now
	if note != "" {
		mailbox.Note = note
	}
	leaseChange, _, err := s.upsertEntityTx(tx, "mailbox_leases", "mailbox-lease", lease.ID, lease)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	mailboxChange, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	eventChange, err := s.appendEventTx(tx, "info", "mailbox_lease", fmt.Sprintf("项目 %s 已预留邮箱 %s，租约 %s", project, mailbox.Email, lease.ID))
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	changes := append(expiryChanges, leaseChange, mailboxChange, eventChange)
	return mailbox, lease, true, s.commitTx(tx, changes)
}

func mailboxMatchesClaimFilter(mailbox domain.Mailbox, filter MailboxClaimFilter) bool {
	if filter.MailboxKind != "" && normalizeMailboxKind(mailbox.MailboxKind) != filter.MailboxKind {
		return false
	}
	if filter.Domain == "" {
		return true
	}
	email := strings.ToLower(strings.TrimSpace(mailbox.Email))
	separator := strings.LastIndexByte(email, '@')
	return separator >= 0 && normalizeDomainName(email[separator+1:]) == filter.Domain
}

func (s *Store) CommitMailboxLease(leaseID, project, note string, now time.Time) (domain.Mailbox, domain.MailboxLease, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	lease, err := s.authorizedLeaseTx(tx, leaseID, project)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	var mailbox domain.Mailbox
	mailboxFound, _ := s.readEntityTx(tx, "mailboxes", lease.MailboxID, &mailbox)
	if lease.State == domain.MailboxLeaseCommitted {
		_ = tx.Rollback()
		if !mailboxFound {
			return domain.Mailbox{}, lease, true, ErrLeaseBindingConflict
		}
		return mailbox, lease, true, nil
	}
	if lease.State == domain.MailboxLeaseReleased {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseReleased
	}
	if lease.State == domain.MailboxLeaseExpired || !lease.ExpiresAt.After(now) {
		changes, _, _ := s.expireMailboxLeasesTx(tx, now)
		if len(changes) > 0 {
			_ = s.commitTx(tx, changes)
		} else {
			_ = tx.Rollback()
		}
		return domain.Mailbox{}, lease, false, ErrLeaseExpired
	}
	if !mailboxFound || mailbox.ActiveLeaseID != lease.ID || mailbox.Status != domain.StatusReserved {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseBindingConflict
	}
	note = strings.TrimSpace(note)
	lease.State, lease.CommittedAt, lease.UpdatedAt = domain.MailboxLeaseCommitted, now, now
	mailbox.Status, mailbox.ActiveLeaseID, mailbox.UpdatedAt = domain.StatusUsed, "", now
	if note != "" {
		lease.Note, mailbox.Note = note, note
	}
	return s.finishLeaseMutation(tx, mailbox, lease, "info", fmt.Sprintf("项目 %s 已提交邮箱租约 %s，邮箱 %s 标记为已使用", lease.Project, lease.ID, mailbox.Email), false)
}

// ReconcileUsedMailboxLease 修复“上游账号已绑定，但租约此前被误释放”的历史状态。
// 租约仍保留 released/expired 终态作为审计记录，只把仍处于 available 且未被
// 重新领取的邮箱标记为 used；如果邮箱已经产生新租约，则拒绝覆盖。
func (s *Store) ReconcileUsedMailboxLease(leaseID, project, note string, now time.Time) (domain.Mailbox, domain.MailboxLease, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	lease, err := s.authorizedLeaseTx(tx, leaseID, project)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	var mailbox domain.Mailbox
	mailboxFound, _ := s.readEntityTx(tx, "mailboxes", lease.MailboxID, &mailbox)
	if !mailboxFound {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseBindingConflict
	}
	if lease.State == domain.MailboxLeaseCommitted || (mailbox.Status == domain.StatusUsed && mailbox.ActiveLeaseID == "") {
		_ = tx.Rollback()
		return mailbox, lease, true, nil
	}
	var latestLeaseID string
	if err := tx.QueryRow(`SELECT id FROM mailbox_leases
		WHERE json_extract(data_json, '$.mailbox_id') = ?
		ORDER BY json_extract(data_json, '$.created_at') DESC, id DESC LIMIT 1`, lease.MailboxID).Scan(&latestLeaseID); err != nil || latestLeaseID != lease.ID {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseBindingConflict
	}
	if lease.State != domain.MailboxLeaseReleased && lease.State != domain.MailboxLeaseExpired {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseReconcileState
	}
	if mailbox.Status != domain.StatusAvailable || mailbox.ActiveLeaseID != "" {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseBindingConflict
	}
	note = strings.TrimSpace(note)
	mailbox.Status, mailbox.ActiveLeaseID, mailbox.UpdatedAt = domain.StatusUsed, "", now
	lease.UpdatedAt = now
	if note != "" {
		lease.Note, mailbox.Note = note, note
	}
	return s.finishLeaseMutation(
		tx,
		mailbox,
		lease,
		"warning",
		fmt.Sprintf("项目 %s 已纠偏邮箱租约 %s，邮箱 %s 标记为已使用", lease.Project, lease.ID, mailbox.Email),
		false,
	)
}

func (s *Store) ReleaseMailboxLease(leaseID, project, note string, now time.Time) (domain.Mailbox, domain.MailboxLease, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	lease, err := s.authorizedLeaseTx(tx, leaseID, project)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	var mailbox domain.Mailbox
	mailboxFound, _ := s.readEntityTx(tx, "mailboxes", lease.MailboxID, &mailbox)
	if lease.State == domain.MailboxLeaseCommitted {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseCommitted
	}
	if lease.State == domain.MailboxLeaseReleased || lease.State == domain.MailboxLeaseExpired {
		_ = tx.Rollback()
		if !mailboxFound {
			return domain.Mailbox{}, lease, true, ErrLeaseBindingConflict
		}
		return mailbox, lease, true, nil
	}
	if !lease.ExpiresAt.After(now) {
		changes, _, _ := s.expireMailboxLeasesTx(tx, now)
		if len(changes) > 0 {
			_ = s.commitTx(tx, changes)
		} else {
			_ = tx.Rollback()
		}
		if !mailboxFound {
			return domain.Mailbox{}, lease, true, ErrLeaseBindingConflict
		}
		mailbox.Status, mailbox.ActiveLeaseID = domain.StatusAvailable, ""
		lease.State = domain.MailboxLeaseExpired
		return mailbox, lease, true, nil
	}
	if !mailboxFound || mailbox.ActiveLeaseID != lease.ID || mailbox.Status != domain.StatusReserved {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseBindingConflict
	}
	note = strings.TrimSpace(note)
	lease.State, lease.ReleasedAt, lease.UpdatedAt = domain.MailboxLeaseReleased, now, now
	mailbox.Status, mailbox.ActiveLeaseID, mailbox.UpdatedAt = domain.StatusAvailable, "", now
	if note != "" {
		lease.Note, mailbox.Note = note, note
	}
	return s.finishLeaseMutation(tx, mailbox, lease, "info", fmt.Sprintf("项目 %s 已释放邮箱租约 %s，邮箱 %s 恢复可用", lease.Project, lease.ID, mailbox.Email), false)
}

func (s *Store) RenewMailboxLease(leaseID, project, note string, ttl time.Duration, now time.Time) (domain.Mailbox, domain.MailboxLease, error) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, domain.MailboxLease{}, err
	}
	lease, err := s.authorizedLeaseTx(tx, leaseID, project)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, err
	}
	if lease.State == domain.MailboxLeaseCommitted {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, ErrLeaseCommitted
	}
	if lease.State == domain.MailboxLeaseReleased {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, ErrLeaseReleased
	}
	if lease.State == domain.MailboxLeaseExpired || !lease.ExpiresAt.After(now) {
		changes, _, _ := s.expireMailboxLeasesTx(tx, now)
		if len(changes) > 0 {
			_ = s.commitTx(tx, changes)
		} else {
			_ = tx.Rollback()
		}
		return domain.Mailbox{}, lease, ErrLeaseExpired
	}
	var mailbox domain.Mailbox
	found, _ := s.readEntityTx(tx, "mailboxes", lease.MailboxID, &mailbox)
	if !found || mailbox.ActiveLeaseID != lease.ID || mailbox.Status != domain.StatusReserved {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, ErrLeaseBindingConflict
	}
	note = strings.TrimSpace(note)
	lease.ExpiresAt, lease.UpdatedAt, mailbox.UpdatedAt = now.Add(ttl), now, now
	if note != "" {
		lease.Note, mailbox.Note = note, note
	}
	mailbox, lease, _, err = s.finishLeaseMutation(tx, mailbox, lease, "info", fmt.Sprintf("项目 %s 已续期邮箱租约 %s，新到期时间 %s", lease.Project, lease.ID, lease.ExpiresAt.Format(time.RFC3339)), false)
	return mailbox, lease, err
}

func (s *Store) SetMailboxLeaseNote(leaseID, project, note string, now time.Time) (domain.Mailbox, domain.MailboxLease, error) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, domain.MailboxLease{}, err
	}
	lease, err := s.authorizedLeaseTx(tx, leaseID, project)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, err
	}
	var mailbox domain.Mailbox
	found, _ := s.readEntityTx(tx, "mailboxes", lease.MailboxID, &mailbox)
	if !found {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, ErrLeaseBindingConflict
	}
	lease.Note, lease.UpdatedAt = strings.TrimSpace(note), now
	if mailbox.ActiveLeaseID == lease.ID || (mailbox.ActiveLeaseID == "" && mailbox.Status == domain.StatusUsed && lease.State == domain.MailboxLeaseCommitted) {
		mailbox.Note, mailbox.UpdatedAt = lease.Note, now
	}
	mailbox, lease, _, err = s.finishLeaseMutation(tx, mailbox, lease, "info", fmt.Sprintf("项目 %s 已更新邮箱租约 %s 的备注", lease.Project, lease.ID), false)
	return mailbox, lease, err
}

// DeactivateMailboxLease 将租约对应的邮箱停用，并同步更新备注。
// 账号删除以邮箱最终状态为准，即使绑定记录来自历史租约，也要将 used 更新为 disabled。
func (s *Store) DeactivateMailboxLease(leaseID, project, note string, now time.Time) (domain.Mailbox, domain.MailboxLease, bool, error) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	lease, err := s.authorizedLeaseTx(tx, leaseID, project)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	var mailbox domain.Mailbox
	found, _ := s.readEntityTx(tx, "mailboxes", lease.MailboxID, &mailbox)
	if !found {
		_ = tx.Rollback()
		return domain.Mailbox{}, lease, false, ErrLeaseBindingConflict
	}
	note = strings.TrimSpace(note)
	wasDisabled := mailbox.Status == domain.StatusDisabled && mailbox.ActiveLeaseID == ""
	if lease.State == domain.MailboxLeaseClaimed && mailbox.ActiveLeaseID == lease.ID {
		lease.State, lease.ReleasedAt = domain.MailboxLeaseReleased, now
	}
	lease.UpdatedAt = now
	mailbox.Status, mailbox.ActiveLeaseID, mailbox.UpdatedAt = domain.StatusDisabled, "", now
	if note != "" {
		lease.Note, mailbox.Note = note, note
	}
	return s.finishLeaseMutation(
		tx,
		mailbox,
		lease,
		"warning",
		fmt.Sprintf("项目 %s 已停用邮箱租约 %s，邮箱 %s 标记为已停用", lease.Project, lease.ID, mailbox.Email),
		wasDisabled,
	)
}

func (s *Store) finishLeaseMutation(tx *sql.Tx, mailbox domain.Mailbox, lease domain.MailboxLease, level, message string, idempotent bool) (domain.Mailbox, domain.MailboxLease, bool, error) {
	leaseChange, _, err := s.upsertEntityTx(tx, "mailbox_leases", "mailbox-lease", lease.ID, lease)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	mailboxChange, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	eventChange, err := s.appendEventTx(tx, level, "mailbox_lease", message)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, domain.MailboxLease{}, false, err
	}
	return mailbox, lease, idempotent, s.commitTx(tx, []Change{leaseChange, mailboxChange, eventChange})
}

func (s *Store) FindMailboxLease(leaseID string) (domain.MailboxLease, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var lease domain.MailboxLease
	found, err := s.readEntity("mailbox_leases", strings.TrimSpace(leaseID), &lease)
	return lease, found && err == nil
}

func (s *Store) LatestMailboxLeaseByEmailProject(email, project string) (domain.MailboxLease, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var data []byte
	err := s.db.QueryRow(`SELECT data_json FROM mailbox_leases WHERE lower(json_extract(data_json, '$.email')) = ? AND json_extract(data_json, '$.project') = ? ORDER BY json_extract(data_json, '$.created_at') DESC LIMIT 1`, strings.ToLower(strings.TrimSpace(email)), normalizeLeaseProject(project)).Scan(&data)
	if err != nil {
		return domain.MailboxLease{}, false
	}
	var lease domain.MailboxLease
	if s.decodeEntity("mailbox_leases", data, &lease) != nil {
		return domain.MailboxLease{}, false
	}
	return lease, true
}

func (s *Store) MailboxLeases() []domain.MailboxLease {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.MailboxLease
	_ = s.loadEntities("mailbox_leases", `json_extract(data_json, '$.created_at') DESC`, &out)
	return out
}

func (s *Store) ExpireMailboxLeases(now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	changes, count, err := s.expireMailboxLeasesTx(tx, now)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if count == 0 {
		_ = tx.Rollback()
		return 0, nil
	}
	return count, s.commitTx(tx, changes)
}

func (s *Store) expireMailboxLeasesTx(tx *sql.Tx, now time.Time) ([]Change, int, error) {
	rows, err := tx.Query(`SELECT data_json FROM mailbox_leases WHERE json_extract(data_json, '$.state') = ? AND json_extract(data_json, '$.expires_at') <= ?`, domain.MailboxLeaseClaimed, now.Format(time.RFC3339Nano))
	if err != nil {
		return nil, 0, err
	}
	leases := []domain.MailboxLease{}
	for rows.Next() {
		var data []byte
		var lease domain.MailboxLease
		if rows.Scan(&data) == nil && s.decodeEntity("mailbox_leases", data, &lease) == nil {
			leases = append(leases, lease)
		}
	}
	_ = rows.Close()
	changes := []Change{}
	for _, lease := range leases {
		lease.State, lease.ExpiredAt, lease.UpdatedAt = domain.MailboxLeaseExpired, now, now
		if change, changed, err := s.upsertEntityTx(tx, "mailbox_leases", "mailbox-lease", lease.ID, lease); err != nil {
			return nil, 0, err
		} else if changed {
			changes = append(changes, change)
		}
		var mailbox domain.Mailbox
		if found, _ := s.readEntityTx(tx, "mailboxes", lease.MailboxID, &mailbox); found && mailbox.ActiveLeaseID == lease.ID {
			mailbox.ActiveLeaseID = ""
			if mailbox.Status == domain.StatusReserved {
				mailbox.Status = domain.StatusAvailable
			}
			mailbox.UpdatedAt = now
			if change, changed, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox); err != nil {
				return nil, 0, err
			} else if changed {
				changes = append(changes, change)
			}
		}
		eventChange, err := s.appendEventTx(tx, "warning", "mailbox_lease", fmt.Sprintf("邮箱租约 %s 已过期，邮箱 %s 已自动回收", lease.ID, lease.Email))
		if err != nil {
			return nil, 0, err
		}
		changes = append(changes, eventChange)
	}
	return changes, len(leases), nil
}

func (s *Store) authorizedLeaseTx(tx *sql.Tx, leaseID, project string) (domain.MailboxLease, error) {
	project = normalizeLeaseProject(project)
	if project == "" {
		return domain.MailboxLease{}, ErrLeaseProjectRequired
	}
	var lease domain.MailboxLease
	found, err := s.readEntityTx(tx, "mailbox_leases", strings.TrimSpace(leaseID), &lease)
	if err != nil {
		return domain.MailboxLease{}, err
	}
	if !found {
		return domain.MailboxLease{}, ErrLeaseNotFound
	}
	if lease.Project != project {
		return domain.MailboxLease{}, ErrLeaseProjectMismatch
	}
	return lease, nil
}

func normalizeLeaseProject(project string) string { return strings.ToLower(strings.TrimSpace(project)) }
