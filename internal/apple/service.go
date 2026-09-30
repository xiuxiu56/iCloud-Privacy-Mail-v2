package apple

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

type Service struct {
	cfg              config.Config
	store            *store.Store
	auth             *protocol.AuthFacade
	client           *protocol.ICloudClient
	validator        *protocol.ICloudSessionValidator
	checkApple       func(context.Context, domain.ICloudSession) (domain.ICloudSession, error)
	checkWeb         func(context.Context, domain.ICloudSession, string) (domain.ICloudSession, error)
	checkIMAP        func(context.Context, string, string) error
	pendingMu        sync.Mutex
	pendingPasswords map[string]pendingPassword
}

type pendingPassword struct {
	value     string
	expiresAt time.Time
}

type LoginRequest struct {
	Flow            protocol.AuthFlow
	AppleID         string
	Password        string
	TwoFactorMethod string
}

type LoginResult struct {
	PendingID string         `json:"pending_id,omitempty"`
	Needs2FA  bool           `json:"needs_2fa"`
	Message   string         `json:"message"`
	AppleID   string         `json:"apple_id"`
	ExpiresAt time.Time      `json:"expires_at,omitempty"`
	Account   AccountSummary `json:"account,omitempty"`
}

type LoginStateSummary struct {
	Kind              string    `json:"kind"`
	Saved             bool      `json:"saved"`
	LastCheckedAt     time.Time `json:"last_checked_at,omitempty"`
	LastCheckOK       bool      `json:"last_check_ok"`
	LastStatusMessage string    `json:"last_status_message,omitempty"`
	ManageExpiresAt   time.Time `json:"manage_expires_at,omitempty"`
	IMAPEmail         string    `json:"imap_email,omitempty"`
}

type AccountSummary struct {
	domain.AppleAccount
	LoginStates     []LoginStateSummary `json:"login_states"`
	IMAPEmail       string              `json:"imap_email,omitempty"`
	IMAPAppPassword string              `json:"imap_app_password,omitempty"`
}

func NewService(cfg config.Config, state *store.Store) *Service {
	service := &Service{
		cfg:              cfg,
		store:            state,
		auth:             protocol.NewAuthFacade(),
		client:           protocol.NewICloudClient(),
		validator:        protocol.NewICloudSessionValidator(),
		pendingPasswords: make(map[string]pendingPassword),
	}
	service.checkApple = service.client.CheckAppleAccountManageSession
	service.checkWeb = service.validator.ValidateSession
	service.checkIMAP = protocol.CheckICloudIMAPLogin
	return service
}

func (s *Service) StartLogin(ctx context.Context, request LoginRequest) (LoginResult, error) {
	result, err := s.auth.Start(ctx, request.Flow, request.AppleID, request.Password, s.cfg.ICloudDefaultHost, s.cfg.ICloudClientID, request.TwoFactorMethod)
	if err != nil {
		return LoginResult{}, err
	}
	out := LoginResult{
		PendingID: result.PendingID,
		Needs2FA:  result.Needs2FA,
		Message:   result.Message,
		AppleID:   result.AppleID,
		ExpiresAt: result.ExpiresAt,
	}
	if result.Needs2FA {
		s.rememberPendingPassword(result.PendingID, request.Password, result.ExpiresAt)
	} else {
		saved, err := s.store.SaveICloudSessionWithPassword(result.Session, request.Password)
		if err != nil {
			return LoginResult{}, err
		}
		out.Account = s.summaryForSession(saved)
	}
	return out, nil
}

func (s *Service) Submit2FA(ctx context.Context, pendingID, code string, phoneNumber json.RawMessage) (LoginResult, error) {
	password := s.pendingPassword(pendingID)
	session, err := s.auth.Submit2FA(ctx, pendingID, code, phoneNumber)
	if err != nil {
		return LoginResult{}, err
	}
	saved, err := s.store.SaveICloudSessionWithPassword(session, password)
	if err != nil {
		return LoginResult{}, err
	}
	s.forgetPendingPassword(pendingID)
	return LoginResult{
		Needs2FA: false,
		Message:  "Apple 登录和 2FA 已完成",
		AppleID:  saved.AppleID,
		Account:  s.summaryForSession(saved),
	}, nil
}

