package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

const mailboxSyncHeartbeatInterval = time.Minute

// MailboxSyncMessage 是一次同步中准备写入的标准邮件。
type MailboxSyncMessage struct {
	RemoteID    string
	RemoteIDs   []string
	CanonicalID string
	Source      string
	Subject     string
	From        string
	Body        string
	HTMLBody    string
	ContentType string
	ReceivedAt  time.Time
}

// MailboxSyncUpdate 描述一个邮箱在本轮同步后的游标和新增邮件。
type MailboxSyncUpdate struct {
	MailboxID string
	LastUID   string
	SyncedAt  time.Time
	Messages  []MailboxSyncMessage
}

type MessageContentUpdate struct {
	MailboxID   string
	MessageID   string
	Body        string
	HTMLBody    string
	ContentType string
}

func (s *Store) AllMailboxes() []domain.Mailbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.Mailbox
	_ = s.loadEntities("mailboxes", `json_extract(data_json, '$.created_at') DESC`, &out)
	for index := range out {
		out[index] = sanitizeMailboxKind(out[index])
	}
	return out
}

func (s *Store) FindMailboxByID(id string) (domain.Mailbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var mailbox domain.Mailbox
	found, err := s.readEntity("mailboxes", strings.TrimSpace(id), &mailbox)
	return sanitizeMailboxKind(mailbox), found && err == nil
}

func (s *Store) FindMailboxByEmail(email string) (domain.Mailbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var data []byte
	err := s.db.QueryRow(`SELECT data_json FROM mailboxes WHERE lower(json_extract(data_json, '$.email')) = ? LIMIT 1`, strings.ToLower(strings.TrimSpace(email))).Scan(&data)
	if err != nil {
		return domain.Mailbox{}, false
	}
	var mailbox domain.Mailbox
	if s.decodeEntity("mailboxes", data, &mailbox) != nil {
		return domain.Mailbox{}, false
	}
	return sanitizeMailboxKind(mailbox), true
}

func (s *Store) UpsertMailboxFromRemote(accountID string, remote domain.RemoteMailbox, defaultNote string) (domain.Mailbox, bool, error) {
	email := strings.ToLower(strings.TrimSpace(remote.Email))
	if email == "" {
		return domain.Mailbox{}, false, errors.New("Apple 返回的隐私邮箱地址为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, false, err
	}
	var mailbox domain.Mailbox
	var data []byte
	err = tx.QueryRow(`SELECT data_json FROM mailboxes WHERE lower(json_extract(data_json, '$.email')) = ? LIMIT 1`, email).Scan(&data)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		_ = tx.Rollback()
		return domain.Mailbox{}, false, err
	}
	now := time.Now()
	if !created {
		if err := s.decodeEntity("mailboxes", data, &mailbox); err != nil {
			_ = tx.Rollback()
			return domain.Mailbox{}, false, err
		}
		mailbox.AccountID = firstNonEmpty(accountID, mailbox.AccountID)
		if strings.TrimSpace(mailbox.MailboxKind) == "" {
			mailbox.MailboxKind = domain.MailboxKindICloudHME
		}
		mailbox.AnonymousID = firstNonEmpty(remote.AnonymousID, mailbox.AnonymousID)
		mailbox.RemoteOrigin = firstNonEmpty(remote.Origin, mailbox.RemoteOrigin)
		mailbox.ForwardToEmail = firstNonEmpty(strings.ToLower(strings.TrimSpace(remote.ForwardToEmail)), mailbox.ForwardToEmail)
		if strings.TrimSpace(remote.Label) != "" {
			mailbox.Label = strings.TrimSpace(remote.Label)
		}
		mailbox.ICloudActive = remote.IsActive
		if strings.TrimSpace(mailbox.Note) == "" {
			mailbox.Note = firstNonEmpty(remote.Note, defaultNote)
		}
		mailbox.UpdatedAt = now
		change, changed, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
		if err != nil {
			_ = tx.Rollback()
			return domain.Mailbox{}, false, err
		}
		if !changed {
			_ = tx.Rollback()
			return mailbox, false, nil
		}
		return mailbox, false, s.commitTx(tx, []Change{change})
	}
	id, err := s.nextIDTx(tx, "mbx")
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, false, err
	}
	token, err := randomAPIToken(24)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, false, err
	}
	ownerID := ""
	var admin domain.Admin
	var adminData []byte
	if tx.QueryRow(`SELECT data_json FROM admins LIMIT 1`).Scan(&adminData) == nil {
		_ = s.decodeEntity("admins", adminData, &admin)
		ownerID = admin.ID
	}
	status := domain.StatusAvailable
	if !remote.IsActive {
		status = domain.StatusDisabled
	}
	mailbox = domain.Mailbox{
		ID: id, OwnerID: ownerID, AccountID: strings.TrimSpace(accountID), AnonymousID: strings.TrimSpace(remote.AnonymousID),
		RemoteOrigin: strings.TrimSpace(remote.Origin), MailboxKind: domain.MailboxKindICloudHME, Label: firstNonEmpty(remote.Label, "隐私邮箱 "+now.Format("0102-150405")),
		Email: email, ForwardToEmail: strings.ToLower(strings.TrimSpace(remote.ForwardToEmail)), APIToken: token, APIActive: true, ICloudActive: remote.IsActive, Status: status,
		Note: firstNonEmpty(remote.Note, defaultNote), CreatedAt: now, UpdatedAt: now,
	}
	mailboxChange, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, false, err
	}
	eventChange, err := s.appendEventTx(tx, "info", "mailbox", "已导入隐私邮箱 "+mailbox.Email)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, false, err
	}
	return mailbox, true, s.commitTx(tx, []Change{mailboxChange, eventChange})
}

