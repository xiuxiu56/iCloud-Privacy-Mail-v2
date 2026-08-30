package httpapi

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"icloud-privacy-mail-v2/internal/domain"
)

var domainMailboxPrefixPattern = regexp.MustCompile(`^[a-z0-9._+-]{0,55}$`)

type domainMailConfigRequest struct {
	domain.DomainMailSettings
	Routes []domain.DomainMailRoute `json:"routes"`
}

func (s *Server) handleDomainMailSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"settings": s.store.DomainMailSettings(),
		"routes":   s.store.DomainMailRoutes(),
	}})
}

func (s *Server) handleSaveDomainMailSettings(w http.ResponseWriter, r *http.Request) {
	var body domainMailConfigRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	for index := range body.Routes {
		route := &body.Routes[index]
		if route.ReceiverType != domain.DomainReceiverAppleAccount {
			continue
		}
		receiverEmail, err := s.domainReceiverEmailForAccount(route.AccountID, route.ForwardToEmail)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_receiver_account", fmt.Sprintf("@%s：%s", route.Domain, err.Error()))
			return
		}
		route.ForwardToEmail = receiverEmail
	}
	settings, routes, err := s.store.SaveDomainMailConfig(body.DomainMailSettings, body.Routes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "domain_mail_settings_invalid", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"settings": settings,
		"routes":   routes,
	}})
}

func (s *Server) domainReceiverEmailForAccount(accountID, requestedEmail string) (string, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "", errors.New("请选择 iCloud IMAP 账号")
	}
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return "", errors.New("Apple 账号不存在")
	}
	requestedEmail = strings.ToLower(strings.TrimSpace(requestedEmail))
	allowed := make(map[string]bool)
	if session, found := s.store.ICloudSessionByAccountID(accountID); found {
		for _, state := range session.LoginStates {
			if state.Kind != domain.LoginStateICloudIMAP {
				continue
			}
			if email := strings.ToLower(strings.TrimSpace(firstNonEmptyHTTP(state.IMAPEmail, state.IMAPUsername))); email != "" {
				allowed[email] = true
			}
		}
	}
	if email := strings.ToLower(strings.TrimSpace(account.AppleID)); email != "" {
		allowed[email] = true
	}
	for _, mailbox := range s.store.AllMailboxes() {
		if mailbox.AccountID == accountID && publicMailboxKind(mailbox) == domain.MailboxKindICloudHME {
			allowed[strings.ToLower(strings.TrimSpace(mailbox.Email))] = true
		}
	}
	if requestedEmail != "" {
		if allowed[requestedEmail] {
			return requestedEmail, nil
		}
		return "", errors.New("接收邮箱不属于所选 Apple 账号")
	}
	for email := range allowed {
		return email, nil
	}
	return "", errors.New("该账号没有可用的 IMAP 接收邮箱")
}

func firstNonEmptyHTTP(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (s *Server) handleDomainMailboxes(w http.ResponseWriter, r *http.Request) {
	items := s.store.DomainMailboxes()
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	domainFilter := strings.ToLower(strings.Trim(strings.TrimSpace(r.URL.Query().Get("domain")), "@"))
	filtered := make([]domain.Mailbox, 0, len(items))
	for _, mailbox := range items {
		separator := strings.LastIndexByte(mailbox.Email, '@')
		mailboxDomain := ""
		if separator >= 0 {
			mailboxDomain = strings.ToLower(mailbox.Email[separator+1:])
		}
		if domainFilter != "" && mailboxDomain != domainFilter {
			continue
		}
		if status != "" && mailbox.Status != status {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(mailbox.Email+" "+mailbox.Label+" "+mailbox.Note), query) {
			continue
		}
		filtered = append(filtered, mailbox)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"items": filtered,
		"total": len(filtered),
	}})
}

