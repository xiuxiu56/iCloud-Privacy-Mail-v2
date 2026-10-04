package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAuthWithTokenAndValidateUsesAccountLoginResponseAfterTwoFA(t *testing.T) {
	validateCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/setup/ws/1/accountLogin":
			w.Header().Add("Set-Cookie", `X-APPLE-WEBAUTH-LOGIN="fixture-login"; Path=/; Domain=.icloud.com; Secure; HttpOnly`)
			w.Header().Add("Set-Cookie", `X-APPLE-WEBAUTH-TOKEN="fixture-token"; Path=/; Domain=.icloud.com; Secure; HttpOnly`)
			w.Header().Add("Set-Cookie", `X-APPLE-WEBAUTH-USER="v=1:s=1:d=123456"; Path=/; Domain=.icloud.com; Secure; HttpOnly`)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"dsInfo": map[string]any{
					"dsid":                            "123456",
					"appleId":                         "fixture@icloud.com",
					"primaryEmail":                    "fixture@icloud.com",
					"isHideMyEmailSubscriptionActive": true,
					"isHideMyEmailFeatureAvailable":   true,
				},
				"webservices": map[string]any{
					"premiummailsettings": map[string]string{"url": "https://p1-maildomainws.icloud.com:443"},
					"mccgateway":          map[string]string{"url": "https://p1-mccgateway.icloud.com:443"},
					"mail":                map[string]string{"url": "https://p1-mailws.icloud.com:443"},
				},
			})
		case "/setup/ws/1/validate":
			validateCalled = true
			w.WriteHeader(http.StatusMisdirectedRequest)
			_, _ = w.Write([]byte(`{"success":false,"trustTokens":["fixture-trust-token"],"error":"Missing X-APPLE-WEBAUTH-TOKEN cookie"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{
		Endpoints:      appleAuthEndpoints{Setup: server.URL + "/setup/ws/1", Host: "www.icloud.com", Home: server.URL},
		AppleID:        "fixture@icloud.com",
		SessionToken:   "fixture-session-token",
		AccountCountry: "HKG",
		TrustToken:     "fixture-trust-token",
	}

	result, err := client.authWithTokenAndValidate(context.Background(), session)
	if err != nil {
		t.Fatalf("验证码提交后的 accountLogin 会话不应因 421 失败：%v", err)
	}
	if validateCalled {
		t.Fatal("验证码提交成功后不应再次调用缺少 Web Token 的 /validate")
	}
	if result.DSID != "123456" || result.MailGatewayBaseURL != "https://p1-mccgateway.icloud.com:443" || result.ClientBuildNumber != iCloudPortalBuildNumber {
		t.Fatalf("未使用 accountLogin 返回的会话信息：%+v", result)
	}
}

func TestAuthWithTokenAndValidateRejectsIncompleteAccountLoginSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setup/ws/1/accountLogin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Add("Set-Cookie", `X-APPLE-WEBAUTH-LOGIN="fixture-login"; Path=/; Domain=.icloud.com; Secure; HttpOnly`)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dsInfo": map[string]any{"dsid": "123456", "appleId": "fixture@icloud.com"},
			"webservices": map[string]any{
				"mccgateway":          map[string]string{"url": "https://p1-mccgateway.icloud.com:443"},
				"mail":                map[string]string{"url": "https://p1-mailws.icloud.com:443"},
				"premiummailsettings": map[string]string{"url": "https://p1-maildomainws.icloud.com:443"},
			},
		})
	}))
	defer server.Close()

	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{
		Endpoints:      appleAuthEndpoints{Setup: server.URL + "/setup/ws/1", Host: "www.icloud.com", Home: server.URL},
		AppleID:        "fixture@icloud.com",
		SessionToken:   "fixture-session-token",
		AccountCountry: "HKG",
	}

	_, err := client.authWithTokenAndValidate(context.Background(), session)
	if err == nil || !strings.Contains(err.Error(), "X-APPLE-WEBAUTH-TOKEN") {
		t.Fatalf("缺少 Web Token 时应拒绝保存不完整会话，得到：%v", err)
	}
}

func TestRequestPhoneSecurityCodeAccepts412AfterSMSWasSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"trustedPhoneNumbers":[{"id":7,"nonFTEU":true,"pushMode":"sms","lastTwoDigits":"41"}]}`))
	}))
	defer server.Close()

	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{
		Endpoints: appleAuthEndpoints{Auth: server.URL},
		UserAgent: appleAuthUserAgent,
	}
	if err := client.requestPhoneSecurityCode(context.Background(), session, nil); err != nil {
		t.Fatalf("412 短信发送结果不应被判定为失败：%v", err)
	}
	if !strings.Contains(string(session.TwoFactorPhone), `"id":7`) {
		t.Fatalf("未保存 Apple 返回的受信任手机号：%s", session.TwoFactorPhone)
	}
}

func TestRequestPhoneSecurityCodeRejects412WithoutPhoneDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error":"precondition_failed"}`))
	}))
	defer server.Close()

	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{Endpoints: appleAuthEndpoints{Auth: server.URL}}
	if err := client.requestPhoneSecurityCode(context.Background(), session, nil); err == nil {
		t.Fatal("缺少受信任手机号信息的 412 响应不应被当作短信已发送")
	}
}

func TestAccountLoginPreservesWebAuthCookieWithAppleExpiresFormat(t *testing.T) {
	u, err := url.Parse("https://setup.icloud.com.cn/setup/ws/1/accountLogin")
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{Request: &http.Request{URL: u}, Header: make(http.Header)}
	resp.Header.Add("Set-Cookie", `X-APPLE-WEBAUTH-TOKEN="fixture-token";Expires=Fri, 1-Jan-2027 20:04:06 GMT;Path=/;Domain=.icloud.com.cn;Secure;HttpOnly`)
	session := &appleAuthSession{}
	session.extract(resp)
	if !hasICloudWebAuthToken(session.Cookies) {
		t.Fatalf("accountLogin 原始 Set-Cookie 未保存 Web Token：%+v", session.Cookies)
	}
}

func TestChinaICloudAuthUsesGlobalIDMSAHost(t *testing.T) {
	endpoints := appleAuthEndpointsForHost("www.icloud.com.cn")
	if endpoints.Auth != "https://idmsa.apple.com/appleauth/auth" {
		t.Fatalf("中国区旧接口认证域名错误：%q", endpoints.Auth)
	}
}
