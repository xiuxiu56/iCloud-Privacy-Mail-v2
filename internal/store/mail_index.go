package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// MailIndexEntry 是同一收件账号共用的 IMAP 邮件索引。
// 未命中已登记地址的邮件只保存邮件头，命中后再按 UID 读取完整正文。
type MailIndexEntry struct {
	ID           string    `json:"id"`
	SourceKey    string    `json:"source_key"`
	Folder       string    `json:"folder"`
	UIDValidity  string    `json:"uid_validity,omitempty"`
	UID          string    `json:"uid"`
	RemoteID     string    `json:"remote_id,omitempty"`
	RemoteIDs    []string  `json:"remote_ids,omitempty"`
	CanonicalID  string    `json:"canonical_id,omitempty"`
	Recipients   []string  `json:"recipients,omitempty"`
	Source       string    `json:"source,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	From         string    `json:"from,omitempty"`
	Body         string    `json:"body,omitempty"`
	HTMLBody     string    `json:"html_body,omitempty"`
	ContentType  string    `json:"content_type,omitempty"`
	ReceivedAt   time.Time `json:"received_at,omitempty"`
	BodyComplete bool      `json:"body_complete"`
	IndexedAt    time.Time `json:"indexed_at"`
}

// MailboxHistoryState 记录某个地址是否已对当前收件账号的历史索引完成过匹配。
type MailboxHistoryState struct {
	MailboxID         string    `json:"mailbox_id"`
	SourceKey         string    `json:"source_key"`
	Email             string    `json:"email"`
	UIDValidity       string    `json:"uid_validity,omitempty"`
	HistoryThroughUID string    `json:"history_through_uid,omitempty"`
	Complete          bool      `json:"complete"`
	CompletedAt       time.Time `json:"completed_at,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// MailIndexCommit 把邮件索引、邮箱历史状态与同步结果一起提交。
type MailIndexCommit struct {
	SourceKey   string
	Folder      string
	UIDValidity string
	Replace     bool
	Entries     []MailIndexEntry
	Histories   []MailboxHistoryState
}

func (commit MailIndexCommit) normalized() MailIndexCommit {
	commit.SourceKey = strings.TrimSpace(commit.SourceKey)
	commit.Folder = strings.ToUpper(strings.TrimSpace(commit.Folder))
	if commit.Folder == "" {
		commit.Folder = "INBOX"
	}
	commit.UIDValidity = strings.TrimSpace(commit.UIDValidity)
	return commit
}

func (s *Store) applyMailIndexCommitTx(tx *sql.Tx, commit MailIndexCommit) (bool, error) {
	commit = commit.normalized()
	if commit.SourceKey == "" {
		return false, nil
	}
	changed := false
	if commit.Replace {
		result, err := tx.Exec(`DELETE FROM mail_message_index WHERE json_extract(data_json, '$.source_key') = ? AND json_extract(data_json, '$.folder') = ?`, commit.SourceKey, commit.Folder)
		if err != nil {
			return false, err
		}
		count, _ := result.RowsAffected()
		changed = count > 0
	}
	for _, entry := range commit.Entries {
		entry.SourceKey = commit.SourceKey
		entry.Folder = commit.Folder
		entry.UIDValidity = commit.UIDValidity
		entryChanged, err := s.upsertMailIndexEntryTx(tx, entry)
		if err != nil {
			return false, err
		}
		changed = changed || entryChanged
	}
	for _, history := range commit.Histories {
		history = normalizeMailboxHistoryState(history)
		if history.MailboxID == "" {
			continue
		}
		var current MailboxHistoryState
		if found, err := s.readEntityTx(tx, "mailbox_history_states", history.MailboxID, &current); err != nil {
			return false, err
		} else if found {
			current = normalizeMailboxHistoryState(current)
			if sameMailboxHistoryProgress(current, history) {
				continue
			}
			if current.Complete && history.Complete &&
				current.SourceKey == history.SourceKey &&
				current.Email == history.Email &&
				current.UIDValidity == history.UIDValidity &&
				!current.CompletedAt.IsZero() {
				history.CompletedAt = current.CompletedAt
			}
		}
		_, historyChanged, err := s.upsertEntityTx(tx, "mailbox_history_states", "mailbox-history", history.MailboxID, history)
		if err != nil {
			return false, err
		}
		changed = changed || historyChanged
	}
	return changed, nil
}

func normalizeMailIndexEntry(entry MailIndexEntry) MailIndexEntry {
	entry.SourceKey = strings.TrimSpace(entry.SourceKey)
	entry.Folder = strings.ToUpper(strings.TrimSpace(entry.Folder))
	if entry.Folder == "" {
		entry.Folder = "INBOX"
	}
	entry.UIDValidity = strings.TrimSpace(entry.UIDValidity)
	entry.UID = strings.TrimSpace(strings.TrimPrefix(entry.UID, "imap:"))
	entry.RemoteID = strings.TrimSpace(entry.RemoteID)
	entry.RemoteIDs = uniqueNonEmptyStrings(entry.RemoteIDs)
	entry.CanonicalID = strings.TrimSpace(entry.CanonicalID)
	entry.Source = strings.TrimSpace(entry.Source)
	if entry.Source == "" {
		entry.Source = "imap"
	}
	recipients := make([]string, 0, len(entry.Recipients))
	seen := make(map[string]bool, len(entry.Recipients))
	for _, email := range entry.Recipients {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		recipients = append(recipients, email)
	}
	sort.Strings(recipients)
	entry.Recipients = recipients
	if entry.ContentType == "" {
		entry.ContentType = "text/plain"
	}
	if entry.IndexedAt.IsZero() {
		entry.IndexedAt = time.Now()
	}
	if entry.ID == "" && entry.SourceKey != "" && entry.UID != "" {
		sum := sha256.Sum256([]byte(strings.Join([]string{entry.SourceKey, entry.Folder, entry.UIDValidity, entry.UID}, "\x00")))
		entry.ID = fmt.Sprintf("midx_%x", sum[:16])
	}
	return entry
}

func mergeMailIndexEntry(current, incoming MailIndexEntry) MailIndexEntry {
	current = normalizeMailIndexEntry(current)
	incoming = normalizeMailIndexEntry(incoming)
	if current.ID == "" {
		return incoming
	}
	current.Recipients = uniqueNonEmptyStrings(append(current.Recipients, incoming.Recipients...))
	sort.Strings(current.Recipients)
	current.RemoteIDs = uniqueNonEmptyStrings(append(append(current.RemoteIDs, incoming.RemoteIDs...), incoming.RemoteID))
	if incoming.RemoteID != "" {
		current.RemoteID = incoming.RemoteID
	}
	if incoming.CanonicalID != "" {
		current.CanonicalID = incoming.CanonicalID
	}
	if incoming.Subject != "" {
		current.Subject = incoming.Subject
	}
	if incoming.From != "" {
		current.From = incoming.From
	}
	if !incoming.ReceivedAt.IsZero() {
		current.ReceivedAt = incoming.ReceivedAt
	}
	if incoming.BodyComplete || !current.BodyComplete {
		current.Body = incoming.Body
		current.HTMLBody = incoming.HTMLBody
		current.ContentType = incoming.ContentType
	}
	current.BodyComplete = current.BodyComplete || incoming.BodyComplete
	return current
}

func (s *Store) upsertMailIndexEntryTx(tx *sql.Tx, entry MailIndexEntry) (bool, error) {
	entry = normalizeMailIndexEntry(entry)
	if entry.ID == "" || entry.SourceKey == "" || entry.UID == "" {
		return false, nil
	}
	var current MailIndexEntry
	if found, err := s.readEntityTx(tx, "mail_message_index", entry.ID, &current); err != nil {
		return false, err
	} else if found {
		entry = mergeMailIndexEntry(current, entry)
	}
	_, changed, err := s.upsertEntityTx(tx, "mail_message_index", "mail-message-index", entry.ID, entry)
	return changed, err
}

func normalizeMailboxHistoryState(state MailboxHistoryState) MailboxHistoryState {
	state.MailboxID = strings.TrimSpace(state.MailboxID)
	state.SourceKey = strings.TrimSpace(state.SourceKey)
	state.Email = strings.ToLower(strings.TrimSpace(state.Email))
	state.UIDValidity = strings.TrimSpace(state.UIDValidity)
	state.HistoryThroughUID = strings.TrimSpace(state.HistoryThroughUID)
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now()
	}
	if state.Complete && state.CompletedAt.IsZero() {
		state.CompletedAt = state.UpdatedAt
	}
	return state
}

