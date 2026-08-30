package protocol

import (
	"strings"
	"testing"
	"time"
)

func TestParseICloudIMAPMessageKeepsHTMLAndPlainText(t *testing.T) {
	raw := strings.Join([]string{
		"From: OpenAI <noreply@example.com>",
		"To: alias@icloud.com",
		"Subject: =?UTF-8?B?6aqM6K+B56CB?=",
		"Content-Type: multipart/alternative; boundary=mail-boundary",
		"",
		"--mail-boundary",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		"纯文本验证码：123456",
		"--mail-boundary",
		"Content-Type: text/html; charset=UTF-8",
		"",
		"<html><body><h1>验证码</h1><p>123456</p></body></html>",
		"--mail-boundary--",
		"",
	}, "\r\n")
	message, recipients, ok := parseICloudIMAPMessage(iCloudIMAPFetchedMessage{UID: "42", Raw: []byte(raw)})
	if !ok {
		t.Fatal("完整邮件解析失败")
	}
	if message.ContentType != "text/html" || !strings.Contains(message.HTMLBody, "<h1>验证码</h1>") {
		t.Fatalf("HTML 正文未保留：%+v", message)
	}
	if !strings.Contains(message.Body, "纯文本验证码：123456") {
		t.Fatalf("纯文本正文未保留：%q", message.Body)
	}
	if !strings.Contains(recipients, "alias@icloud.com") {
		t.Fatalf("收件人解析错误：%q", recipients)
	}
}

func TestMatchingDiscoveredEmailsOnlyAcceptsConfiguredDomains(t *testing.T) {
	recipients := `To: "域名邮箱" <MRHUANG1@xiummm.com>, receiver@icloud.com
Delivered-To: random@other.example`
	items := matchingDiscoveredEmails(recipients, []string{"xiummm.com"}, map[string]string{"known": "known@xiummm.com"})
	if len(items) != 1 || items[0] != "mrhuang1@xiummm.com" {
		t.Fatalf("自动发现收件人结果不正确：%v", items)
	}
}

func TestIMAPSearchUsesAccountCursorBeforeMailboxCursor(t *testing.T) {
	mailboxes := []Mailbox{{ID: "mailbox-1", LastSyncUID: "900"}}
	command, incremental := imapSearchCommand("42", mailboxes, time.Time{}, true, false)
	if !incremental || command != "UID SEARCH UID 43:*" {
		t.Fatalf("账号游标搜索命令不正确：%q，incremental=%t", command, incremental)
	}
}

func TestIMAPSearchIgnoresLegacyMailboxCursorAfterUIDValidityStateExists(t *testing.T) {
	mailboxes := []Mailbox{{ID: "mailbox-1", LastSyncUID: "900"}}
	after := time.Date(2026, 8, 20, 0, 0, 0, 0, time.Local)
	command, incremental := imapSearchCommand("", mailboxes, after, true, false)
	if incremental || command != "UID SEARCH SINCE 20-Aug-2026" {
		t.Fatalf("UIDVALIDITY 重置后不应复用旧邮箱游标：%q，incremental=%t", command, incremental)
	}
}

func TestIMAPSearchCanMigrateLegacyMailboxCursor(t *testing.T) {
	mailboxes := []Mailbox{{ID: "mailbox-1", LastSyncUID: "15"}, {ID: "mailbox-2", LastSyncUID: "20"}}
	command, incremental := imapSearchCommand("", mailboxes, time.Time{}, true, true)
	if !incremental || command != "UID SEARCH UID 16:*" {
		t.Fatalf("旧邮箱游标迁移搜索命令不正确：%q，incremental=%t", command, incremental)
	}
}

func TestIMAPManualSyncSearchesAllBeforeTakingLatestLimit(t *testing.T) {
	command, incremental := imapSearchCommand("", nil, time.Time{}, false, false)
	if incremental || command != "UID SEARCH ALL" {
		t.Fatalf("手动同步应搜索整个收件箱再截取最新邮件：%q，incremental=%t", command, incremental)
	}
}

func TestIMAPFullScanIsBoundedAtSelectedSnapshot(t *testing.T) {
	command, incremental := imapSearchCommand("", nil, time.Time{}, false, false, 1211)
	if incremental || command != "UID SEARCH UID 1:1211" {
		t.Fatalf("全量扫描应固定在 SELECT 时的最高 UID：%q，incremental=%t", command, incremental)
	}
}

func TestParseICloudIMAPHeaderIndexesForwardRecipient(t *testing.T) {
	raw := strings.Join([]string{
		"From: Sender <sender@example.com>",
		"To: receiver@icloud.com",
		"X-Original-To: MRHUANG1@xiummm.com",
		"Message-ID: <history@example.com>",
		"Subject: 历史邮件",
		"Date: Fri, 28 Aug 2026 12:00:00 +0800",
		"",
		"",
	}, "\r\n")
	entry, recipients, ok := parseICloudIMAPHeader(iCloudIMAPFetchedMessage{UID: "42", Raw: []byte(raw)})
	if !ok || entry.UID != "42" || entry.CanonicalID != "message-id:history@example.com" {
		t.Fatalf("邮件头索引解析失败：%+v", entry)
	}
	if !strings.Contains(strings.ToLower(recipients), "mrhuang1@xiummm.com") || len(entry.Recipients) != 2 {
		t.Fatalf("转发收件地址未纳入索引：%q %+v", recipients, entry.Recipients)
	}
}

func TestIMAPSelectUIDValidity(t *testing.T) {
	value := imapSelectUIDValidity([]string{"* OK [UIDVALIDITY 3857529045] UIDs valid", "A002 OK SELECT completed"})
	if value != "3857529045" {
		t.Fatalf("UIDVALIDITY 解析错误：%q", value)
	}
}
