package mailbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

func imapSourceSignature(state protocol.LoginState) string {
	value := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(state.IMAPHost)), fmt.Sprint(state.IMAPPort),
		strings.ToLower(strings.TrimSpace(state.IMAPUsername)), strings.ToLower(strings.TrimSpace(state.IMAPEmail)),
	}, "\x00")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func pendingHistoryMailboxes(state *store.Store, mailboxes []domain.Mailbox, sourceKey, uidValidity, throughUID string) []domain.Mailbox {
	ids := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		ids = append(ids, mailbox.ID)
	}
	histories := state.MailboxHistoryStates(ids)
	pending := make([]domain.Mailbox, 0)
	for _, mailbox := range mailboxes {
		history, found := histories[mailbox.ID]
		valid := found && history.Complete && history.SourceKey == strings.TrimSpace(sourceKey) &&
			strings.EqualFold(history.Email, mailbox.Email)
		if valid && strings.TrimSpace(uidValidity) != "" {
			valid = history.UIDValidity == strings.TrimSpace(uidValidity)
		}
		if valid && imapUIDNumber(throughUID) > 0 {
			valid = imapUIDNumber(history.HistoryThroughUID) >= imapUIDNumber(throughUID)
		}
		if !valid {
			pending = append(pending, mailbox)
		}
	}
	return pending
}

func (s *Service) mailboxesForSharedSource(seed []domain.Mailbox, customRoute domain.DomainMailRoute) []domain.Mailbox {
	if len(seed) == 0 {
		return nil
	}
	if strings.TrimSpace(customRoute.ID) != "" {
		out := make([]domain.Mailbox, 0)
		for _, mailbox := range s.store.DomainMailboxes() {
			if mailbox.DomainRouteID == customRoute.ID && mailbox.ICloudActive && mailbox.Status != domain.StatusDisabled {
				out = append(out, mailbox)
			}
		}
		return out
	}
	accountID := strings.TrimSpace(seed[0].AccountID)
	domainEnabled := s.store.DomainMailSettings().Enabled
	out := make([]domain.Mailbox, 0)
	for _, mailbox := range s.store.AllMailboxes() {
		if strings.TrimSpace(mailbox.AccountID) != accountID || !mailbox.ICloudActive || mailbox.Status == domain.StatusDisabled {
			continue
		}
		if mailboxKind(mailbox) == domain.MailboxKindDomainForward {
			if !domainEnabled || strings.TrimSpace(mailbox.DomainRouteID) == "" {
				continue
			}
			route, found := s.store.DomainMailRouteForSync(mailbox.DomainRouteID)
			if !found || !route.Enabled || route.ReceiverType != domain.DomainReceiverAppleAccount || strings.TrimSpace(route.AccountID) != accountID {
				continue
			}
		}
		out = append(out, mailbox)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Service) materializeIndexedMail(ctx context.Context, backend messageSyncBackend, imapState protocol.LoginState, sourceKey, uidValidity string, mailboxes []domain.Mailbox) (protocol.MailSyncBatchResult, []store.MailIndexEntry, error) {
	result := protocol.MailSyncBatchResult{MessagesByMailbox: make(map[string][]protocol.ICloudSyncedMessage)}
	if len(mailboxes) == 0 {
		return result, nil, nil
	}
	indexed, err := s.store.IndexedMailForMailboxes(sourceKey, uidValidity, mailboxes)
	if err != nil {
		return result, nil, err
	}
	uids := make([]string, 0)
	seenUID := make(map[string]bool)
	for _, entries := range indexed {
		for _, entry := range entries {
			if !entry.BodyComplete && entry.UID != "" && !seenUID[entry.UID] {
				seenUID[entry.UID] = true
				uids = append(uids, entry.UID)
			}
		}
	}
	sort.Slice(uids, func(i, j int) bool { return imapUIDNumber(uids[i]) < imapUIDNumber(uids[j]) })
	fetched := make(map[string]protocol.ICloudSyncedMessage)
	if len(uids) > 0 {
		fetched, err = backend.FetchIMAP(ctx, imapState, uids)
		if err != nil {
			return result, nil, err
		}
		for _, uid := range uids {
			if _, found := fetched[uid]; !found {
				return result, nil, errors.New("IMAP 历史邮件正文未完整返回，稍后将继续补扫")
			}
		}
	}
	indexUpdates := make([]store.MailIndexEntry, 0, len(fetched))
	updatedUIDs := make(map[string]bool)
	for mailboxID, entries := range indexed {
		for _, entry := range entries {
			message := indexedEntryMessage(entry)
			if fetchedMessage, found := fetched[entry.UID]; found {
				message = fetchedMessage
				message.Recipients = append([]string(nil), entry.Recipients...)
				if !updatedUIDs[entry.UID] {
					indexUpdates = append(indexUpdates, storeIndexEntryFromMessage(sourceKey, uidValidity, message, entry.Recipients, true))
					updatedUIDs[entry.UID] = true
				}
			}
			result.MessagesByMailbox[mailboxID] = append(result.MessagesByMailbox[mailboxID], message)
		}
	}
	for mailboxID := range result.MessagesByMailbox {
		sort.SliceStable(result.MessagesByMailbox[mailboxID], func(i, j int) bool {
			return result.MessagesByMailbox[mailboxID][i].ReceivedAt.After(result.MessagesByMailbox[mailboxID][j].ReceivedAt)
		})
	}
	result.Matched = countProtocolMessages(result.MessagesByMailbox)
	return result, indexUpdates, nil
}

func indexedEntryMessage(entry store.MailIndexEntry) protocol.ICloudSyncedMessage {
	return protocol.ICloudSyncedMessage{
		RemoteID: entry.RemoteID, RemoteIDs: append([]string(nil), entry.RemoteIDs...), CanonicalID: entry.CanonicalID,
		Source: entry.Source, UID: entry.UID, Subject: entry.Subject, From: entry.From, Body: entry.Body,
		HTMLBody: entry.HTMLBody, ContentType: entry.ContentType, ReceivedAt: entry.ReceivedAt,
		Recipients: append([]string(nil), entry.Recipients...),
	}
}

func storeIndexEntries(sourceKey, uidValidity string, entries []protocol.MailIndexEntry) []store.MailIndexEntry {
	out := make([]store.MailIndexEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, store.MailIndexEntry{
			SourceKey: sourceKey, Folder: "INBOX", UIDValidity: uidValidity, UID: entry.UID,
			RemoteID: entry.RemoteID, RemoteIDs: entry.RemoteIDs, CanonicalID: entry.CanonicalID,
			Recipients: entry.Recipients, Source: entry.Source, Subject: entry.Subject, From: entry.From,
			Body: entry.Body, HTMLBody: entry.HTMLBody, ContentType: entry.ContentType,
			ReceivedAt: entry.ReceivedAt, BodyComplete: entry.BodyComplete, IndexedAt: time.Now(),
		})
	}
	return out
}

