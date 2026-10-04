package protocol

import "strings"

// iCloud 网页当前抓取到的门户与邮件服务版本。两者不是同一个构建号。
const (
	iCloudPortalBuildNumber = "2636Build34"
	iCloudMailBuildNumber   = "2634Hotfix39"
	iCloudWebUserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
)

func isAppleMailGateway(rawURL string) bool {
	host := strings.ToLower(strings.TrimSpace(rawURL))
	return strings.Contains(host, "-mccgateway.icloud.com") || strings.Contains(host, "-mccgateway.icloud.com.cn")
}