func (s *Store) SetMailboxRemoteIdentity(id, anonymousID, origin string) (domain.Mailbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, err
	}
	var mailbox domain.Mailbox
	if found, err := s.readEntityTx(tx, "mailboxes", id, &mailbox); err != nil || !found {
		_ = tx.Rollback()
		if err != nil {
			return domain.Mailbox{}, err
		}
		return domain.Mailbox{}, errors.New("邮箱不存在")
	}
	anonymousID, origin = strings.TrimSpace(anonymousID), strings.TrimSpace(origin)
	if mailbox.AnonymousID == anonymousID && mailbox.RemoteOrigin == origin {
		_ = tx.Rollback()
		return mailbox, nil
	}
	mailbox.AnonymousID, mailbox.RemoteOrigin, mailbox.UpdatedAt = anonymousID, origin, time.Now()
	change, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, err
	}
	return mailbox, s.commitTx(tx, []Change{change})
}

func (s *Store) SetMailboxStatus(id string, apiActive, icloudActive *bool, status string, note *string) (domain.Mailbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, err
	}
	var mailbox domain.Mailbox
	if found, err := s.readEntityTx(tx, "mailboxes", id, &mailbox); err != nil || !found {
		_ = tx.Rollback()
		if err != nil {
			return domain.Mailbox{}, err
		}
		return domain.Mailbox{}, errors.New("邮箱不存在")
	}
	now := time.Now()
	changes := []Change{}
	if apiActive != nil {
		mailbox.APIActive = *apiActive
	}
	if icloudActive != nil {
		mailbox.ICloudActive = *icloudActive
	}
	desiredStatus := strings.TrimSpace(status)
	if desiredStatus != "" {
		if mailbox.ActiveLeaseID != "" && desiredStatus != domain.StatusReserved {
			var lease domain.MailboxLease
			if found, _ := s.readEntityTx(tx, "mailbox_leases", mailbox.ActiveLeaseID, &lease); found && lease.State == domain.MailboxLeaseClaimed {
				lease.UpdatedAt = now
				if desiredStatus == domain.StatusUsed {
					lease.State, lease.CommittedAt = domain.MailboxLeaseCommitted, now
				} else {
					lease.State, lease.ReleasedAt = domain.MailboxLeaseReleased, now
				}
				if change, changed, err := s.upsertEntityTx(tx, "mailbox_leases", "mailbox-lease", lease.ID, lease); err != nil {
					_ = tx.Rollback()
					return domain.Mailbox{}, err
				} else if changed {
					changes = append(changes, change)
				}
			}
			mailbox.ActiveLeaseID = ""
		}
		mailbox.Status = desiredStatus
	}
	if note != nil {
		mailbox.Note = strings.TrimSpace(*note)
		if mailbox.ActiveLeaseID != "" {
			var lease domain.MailboxLease
			if found, _ := s.readEntityTx(tx, "mailbox_leases", mailbox.ActiveLeaseID, &lease); found && lease.State == domain.MailboxLeaseClaimed {
				lease.Note, lease.UpdatedAt = mailbox.Note, now
				if change, changed, err := s.upsertEntityTx(tx, "mailbox_leases", "mailbox-lease", lease.ID, lease); err != nil {
					_ = tx.Rollback()
					return domain.Mailbox{}, err
				} else if changed {
					changes = append(changes, change)
				}
			}
		}
	}
	mailbox.UpdatedAt = now
	mailboxChange, changed, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, err
	}
	if !changed && len(changes) == 0 {
		_ = tx.Rollback()
		return mailbox, nil
	}
	if changed {
		changes = append(changes, mailboxChange)
	}
	eventChange, err := s.appendEventTx(tx, "info", "mailbox", "已更新邮箱状态 "+mailbox.Email)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, err
	}
	changes = append(changes, eventChange)
	return mailbox, s.commitTx(tx, changes)
}