func (s *Server) handleGenerateDomainMailboxes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RouteID      string   `json:"route_id"`
		Domain       string   `json:"domain"`
		Emails       []string `json:"emails"`
		Prefix       string   `json:"prefix"`
		Mode         string   `json:"mode"`
		Count        int      `json:"count"`
		Start        *int     `json:"start"`
		RandomLength int      `json:"random_length"`
		Label        string   `json:"label"`
		Note         string   `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	route, ok := s.store.FindDomainMailRoute(strings.TrimSpace(body.RouteID))
	if !ok && strings.TrimSpace(body.Domain) != "" {
		route, ok = s.store.FindDomainMailRouteByDomain(body.Domain)
	}
	if !ok {
		writeError(w, http.StatusNotFound, "domain_route_not_found", "域名收件路由不存在")
		return
	}
	emails := append([]string(nil), body.Emails...)
	if len(emails) == 0 {
		generated, err := generateDomainMailboxEmails(route.Domain, body.Mode, body.Prefix, body.Count, body.Start, body.RandomLength)
		if err != nil {
			writeError(w, http.StatusBadRequest, "domain_mailbox_generate_invalid", err.Error())
			return
		}
		emails = generated
	}
	settings := s.store.DomainMailSettings()
	items, err := s.store.CreateDomainMailboxes(route.ID, emails, body.Label, body.Note, settings.DefaultAPIActive)
	if err != nil {
		writeError(w, http.StatusBadRequest, "domain_mailbox_create_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"success": true, "data": map[string]any{"items": items, "count": len(items)}})
}

func generateDomainMailboxEmails(domainName, mode, prefix string, count int, start *int, randomLength int) ([]string, error) {
	domainName = strings.ToLower(strings.Trim(strings.TrimSpace(domainName), "@"))
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "sequence"
	}
	if !domainMailboxPrefixPattern.MatchString(prefix) {
		return nil, errors.New("邮箱前缀仅支持字母、数字、点、下划线、加号和短横线，最长 55 个字符")
	}
	switch mode {
	case "sequence":
		if prefix == "" {
			return nil, errors.New("指定前缀模式需要填写邮箱前缀")
		}
		if start == nil {
			return []string{prefix + "@" + domainName}, nil
		}
		if count < 1 || count > 500 {
			return nil, errors.New("生成数量需要在 1-500 之间")
		}
		if *start < 0 || *start > 999999999 {
			return nil, errors.New("开始编号需要在 0-999999999 之间")
		}
		emails := make([]string, 0, count)
		for index := 0; index < count; index++ {
			local := fmt.Sprintf("%s%d", prefix, *start+index)
			if len(local) > 64 {
				return nil, errors.New("邮箱前缀与编号总长度不能超过 64")
			}
			emails = append(emails, local+"@"+domainName)
		}
		return emails, nil
	case "random":
		if count < 1 || count > 500 {
			return nil, errors.New("生成数量需要在 1-500 之间")
		}
		if randomLength < 4 || randomLength > 32 {
			return nil, errors.New("随机字符长度需要在 4-32 之间")
		}
		if len(prefix)+randomLength > 64 {
			return nil, errors.New("邮箱前缀与随机字符总长度不能超过 64")
		}
		emails := make([]string, 0, count)
		seen := make(map[string]bool, count)
		for len(emails) < count {
			local, err := randomDomainMailboxLocalPart(randomLength)
			if err != nil {
				return nil, err
			}
			local = prefix + local
			if seen[local] {
				continue
			}
			seen[local] = true
			emails = append(emails, local+"@"+domainName)
		}
		return emails, nil
	default:
		return nil, errors.New("生成方式只支持 sequence 或 random")
	}
}

func randomDomainMailboxLocalPart(length int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	for index := range data {
		data[index] = alphabet[int(data[index])%len(alphabet)]
	}
	return string(data), nil
}

func (s *Server) handleSyncDomainMailboxes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MailboxIDs []string `json:"mailbox_ids"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	result, err := s.mailbox.SyncDomainMailboxes(r.Context(), body.MailboxIDs)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