func (s *Service) rememberPendingPassword(pendingID, password string, expiresAt time.Time) {
	pendingID = strings.TrimSpace(pendingID)
	if pendingID == "" || strings.TrimSpace(password) == "" {
		return
	}
	now := time.Now()
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, item := range s.pendingPasswords {
		if !item.expiresAt.IsZero() && now.After(item.expiresAt) {
			delete(s.pendingPasswords, id)
		}
	}
	s.pendingPasswords[pendingID] = pendingPassword{value: password, expiresAt: expiresAt}
}

func (s *Service) pendingPassword(pendingID string) string {
	pendingID = strings.TrimSpace(pendingID)
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	item, ok := s.pendingPasswords[pendingID]
	if !ok || (!item.expiresAt.IsZero() && time.Now().After(item.expiresAt)) {
		delete(s.pendingPasswords, pendingID)
		return ""
	}
	return item.value
}

func (s *Service) forgetPendingPassword(pendingID string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	delete(s.pendingPasswords, strings.TrimSpace(pendingID))
}

func (s *Service) Accounts() []AccountSummary {
	accounts := s.store.AppleAccounts()
	sessions := s.store.ICloudSessions()
	byAccount := make(map[string]domain.ICloudSession, len(sessions))
	for _, session := range sessions {
		byAccount[session.AccountID] = session
	}
	out := make([]AccountSummary, 0, len(accounts))
	for _, account := range accounts {
		account.Password = ""
		summary := AccountSummary{AppleAccount: account, LoginStates: []LoginStateSummary{}}
		if session, ok := byAccount[account.ID]; ok {
			summary.LoginStates = publicLoginStates(session)
		}
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (s *Service) Account(accountID string) (AccountSummary, error) {
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return AccountSummary{}, errors.New("Apple 账号不存在")
	}
	summary := AccountSummary{AppleAccount: account, LoginStates: []LoginStateSummary{}}
	if session, ok := s.store.ICloudSessionByAccountID(accountID); ok {
		summary = accountSummaryWithSession(account, session)
	}
	return summary, nil
}

func (s *Service) Check(ctx context.Context, accountID string) (AccountSummary, error) {
	return s.CheckKind(ctx, accountID, "")
}

// CheckKind 只检测指定通道；kind 为空时沿用全部通道检测。
func (s *Service) CheckKind(ctx context.Context, accountID, kind string) (AccountSummary, error) {
	kind = strings.TrimSpace(kind)
	if kind != "" && kind != domain.LoginStateAppleAccount && kind != domain.LoginStateICloudWeb && kind != domain.LoginStateICloudIMAP {
		return AccountSummary{}, errors.New("登录通道不正确")
	}
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		return AccountSummary{}, errors.New("Apple 账号登录态不存在")
	}
	checkedAny := false
	var lastErr error
	if _, ok := protocol.LoginStateForKind(session, domain.LoginStateAppleAccount); ok && (kind == "" || kind == domain.LoginStateAppleAccount) {
		checkedAny = true
		updated, err := s.checkApple(ctx, session)
		if err != nil {
			lastErr = err
			markLoginState(&session, domain.LoginStateAppleAccount, false, err.Error())
		} else {
			session = updated
			markLoginState(&session, domain.LoginStateAppleAccount, true, "Apple Account 登录态正常")
		}
	}
	_, webSaved := protocol.LoginStateForKind(session, domain.LoginStateICloudWeb)
	if (webSaved || len(session.Cookies) > 0) && (kind == "" || kind == domain.LoginStateICloudWeb) {
		checkedAny = true
		updated, err := s.checkWeb(ctx, session, s.cfg.ICloudDefaultHost)
		session = updated
		if err != nil {
			lastErr = err
			markLoginState(&session, domain.LoginStateICloudWeb, false, err.Error())
		} else {
			markLoginState(&session, domain.LoginStateICloudWeb, true, "iCloud Web 登录态正常")
		}
	}
	if state, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP); ok && (kind == "" || kind == domain.LoginStateICloudIMAP) {
		checkedAny = true
		err := s.checkIMAP(ctx, state.IMAPEmail, state.IMAPAppPassword)
		if err != nil {
			lastErr = err
			markLoginState(&session, domain.LoginStateICloudIMAP, false, err.Error())
		} else {
			markLoginState(&session, domain.LoginStateICloudIMAP, true, "IMAP 取码登录正常")
		}
	}
	if !checkedAny {
		return AccountSummary{}, errors.New("该账号没有可检测的登录态或指定通道未配置")
	}
	saved, err := s.store.SaveICloudSession(session)
	if err != nil {
		return AccountSummary{}, err
	}
	summary := s.summaryForSession(saved)
	if lastErr != nil {
		return summary, lastErr
	}
	return summary, nil
}