func (s *Store) DeleteMailbox(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	var mailbox domain.Mailbox
	if found, err := s.readEntityTx(tx, "mailboxes", id, &mailbox); err != nil || !found {
		_ = tx.Rollback()
		if err != nil {
			return err
		}
		return errors.New("邮箱不存在")
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE json_extract(data_json, '$.mailbox_id') = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mailbox_leases WHERE json_extract(data_json, '$.mailbox_id') = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mailbox_history_states WHERE id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	change, _, err := s.deleteEntityTx(tx, "mailboxes", "mailbox", id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	eventChange, err := s.appendEventTx(tx, "warning", "mailbox", "已删除本地邮箱 "+mailbox.Email)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.commitTx(tx, []Change{change, eventChange})
}

func (s *Store) UpsertMessage(mailboxID, remoteID, source, subject, from, body string, receivedAt time.Time) (domain.Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Message{}, false, err
	}
	message, added, mailbox, changes, err := s.upsertMessageTx(tx, mailboxID, remoteID, source, subject, from, body, "", "text/plain", receivedAt)
	if err != nil {
		_ = tx.Rollback()
		return domain.Message{}, false, err
	}
	if !added {
		if len(changes) == 0 {
			_ = tx.Rollback()
			return message, false, nil
		}
		return message, false, s.commitTx(tx, changes)
	}
	mailbox.LastSyncAt, mailbox.UpdatedAt = time.Now(), time.Now()
	mailboxChange, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return domain.Message{}, false, err
	}
	changes = append(changes, mailboxChange)
	return message, true, s.commitTx(tx, changes)
}

func (s *Store) upsertMessageTx(tx *sql.Tx, mailboxID, remoteID, source, subject, from, body, htmlBody, contentType string, receivedAt time.Time) (domain.Message, bool, domain.Mailbox, []Change, error) {
	return s.upsertMessageDetailedTx(tx, mailboxID, MailboxSyncMessage{
		RemoteID: remoteID, RemoteIDs: []string{remoteID}, Source: source, Subject: subject, From: from,
		Body: body, HTMLBody: htmlBody, ContentType: contentType, ReceivedAt: receivedAt,
	})
}

