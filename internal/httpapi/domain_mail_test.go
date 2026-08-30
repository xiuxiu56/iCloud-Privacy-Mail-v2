package httpapi

import "testing"

func TestGenerateDomainMailboxEmailsWithoutStartCreatesSingleExactPrefix(t *testing.T) {
	emails, err := generateDomainMailboxEmails("xiummm.com", "sequence", "mrhuang", 500, nil, 0)
	if err != nil {
		t.Fatalf("生成单个指定前缀邮箱失败：%v", err)
	}
	if len(emails) != 1 || emails[0] != "mrhuang@xiummm.com" {
		t.Fatalf("开始编号留空时应忽略数量并生成单个指定前缀：%v", emails)
	}
}

func TestGenerateDomainMailboxEmailsWithStartCreatesSequence(t *testing.T) {
	start := 0
	emails, err := generateDomainMailboxEmails("xiummm.com", "sequence", "mrhuang", 2, &start, 0)
	if err != nil {
		t.Fatalf("生成递增编号邮箱失败：%v", err)
	}
	if len(emails) != 2 || emails[0] != "mrhuang0@xiummm.com" || emails[1] != "mrhuang1@xiummm.com" {
		t.Fatalf("递增编号邮箱结果不正确：%v", emails)
	}
}