func (s *Service) SaveIMAP(ctx context.Context, accountID, email, appPassword string) (AccountSummary, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	appPassword = strings.TrimSpace(appPassword)
	if err := protocol.CheckICloudIMAPLogin(ctx, email, appPassword); err != nil {
		return AccountSummary{}, err
	}
	session, ok := s.store.ICloudSessionByAccountID(accountID)
	if !ok {
		account, exists := s.store.FindAppleAccount(accountID)
		if !exists {
			return AccountSummary{}, errors.New("Apple 账号不存在")
		}
		session = domain.ICloudSession{AccountID: account.ID, AppleID: account.AppleID, SavedAt: time.Now()}
	}
	state := domain.LoginState{
		Kind:              domain.LoginStateICloudIMAP,
		Host:              "imap.mail.me.com",
		Origin:            "imaps://imap.mail.me.com",
		SavedAt:           time.Now(),
		IMAPEmail:         email,
		IMAPUsername:      email,
		IMAPHost:          "imap.mail.me.com",
		IMAPPort:          993,
		IMAPAppPassword:   appPassword,
		LastCheckedAt:     time.Now(),
		LastCheckOK:       true,
		LastStatusMessage: "IMAP 取码登录正常",
	}
	session = protocol.WithLoginState(session, state)
	saved, err := s.store.SaveICloudSession(session)
	if err != nil {
		return AccountSummary{}, err
	}
	return s.summaryForSession(saved), nil
}

func (s *Service) KeepAliveState(ctx context.Context, state domain.LoginState) (domain.LoginState, error) {
	return s.client.KeepAliveAppleAccountManageState(ctx, state)
}

func (s *Service) summaryForSession(session domain.ICloudSession) AccountSummary {
	account, _ := s.store.FindAppleAccount(session.AccountID)
	return accountSummaryWithSession(account, session)
}

// accountSummaryWithSession 生成单账号操作响应，并附带已解密的 IMAP 凭据。
func accountSummaryWithSession(account domain.AppleAccount, session domain.ICloudSession) AccountSummary {
	summary := AccountSummary{AppleAccount: account, LoginStates: publicLoginStates(session)}
	if state, ok := protocol.LoginStateForKind(session, domain.LoginStateICloudIMAP); ok {
		summary.IMAPEmail = state.IMAPEmail
		summary.IMAPAppPassword = state.IMAPAppPassword
	}
	return summary
}

func publicLoginStates(session domain.ICloudSession) []LoginStateSummary {
	out := make([]LoginStateSummary, 0, len(session.LoginStates)+1)
	seenWeb := false
	for _, state := range session.LoginStates {
		if state.Kind == domain.LoginStateICloudWeb {
			seenWeb = true
		}
		out = append(out, LoginStateSummary{
			Kind:              state.Kind,
			Saved:             loginStateSaved(state),
			LastCheckedAt:     state.LastCheckedAt,
			LastCheckOK:       state.LastCheckOK,
			LastStatusMessage: state.LastStatusMessage,
			ManageExpiresAt:   state.ManageExpiresAt,
			IMAPEmail:         state.IMAPEmail,
		})
	}
	if !seenWeb && len(session.Cookies) > 0 {
		out = append(out, LoginStateSummary{Kind: domain.LoginStateICloudWeb, Saved: true, LastCheckedAt: session.LastCheckedAt, LastCheckOK: session.LastCheckOK, LastStatusMessage: session.LastStatusMessage})
	}
	return out
}

func loginStateSaved(state domain.LoginState) bool {
	switch state.Kind {
	case domain.LoginStateAppleAccount:
		return strings.TrimSpace(state.Scnt) != "" && len(state.Cookies) > 0
	case domain.LoginStateICloudIMAP:
		return strings.TrimSpace(state.IMAPEmail) != "" && strings.TrimSpace(state.IMAPAppPassword) != ""
	default:
		return len(state.Cookies) > 0
	}
}

func markLoginState(session *domain.ICloudSession, kind string, ok bool, message string) {
	state, exists := protocol.LoginStateForKind(*session, kind)
	if !exists {
		state.Kind = kind
		if kind == domain.LoginStateICloudWeb {
			state.Cookies = append([]domain.SessionCookie(nil), session.Cookies...)
		}
	}
	state.LastCheckedAt = time.Now()
	state.LastCheckOK = ok
	state.LastStatusMessage = message
	*session = protocol.WithLoginState(*session, state)
}