func (s *Store) upsertMessageDetailedTx(tx *sql.Tx, mailboxID string, incoming MailboxSyncMessage) (domain.Message, bool, domain.Mailbox, []Change, error) {
	var mailbox domain.Mailbox
	if found, err := s.readEntityTx(tx, "mailboxes", mailboxID, &mailbox); err != nil || !found {
		if err != nil {
			return domain.Message{}, false, mailbox, nil, err
		}
		return domain.Message{}, false, mailbox, nil, errors.New("邮箱不存在")
	}
	remoteID := strings.TrimSpace(incoming.RemoteID)
	canonicalID := strings.TrimSpace(incoming.CanonicalID)
	remoteIDs := uniqueNonEmptyStrings(append(append([]string(nil), incoming.RemoteIDs...), remoteID))
	if remoteID != "" || canonicalID != "" {
		var data []byte
		err := sql.ErrNoRows
		if canonicalID != "" {
			err = tx.QueryRow(`SELECT data_json FROM messages WHERE json_extract(data_json, '$.mailbox_id') = ? AND json_extract(data_json, '$.canonical_id') = ? LIMIT 1`, mailboxID, canonicalID).Scan(&data)
		}
		if errors.Is(err, sql.ErrNoRows) && remoteID != "" {
			err = tx.QueryRow(`SELECT data_json FROM messages WHERE json_extract(data_json, '$.mailbox_id') = ? AND json_extract(data_json, '$.remote_id') = ? LIMIT 1`, mailboxID, remoteID).Scan(&data)
		}
		if errors.Is(err, sql.ErrNoRows) && remoteID != "" {
			err = tx.QueryRow(`SELECT data_json FROM messages
				WHERE json_extract(data_json, '$.mailbox_id') = ?
				AND EXISTS (SELECT 1 FROM json_each(COALESCE(json_extract(data_json, '$.remote_ids'), '[]')) WHERE value = ?)
				LIMIT 1`, mailboxID, remoteID).Scan(&data)
		}
		if err == nil {
			var existing domain.Message
			if err := s.decodeEntity("messages", data, &existing); err != nil {
				return domain.Message{}, false, mailbox, nil, err
			}
			changed := false
			if existing.CanonicalID == "" && canonicalID != "" {
				existing.CanonicalID = canonicalID
				changed = true
			}
			mergedRemoteIDs := uniqueNonEmptyStrings(append(append([]string(nil), existing.RemoteIDs...), remoteIDs...))
			if !equalStrings(existing.RemoteIDs, mergedRemoteIDs) {
				existing.RemoteIDs = mergedRemoteIDs
				changed = true
			}
			if strings.TrimSpace(existing.ContentType) == "" && strings.TrimSpace(incoming.ContentType) != "" {
				existing.ContentType = strings.TrimSpace(incoming.ContentType)
				changed = true
			}
			if strings.TrimSpace(incoming.HTMLBody) != "" && existing.HTMLBody != incoming.HTMLBody {
				existing.HTMLBody = incoming.HTMLBody
				existing.ContentType = "text/html"
				changed = true
			}
			if strings.TrimSpace(incoming.Body) != "" && (strings.TrimSpace(existing.Body) == "" || changed) && existing.Body != incoming.Body {
				existing.Body = incoming.Body
				changed = true
			}
			if !changed {
				return existing, false, mailbox, nil, nil
			}
			change, _, err := s.upsertEntityTx(tx, "messages", "message", existing.ID, existing)
			if err != nil {
				return domain.Message{}, false, mailbox, nil, err
			}
			return existing, false, mailbox, []Change{change}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return domain.Message{}, false, mailbox, nil, err
		}
	}
	id, err := s.nextIDTx(tx, "msg")
	if err != nil {
		return domain.Message{}, false, mailbox, nil, err
	}
	now := time.Now()
	if incoming.ReceivedAt.IsZero() {
		incoming.ReceivedAt = now
	}
	contentType := strings.TrimSpace(incoming.ContentType)
	if contentType == "" {
		contentType = "text/plain"
	}
	message := domain.Message{
		ID: id, OwnerID: mailbox.OwnerID, MailboxID: mailboxID, RemoteID: remoteID, RemoteIDs: remoteIDs, CanonicalID: canonicalID,
		Source: strings.TrimSpace(incoming.Source), Subject: strings.TrimSpace(incoming.Subject), From: strings.TrimSpace(incoming.From),
		Body: incoming.Body, HTMLBody: incoming.HTMLBody, ContentType: contentType, ReceivedAt: incoming.ReceivedAt, CreatedAt: now,
	}
	change, _, err := s.upsertEntityTx(tx, "messages", "message", message.ID, message)
	if err != nil {
		return domain.Message{}, false, mailbox, nil, err
	}
	mailbox.ReceiveCount++
	return message, true, mailbox, []Change{change}, nil
}

