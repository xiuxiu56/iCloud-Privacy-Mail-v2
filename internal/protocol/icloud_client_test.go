package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestAppleAccountEmptyUnauthorizedResponseIsAuthFailure(t *testing.T) {
	err := appleAccountAPIError(http.StatusUnauthorized, nil, "刷新管理 token")
	code, message, retryable := ErrorDetails(err)
	if code != "apple_account_auth_failed" || !retryable {
		t.Fatalf("空响应 401 的错误分类为 code=%q retryable=%t，期望登录态失效", code, retryable)
	}
	if message == "" {
		t.Fatal("空响应 401 缺少错误说明")
	}
}

func TestCanonicalMailIDNormalizesMessageIDAcrossReadPaths(t *testing.T) {
	receivedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	fromIMAP := canonicalMailID("<Message-42@Example.COM>", "sender@example.com", "主题", receivedAt)
	fromWeb := canonicalMailID("message-42@example.com", "other@example.com", "不同主题", receivedAt.Add(time.Hour))
	if fromIMAP != fromWeb || fromIMAP != "message-id:message-42@example.com" {
		t.Fatalf("跨路径 Message-ID 规范化结果不一致：IMAP=%q，Web=%q", fromIMAP, fromWeb)
	}
}

func TestMailHeaderValueReadsFoldedMessageID(t *testing.T) {
	header := "From: sender@example.com\r\nMessage-ID:\r\n <folded-42@example.com>\r\nSubject: 测试"
	if value := mailHeaderValue(header, "Message-ID"); value != "<folded-42@example.com>" {
		t.Fatalf("长邮件头 Message-ID 解析错误：%q", value)
	}
}

func TestMailThreadSearchLimitUsesWebValidatedBoundForFullScan(t *testing.T) {
	options := MailSyncOptions{FullScan: true, Limit: 20}
	if limit := mailThreadSearchLimit(mailFolder{MessageCount: 738}, options); limit != 1000 {
		t.Fatalf("全量 Web 同步上限不正确：%d", limit)
	}
	if limit := mailThreadSearchLimit(mailFolder{MessageCount: 5000}, options); limit != 1000 {
		t.Fatalf("全量 Web 同步不应直接使用文件夹邮件数：%d", limit)
	}
}

func TestMailThreadSearchLimitKeepsIncrementalBound(t *testing.T) {
	if limit := mailThreadSearchLimit(mailFolder{MessageCount: 738}, MailSyncOptions{Limit: 20}); limit != 20 {
		t.Fatalf("增量同步限制不正确：%d", limit)
	}
}

func TestMailThreadSearchBodyUsesBrowserFullScanMode(t *testing.T) {
	body := mailThreadSearchBody(mailFolder{Name: "INBOX", MessageCount: 53}, 1000, true)
	if body["responseType"] != "THREAD_ID_AND_DATE" || body["includeFolderStatus"] != true || body["maxResults"] != 1000 {
		t.Fatalf("全量 Web 检索请求体不正确：%+v", body)
	}
	headers, ok := body["sessionHeaders"].(map[string]any)
	if !ok || headers["folder"] != "INBOX" || headers["modseq"] != nil || headers["threadmodseq"] != nil {
		t.Fatalf("首次全量 Web 检索的会话头不正确：%+v", body["sessionHeaders"])
	}
}

func TestMailThreadSearchBodyKeepsDigestModeForIncrementalSync(t *testing.T) {
	body := mailThreadSearchBody(mailFolder{Name: "INBOX"}, 20, false)
	if body["responseType"] != "THREAD_DIGEST" || body["includeFolderStatus"] != false || body["maxResults"] != 20 {
		t.Fatalf("增量 Web 检索请求体不正确：%+v", body)
	}
}

