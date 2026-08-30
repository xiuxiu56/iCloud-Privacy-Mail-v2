package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

var domainNamePattern = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+)$`)

func normalizeDomainMailSettings(settings *domain.DomainMailSettings) {
	defaults := domain.DefaultDomainMailSettings()
	if settings.MatchMode != domain.DomainMatchRegisteredOnly && settings.MatchMode != domain.DomainMatchCatchAll {
		settings.MatchMode = defaults.MatchMode
	}
}

func normalizeDomainName(value string) string {
	return strings.TrimSuffix(strings.TrimLeft(strings.ToLower(strings.TrimSpace(value)), "@"), ".")
}

func normalizeMailboxKind(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return domain.MailboxKindICloudHME
	}
	return value
}

func sanitizeDomainMailRoute(route domain.DomainMailRoute) domain.DomainMailRoute {
	route.IMAPPasswordConfigured = strings.TrimSpace(route.IMAPPassword) != ""
	route.IMAPPassword = ""
	return route
}

func sanitizeMailboxKind(mailbox domain.Mailbox) domain.Mailbox {
	mailbox.MailboxKind = normalizeMailboxKind(mailbox.MailboxKind)
	return mailbox
}

func (s *Store) DomainMailSettings() domain.DomainMailSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	settings := domain.DefaultDomainMailSettings()
	_, _ = s.readEntity("domain_mail_settings", "system", &settings)
	normalizeDomainMailSettings(&settings)
	return settings
}

func (s *Store) DomainMailRoutes() []domain.DomainMailRoute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var routes []domain.DomainMailRoute
	_ = s.loadEntities("domain_mail_routes", `lower(json_extract(data_json, '$.domain'))`, &routes)
	for index := range routes {
		routes[index] = sanitizeDomainMailRoute(routes[index])
	}
	return routes
}

func (s *Store) FindDomainMailRoute(id string) (domain.DomainMailRoute, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var route domain.DomainMailRoute
	found, err := s.readEntity("domain_mail_routes", strings.TrimSpace(id), &route)
	return sanitizeDomainMailRoute(route), found && err == nil
}

// DomainMailRouteForSync 仅供后端收件服务读取已解密的标准 IMAP 登录态。
func (s *Store) DomainMailRouteForSync(id string) (domain.DomainMailRoute, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var route domain.DomainMailRoute
	found, err := s.readEntity("domain_mail_routes", strings.TrimSpace(id), &route)
	return route, found && err == nil
}

func (s *Store) FindDomainMailRouteByDomain(value string) (domain.DomainMailRoute, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var data []byte
	err := s.db.QueryRow(`SELECT data_json FROM domain_mail_routes WHERE lower(json_extract(data_json, '$.domain')) = ? LIMIT 1`, normalizeDomainName(value)).Scan(&data)
	if err != nil {
		return domain.DomainMailRoute{}, false
	}
	var route domain.DomainMailRoute
	if s.decodeEntity("domain_mail_routes", data, &route) != nil {
		return domain.DomainMailRoute{}, false
	}
	return sanitizeDomainMailRoute(route), true
}

// SaveDomainMailConfig 在一个事务中保存全局设置和全部域名接收链路。
func (s *Store) SaveDomainMailConfig(settings domain.DomainMailSettings, routes []domain.DomainMailRoute) (domain.DomainMailSettings, []domain.DomainMailRoute, error) {
	normalizeDomainMailSettings(&settings)
	now := time.Now()
	settings.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.DomainMailSettings{}, nil, err
	}
	existing, err := s.domainMailRoutesTx(tx)
	if err != nil {
		_ = tx.Rollback()
		return domain.DomainMailSettings{}, nil, err
	}
	existingByID := make(map[string]domain.DomainMailRoute, len(existing))
	existingByDomain := make(map[string]domain.DomainMailRoute, len(existing))
	for _, route := range existing {
		existingByID[route.ID] = route
		existingByDomain[normalizeDomainName(route.Domain)] = route
	}
	seenDomains := make(map[string]bool, len(routes))
	keptIDs := make(map[string]bool, len(routes))
	out := make([]domain.DomainMailRoute, 0, len(routes))
	changes := make([]Change, 0, len(routes)+2)
	for _, route := range routes {
		route.Domain = normalizeDomainName(route.Domain)
		if !domainNamePattern.MatchString(route.Domain) {
			_ = tx.Rollback()
			return domain.DomainMailSettings{}, nil, fmt.Errorf("接收域名格式不正确：%s", route.Domain)
		}
		if seenDomains[route.Domain] {
			_ = tx.Rollback()
			return domain.DomainMailSettings{}, nil, fmt.Errorf("接收域名重复：%s", route.Domain)
		}
		seenDomains[route.Domain] = true
		previous, found := existingByID[strings.TrimSpace(route.ID)]
		if !found {
			previous, found = existingByDomain[route.Domain]
		}
		if found {
			route.ID = previous.ID
			route.CreatedAt = previous.CreatedAt
			if strings.TrimSpace(route.IMAPPassword) == "" {
				route.IMAPPassword = previous.IMAPPassword
			}
		} else {
			route.ID, err = s.nextIDTx(tx, "dmr")
			if err != nil {
				_ = tx.Rollback()
				return domain.DomainMailSettings{}, nil, err
			}
			route.CreatedAt = now
		}
		route.ReceiverType = strings.ToLower(strings.TrimSpace(route.ReceiverType))
		if route.ReceiverType == "" {
			route.ReceiverType = domain.DomainReceiverAppleAccount
		}
		if route.ReceiverType != domain.DomainReceiverAppleAccount && route.ReceiverType != domain.DomainReceiverCustomIMAP {
			_ = tx.Rollback()
			return domain.DomainMailSettings{}, nil, fmt.Errorf("@%s 的收件通道不正确", route.Domain)
		}
		route.AccountID = strings.TrimSpace(route.AccountID)
		route.ReceiverLabel = strings.TrimSpace(route.ReceiverLabel)
		route.ForwardToEmail = strings.ToLower(strings.TrimSpace(route.ForwardToEmail))
		if address, parseErr := mail.ParseAddress(route.ForwardToEmail); parseErr != nil || !strings.EqualFold(strings.TrimSpace(address.Address), route.ForwardToEmail) {
			_ = tx.Rollback()
			return domain.DomainMailSettings{}, nil, fmt.Errorf("@%s 的接收邮箱格式不正确", route.Domain)
		}
		if route.ReceiverType == domain.DomainReceiverAppleAccount {
			if route.AccountID == "" {
				_ = tx.Rollback()
				return domain.DomainMailSettings{}, nil, fmt.Errorf("请为 @%s 选择 iCloud IMAP 账号", route.Domain)
			}
			var accountData []byte
			if scanErr := tx.QueryRow(`SELECT data_json FROM apple_accounts WHERE id = ?`, route.AccountID).Scan(&accountData); scanErr != nil {
				_ = tx.Rollback()
				return domain.DomainMailSettings{}, nil, fmt.Errorf("@%s 绑定的 Apple 账号不存在", route.Domain)
			}
			route.IMAPHost, route.IMAPUsername, route.IMAPPassword = "", "", ""
			route.IMAPPort, route.IMAPTLS = 0, true
		} else {
			route.AccountID = ""
			route.IMAPHost = strings.ToLower(strings.TrimSpace(route.IMAPHost))
			route.IMAPUsername = strings.TrimSpace(route.IMAPUsername)
			if route.IMAPPort <= 0 || route.IMAPPort > 65535 {
				route.IMAPPort = 993
			}
			if route.IMAPHost == "" || route.IMAPUsername == "" || strings.TrimSpace(route.IMAPPassword) == "" {
				_ = tx.Rollback()
				return domain.DomainMailSettings{}, nil, fmt.Errorf("请完整填写 @%s 的标准 IMAP 参数", route.Domain)
			}
		}
		route.Enabled = true
		route.UpdatedAt = now
		change, changed, saveErr := s.upsertEntityTx(tx, "domain_mail_routes", "domain-mail-route", route.ID, route)
		if saveErr != nil {
			_ = tx.Rollback()
			return domain.DomainMailSettings{}, nil, saveErr
		}
		if changed {
			changes = append(changes, change)
		}
		mailboxRows, queryErr := tx.Query(`SELECT data_json FROM mailboxes WHERE json_extract(data_json, '$.domain_route_id') = ?`, route.ID)
		if queryErr != nil {
			_ = tx.Rollback()
			return domain.DomainMailSettings{}, nil, queryErr
		}
		boundMailboxes := make([]domain.Mailbox, 0)
		for mailboxRows.Next() {
			var data []byte
			var mailbox domain.Mailbox
			if scanErr := mailboxRows.Scan(&data); scanErr != nil {
				_ = mailboxRows.Close()
				_ = tx.Rollback()
				return domain.DomainMailSettings{}, nil, scanErr
			}
			if decodeErr := s.decodeEntity("mailboxes", data, &mailbox); decodeErr != nil {
				_ = mailboxRows.Close()
				_ = tx.Rollback()
				return domain.DomainMailSettings{}, nil, decodeErr
			}
			boundMailboxes = append(boundMailboxes, mailbox)
		}
		if rowsErr := mailboxRows.Close(); rowsErr != nil {
			_ = tx.Rollback()
			return domain.DomainMailSettings{}, nil, rowsErr
		}
		for _, mailbox := range boundMailboxes {
			if mailbox.AccountID == route.AccountID && mailbox.ForwardToEmail == route.ForwardToEmail {
				continue
			}
			mailbox.AccountID = route.AccountID
			mailbox.ForwardToEmail = route.ForwardToEmail
			mailbox.UpdatedAt = now
			mailboxChange, mailboxChanged, updateErr := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
			if updateErr != nil {
				_ = tx.Rollback()
				return domain.DomainMailSettings{}, nil, updateErr
			}
			if mailboxChanged {
				changes = append(changes, mailboxChange)
			}
		}
		keptIDs[route.ID] = true
		out = append(out, sanitizeDomainMailRoute(route))
	}
	for _, route := range existing {
		if keptIDs[route.ID] {
			continue
		}
		change, changed, deleteErr := s.deleteEntityTx(tx, "domain_mail_routes", "domain-mail-route", route.ID)
		if deleteErr != nil {
			_ = tx.Rollback()
			return domain.DomainMailSettings{}, nil, deleteErr
		}
		if changed {
			changes = append(changes, change)
		}
	}
	settingsChange, changed, err := s.upsertEntityTx(tx, "domain_mail_settings", "domain-mail-settings", "system", settings)
	if err != nil {
		_ = tx.Rollback()
		return domain.DomainMailSettings{}, nil, err
	}
	if changed {
		changes = append(changes, settingsChange)
	}
	eventChange, err := s.appendEventTx(tx, "info", "settings", fmt.Sprintf("已保存域名邮箱设置：%d 个接收域名", len(out)))
	if err != nil {
		_ = tx.Rollback()
		return domain.DomainMailSettings{}, nil, err
	}
	changes = append(changes, eventChange)
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return settings, out, s.commitTx(tx, changes)
}

func (s *Store) domainMailRoutesTx(tx *sql.Tx) ([]domain.DomainMailRoute, error) {
	rows, err := tx.Query(`SELECT data_json FROM domain_mail_routes ORDER BY lower(json_extract(data_json, '$.domain'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []domain.DomainMailRoute
	for rows.Next() {
		var data []byte
		var route domain.DomainMailRoute
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := s.decodeEntity("domain_mail_routes", data, &route); err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (s *Store) DomainMailboxes() []domain.Mailbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT data_json FROM mailboxes WHERE json_extract(data_json, '$.mailbox_kind') = ? ORDER BY json_extract(data_json, '$.created_at') DESC, id DESC`, domain.MailboxKindDomainForward)
	if err != nil {
		return nil
	}
	defer rows.Close()
	items := []domain.Mailbox{}
	for rows.Next() {
		var data []byte
		var mailbox domain.Mailbox
		if rows.Scan(&data) == nil && s.decodeEntity("mailboxes", data, &mailbox) == nil {
			mailbox.APIToken = ""
			items = append(items, sanitizeMailboxKind(mailbox))
		}
	}
	return items
}

// CreateDomainMailboxes 把已生成的域名地址写入统一邮箱池，使取码、邮件与租约接口直接复用现有实现。
func (s *Store) CreateDomainMailboxes(routeID string, emails []string, label, note string, apiActive bool) ([]domain.Mailbox, error) {
	routeID = strings.TrimSpace(routeID)
	if len(emails) == 0 || len(emails) > 500 {
		return nil, errors.New("域名邮箱数量必须是 1-500")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	var route domain.DomainMailRoute
	if found, readErr := s.readEntityTx(tx, "domain_mail_routes", routeID, &route); readErr != nil || !found {
		_ = tx.Rollback()
		if readErr != nil {
			return nil, readErr
		}
		return nil, errors.New("域名收件路由不存在")
	}
	var admin domain.Admin
	var adminData []byte
	if tx.QueryRow(`SELECT data_json FROM admins LIMIT 1`).Scan(&adminData) == nil {
		_ = s.decodeEntity("admins", adminData, &admin)
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "域名邮箱"
	}
	note = strings.TrimSpace(note)
	now := time.Now()
	created := make([]domain.Mailbox, 0, len(emails))
	changes := make([]Change, 0, len(emails)+1)
	seen := make(map[string]bool, len(emails))
	for _, rawEmail := range emails {
		email := strings.ToLower(strings.TrimSpace(rawEmail))
		address, parseErr := mail.ParseAddress(email)
		if parseErr != nil || !strings.EqualFold(strings.TrimSpace(address.Address), email) || !strings.HasSuffix(email, "@"+route.Domain) {
			_ = tx.Rollback()
			return nil, fmt.Errorf("域名邮箱格式或接收域名不正确：%s", rawEmail)
		}
		if seen[email] {
			_ = tx.Rollback()
			return nil, fmt.Errorf("域名邮箱重复：%s", email)
		}
		seen[email] = true
		var exists int
		if scanErr := tx.QueryRow(`SELECT 1 FROM mailboxes WHERE lower(json_extract(data_json, '$.email')) = ? LIMIT 1`, email).Scan(&exists); scanErr == nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("邮箱已经存在：%s", email)
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			_ = tx.Rollback()
			return nil, scanErr
		}
		id, idErr := s.nextIDTx(tx, "dm")
		if idErr != nil {
			_ = tx.Rollback()
			return nil, idErr
		}
		token, tokenErr := randomAPIToken(24)
		if tokenErr != nil {
			_ = tx.Rollback()
			return nil, tokenErr
		}
		mailbox := domain.Mailbox{
			ID: id, OwnerID: admin.ID, AccountID: route.AccountID, MailboxKind: domain.MailboxKindDomainForward,
			DomainRouteID: route.ID, RemoteOrigin: "domain_forward", Label: label, Email: email,
			ForwardToEmail: route.ForwardToEmail, APIToken: token, APIActive: apiActive, ICloudActive: true,
			Status: domain.StatusAvailable, Note: note, CreatedAt: now, UpdatedAt: now,
		}
		change, _, saveErr := s.upsertEntityTx(tx, "mailboxes", "mailbox", mailbox.ID, mailbox)
		if saveErr != nil {
			_ = tx.Rollback()
			return nil, saveErr
		}
		changes = append(changes, change)
		mailbox.APIToken = ""
		created = append(created, mailbox)
	}
	eventChange, err := s.appendEventTx(tx, "info", "mailbox", fmt.Sprintf("已生成并登记域名邮箱：%d 个", len(created)))
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	changes = append(changes, eventChange)
	return created, s.commitTx(tx, changes)
}