// ApplyMailboxSyncBatch 使用一个 SQLite 事务写入整组邮箱同步结果，并只发布一条批量 SSE。
func (s *Store) ApplyMailboxSyncBatch(updates []MailboxSyncUpdate) (int, error) {
	return s.applyMailboxSyncBatch(updates, "", nil, nil)
}

func (s *Store) ApplyMailboxSyncBatchWithRuntimeState(updates []MailboxSyncUpdate, stateID string, state any) (int, error) {
	return s.applyMailboxSyncBatch(updates, strings.TrimSpace(stateID), state, nil)
}

func (s *Store) ApplyMailboxSyncBatchWithIndex(updates []MailboxSyncUpdate, stateID string, state any, index MailIndexCommit) (int, error) {
	return s.applyMailboxSyncBatch(updates, strings.TrimSpace(stateID), state, &index)
}

func (s *Store) applyMailboxSyncBatch(updates []MailboxSyncUpdate, stateID string, state any, index *MailIndexCommit) (int, error) {
	if len(updates) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	created := 0
	changedMailboxes := make([]domain.Mailbox, 0, len(updates))
	changedMessages := make([]domain.Message, 0)
	updatedMessages := 0
	for _, update := range updates {
		var mailbox domain.Mailbox
		if found, err := s.readEntityTx(tx, "mailboxes", strings.TrimSpace(update.MailboxID), &mailbox); err != nil || !found {
			_ = tx.Rollback()
			if err != nil {
				return created, err
			}
			return created, errors.New("邮箱不存在")
		}
		mailboxChanged := false
		addedForMailbox := 0
		for _, incoming := range update.Messages {
			message, added, _, messageChanges, err := s.upsertMessageDetailedTx(tx, mailbox.ID, incoming)
			if err != nil {
				_ = tx.Rollback()
				return created, err
			}
			if added {
				created++
				addedForMailbox++
				mailboxChanged = true
				changedMessages = append(changedMessages, message)
			} else if len(messageChanges) > 0 {
				updatedMessages++
				changedMessages = append(changedMessages, message)
			}
		}
		mailbox.ReceiveCount += addedForMailbox
		syncedAt := update.SyncedAt
		if syncedAt.IsZero() {
			syncedAt = time.Now()
		}
		lastUID := strings.TrimSpace(update.LastUID)
		uidChanged := lastUID != "" && lastUID != mailbox.LastSyncUID
		heartbeatDue := mailbox.LastSyncAt.IsZero() || syncedAt.Sub(mailbox.LastSyncAt) >= mailboxSyncHeartbeatInterval
		if mailboxChanged || uidChanged || heartbeatDue {
			mailbox.LastSyncAt = syncedAt
			if lastUID != "" {
				mailbox.LastSyncUID = lastUID
			}
			mailbox.UpdatedAt = syncedAt
			if _, changed, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox); err != nil {
				_ = tx.Rollback()
				return created, err
			} else if changed {
				changedMailboxes = append(changedMailboxes, sanitizeMailbox(mailbox))
			}
		}
	}
	stateChanged := false
	if index != nil {
		indexChanged, err := s.applyMailIndexCommitTx(tx, *index)
		if err != nil {
			_ = tx.Rollback()
			return created, err
		}
		stateChanged = stateChanged || indexChanged
	}
	if stateID != "" && state != nil {
		if _, changed, err := s.upsertEntityTx(tx, "runtime_states", "mail-sync", stateID, state); err != nil {
			_ = tx.Rollback()
			return created, err
		} else {
			stateChanged = stateChanged || changed
		}
	}
	if created == 0 && updatedMessages == 0 && len(changedMailboxes) == 0 && !stateChanged {
		_ = tx.Rollback()
		return 0, nil
	}
	payload, _ := json.Marshal(map[string]any{"operation": "batch-updated", "created_message_count": created, "updated_message_count": updatedMessages, "items": changedMailboxes, "messages": changedMessages})
	change := Change{Type: "mailbox.batch-updated", Resource: "mailbox", ResourceID: "batch", Operation: "batch-updated", Payload: payload, CreatedAt: time.Now()}
	return created, s.commitTx(tx, []Change{change})
}

func uniqueNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
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

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sanitizeMailbox(mailbox domain.Mailbox) domain.Mailbox { mailbox.APIToken = ""; return mailbox }