func (s *Server) handleDeleteDomainMailboxMessages(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MailboxIDs []string `json:"mailbox_ids"`
		Emails     []string `json:"emails"`
		All        bool     `json:"all"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	targets := make(map[string]domain.Mailbox)
	if body.All {
		for _, mailbox := range s.store.DomainMailboxes() {
			targets[mailbox.ID] = mailbox
		}
	}
	for _, id := range body.MailboxIDs {
		if mailbox, ok := s.store.FindMailboxByID(id); ok && publicMailboxKind(mailbox) == domain.MailboxKindDomainForward {
			targets[mailbox.ID] = mailbox
		}
	}
	for _, email := range body.Emails {
		if mailbox, ok := s.store.FindMailboxByEmail(email); ok && publicMailboxKind(mailbox) == domain.MailboxKindDomainForward {
			targets[mailbox.ID] = mailbox
		}
	}
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, "domain_mailbox_empty", "没有找到要清理的域名邮箱")
		return
	}
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	result, err := s.mailbox.CleanDomainRemoteMessages(r.Context(), ids, true)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"accounts": result.Accounts, "mailboxes": result.Mailboxes, "moved_to_trash": result.MovedToTrash,
		"sync_scanned": result.SyncScanned, "verified_mailboxes": result.VerifiedMailboxes, "fallback_accounts": result.FallbackAccounts,
		"threads_scanned": result.ThreadsScanned, "cloud_messages_found": result.CloudMessagesFound,
		"destroyed": result.Destroyed, "skipped": result.Skipped, "deleted": result.LocalRemoved,
	}})
}

func (s *Server) handleDeleteDomainMailboxes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MailboxIDs []string `json:"mailbox_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	seen := make(map[string]bool)
	ids := make([]string, 0, len(body.MailboxIDs))
	for _, id := range body.MailboxIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		mailbox, ok := s.store.FindMailboxByID(id)
		if !ok || publicMailboxKind(mailbox) != domain.MailboxKindDomainForward {
			writeError(w, http.StatusBadRequest, "domain_mailbox_not_found", "选中项中包含不存在的域名邮箱")
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "domain_mailbox_empty", "请至少选择一个域名邮箱")
		return
	}
	result, err := s.mailbox.DeleteDomainMailboxesWithRemoteMessages(r.Context(), ids)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"accounts": result.Accounts, "threads_scanned": result.ThreadsScanned, "cloud_messages_found": result.CloudMessagesFound,
		"sync_scanned": result.SyncScanned, "verified_mailboxes": result.VerifiedMailboxes, "fallback_accounts": result.FallbackAccounts,
		"moved_to_trash": result.MovedToTrash, "destroyed": result.Destroyed, "local_removed": result.LocalRemoved,
		"deleted": result.DeletedMailboxes,
	}})
}

func (s *Server) handleCleanDomainMailboxRemoteMessages(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MailboxIDs []string `json:"mailbox_ids"`
		PurgeLocal *bool    `json:"purge_local"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	purgeLocal := true
	if body.PurgeLocal != nil {
		purgeLocal = *body.PurgeLocal
	}
	result, err := s.mailbox.CleanDomainRemoteMessages(r.Context(), body.MailboxIDs, purgeLocal)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

func (s *Server) handleDeleteDomainMailbox(w http.ResponseWriter, r *http.Request) {
	mailbox, ok := s.store.FindMailboxByID(r.PathValue("id"))
	if !ok || publicMailboxKind(mailbox) != domain.MailboxKindDomainForward {
		writeError(w, http.StatusNotFound, "domain_mailbox_not_found", "域名邮箱不存在")
		return
	}
	result, err := s.mailbox.DeleteDomainMailboxesWithRemoteMessages(r.Context(), []string{mailbox.ID})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}
