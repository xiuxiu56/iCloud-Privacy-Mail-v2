package domain

import (
	"encoding/json"
	"time"
)

const SchemaVersion = 6

const (
	StatusActive    = "active"
	StatusAvailable = "available"
	StatusReserved  = "reserved"
	StatusUsed      = "used"
	StatusFailed    = "failed"
	StatusDisabled  = "disabled"

	MailboxLeaseClaimed   = "claimed"
	MailboxLeaseCommitted = "committed"
	MailboxLeaseReleased  = "released"
	MailboxLeaseExpired   = "expired"

	ICloudStatusActive       = "active"
	ICloudStatusNeedLogin    = "need_login"
	ICloudStatusNeed2FA      = "need_2fa"
	ICloudStatusNoICloudPlus = "no_icloud_plus"
	ICloudStatusRateLimited  = "rate_limited"
	ICloudStatusPartial      = "partial"
	ICloudStatusFailed       = "failed"

	LoginStateICloudWeb    = "icloud_web"
	LoginStateAppleAccount = "apple_account"
	LoginStateICloudIMAP   = "icloud_imap"

	MailboxKindICloudHME     = "icloud_hme"
	MailboxKindDomainForward = "domain_forward"

	DomainReceiverAppleAccount = "apple_account"
	DomainReceiverCustomIMAP   = "custom_imap"

	DomainMatchCatchAll       = "catch_all"
	DomainMatchRegisteredOnly = "registered_only"
)