func (s *Store) MessagesForMailbox(mailboxID string) []domain.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT data_json FROM messages WHERE json_extract(data_json, '$.mailbox_id') = ? ORDER BY json_extract(data_json, '$.received_at') DESC`, strings.TrimSpace(mailboxID))
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []domain.Message{}
	for rows.Next() {
		var data []byte
		var message domain.Message
		if rows.Scan(&data) == nil && s.decodeEntity("messages", data, &message) == nil {
			out = append(out, message)
		}
	}
	return out
}

func (s *Store) FindMessageForMailbox(mailboxID, messageID string) (domain.Message, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var message domain.Message
	found, err := s.readEntity("messages", strings.TrimSpace(messageID), &message)
	return message, found && err == nil && message.MailboxID == strings.TrimSpace(mailboxID)
}

func (s *Store) MessagesMissingContent(limit int) []domain.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	query := `SELECT data_json FROM messages
		WHERE lower(COALESCE(json_extract(data_json, '$.source'), '')) = 'imap'
		AND COALESCE(json_extract(data_json, '$.content_type'), '') = ''
		ORDER BY json_extract(data_json, '$.received_at') DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []domain.Message{}
	for rows.Next() {
		var data []byte
		var message domain.Message
		if rows.Scan(&data) == nil && s.decodeEntity("messages", data, &message) == nil {
			out = append(out, message)
		}
	}
	return out
}

// ApplyMessageContentUpdates 批量补全邮件正文，只发布一条不含正文的实时变更。
func (s *Store) ApplyMessageContentUpdates(updates []MessageContentUpdate) (int, error) {
	if len(updates) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	updated := 0
	mailboxIDs := map[string]bool{}
	for _, update := range updates {
		var message domain.Message
		found, err := s.readEntityTx(tx, "messages", strings.TrimSpace(update.MessageID), &message)
		if err != nil {
			_ = tx.Rollback()
			return updated, err
		}
		if !found || message.MailboxID != strings.TrimSpace(update.MailboxID) {
			continue
		}
		if strings.TrimSpace(update.Body) != "" {
			message.Body = update.Body
		}
		if strings.TrimSpace(update.HTMLBody) != "" {
			message.HTMLBody = update.HTMLBody
			message.ContentType = "text/html"
		} else if strings.TrimSpace(update.ContentType) != "" {
			message.ContentType = strings.TrimSpace(update.ContentType)
		}
		if _, changed, err := s.upsertEntityTx(tx, "messages", "message", message.ID, message); err != nil {
			_ = tx.Rollback()
			return updated, err
		} else if changed {
			updated++
			mailboxIDs[message.MailboxID] = true
		}
	}
	if updated == 0 {
		_ = tx.Rollback()
		return 0, nil
	}
	ids := make([]string, 0, len(mailboxIDs))
	for id := range mailboxIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	payload, _ := json.Marshal(map[string]any{"operation": "content-backfilled", "updated_message_count": updated, "mailbox_ids": ids})
	change := Change{Type: "message.content-backfilled", Resource: "message", ResourceID: "batch", Operation: "content-backfilled", Payload: payload, CreatedAt: time.Now()}
	return updated, s.commitTx(tx, []Change{change})
}

// UpdateMessageContent 补全本地邮件正文，不改变邮箱收件数量。
func (s *Store) UpdateMessageContent(mailboxID, messageID, body, htmlBody, contentType string) (domain.Message, error) {
	if _, err := s.ApplyMessageContentUpdates([]MessageContentUpdate{{MailboxID: mailboxID, MessageID: messageID, Body: body, HTMLBody: htmlBody, ContentType: contentType}}); err != nil {
		return domain.Message{}, err
	}
	message, ok := s.FindMessageForMailbox(mailboxID, messageID)
	if !ok {
		return domain.Message{}, errors.New("邮件不存在")
	}
	return message, nil
}

