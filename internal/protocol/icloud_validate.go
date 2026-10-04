package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ICloudSessionValidator struct {
	httpClient *http.Client
}

type validateResult struct {
	AppleID            string
	DSID               string
	ClientID           string
	ClientBuildNumber  string
	MasteringNumber    string
	PremiumMailBaseURL string
	MailGatewayBaseURL string
	MailBaseURL        string
	IsICloudPlus       bool
	CanCreateHME       bool
}

func NewICloudSessionValidator() *ICloudSessionValidator {
	return &ICloudSessionValidator{httpClient: &http.Client{Timeout: 15 * time.Second}}
}

func (c *ICloudSessionValidator) Validate(ctx context.Context, cookies []SessionCookie, defaultHost string) (validateResult, error) {
	result, err := c.validateOnHost(ctx, cookies, defaultHost)
	if err == nil {
		return result, nil
	}
	// 非中国账号的登录响应有时没有及时返回 domainToUse，导致首次校验错误地
	// 访问 setup.icloud.com.cn；收到这类失败后用国际域名重试一次。
	host := strings.ToLower(strings.TrimSpace(defaultHost))
	if strings.Contains(host, "icloud.com.cn") {
		if fallback, fallbackErr := c.validateOnHost(ctx, cookies, "www.icloud.com"); fallbackErr == nil {
			return fallback, nil
		}
	}
	return result, err
}

func (c *ICloudSessionValidator) validateOnHost(ctx context.Context, cookies []SessionCookie, defaultHost string) (validateResult, error) {
	host := strings.TrimSpace(defaultHost)
	if host == "" {
		host = "www.icloud.com.cn"
	}
	setupHost := "setup.icloud.com.cn"
	if strings.HasSuffix(host, "icloud.com") && !strings.HasSuffix(host, "icloud.com.cn") {
		setupHost = "setup.icloud.com"
	}

	clientID, err := randomUUID()
	if err != nil {
		return validateResult{}, err
	}
	requestID, err := randomUUID()
	if err != nil {
		return validateResult{}, err
	}
	buildNumber := iCloudPortalBuildNumber
	masteringNumber := buildNumber

	u := url.URL{
		Scheme: "https",
		Host:   setupHost,
		Path:   "/setup/ws/1/validate",
	}
	q := u.Query()
	q.Set("clientBuildNumber", buildNumber)
	q.Set("clientMasteringNumber", masteringNumber)
	q.Set("clientId", clientID)
	q.Set("requestId", requestID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return validateResult{}, err
	}
	session := ICloudSession{Host: host, Cookies: cookies}
	setICloudFetchHeaders(req, session, "*/*", "text/plain;charset=UTF-8")
	if cookie := cookieHeader(cookies, u.String()); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return validateResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return validateResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return validateResult{}, errCode("icloud_validate_failed", icloudValidateErrorMessage(resp.StatusCode, data), true)
	}
	var account struct {
		DSInfo struct {
			DSID                            string `json:"dsid"`
			AppleID                         string `json:"appleId"`
			PrimaryEmail                    string `json:"primaryEmail"`
			IsHideMyEmailSubscriptionActive bool   `json:"isHideMyEmailSubscriptionActive"`
			IsHideMyEmailFeatureAvailable   bool   `json:"isHideMyEmailFeatureAvailable"`
		} `json:"dsInfo"`
		Webservices map[string]struct {
			URL    string `json:"url"`
			Status string `json:"status"`
		} `json:"webservices"`
	}
	if err := json.Unmarshal(data, &account); err != nil {
		return validateResult{}, errCode("icloud_validate_bad_response", "iCloud 登录态校验返回无法解析", true)
	}
	premium := account.Webservices["premiummailsettings"].URL
	mailGateway := account.Webservices["mccgateway"].URL
	mail := account.Webservices["mail"].URL
	appleID := account.DSInfo.AppleID
	if appleID == "" {
		appleID = account.DSInfo.PrimaryEmail
	}
	return validateResult{
		AppleID:            appleID,
		DSID:               account.DSInfo.DSID,
		ClientID:           clientID,
		ClientBuildNumber:  buildNumber,
		MasteringNumber:    masteringNumber,
		PremiumMailBaseURL: premium,
		MailGatewayBaseURL: mailGateway,
		MailBaseURL:        mail,
		IsICloudPlus:       account.DSInfo.IsHideMyEmailSubscriptionActive,
		CanCreateHME:       account.DSInfo.IsHideMyEmailFeatureAvailable,
	}, nil
}

func icloudValidateErrorMessage(status int, data []byte) string {
	if status == http.StatusMisdirectedRequest {
		var partial struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &partial); err == nil && strings.TrimSpace(partial.Error) != "" {
			return fmt.Sprintf("iCloud 登录态校验失败，HTTP %d：%s", status, strings.TrimSpace(partial.Error))
		}
		return fmt.Sprintf("iCloud 登录态校验失败，HTTP %d：当前会话缺少 iCloud Web 登录 Cookie，请先完成 iCloud Web 登录", status)
	}
	return fmt.Sprintf("iCloud 登录态校验失败，HTTP %d: %s", status, trimForError(data))
}