type State struct {
	SchemaVersion       int                `json:"schema_version"`
	NextID              int                `json:"next_id"`
	Admin               *Admin             `json:"admin,omitempty"`
	Sessions            []WebSession       `json:"sessions,omitempty"`
	AppleAccounts       []AppleAccount     `json:"apple_accounts,omitempty"`
	Mailboxes           []Mailbox          `json:"mailboxes,omitempty"`
	DomainMailRoutes    []DomainMailRoute  `json:"domain_mail_routes,omitempty"`
	DomainMailSettings  DomainMailSettings `json:"domain_mail_settings"`
	MailboxLeases       []MailboxLease     `json:"mailbox_leases,omitempty"`
	Messages            []Message          `json:"messages,omitempty"`
	Events              []Event            `json:"events,omitempty"`
	Settings            Settings           `json:"settings"`
	CreateSettings      CreateSettings     `json:"create_settings"`
	ICloudSessions      []ICloudSession    `json:"icloud_sessions,omitempty"`
	LegacyICloudSession json.RawMessage    `json:"legacy_icloud_session,omitempty"`
	LegacyICloudStates  []json.RawMessage  `json:"legacy_icloud_sessions,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
}

type Admin struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastLoginAt  time.Time `json:"last_login_at,omitempty"`
}

type WebSession struct {
	TokenHash  string    `json:"token_hash"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type AppleAccount struct {
	ID           string    `json:"id"`
	OwnerID      string    `json:"owner_id,omitempty"`
	Label        string    `json:"label"`
	AppleID      string    `json:"apple_id"`
	Password     string    `json:"password,omitempty"`
	Status       string    `json:"status"`
	ICloudStatus string    `json:"icloud_status"`
	Note         string    `json:"note"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Mailbox struct {
	ID                string    `json:"id"`
	OwnerID           string    `json:"owner_id,omitempty"`
	AccountID         string    `json:"account_id,omitempty"`
	AnonymousID       string    `json:"anonymous_id,omitempty"`
	RemoteOrigin      string    `json:"remote_origin,omitempty"`
	MailboxKind       string    `json:"mailbox_kind,omitempty"`
	DomainRouteID     string    `json:"domain_route_id,omitempty"`
	Label             string    `json:"label"`
	Email             string    `json:"email"`
	ForwardToEmail    string    `json:"forward_to_email,omitempty"`
	APIToken          string    `json:"api_token,omitempty"`
	APIActive         bool      `json:"api_active"`
	ICloudActive      bool      `json:"icloud_active"`
	ReceiveCount      int       `json:"receive_count"`
	Status            string    `json:"status"`
	Note              string    `json:"note"`
	ActiveLeaseID     string    `json:"active_lease_id,omitempty"`
	LastSyncAt        time.Time `json:"last_sync_at,omitempty"`
	LastSyncUID       string    `json:"last_sync_uid,omitempty"`
	LastCodeMessageID string    `json:"last_code_message_id,omitempty"`
	LastCodeAt        time.Time `json:"last_code_at,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// DomainMailSettings 保存域名邮箱功能的全局行为；具体收件链路由 DomainMailRoute 描述。
type DomainMailSettings struct {
	Enabled          bool      `json:"enabled"`
	MatchMode        string    `json:"match_mode"`
	AutoDiscover     bool      `json:"auto_discover"`
	DefaultAPIActive bool      `json:"default_api_active"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

// DomainMailRoute 把一个接收域名绑定到真正拥有收件箱访问能力的账号或标准 IMAP 登录态。
type DomainMailRoute struct {
	ID                     string    `json:"id"`
	Domain                 string    `json:"domain"`
	ReceiverType           string    `json:"receiver_type"`
	AccountID              string    `json:"account_id,omitempty"`
	ReceiverLabel          string    `json:"receiver_label,omitempty"`
	ForwardToEmail         string    `json:"forward_to_email"`
	IMAPHost               string    `json:"imap_host,omitempty"`
	IMAPPort               int       `json:"imap_port,omitempty"`
	IMAPUsername           string    `json:"imap_username,omitempty"`
	IMAPPassword           string    `json:"imap_password,omitempty"`
	IMAPPasswordConfigured bool      `json:"imap_password_configured,omitempty"`
	IMAPTLS                bool      `json:"imap_tls"`
	Enabled                bool      `json:"enabled"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// MailboxLease 记录外部调用方对邮箱的临时占用，只有提交租约才会把邮箱标记为已使用。
type MailboxLease struct {
	ID          string    `json:"id"`
	MailboxID   string    `json:"mailbox_id"`
	Email       string    `json:"email"`
	Project     string    `json:"project"`
	Purpose     string    `json:"purpose,omitempty"`
	RequestID   string    `json:"request_id,omitempty"`
	State       string    `json:"state"`
	Note        string    `json:"note,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CommittedAt time.Time `json:"committed_at,omitempty"`
	ReleasedAt  time.Time `json:"released_at,omitempty"`
	ExpiredAt   time.Time `json:"expired_at,omitempty"`
}

type Message struct {
	ID          string    `json:"id"`
	OwnerID     string    `json:"owner_id,omitempty"`
	MailboxID   string    `json:"mailbox_id"`
	RemoteID    string    `json:"remote_id,omitempty"`
	RemoteIDs   []string  `json:"remote_ids,omitempty"`
	CanonicalID string    `json:"canonical_id,omitempty"`
	Source      string    `json:"source,omitempty"`
	Subject     string    `json:"subject"`
	From        string    `json:"from"`
	Body        string    `json:"body"`
	HTMLBody    string    `json:"html_body,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	ReceivedAt  time.Time `json:"received_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type RemoteMailbox struct {
	AnonymousID    string `json:"anonymous_id,omitempty"`
	Email          string `json:"email"`
	ForwardToEmail string `json:"forward_to_email,omitempty"`
	Label          string `json:"label,omitempty"`
	Note           string `json:"note,omitempty"`
	IsActive       bool   `json:"is_active"`
	Origin         string `json:"origin,omitempty"`
}

type Event struct {
	ID        string    `json:"id"`
	Level     string    `json:"level"`
	Category  string    `json:"category"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

type Settings struct {
	EnableMailWatcher              bool   `json:"enable_mail_watcher"`
	EnableAppleKeepAlive           bool   `json:"enable_apple_keep_alive"`
	EnablePublicMailboxAPI         bool   `json:"enable_public_mailbox_api"`
	EnablePublicCodePage           bool   `json:"enable_public_code_page"`
	EnableWebCodeSync              bool   `json:"enable_web_code_sync"`
	EnableWebManualMailSync        bool   `json:"enable_web_manual_mail_sync"`
	EnableWebBackgroundMail        bool   `json:"enable_web_background_mail"`
	EnableWebRemoteMailCleanup     bool   `json:"enable_web_remote_mail_cleanup"`
	PublicAPIKey                   string `json:"public_api_key,omitempty"`
	AppleAccountModuleReady        bool   `json:"apple_account_module_ready"`
	ServerChanSendKey              string `json:"server_chan_send_key,omitempty"`
	ServerChanHideIP               bool   `json:"server_chan_hide_ip"`
	NotifyAdminLogin               bool   `json:"notify_admin_login"`
	NotifyAccountLoginStateOffline bool   `json:"notify_account_login_state_offline"`
}

type CreateSettings struct {
	OwnerID                            string    `json:"owner_id,omitempty"`
	Mode                               string    `json:"mode,omitempty"`
	Label                              string    `json:"label,omitempty"`
	Note                               string    `json:"note,omitempty"`
	AccountIDs                         []string  `json:"account_ids,omitempty"`
	CreateChannel                      string    `json:"create_channel,omitempty"`
	SchedulerCreateChannel             string    `json:"scheduler_create_channel,omitempty"`
	AppleAccountTwoFactorMethod        string    `json:"apple_account_two_factor_method,omitempty"`
	ICloudWebTwoFactorMethod           string    `json:"icloud_web_two_factor_method,omitempty"`
	SchedulerIntervalMinMinutes        int       `json:"scheduler_interval_min_minutes,omitempty"`
	SchedulerIntervalMaxMinutes        int       `json:"scheduler_interval_max_minutes,omitempty"`
	SchedulerAccountIntervalMinSeconds int       `json:"scheduler_account_interval_min_seconds,omitempty"`
	SchedulerAccountIntervalMaxSeconds int       `json:"scheduler_account_interval_max_seconds,omitempty"`
	UpdatedAt                          time.Time `json:"updated_at,omitempty"`
}

type ICloudSession struct {
	OwnerID            string          `json:"owner_id,omitempty"`
	AccountID          string          `json:"account_id,omitempty"`
	SavedAt            time.Time       `json:"saved_at"`
	AppleID            string          `json:"apple_id,omitempty"`
	DSID               string          `json:"dsid"`
	ClientID           string          `json:"client_id"`
	ClientBuildNumber  string          `json:"client_build_number"`
	MasteringNumber    string          `json:"client_mastering_number"`
	PremiumMailBaseURL string          `json:"premium_mail_base_url"`
	MailGatewayBaseURL string          `json:"mail_gateway_base_url,omitempty"`
	MailBaseURL        string          `json:"mail_base_url,omitempty"`
	Host               string          `json:"host"`
	IsICloudPlus       bool            `json:"is_icloud_plus"`
	CanCreateHME       bool            `json:"can_create_hme"`
	Cookies            []SessionCookie `json:"cookies,omitempty"`
	LoginStates        []LoginState    `json:"login_states,omitempty"`
	Note               string          `json:"note,omitempty"`
	LastCheckedAt      time.Time       `json:"last_checked_at,omitempty"`
	LastCheckOK        bool            `json:"last_check_ok,omitempty"`
	LastStatusMessage  string          `json:"last_status_message,omitempty"`
}

type LoginState struct {
	Kind              string          `json:"kind"`
	Host              string          `json:"host,omitempty"`
	Origin            string          `json:"origin,omitempty"`
	SavedAt           time.Time       `json:"saved_at,omitempty"`
	Cookies           []SessionCookie `json:"cookies,omitempty"`
	Scnt              string          `json:"scnt,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	APIKey            string          `json:"api_key,omitempty"`
	DataAccessToken   string          `json:"data_access_token,omitempty"`
	UserAgent         string          `json:"user_agent,omitempty"`
	Note              string          `json:"note,omitempty"`
	IMAPEmail         string          `json:"imap_email,omitempty"`
	IMAPUsername      string          `json:"imap_username,omitempty"`
	IMAPHost          string          `json:"imap_host,omitempty"`
	IMAPPort          int             `json:"imap_port,omitempty"`
	IMAPAppPassword   string          `json:"imap_app_password,omitempty"`
	IMAPLastSyncAt    time.Time       `json:"imap_last_sync_at,omitempty"`
	IMAPLastSyncUID   string          `json:"imap_last_sync_uid,omitempty"`
	IMAPUIDValidity   string          `json:"imap_uid_validity,omitempty"`
	ManageExpiresAt   time.Time       `json:"manage_expires_at,omitempty"`
	LastCheckedAt     time.Time       `json:"last_checked_at,omitempty"`
	LastCheckOK       bool            `json:"last_check_ok,omitempty"`
	LastStatusMessage string          `json:"last_status_message,omitempty"`
}

type SessionCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"http_only,omitempty"`
	SameSite string  `json:"same_site,omitempty"`
}

type Dashboard struct {
	AppleAccountCount  int     `json:"apple_account_count"`
	ActiveAccountCount int     `json:"active_account_count"`
	MailboxCount       int     `json:"mailbox_count"`
	AvailableCount     int     `json:"available_count"`
	MessageCount       int     `json:"message_count"`
	Events             []Event `json:"events"`
}

type MailboxPage struct {
	Items      []Mailbox `json:"items"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	Total      int       `json:"total"`
	TotalPages int       `json:"total_pages"`
}

type Task struct {
	ID                       string     `json:"id"`
	Name                     string     `json:"name"`
	Description              string     `json:"description"`
	Status                   string     `json:"status"`
	Progress                 int        `json:"progress"`
	Module                   string     `json:"module"`
	NextRunAt                *time.Time `json:"next_run_at,omitempty"`
	ScheduledIntervalSeconds int        `json:"scheduled_interval_seconds,omitempty"`
	JitterPercent            int        `json:"jitter_percent,omitempty"`
}

func DefaultSettings() Settings {
	return Settings{
		EnableMailWatcher:          false,
		EnableAppleKeepAlive:       false,
		EnablePublicMailboxAPI:     false,
		EnablePublicCodePage:       false,
		EnableWebCodeSync:          false,
		EnableWebManualMailSync:    true,
		EnableWebBackgroundMail:    true,
		EnableWebRemoteMailCleanup: true,
		AppleAccountModuleReady:    true,
		ServerChanHideIP:           true,
	}
}

func DefaultDomainMailSettings() DomainMailSettings {
	return DomainMailSettings{
		MatchMode:        DomainMatchCatchAll,
		AutoDiscover:     true,
		DefaultAPIActive: true,
	}
}

func DefaultCreateSettings() CreateSettings {
	return CreateSettings{
		Mode:                               "once",
		Label:                              "x",
		CreateChannel:                      "auto",
		SchedulerCreateChannel:             "auto",
		AppleAccountTwoFactorMethod:        "trusted_device",
		ICloudWebTwoFactorMethod:           "trusted_device",
		SchedulerIntervalMinMinutes:        60,
		SchedulerIntervalMaxMinutes:        60,
		SchedulerAccountIntervalMinSeconds: 5,
		SchedulerAccountIntervalMaxSeconds: 5,
	}
}