func (s *Store) DeleteMailboxMessages(mailboxID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	var mailbox domain.Mailbox
	if found, err := s.readEntityTx(tx, "mailboxes", mailboxID, &mailbox); err != nil || !found {
		_ = tx.Rollback()
		if err != nil {
			return 0, err
		}
		return 0, errors.New("邮箱不存在")
	}
	result, err := tx.Exec(`DELETE FROM messages WHERE json_extract(data_json, '$.mailbox_id') = ?`, mailboxID)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	count64, _ := result.RowsAffected()
	count := int(count64)
	metadataChanged := mailbox.ReceiveCount != 0 || mailbox.LastCodeMessageID != "" || !mailbox.LastCodeAt.IsZero()
	if count == 0 && !metadataChanged {
		_ = tx.Rollback()
		return 0, nil
	}
	mailbox.ReceiveCount, mailbox.LastCodeMessageID, mailbox.LastCodeAt, mailbox.UpdatedAt = 0, "", time.Time{}, time.Now()
	mailboxChange, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	eventChange, err := s.appendEventTx(tx, "info", "mailbox", fmt.Sprintf("已清空本地邮件 %s：%d 封", mailbox.Email, count))
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	messageChange := makeEntityChange("messages", "message", mailboxID, "batch-deleted", nil, time.Now())
	return count, s.commitTx(tx, []Change{mailboxChange, messageChange, eventChange})
}

// DeleteAccountMessages 在一个事务中清理指定 Apple 账号的全部本地邮件，并重置邮箱收件统计。
func (s *Store) DeleteAccountMessages(accountID string) (int, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, errors.New("Apple 账号标识不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(`SELECT data_json FROM mailboxes WHERE json_extract(data_json, '$.account_id') = ? ORDER BY id`, accountID)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	mailboxes := make([]domain.Mailbox, 0)
	for rows.Next() {
		var data []byte
		var mailbox domain.Mailbox
		if err := rows.Scan(&data); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return 0, err
		}
		if err := s.decodeEntity("mailboxes", data, &mailbox); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return 0, err
		}
		mailboxes = append(mailboxes, mailbox)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_ = tx.Rollback()
		return 0, err
	}
	_ = rows.Close()
	result, err := tx.Exec(`DELETE FROM messages WHERE json_extract(data_json, '$.mailbox_id') IN (SELECT id FROM mailboxes WHERE json_extract(data_json, '$.account_id') = ?)`, accountID)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	count64, _ := result.RowsAffected()
	count := int(count64)
	changes := make([]Change, 0, len(mailboxes)+2)
	now := time.Now()
	for _, mailbox := range mailboxes {
		if mailbox.ReceiveCount == 0 && mailbox.LastCodeMessageID == "" && mailbox.LastCodeAt.IsZero() {
			continue
		}
		mailbox.ReceiveCount = 0
		mailbox.LastCodeMessageID = ""
		mailbox.LastCodeAt = time.Time{}
		mailbox.UpdatedAt = now
		change, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		changes = append(changes, change)
	}
	if count == 0 && len(changes) == 0 {
		_ = tx.Rollback()
		return 0, nil
	}
	payload, _ := json.Marshal(map[string]any{"account_id": accountID, "deleted": count})
	changes = append(changes, Change{Type: "message.batch-deleted", Resource: "message", ResourceID: accountID, Operation: "batch-deleted", Payload: payload, CreatedAt: now})
	eventChange, err := s.appendEventTx(tx, "info", "mailbox", fmt.Sprintf("已清空 Apple 账号本地邮件：%d 封", count))
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	changes = append(changes, eventChange)
	return count, s.commitTx(tx, changes)
}

