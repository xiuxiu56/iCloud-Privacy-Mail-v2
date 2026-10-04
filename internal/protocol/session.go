package protocol

import (
	"context"
	"strings"
	"time"
)

func LoginStateForKind(session ICloudSession, kind string) (LoginState, bool) {
	for _, state := range session.LoginStates {
		if state.Kind == kind {
			return state, true
		}
	}
	return LoginState{}, false
}

func WithLoginState(session ICloudSession, next LoginState) ICloudSession {
	next.Kind = strings.TrimSpace(next.Kind)
	for i := range session.LoginStates {
		if session.LoginStates[i].Kind == next.Kind {
			session.LoginStates[i] = next
			return session
		}
	}
	session.LoginStates = append(session.LoginStates, next)
	return session
}

// normalizeICloudWebSession 让旧接口请求优先使用 iCloud Web 专属登录态。
// Apple Account 新接口保活后可能会更新顶层 Host，但不应影响 iCloud Web 的中国区域名和 Cookie。
func normalizeICloudWebSession(session ICloudSession) ICloudSession {
	state, ok := LoginStateForKind(session, LoginStateICloudWeb)
	if !ok {
		return session
	}
	if host := strings.TrimSpace(state.Host); host != "" {
		session.Host = host
	}
	if len(state.Cookies) > 0 {
		session.Cookies = append([]SessionCookie(nil), state.Cookies...)
	}
	return session
}

func CanUseICloudWebMail(session ICloudSession) bool {
	session = normalizeICloudWebSession(session)
	if len(session.LoginStates) > 0 {
		state, ok := LoginStateForKind(session, LoginStateICloudWeb)
		_, appleAccountSaved := LoginStateForKind(session, LoginStateAppleAccount)
		if (!ok && appleAccountSaved) || (ok && !state.LastCheckedAt.IsZero() && !state.LastCheckOK) {
			return false
		}
	}
	if strings.TrimSpace(session.DSID) == "" || len(session.Cookies) == 0 {
		return false
	}
	if !hasICloudWebAuthToken(session.Cookies) {
		return false
	}
	_, err := mailGatewayBaseURL(session)
	return err == nil
}

func (c *ICloudSessionValidator) ValidateSession(ctx context.Context, session ICloudSession, defaultHost string) (ICloudSession, error) {
	session = normalizeICloudWebSession(session)
	if !hasICloudWebAuthToken(session.Cookies) {
		err := errCode("icloud_web_cookie_missing", "iCloud Web 登录态不完整，缺少 X-APPLE-WEBAUTH-TOKEN cookie，请重新完成旧接口登录", true)
		session.LastCheckedAt = time.Now()
		session.LastCheckOK = false
		session.LastStatusMessage = err.Error()
		return session, err
	}
	result, err := c.Validate(ctx, session.Cookies, firstNonEmpty(session.Host, defaultHost))
	checkedAt := time.Now()
	if err != nil {
		session.LastCheckedAt = checkedAt
		session.LastCheckOK = false
		session.LastStatusMessage = err.Error()
		return session, err
	}
	if strings.Contains(strings.ToLower(result.MailGatewayBaseURL), ".icloud.com") && !strings.Contains(strings.ToLower(result.MailGatewayBaseURL), ".icloud.com.cn") && strings.Contains(strings.ToLower(session.Host), ".icloud.com.cn") {
		session.Host = "www.icloud.com"
		for i := range session.LoginStates {
			if session.LoginStates[i].Kind == LoginStateICloudWeb {
				session.LoginStates[i].Host = session.Host
				session.LoginStates[i].Origin = "https://www.icloud.com"
			}
		}
	}
	session.AppleID = firstNonEmpty(result.AppleID, session.AppleID)
	session.DSID = firstNonEmpty(result.DSID, session.DSID)
	session.ClientID = firstNonEmpty(result.ClientID, session.ClientID)
	session.ClientBuildNumber = firstNonEmpty(result.ClientBuildNumber, session.ClientBuildNumber)
	session.MasteringNumber = firstNonEmpty(result.MasteringNumber, session.MasteringNumber)
	session.PremiumMailBaseURL = firstNonEmpty(result.PremiumMailBaseURL, session.PremiumMailBaseURL)
	session.MailGatewayBaseURL = firstNonEmpty(result.MailGatewayBaseURL, session.MailGatewayBaseURL)
	session.MailBaseURL = firstNonEmpty(result.MailBaseURL, session.MailBaseURL)
	session.IsICloudPlus = result.IsICloudPlus
	session.CanCreateHME = result.CanCreateHME
	session.LastCheckedAt = checkedAt
	session.LastCheckOK = true
	session.LastStatusMessage = "iCloud Web 登录态正常"
	return session, nil
}

func hasICloudWebAuthToken(cookies []SessionCookie) bool {
	for _, cookie := range cookies {
		if !strings.EqualFold(strings.TrimSpace(cookie.Name), "X-APPLE-WEBAUTH-TOKEN") {
			continue
		}
		if strings.TrimSpace(cookie.Value) != "" {
			return true
		}
	}
	return false
}