func TestMoveRemoteMessagesToTrashAndDestroyOnlyDeletesTargetMessages(t *testing.T) {
	var operations []string
	messageListCalls := 0
	moved := false
	destroyed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mailws2/v1/geqs/query":
			operations = append(operations, "读取邮件夹")
			_, _ = w.Write([]byte(`{"domainObjects":[{"identifier":"folder-inbox","name":"INBOX"},{"identifier":"folder-trash","name":"Deleted Messages"}]}`))
		case "/mailws2/v1/message/list":
			messageListCalls++
			switch {
			case !moved && messageListCalls == 1:
				operations = append(operations, "定位收件箱目标邮件")
				_, _ = w.Write([]byte(`{"domainObjects":[{"uid":101,"identifier":"message-source","mboxRef":{"id":"folder-inbox"}}]}`))
			case !moved:
				operations = append(operations, "读取移动前废纸篓")
				_, _ = w.Write([]byte(`{"domainObjects":[]}`))
			case !destroyed:
				operations = append(operations, "确认移入废纸篓")
				_, _ = w.Write([]byte(`{"domainObjects":[{"uid":901,"identifier":"message-trash-target","mboxRef":{"id":"folder-trash"}}]}`))
			default:
				operations = append(operations, "确认废纸篓已删除")
				_, _ = w.Write([]byte(`{"domainObjects":[]}`))
			}
		case "/mailws2/v1/email/set":
			if r.URL.Query().Get("clientIntent") != iCloudMailDeleteClientIntent {
				t.Fatalf("删除请求缺少 clientIntent：%s", r.URL.RawQuery)
			}
			if contentType := r.Header.Get("Content-Type"); contentType != "text/plain;charset=UTF-8" {
				t.Fatalf("删除请求 Content-Type 不正确：%s", contentType)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("解析邮件操作请求失败：%v", err)
			}
			if _, ok := body["batchUpdate"]; ok {
				operations = append(operations, "移入废纸篓")
				moved = true
				_, _ = w.Write([]byte(`{"updated":{"message-source":{}}}`))
				return
			}
			operations = append(operations, "彻底删除目标邮件")
			if identifiers, ok := body["destroy"].([]any); !ok || len(identifiers) != 1 || identifiers[0] != "message-trash-target" {
				t.Fatalf("彻底删除的邮件标识不正确：%+v", body)
			}
			destroyed = true
			_, _ = w.Write([]byte(`{"destroyed":["message-trash-target"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewICloudClient()
	session := ICloudSession{
		DSID: "fixture-dsid", MailGatewayBaseURL: server.URL,
		Cookies: []SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-token"}},
	}
	result, err := client.MoveRemoteMessagesToTrashAndDestroy(context.Background(), session, []string{"icloud:INBOX:101"})
	if err != nil {
		t.Fatalf("彻底删除指定云端邮件失败：%v", err)
	}
	if result.MovedToTrash != 1 || result.Destroyed != 1 {
		t.Fatalf("云端删除统计不正确：%+v", result)
	}
	expected := []string{"读取邮件夹", "定位收件箱目标邮件", "读取移动前废纸篓", "移入废纸篓", "确认移入废纸篓", "彻底删除目标邮件", "确认废纸篓已删除"}
	if !reflect.DeepEqual(operations, expected) {
		t.Fatalf("云端邮件删除顺序不正确：%v", operations)
	}
}

func TestMoveRemoteMessagesToTrashAndDestroyDeletesMessagesAlreadyInTrash(t *testing.T) {
	messageListCalls := 0
	destroyed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mailws2/v1/geqs/query":
			_, _ = w.Write([]byte(`{"domainObjects":[{"identifier":"folder-inbox","name":"INBOX"},{"identifier":"folder-trash","name":"Deleted Messages"}]}`))
		case "/mailws2/v1/message/list":
			messageListCalls++
			if !destroyed {
				_, _ = w.Write([]byte(`{"domainObjects":[{"uid":909,"identifier":"trash-target","mboxRef":{"id":"folder-trash"}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"domainObjects":[]}`))
		case "/mailws2/v1/email/set":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("解析废纸篓删除请求失败：%v", err)
			}
			if _, moving := body["batchUpdate"]; moving {
				t.Fatal("已在废纸篓的邮件不应再次执行移动")
			}
			if identifiers, ok := body["destroy"].([]any); !ok || len(identifiers) != 1 || identifiers[0] != "trash-target" {
				t.Fatalf("废纸篓彻底删除标识不正确：%+v", body)
			}
			destroyed = true
			_, _ = w.Write([]byte(`{"destroyed":["trash-target"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewICloudClient()
	session := ICloudSession{
		DSID: "fixture-dsid", MailGatewayBaseURL: server.URL,
		Cookies: []SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-token"}},
	}
	result, err := client.MoveRemoteMessagesToTrashAndDestroy(context.Background(), session, []string{"icloud:Deleted Messages:909"})
	if err != nil {
		t.Fatalf("彻底删除废纸篓已有邮件失败：%v", err)
	}
	if result.MovedToTrash != 0 || result.Destroyed != 1 || messageListCalls != 3 {
		t.Fatalf("废纸篓已有邮件删除结果不正确：result=%+v listCalls=%d", result, messageListCalls)
	}
}

func TestFindRemoteMessageIDsByMailboxIncludesTrash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mailws2/v1/geqs/query":
			_, _ = w.Write([]byte(`{"domainObjects":[{"identifier":"folder-inbox","name":"INBOX","messageCount":0},{"identifier":"folder-trash","name":"Deleted Messages","messageCount":1}]}`))
		case "/mailws2/v1/thread/search":
			var body struct {
				SessionHeaders map[string]any `json:"sessionHeaders"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("解析邮件线程检索请求失败：%v", err)
			}
			if body.SessionHeaders["folder"] == "Deleted Messages" {
				_, _ = w.Write([]byte(`{"threadList":[{"threadId":"trash-thread","timestamp":1788105600000}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"threadList":[]}`))
		case "/mailws2/v1/thread/get":
			_, _ = w.Write([]byte(`{"messageMetadataList":[{"uid":909,"folder":"Deleted Messages","to":[{"email":"code@example.com"}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewICloudClient()
	session := ICloudSession{
		DSID: "fixture-dsid", MailGatewayBaseURL: server.URL,
		Cookies: []SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-token"}},
	}
	result, err := client.FindRemoteMessageIDsByMailbox(context.Background(), session, []Mailbox{{ID: "domain-mailbox-1", Email: "code@example.com"}})
	if err != nil {
		t.Fatalf("扫描废纸篓中的域名收件邮件失败：%v", err)
	}
	expected := []string{"icloud:Deleted Messages:909"}
	if !reflect.DeepEqual(result.RemoteIDsByMailbox["domain-mailbox-1"], expected) {
		t.Fatalf("废纸篓邮件未按收件人匹配：%+v", result.RemoteIDsByMailbox)
	}
}