func (s *Store) DeleteMailboxMessagesByRemoteIDs(mailboxID string, remoteIDs []string) (int, error) {
	targets := make([]string, 0, len(remoteIDs))
	for _, id := range remoteIDs {
		if id = strings.TrimSpace(id); id != "" {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	var mailbox domain.Mailbox
	if found, err := s.readEntityTx(tx, "mailboxes", mailboxID, &mailbox); err != nil || !found {
		_ = tx.Rollback()
		if err != nil {
			return 0, err
		}
		return 0, errors.New("邮箱不存在")
	}
	removed := 0
	for _, remoteID := range targets {
		result, err := tx.Exec(`DELETE FROM messages
			WHERE json_extract(data_json, '$.mailbox_id') = ?
			AND (
				json_extract(data_json, '$.remote_id') = ?
				OR EXISTS (SELECT 1 FROM json_each(COALESCE(json_extract(data_json, '$.remote_ids'), '[]')) WHERE value = ?)
			)`, mailboxID, remoteID, remoteID)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		count, _ := result.RowsAffected()
		removed += int(count)
	}
	if removed == 0 {
		_ = tx.Rollback()
		return 0, nil
	}
	_ = tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE json_extract(data_json, '$.mailbox_id') = ?`, mailboxID).Scan(&mailbox.ReceiveCount)
	mailbox.UpdatedAt = time.Now()
	mailboxChange, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	eventChange, err := s.appendEventTx(tx, "info", "mailbox", fmt.Sprintf("已清理本地邮件缓存 %s：%d 封", mailbox.Email, removed))
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	messageChange := makeEntityChange("messages", "message", mailboxID, "batch-deleted", nil, time.Now())
	return removed, s.commitTx(tx, []Change{mailboxChange, messageChange, eventChange})
}

func (s *Store) SetMailboxSyncCursor(id string, syncedAt time.Time, lastUID string) (domain.Mailbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Mailbox{}, err
	}
	var mailbox domain.Mailbox
	if found, err := s.readEntityTx(tx, "mailboxes", id, &mailbox); err != nil || !found {
		_ = tx.Rollback()
		if err != nil {
			return domain.Mailbox{}, err
		}
		return domain.Mailbox{}, errors.New("邮箱不存在")
	}
	if syncedAt.IsZero() {
		syncedAt = time.Now()
	}
	lastUID = strings.TrimSpace(lastUID)
	if lastUID == mailbox.LastSyncUID && !mailbox.LastSyncAt.IsZero() && syncedAt.Sub(mailbox.LastSyncAt) < mailboxSyncHeartbeatInterval {
		_ = tx.Rollback()
		return mailbox, nil
	}
	mailbox.LastSyncAt = syncedAt
	if lastUID != "" {
		mailbox.LastSyncUID = lastUID
	}
	mailbox.UpdatedAt = syncedAt
	change, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return domain.Mailbox{}, err
	}
	return mailbox, s.commitTx(tx, []Change{change})
}

func (s *Store) SetMailboxLastCode(id, messageID string, servedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	var mailbox domain.Mailbox
	if found, err := s.readEntityTx(tx, "mailboxes", id, &mailbox); err != nil || !found {
		_ = tx.Rollback()
		if err != nil {
			return err
		}
		return errors.New("邮箱不存在")
	}
	mailbox.LastCodeMessageID, mailbox.LastCodeAt, mailbox.UpdatedAt = strings.TrimSpace(messageID), servedAt, time.Now()
	change, _, err := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.commitTx(tx, []Change{change})
}

// PruneMessagesBefore 清理保留期之外的邮件，并修正邮箱收件数。
func (s *Store) PruneMessagesBefore(before time.Time) (int, error) {
	if before.IsZero() {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(`SELECT DISTINCT json_extract(data_json, '$.mailbox_id') FROM messages WHERE json_extract(data_json, '$.received_at') < ?`, before.Format(time.RFC3339Nano))
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	result, err := tx.Exec(`DELETE FROM messages WHERE json_extract(data_json, '$.received_at') < ?`, before.Format(time.RFC3339Nano))
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	count64, _ := result.RowsAffected()
	count := int(count64)
	if count == 0 {
		_ = tx.Rollback()
		return 0, nil
	}
	for _, id := range ids {
		var mailbox domain.Mailbox
		if found, _ := s.readEntityTx(tx, "mailboxes", id, &mailbox); !found {
			continue
		}
		_ = tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE json_extract(data_json, '$.mailbox_id') = ?`, id).Scan(&mailbox.ReceiveCount)
		mailbox.UpdatedAt = time.Now()
		_, _, _ = s.upsertEntityTx(tx, "mailboxes", "mailbox", id, mailbox)
	}
	payload, _ := json.Marshal(map[string]any{"operation": "retention-pruned", "deleted": count})
	change := Change{Type: "message.retention-pruned", Resource: "message", ResourceID: "retention", Operation: "retention-pruned", Payload: payload, CreatedAt: time.Now()}
	return count, s.commitTx(tx, []Change{change})
}

func randomAPIToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