func storeIndexEntryFromMessage(sourceKey, uidValidity string, message protocol.ICloudSyncedMessage, recipients []string, complete bool) store.MailIndexEntry {
	return store.MailIndexEntry{
		SourceKey: sourceKey, Folder: "INBOX", UIDValidity: uidValidity, UID: message.UID,
		RemoteID: message.RemoteID, RemoteIDs: message.RemoteIDs, CanonicalID: message.CanonicalID,
		Recipients: recipients, Source: message.Source, Subject: message.Subject, From: message.From,
		Body: message.Body, HTMLBody: message.HTMLBody, ContentType: message.ContentType,
		ReceivedAt: message.ReceivedAt, BodyComplete: complete, IndexedAt: time.Now(),
	}
}

func mailboxHistoryCommits(mailboxes []domain.Mailbox, sourceKey, uidValidity, throughUID string, completedAt time.Time) []store.MailboxHistoryState {
	out := make([]store.MailboxHistoryState, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		out = append(out, store.MailboxHistoryState{
			MailboxID: mailbox.ID, SourceKey: sourceKey, Email: mailbox.Email, UIDValidity: uidValidity,
			HistoryThroughUID: throughUID, Complete: true, CompletedAt: completedAt, UpdatedAt: completedAt,
		})
	}
	return out
}

func countProtocolMessages(messages map[string][]protocol.ICloudSyncedMessage) int {
	total := 0
	for _, items := range messages {
		total += len(items)
	}
	return total
}

func imapUIDNumber(value string) int64 {
	value = strings.TrimSpace(strings.TrimPrefix(value, "imap:"))
	var uid int64
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0
		}
		uid = uid*10 + int64(char-'0')
	}
	return uid
}