func sameMailboxHistoryProgress(current, incoming MailboxHistoryState) bool {
	return current.SourceKey == incoming.SourceKey &&
		current.Email == incoming.Email &&
		current.UIDValidity == incoming.UIDValidity &&
		current.HistoryThroughUID == incoming.HistoryThroughUID &&
		current.Complete == incoming.Complete
}

// MailboxHistoryStates 读取邮箱级历史匹配状态。
func (s *Store) MailboxHistoryStates(mailboxIDs []string) map[string]MailboxHistoryState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]MailboxHistoryState, len(mailboxIDs))
	for _, mailboxID := range mailboxIDs {
		mailboxID = strings.TrimSpace(mailboxID)
		if mailboxID == "" {
			continue
		}
		var state MailboxHistoryState
		if found, err := s.readEntity("mailbox_history_states", mailboxID, &state); err == nil && found {
			out[mailboxID] = normalizeMailboxHistoryState(state)
		}
	}
	return out
}

// IndexedMailForMailboxes 从账号索引匹配一组收件地址，不再重新扫描 IMAP 收件箱。
func (s *Store) IndexedMailForMailboxes(sourceKey, uidValidity string, mailboxes []domain.Mailbox) (map[string][]MailIndexEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sourceKey = strings.TrimSpace(sourceKey)
	uidValidity = strings.TrimSpace(uidValidity)
	emailToMailboxIDs := make(map[string][]string, len(mailboxes))
	for _, mailbox := range mailboxes {
		email := strings.ToLower(strings.TrimSpace(mailbox.Email))
		if email != "" && strings.TrimSpace(mailbox.ID) != "" {
			emailToMailboxIDs[email] = append(emailToMailboxIDs[email], mailbox.ID)
		}
	}
	out := make(map[string][]MailIndexEntry, len(mailboxes))
	query := `SELECT data_json FROM mail_message_index WHERE json_extract(data_json, '$.source_key') = ?`
	args := []any{sourceKey}
	if uidValidity != "" {
		query += ` AND COALESCE(json_extract(data_json, '$.uid_validity'), '') = ?`
		args = append(args, uidValidity)
	}
	query += ` ORDER BY CAST(json_extract(data_json, '$.uid') AS INTEGER) ASC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		var entry MailIndexEntry
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := s.decodeEntity("mail_message_index", data, &entry); err != nil {
			return nil, err
		}
		entry = normalizeMailIndexEntry(entry)
		matched := make(map[string]bool)
		for _, recipient := range entry.Recipients {
			for _, mailboxID := range emailToMailboxIDs[recipient] {
				if !matched[mailboxID] {
					out[mailboxID] = append(out[mailboxID], entry)
					matched[mailboxID] = true
				}
			}
		}
	}
	return out, rows.Err()
}

// DeleteMailIndexByRemoteIDs 在云端邮件已彻底删除后同步清理本地账号索引。
func (s *Store) DeleteMailIndexByRemoteIDs(sourceKey string, remoteIDs []string) error {
	sourceKey = strings.TrimSpace(sourceKey)
	remoteIDs = uniqueNonEmptyStrings(remoteIDs)
	if sourceKey == "" || len(remoteIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, remoteID := range remoteIDs {
		if _, err := tx.Exec(`DELETE FROM mail_message_index
			WHERE json_extract(data_json, '$.source_key') = ?
			AND (json_extract(data_json, '$.remote_id') = ?
			OR EXISTS (SELECT 1 FROM json_each(COALESCE(json_extract(data_json, '$.remote_ids'), '[]')) WHERE value = ?))`, sourceKey, remoteID, remoteID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return s.commitTx(tx, nil)
}

// DeleteMailIndexForAddresses 在按收件地址完成云端清理后，删除会导致历史邮件被再次关联的本地索引。
func (s *Store) DeleteMailIndexForAddresses(sourceKey string, emails []string) error {
	sourceKey = strings.TrimSpace(sourceKey)
	normalized := make([]string, 0, len(emails))
	for _, email := range emails {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			normalized = append(normalized, email)
		}
	}
	normalized = uniqueNonEmptyStrings(normalized)
	if sourceKey == "" || len(normalized) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, email := range normalized {
		if _, err := tx.Exec(`DELETE FROM mail_message_index
			WHERE json_extract(data_json, '$.source_key') = ?
			AND EXISTS (SELECT 1 FROM json_each(COALESCE(json_extract(data_json, '$.recipients'), '[]')) WHERE lower(value) = ?)`, sourceKey, email); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return s.commitTx(tx, nil)
}
