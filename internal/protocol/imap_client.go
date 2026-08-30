package protocol

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultICloudIMAPHost = "imap.mail.me.com"
	defaultICloudIMAPPort = 993
	imapIdleReadTimeout   = 25 * time.Minute
)

var iCloudIMAPDNSServers = []string{"1.1.1.1:53", "8.8.8.8:53", "127.0.0.53:53"}
var iCloudIMAPFallbackIPv4 = []net.IP{
	net.IPv4(17, 57, 152, 32),
	net.IPv4(17, 57, 152, 35),
	net.IPv4(17, 57, 155, 39),
	net.IPv4(17, 42, 251, 69),
	net.IPv4(17, 42, 251, 72),
	net.IPv4(17, 56, 9, 32),
	net.IPv4(17, 156, 192, 7),
}

type iCloudIMAPSyncResult = MailSyncBatchResult

func newICloudIMAPDialer(serverName string) tls.Dialer {
	serverName = firstNonEmpty(strings.TrimSpace(serverName), defaultICloudIMAPHost)
	return tls.Dialer{
		NetDialer: &net.Dialer{
			Timeout:  15 * time.Second,
			Resolver: newICloudIMAPResolver(iCloudIMAPDNSServers),
		},
		Config: &tls.Config{
			ServerName: serverName,
			MinVersion: tls.VersionTLS12,
		},
	}
}

func dialICloudIMAPTLS(ctx context.Context, serverName string, port int) (net.Conn, error) {
	serverName = firstNonEmpty(strings.TrimSpace(serverName), defaultICloudIMAPHost)
	if port <= 0 {
		port = defaultICloudIMAPPort
	}
	var lastErr error
	if strings.EqualFold(strings.TrimSuffix(serverName, "."), defaultICloudIMAPHost) {
		if conn, err := dialICloudIMAPTLSIPs(ctx, serverName, port, iCloudIMAPFallbackIPv4); err == nil {
			return conn, nil
		} else {
			lastErr = err
		}
	}
	ips, lookupErr := lookupICloudIMAPIPv4(ctx, serverName)
	if len(appendUniqueIPv4(nil, ips...)) > 0 {
		if conn, err := dialICloudIMAPTLSIPs(ctx, serverName, port, ips); err == nil {
			return conn, nil
		} else if err != nil {
			lastErr = err
		}
	}
	address := net.JoinHostPort(serverName, strconv.Itoa(port))
	dialer := newICloudIMAPDialer(serverName)
	conn, err := dialer.DialContext(ctx, "tcp4", address)
	if err == nil {
		return conn, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("IMAP IPv4 直连失败：%v；域名拨号失败：%w", lastErr, err)
	}
	if lookupErr != nil {
		return nil, fmt.Errorf("IMAP DNS-over-TCP 解析失败：%v；域名拨号失败：%w", lookupErr, err)
	}
	return nil, err
}

func dialICloudIMAPTLSIPs(ctx context.Context, serverName string, port int, ips []net.IP) (net.Conn, error) {
	ips = appendUniqueIPv4(nil, ips...)
	if len(ips) == 0 {
		return nil, errors.New("没有可用的 IPv4 地址")
	}
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	results := make(chan result, len(ips))
	var wg sync.WaitGroup
	for _, ip := range ips {
		ip := ip
		wg.Add(1)
		go func() {
			defer wg.Done()
			address := net.JoinHostPort(ip.String(), strconv.Itoa(port))
			dialer := tls.Dialer{
				NetDialer: &net.Dialer{Timeout: 5 * time.Second},
				Config: &tls.Config{
					ServerName: serverName,
					MinVersion: tls.VersionTLS12,
				},
			}
			conn, err := dialer.DialContext(dialCtx, "tcp4", address)
			select {
			case results <- result{conn: conn, err: err}:
			case <-dialCtx.Done():
				if conn != nil {
					_ = conn.Close()
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	var errs []string
	for item := range results {
		if item.err == nil && item.conn != nil {
			cancel()
			return item.conn, nil
		}
		if item.conn != nil {
			_ = item.conn.Close()
		}
		if item.err != nil && !errors.Is(item.err, context.Canceled) {
			errs = append(errs, item.err.Error())
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if len(errs) == 0 {
		return nil, errors.New("所有 IMAP IPv4 连接均失败")
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

func appendUniqueIPv4(base []net.IP, extra ...net.IP) []net.IP {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]net.IP, 0, len(base)+len(extra))
	for _, ip := range base {
		if ip == nil || ip.To4() == nil {
			continue
		}
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ip)
	}
	for _, ip := range extra {
		if ip == nil || ip.To4() == nil {
			continue
		}
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ip)
	}
	return out
}

func newICloudIMAPResolver(servers []string) *net.Resolver {
	cleaned := make([]string, 0, len(servers))
	for _, server := range servers {
		server = strings.TrimSpace(server)
		if server != "" {
			cleaned = append(cleaned, server)
		}
	}
	if len(cleaned) == 0 {
		cleaned = []string{"1.1.1.1:53"}
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var lastErr error
			for _, dnsNetwork := range dnsFallbackNetworks(network) {
				for _, server := range cleaned {
					dialer := net.Dialer{Timeout: 2 * time.Second}
					conn, err := dialer.DialContext(ctx, dnsNetwork, server)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, lastErr
		},
	}
}

func lookupICloudIMAPIPv4(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, errors.New("主机地址为空")
	}
	var messages []string
	seen := make(map[string]struct{})
	var out []net.IP
	for _, server := range iCloudIMAPDNSServers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		ips, err := lookupDNSAOverTCP(ctx, server, host)
		if err != nil {
			messages = append(messages, server+": "+err.Error())
			continue
		}
		for _, ip := range ips {
			if ip == nil || ip.To4() == nil {
				continue
			}
			key := ip.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ip)
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	if len(messages) == 0 {
		return nil, errors.New("没有配置 DNS 服务器")
	}
	return nil, errors.New(strings.Join(messages, "; "))
}

func lookupDNSAOverTCP(ctx context.Context, server, host string) ([]net.IP, error) {
	queryID := uint16(time.Now().UnixNano())
	query, err := dnsBuildAQuery(host, queryID)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp4", server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	prefix := make([]byte, 2)
	binary.BigEndian.PutUint16(prefix, uint16(len(query)))
	if _, err := conn.Write(prefix); err != nil {
		return nil, err
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(conn, prefix); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(prefix))
	if size <= 0 || size > 65535 {
		return nil, fmt.Errorf("DNS 响应长度无效：%d", size)
	}
	response := make([]byte, size)
	if _, err := io.ReadFull(conn, response); err != nil {
		return nil, err
	}
	return dnsParseAResponse(response, queryID)
}

func dnsBuildAQuery(host string, id uint16) ([]byte, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, errors.New("DNS 主机为空")
	}
	buf := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(buf[0:2], id)
	binary.BigEndian.PutUint16(buf[2:4], 0x0100)
	binary.BigEndian.PutUint16(buf[4:6], 1)
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return nil, fmt.Errorf("DNS 标签为空：%q", host)
		}
		if len(label) > 63 {
			return nil, fmt.Errorf("DNS 标签过长：%q", host)
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 1)
	buf = binary.BigEndian.AppendUint16(buf, 1)
	return buf, nil
}

func dnsParseAResponse(message []byte, id uint16) ([]net.IP, error) {
	if len(message) < 12 {
		return nil, errors.New("DNS 响应过短")
	}
	if binary.BigEndian.Uint16(message[0:2]) != id {
		return nil, errors.New("DNS 响应 ID 不匹配")
	}
	flags := binary.BigEndian.Uint16(message[2:4])
	if flags&0x000f != 0 {
		return nil, fmt.Errorf("DNS 响应码：%d", flags&0x000f)
	}
	qdCount := int(binary.BigEndian.Uint16(message[4:6]))
	anCount := int(binary.BigEndian.Uint16(message[6:8]))
	offset := 12
	var err error
	for i := 0; i < qdCount; i++ {
		offset, err = dnsSkipName(message, offset)
		if err != nil {
			return nil, err
		}
		if offset+4 > len(message) {
			return nil, errors.New("DNS 问题段过短")
		}
		offset += 4
	}
	var out []net.IP
	for i := 0; i < anCount; i++ {
		offset, err = dnsSkipName(message, offset)
		if err != nil {
			return nil, err
		}
		if offset+10 > len(message) {
			return nil, errors.New("DNS 答案段过短")
		}
		recordType := binary.BigEndian.Uint16(message[offset : offset+2])
		recordClass := binary.BigEndian.Uint16(message[offset+2 : offset+4])
		rdLength := int(binary.BigEndian.Uint16(message[offset+8 : offset+10]))
		offset += 10
		if offset+rdLength > len(message) {
			return nil, errors.New("DNS RDATA 过短")
		}
		if recordType == 1 && recordClass == 1 && rdLength == 4 {
			out = append(out, net.IPv4(message[offset], message[offset+1], message[offset+2], message[offset+3]))
		}
		offset += rdLength
	}
	if len(out) == 0 {
		return nil, errors.New("DNS 响应没有 A 记录")
	}
	return out, nil
}

func dnsSkipName(message []byte, offset int) (int, error) {
	for jumps := 0; ; jumps++ {
		if jumps > 128 {
			return 0, errors.New("DNS 名称压缩出现循环")
		}
		if offset >= len(message) {
			return 0, errors.New("DNS 名称过短")
		}
		length := int(message[offset])
		if length&0xc0 == 0xc0 {
			if offset+1 >= len(message) {
				return 0, errors.New("DNS 压缩指针过短")
			}
			return offset + 2, nil
		}
		if length&0xc0 != 0 {
			return 0, errors.New("DNS 标签无效")
		}
		offset++
		if length == 0 {
			return offset, nil
		}
		if offset+length > len(message) {
			return 0, errors.New("DNS 标签过短")
		}
		offset += length
	}
}

func dnsFallbackNetworks(network string) []string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "udp", "udp4", "udp6":
		return []string{"tcp", "udp"}
	case "tcp", "tcp4", "tcp6":
		return []string{"tcp", "udp"}
	case "":
		return []string{"tcp", "udp"}
	default:
		return []string{network, "udp", "tcp"}
	}
}

func CheckICloudIMAPLogin(ctx context.Context, email, appPassword string) error {
	email = strings.TrimSpace(email)
	appPassword = strings.TrimSpace(appPassword)
	if email == "" {
		return errCode("imap_email_missing", "请输入 iCloud 邮箱账号", false)
	}
	if appPassword == "" {
		return errCode("imap_app_password_missing", "请输入 App 专用密码", false)
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	conn, err := dialICloudIMAPTLS(ctx, defaultICloudIMAPHost, defaultICloudIMAPPort)
	if err != nil {
		return errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(25 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}

	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(email)+" "+imapQuote(appPassword))
	if err != nil {
		return errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}

	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
	return nil
}

func WatchICloudIMAPExists(ctx context.Context, state LoginState, onReady, onExists func()) error {
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(state.IMAPUsername)+" "+imapQuote(state.IMAPAppPassword))
	if err != nil {
		return errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	if onReady != nil {
		onReady()
	}

	tagIndex := 3
	for ctx.Err() == nil {
		tag := fmt.Sprintf("A%03d", tagIndex)
		tagIndex++
		if err := imapWaitForExists(ctx, conn, reader, tag); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if onExists != nil {
			onExists()
		}
	}
	return ctx.Err()
}

func imapWaitForExists(ctx context.Context, conn net.Conn, reader *bufio.Reader, tag string) error {
	if _, err := fmt.Fprintf(conn, "%s IDLE\r\n", tag); err != nil {
		return err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "+") {
		return fmt.Errorf("IMAP IDLE 未进入等待状态：%s", strings.TrimSpace(line))
	}
	for ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(imapIdleReadTimeout))
		line, err = reader.ReadString('\n')
		if err != nil {
			_, _ = fmt.Fprint(conn, "DONE\r\n")
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if imapLineHasExistsEvent(line) {
			_, _ = fmt.Fprint(conn, "DONE\r\n")
			_, _ = imapReadUntilTag(reader, tag)
			return nil
		}
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			return nil
		}
	}
	_, _ = fmt.Fprint(conn, "DONE\r\n")
	_, _ = imapReadUntilTag(reader, tag)
	return ctx.Err()
}

func imapReadUntilTag(reader *bufio.Reader, tag string) ([]string, error) {
	lines := make([]string, 0, 4)
	for i := 0; i < 200; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return lines, err
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, strings.TrimSpace(line))
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			return lines, nil
		}
	}
	return lines, errors.New("IMAP IDLE 响应过长")
}

func imapCommand(conn net.Conn, reader *bufio.Reader, tag, command string) ([]string, error) {
	lines, _, err := imapCommandWithLiterals(conn, reader, tag, command)
	return lines, err
}

func imapCommandWithLiterals(conn net.Conn, reader *bufio.Reader, tag, command string) ([]string, [][]byte, error) {
	if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, command); err != nil {
		return nil, nil, err
	}
	lines := make([]string, 0, 4)
	literals := make([][]byte, 0)
	for i := 0; i < 200; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return lines, literals, err
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, strings.TrimSpace(line))
		if n, ok := imapLiteralSize(line); ok {
			body := make([]byte, n)
			if _, err := io.ReadFull(reader, body); err != nil {
				return lines, literals, err
			}
			literals = append(literals, body)
		}
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			return lines, literals, nil
		}
	}
	return lines, literals, errors.New("IMAP 响应过长")
}

func imapTaggedOK(lines []string, tag string) bool {
	for _, line := range lines {
		upper := strings.ToUpper(strings.TrimSpace(line))
		if strings.HasPrefix(upper, strings.ToUpper(tag)+" ") {
			return strings.Contains(upper, " OK")
		}
	}
	return false
}

func imapQuote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	return `"` + value + `"`
}

func imapResponseSummary(lines []string) string {
	joined := strings.Join(lines, "；")
	return trimForError([]byte(joined))
}

func SyncICloudIMAPMessages(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int) (map[string][]ICloudSyncedMessage, error) {
	result, err := SyncICloudIMAPMessagesWithCursor(ctx, state, mailboxes, after, keyword, maxMessages)
	if err != nil {
		return nil, err
	}
	return result.MessagesByMailbox, nil
}

// FetchICloudIMAPMessageByUID 按 UID 重新读取一封完整邮件，用于补全旧缓存中的 HTML 正文。
func FetchICloudIMAPMessageByUID(ctx context.Context, state LoginState, uid string) (ICloudSyncedMessage, error) {
	messages, err := FetchICloudIMAPMessagesByUID(ctx, state, []string{uid})
	if err != nil {
		return ICloudSyncedMessage{}, err
	}
	uid = strconv.Itoa(imapUIDNumber(uid))
	message, ok := messages[uid]
	if !ok {
		return ICloudSyncedMessage{}, errCode("imap_message_not_found", "iCloud IMAP 中未找到这封邮件", true)
	}
	return message, nil
}

// FetchICloudIMAPMessagesByUID 复用一个 IMAP 连接批量读取完整邮件正文。
func FetchICloudIMAPMessagesByUID(ctx context.Context, state LoginState, uids []string) (map[string]ICloudSyncedMessage, error) {
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return nil, err
	}
	uidNumbers := make([]int, 0, len(uids))
	seen := map[int]bool{}
	for _, uid := range uids {
		uidNumber := imapUIDNumber(uid)
		if uidNumber > 0 && !seen[uidNumber] {
			seen[uidNumber] = true
			uidNumbers = append(uidNumbers, uidNumber)
		}
	}
	if len(uidNumbers) == 0 {
		return nil, errCode("invalid_imap_uid", "邮件 UID 无效", false)
	}
	sortInts(uidNumbers)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return nil, errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(strings.ToUpper(greeting), "OK") {
		if err != nil {
			return nil, errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
		}
		return nil, errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+strings.TrimSpace(greeting), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(state.IMAPUsername)+" "+imapQuote(state.IMAPAppPassword))
	if err != nil {
		return nil, errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return nil, errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return nil, errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return nil, errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	out := make(map[string]ICloudSyncedMessage, len(uidNumbers))
	for index, chunk := range chunkInts(uidNumbers, 8) {
		tag := fmt.Sprintf("A%03d", index+3)
		lines, literals, err := imapCommandWithLiterals(conn, reader, tag, "UID FETCH "+imapUIDSet(chunk)+" (UID BODY.PEEK[]<0.4194304>)")
		if err != nil {
			return out, errCode("imap_fetch_failed", "批量读取完整邮件失败："+err.Error(), true)
		}
		if !imapTaggedOK(lines, tag) {
			return out, errCode("imap_fetch_failed", "批量读取完整邮件失败："+imapResponseSummary(lines), true)
		}
		fetchUIDs := imapFetchUIDs(lines)
		for literalIndex, raw := range literals {
			if literalIndex >= len(fetchUIDs) {
				continue
			}
			uid := strconv.Itoa(fetchUIDs[literalIndex])
			message, _, ok := parseICloudIMAPMessage(iCloudIMAPFetchedMessage{UID: uid, Raw: raw})
			if ok {
				out[uid] = message
			}
		}
	}
	_, _ = imapCommand(conn, reader, fmt.Sprintf("A%03d", len(chunkInts(uidNumbers, 8))+3), "LOGOUT")
	return out, nil
}

func LatestICloudIMAPUID(ctx context.Context, state LoginState) (string, error) {
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return "", errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return "", errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return "", errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(state.IMAPUsername)+" "+imapQuote(state.IMAPAppPassword))
	if err != nil {
		return "", errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return "", errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return "", errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return "", errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
	uid := imapSelectLastUID(selectLines)
	if uid <= 0 {
		return "", nil
	}
	return strconv.Itoa(uid), nil
}

func SyncICloudIMAPMessagesWithCursor(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int) (iCloudIMAPSyncResult, error) {
	return syncICloudIMAPMessages(ctx, state, mailboxes, MailSyncOptions{
		Mode:      MailSyncModeVerification,
		Keyword:   keyword,
		After:     after,
		Limit:     maxMessages,
		UseCursor: true,
		CursorUID: state.IMAPLastSyncUID,
	})
}

func syncICloudIMAPMessages(ctx context.Context, state LoginState, mailboxes []Mailbox, options MailSyncOptions) (iCloudIMAPSyncResult, error) {
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return iCloudIMAPSyncResult{}, err
	}
	maxMessages := options.Limit
	if options.FullScan {
		maxMessages = 0
	} else {
		if maxMessages <= 0 {
			maxMessages = 50
		}
		if maxMessages > 200 {
			maxMessages = 200
		}
	}
	keyword := strings.TrimSpace(options.Keyword)
	if options.VerificationOnly() && keyword == "" {
		keyword = "OpenAI"
	}
	if len(mailboxes) == 0 {
		return iCloudIMAPSyncResult{MessagesByMailbox: map[string][]ICloudSyncedMessage{}}, nil
	}

	syncTimeout := 60 * time.Second
	if options.FullScan {
		syncTimeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(syncTimeout))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return iCloudIMAPSyncResult{}, errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(state.IMAPUsername)+" "+imapQuote(state.IMAPAppPassword))
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return iCloudIMAPSyncResult{}, errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return iCloudIMAPSyncResult{}, errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	uidValidity := imapSelectUIDValidity(selectLines)
	snapshotHighUID := imapSelectLastUID(selectLines)
	cursorUID := strings.TrimSpace(options.CursorUID)
	if cursorUID == "" && options.UseCursor && !options.FullScan {
		cursorUID = strings.TrimSpace(state.IMAPLastSyncUID)
	}
	savedValidity := strings.TrimSpace(options.CursorUIDValidity)
	uidValidityChanged := savedValidity != "" && uidValidity != "" && savedValidity != uidValidity
	useSearchCursor := options.UseCursor && !options.FullScan && !uidValidityChanged
	useMailboxCursor := useSearchCursor && savedValidity == ""
	if uidValidityChanged {
		cursorUID = ""
		useMailboxCursor = false
		options.Mode = MailSyncModeAllRecent
		options.FullScan = true
		options.After = time.Time{}
		maxMessages = 0
	}
	if useSearchCursor && imapUIDNumber(cursorUID) >= snapshotHighUID {
		_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
		return iCloudIMAPSyncResult{MessagesByMailbox: map[string][]ICloudSyncedMessage{}, UIDValidity: uidValidity, LastUID: cursorUID}, nil
	}
	if snapshotHighUID <= 0 {
		_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
		return iCloudIMAPSyncResult{MessagesByMailbox: map[string][]ICloudSyncedMessage{}, UIDValidity: uidValidity}, nil
	}
	searchCommand, incremental := imapSearchCommand(cursorUID, mailboxes, options.After, useSearchCursor, useMailboxCursor, snapshotHighUID)
	searchLines, err := imapCommand(conn, reader, "A003", searchCommand)
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_search_failed", "搜索 iCloud IMAP 邮件失败："+err.Error(), true)
	}
	if !imapTaggedOK(searchLines, "A003") {
		return iCloudIMAPSyncResult{}, errCode("imap_search_failed", "搜索 iCloud IMAP 邮件失败："+imapResponseSummary(searchLines), true)
	}
	uids := imapSearchUIDs(searchLines)
	if len(uids) == 0 {
		_, _ = imapCommand(conn, reader, "A004", "LOGOUT")
		lastUID := cursorUID
		if snapshotHighUID > imapUIDNumber(lastUID) {
			lastUID = strconv.Itoa(snapshotHighUID)
		}
		return iCloudIMAPSyncResult{MessagesByMailbox: map[string][]ICloudSyncedMessage{}, UIDValidity: uidValidity, LastUID: lastUID}, nil
	}
	sortInts(uids)
	totalUIDs := len(uids)
	initializedCursor := !options.FullScan && !incremental && totalUIDs > maxMessages
	if !options.FullScan {
		if incremental {
			uids = firstIntValues(uids, maxMessages)
		} else {
			uids = lastIntValues(uids, maxMessages)
		}
	}
	lastUID := ""
	if len(uids) > 0 {
		lastUID = strconv.Itoa(uids[len(uids)-1])
	}
	if totalUIDs == len(uids) && snapshotHighUID > imapUIDNumber(lastUID) {
		lastUID = strconv.Itoa(snapshotHighUID)
	}

	tag := 4
	messages := make(map[string][]ICloudSyncedMessage)
	discovered := make(map[string][]ICloudSyncedMessage)
	indexEntries := make([]MailIndexEntry, 0, len(uids))
	scanned := 0
	uidBatches := [][]int{uids}
	if options.FullScan {
		uidBatches = chunkInts(uids, 200)
	}
	for _, uidBatch := range uidBatches {
		batchMessages, batchDiscovered, batchIndex, batchScanned, fetchErr := fetchAndMatchICloudIMAPMessages(conn, reader, &tag, uidBatch, mailboxes, options, keyword, state)
		if fetchErr != nil {
			return iCloudIMAPSyncResult{}, fetchErr
		}
		scanned += batchScanned
		indexEntries = append(indexEntries, batchIndex...)
		for mailboxID, items := range batchMessages {
			messages[mailboxID] = append(messages[mailboxID], items...)
		}
		for email, items := range batchDiscovered {
			discovered[email] = append(discovered[email], items...)
		}
	}
	_, _ = imapCommand(conn, reader, fmt.Sprintf("A%03d", tag+1), "LOGOUT")
	result := iCloudIMAPSyncResult{
		MessagesByMailbox: messages,
		DiscoveredByEmail: discovered,
		UIDValidity:       uidValidity,
		LastUID:           lastUID,
		Scanned:           scanned,
		HasMore:           !options.FullScan && totalUIDs > len(uids),
		InitializedCursor: initializedCursor,
		IndexEntries:      indexEntries,
	}
	result.Matched = countMailSyncMatches(result)
	return result, nil
}

func fetchAndMatchICloudIMAPMessages(conn net.Conn, reader *bufio.Reader, tag *int, uids []int, mailboxes []Mailbox, options MailSyncOptions, keyword string, state LoginState) (map[string][]ICloudSyncedMessage, map[string][]ICloudSyncedMessage, []MailIndexEntry, int, error) {
	headers, err := fetchICloudIMAPRawHeaders(conn, reader, tag, uids, 50)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	aliases := mailboxAliases(mailboxes, state.IMAPEmail, state.IMAPUsername)
	matchedUIDs := make([]int, 0)
	indexEntries := make([]MailIndexEntry, 0, len(headers))
	seenMatched := make(map[int]bool)
	for _, header := range headers {
		entry, recipients, ok := parseICloudIMAPHeader(header)
		if !ok {
			continue
		}
		indexEntries = append(indexEntries, entry)
		uid := imapUIDNumber(entry.UID)
		if uid <= 0 || seenMatched[uid] {
			continue
		}
		if len(matchingMailboxIDs(recipients, aliases)) > 0 || len(matchingDiscoveredEmails(recipients, options.DiscoveryDomains, aliases)) > 0 {
			seenMatched[uid] = true
			matchedUIDs = append(matchedUIDs, uid)
		}
	}
	sortInts(matchedUIDs)
	fetched := make([]iCloudIMAPFetchedMessage, 0, len(matchedUIDs))
	if len(matchedUIDs) > 0 {
		fetched, err = fetchICloudIMAPRawMessages(conn, reader, tag, matchedUIDs, 8, 4<<20)
		if err != nil {
			return nil, nil, nil, 0, err
		}
	}
	matchingMailboxes := mailboxes
	matchingAfter := options.After
	if options.FullScan {
		matchingMailboxes = append([]Mailbox(nil), mailboxes...)
		for index := range matchingMailboxes {
			matchingMailboxes[index].LastSyncAt = time.Time{}
			matchingMailboxes[index].LastSyncUID = ""
		}
		matchingAfter = time.Time{}
	}
	messages, discovered := iCloudIMAPMessagesByMailboxWithDiscovery(fetched, matchingMailboxes, matchingAfter, keyword, options.VerificationOnly(), options.DiscoveryDomains, state.IMAPEmail, state.IMAPUsername)
	completeByUID := make(map[string]ICloudSyncedMessage, len(fetched))
	for _, item := range fetched {
		if message, _, ok := parseICloudIMAPMessage(item); ok {
			completeByUID[message.UID] = message
		}
	}
	for index := range indexEntries {
		if message, found := completeByUID[indexEntries[index].UID]; found {
			indexEntries[index] = mailIndexEntryFromMessage(indexEntries[index], message)
		}
	}
	return messages, discovered, indexEntries, len(headers), nil
}

func fetchICloudIMAPRawHeaders(conn net.Conn, reader *bufio.Reader, tag *int, uids []int, chunkSize int) ([]iCloudIMAPFetchedMessage, error) {
	if chunkSize <= 0 {
		chunkSize = 50
	}
	fetched := make([]iCloudIMAPFetchedMessage, 0, len(uids))
	for _, chunk := range chunkInts(uids, chunkSize) {
		*tag++
		tagName := fmt.Sprintf("A%03d", *tag)
		lines, literals, err := imapCommandWithLiterals(conn, reader, tagName, "UID FETCH "+imapUIDSet(chunk)+" (UID BODY.PEEK[HEADER])")
		if err != nil {
			return nil, errCode("imap_fetch_failed", "读取 iCloud IMAP 邮件头失败："+err.Error(), true)
		}
		if !imapTaggedOK(lines, tagName) {
			return nil, errCode("imap_fetch_failed", "读取 iCloud IMAP 邮件头失败："+imapResponseSummary(lines), true)
		}
		fetchUIDs := imapFetchUIDs(lines)
		for index, raw := range literals {
			if index < len(fetchUIDs) {
				fetched = append(fetched, iCloudIMAPFetchedMessage{UID: strconv.Itoa(fetchUIDs[index]), Raw: raw})
			}
		}
	}
	return fetched, nil
}

func mergeMessageMaps(groups ...map[string][]ICloudSyncedMessage) map[string][]ICloudSyncedMessage {
	out := make(map[string][]ICloudSyncedMessage)
	for _, group := range groups {
		for key, items := range group {
			out[key] = append(out[key], items...)
		}
	}
	return out
}

func fetchICloudIMAPRawMessages(conn net.Conn, reader *bufio.Reader, tag *int, uids []int, chunkSize, byteLimit int) ([]iCloudIMAPFetchedMessage, error) {
	fetched := make([]iCloudIMAPFetchedMessage, 0, len(uids))
	if byteLimit <= 0 {
		byteLimit = 64 << 10
	}
	for _, chunk := range chunkInts(uids, chunkSize) {
		*tag++
		tagName := fmt.Sprintf("A%03d", *tag)
		command := fmt.Sprintf("UID FETCH %s (UID BODY.PEEK[]<0.%d>)", imapUIDSet(chunk), byteLimit)
		lines, literals, err := imapCommandWithLiterals(conn, reader, tagName, command)
		if err != nil {
			return nil, errCode("imap_fetch_failed", "读取 iCloud IMAP 邮件失败："+err.Error(), true)
		}
		if !imapTaggedOK(lines, tagName) {
			return nil, errCode("imap_fetch_failed", "读取 iCloud IMAP 邮件失败："+imapResponseSummary(lines), true)
		}
		fetchUIDs := imapFetchUIDs(lines)
		for index, raw := range literals {
			if index >= len(fetchUIDs) {
				continue
			}
			fetched = append(fetched, iCloudIMAPFetchedMessage{UID: strconv.Itoa(fetchUIDs[index]), Raw: raw})
		}
	}
	return fetched, nil
}

func matchedIMAPUIDs(messages map[string][]ICloudSyncedMessage) []int {
	seen := make(map[int]bool)
	var uids []int
	for _, items := range messages {
		for _, message := range items {
			uid := imapUIDNumber(message.UID)
			if uid <= 0 || seen[uid] {
				continue
			}
			seen[uid] = true
			uids = append(uids, uid)
		}
	}
	sortInts(uids)
	return uids
}

func imapSearchCommand(cursorUID string, mailboxes []Mailbox, after time.Time, useCursor, useMailboxCursor bool, highUID ...int) (string, bool) {
	high := "*"
	if len(highUID) > 0 && highUID[0] > 0 {
		high = strconv.Itoa(highUID[0])
	}
	if uid := imapUIDNumber(cursorUID); useCursor && uid > 0 {
		return "UID SEARCH UID " + strconv.Itoa(uid+1) + ":" + high, true
	}
	nextUID := 0
	for _, mailbox := range mailboxes {
		if !useMailboxCursor {
			break
		}
		uid := imapUIDNumber(mailbox.LastSyncUID)
		if uid <= 0 {
			nextUID = 0
			break
		}
		if nextUID == 0 || uid+1 < nextUID {
			nextUID = uid + 1
		}
	}
	if nextUID > 0 {
		return "UID SEARCH UID " + strconv.Itoa(nextUID) + ":" + high, true
	}
	searchAfter := after
	if searchAfter.IsZero() {
		if !useCursor {
			if high != "*" {
				return "UID SEARCH UID 1:" + high, false
			}
			return "UID SEARCH ALL", false
		}
		searchAfter = time.Now().Add(-24 * time.Hour)
	}
	command := "UID SEARCH SINCE " + searchAfter.Format("2-Jan-2006")
	if high != "*" {
		command += " UID 1:" + high
	}
	return command, false
}

func imapSelectUIDValidity(lines []string) string {
	for _, line := range lines {
		upper := strings.ToUpper(line)
		index := strings.Index(upper, "UIDVALIDITY")
		if index < 0 {
			continue
		}
		fields := strings.FieldsFunc(line[index+len("UIDVALIDITY"):], func(r rune) bool { return r < '0' || r > '9' })
		if len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

func imapSelectLastUID(lines []string) int {
	for _, line := range lines {
		upper := strings.ToUpper(line)
		idx := strings.Index(upper, "UIDNEXT")
		if idx < 0 {
			continue
		}
		tail := line[idx+len("UIDNEXT"):]
		fields := strings.FieldsFunc(tail, func(r rune) bool {
			return r < '0' || r > '9'
		})
		if len(fields) == 0 {
			continue
		}
		nextUID, err := strconv.Atoi(fields[0])
		if err == nil && nextUID > 1 {
			return nextUID - 1
		}
	}
	return 0
}

func imapUIDNumber(value string) int {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "imap:")
	uid, err := strconv.Atoi(value)
	if err != nil || uid <= 0 {
		return 0
	}
	return uid
}

func imapLineHasExistsEvent(line string) bool {
	parts := strings.Fields(strings.TrimSpace(line))
	return len(parts) >= 3 && parts[0] == "*" && strings.EqualFold(parts[2], "EXISTS")
}

type iCloudIMAPFetchedMessage struct {
	UID string
	Raw []byte
}

func iCloudIMAPMessagesByMailbox(fetched []iCloudIMAPFetchedMessage, mailboxes []Mailbox, after time.Time, keyword string, verificationOnly bool, ignoredEmails ...string) map[string][]ICloudSyncedMessage {
	messages, _ := iCloudIMAPMessagesByMailboxWithDiscovery(fetched, mailboxes, after, keyword, verificationOnly, nil, ignoredEmails...)
	return messages
}

func iCloudIMAPMessagesByMailboxWithDiscovery(fetched []iCloudIMAPFetchedMessage, mailboxes []Mailbox, after time.Time, keyword string, verificationOnly bool, discoveryDomains []string, ignoredEmails ...string) (map[string][]ICloudSyncedMessage, map[string][]ICloudSyncedMessage) {
	now := time.Now()
	aliases := make(map[string]string)
	afterByMailbox := make(map[string]time.Time)
	ignored := make(map[string]struct{}, len(ignoredEmails))
	for _, email := range ignoredEmails {
		if email = normalizeICloudIMAPEmail(email); email != "" {
			ignored[email] = struct{}{}
		}
	}
	for _, mailbox := range mailboxes {
		id := strings.TrimSpace(mailbox.ID)
		email := normalizeICloudIMAPEmail(mailbox.Email)
		if id == "" || email == "" {
			continue
		}
		if _, skip := ignored[email]; skip {
			continue
		}
		aliases[id] = email
		afterByMailbox[id] = mailboxSyncAfter(mailbox, after, now)
	}
	out := make(map[string][]ICloudSyncedMessage, len(aliases))
	discovered := make(map[string][]ICloudSyncedMessage)
	for _, item := range fetched {
		message, recipients, ok := parseICloudIMAPMessage(item)
		if !ok {
			continue
		}
		if verificationOnly && !looksLikeVerificationText(message.Subject+"\n"+message.Body, keyword) {
			continue
		}
		matchedMailboxIDs := matchingMailboxIDs(recipients, aliases)
		discoveredEmails := matchingDiscoveredEmails(recipients, discoveryDomains, aliases)
		if len(matchedMailboxIDs) == 0 && len(discoveredEmails) == 0 {
			continue
		}
		for _, mailboxID := range matchedMailboxIDs {
			if after := afterByMailbox[mailboxID]; !after.IsZero() && message.ReceivedAt.Before(after) {
				continue
			}
			out[mailboxID] = append(out[mailboxID], message)
		}
		for _, email := range discoveredEmails {
			discovered[email] = append(discovered[email], message)
		}
	}
	for mailboxID := range out {
		sortMessagesByReceivedAt(out[mailboxID])
	}
	for email := range discovered {
		sortMessagesByReceivedAt(discovered[email])
	}
	return out, discovered
}

func parseICloudIMAPMessage(item iCloudIMAPFetchedMessage) (ICloudSyncedMessage, string, bool) {
	msg, err := mail.ReadMessage(bytes.NewReader(item.Raw))
	if err != nil {
		return ICloudSyncedMessage{}, "", false
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(msg.Body, 4<<20))
	body := decodeICloudIMAPBody(msg.Header, bodyBytes)
	subject := decodeMIMEHeader(msg.Header.Get("Subject"))
	from := decodeMIMEHeader(msg.Header.Get("From"))
	receivedAt := time.Now()
	if date, err := msg.Header.Date(); err == nil {
		receivedAt = date
	}
	uid := strings.TrimSpace(item.UID)
	remoteID := "imap"
	if uid != "" {
		remoteID += ":" + uid
	}
	recipients := imapRecipientHeaderText(msg.Header)
	textBody := strings.TrimSpace(body.Text)
	if textBody == "" && strings.TrimSpace(body.HTML) != "" {
		textBody = normalizeMailBody(body.HTML)
	}
	contentType := "text/plain"
	if strings.TrimSpace(body.HTML) != "" {
		contentType = "text/html"
	}
	return ICloudSyncedMessage{
		RemoteID:    remoteID,
		RemoteIDs:   []string{remoteID},
		CanonicalID: canonicalMailID(msg.Header.Get("Message-ID"), from, subject, receivedAt),
		Source:      "imap",
		UID:         uid,
		Subject:     subject,
		From:        from,
		Body:        textBody,
		HTMLBody:    strings.TrimSpace(body.HTML),
		ContentType: contentType,
		ReceivedAt:  receivedAt,
		Recipients:  extractRecipientEmails(recipients),
	}, recipients, true
}

func parseICloudIMAPHeader(item iCloudIMAPFetchedMessage) (MailIndexEntry, string, bool) {
	msg, err := mail.ReadMessage(bytes.NewReader(item.Raw))
	if err != nil {
		return MailIndexEntry{}, "", false
	}
	subject := decodeMIMEHeader(msg.Header.Get("Subject"))
	from := decodeMIMEHeader(msg.Header.Get("From"))
	receivedAt := time.Now()
	if date, err := msg.Header.Date(); err == nil {
		receivedAt = date
	}
	uid := strings.TrimSpace(item.UID)
	remoteID := "imap"
	if uid != "" {
		remoteID += ":" + uid
	}
	recipientText := imapRecipientHeaderText(msg.Header)
	entry := MailIndexEntry{
		UID: uid, RemoteID: remoteID, RemoteIDs: []string{remoteID},
		CanonicalID: canonicalMailID(msg.Header.Get("Message-ID"), from, subject, receivedAt),
		Recipients:  extractRecipientEmails(recipientText), Source: "imap", Subject: subject, From: from,
		ContentType: "text/plain", ReceivedAt: receivedAt,
	}
	return entry, recipientText, uid != ""
}

func mailIndexEntryFromMessage(entry MailIndexEntry, message ICloudSyncedMessage) MailIndexEntry {
	entry.UID = firstNonEmpty(message.UID, entry.UID)
	entry.RemoteID = firstNonEmpty(message.RemoteID, entry.RemoteID)
	entry.RemoteIDs = uniqueStrings(append(append(entry.RemoteIDs, message.RemoteIDs...), entry.RemoteID))
	entry.CanonicalID = firstNonEmpty(message.CanonicalID, entry.CanonicalID)
	entry.Source = firstNonEmpty(message.Source, entry.Source)
	entry.Subject = firstNonEmpty(message.Subject, entry.Subject)
	entry.From = firstNonEmpty(message.From, entry.From)
	entry.Body = message.Body
	entry.HTMLBody = message.HTMLBody
	entry.ContentType = firstNonEmpty(message.ContentType, entry.ContentType)
	if !message.ReceivedAt.IsZero() {
		entry.ReceivedAt = message.ReceivedAt
	}
	entry.BodyComplete = true
	return entry
}

func mailboxAliases(mailboxes []Mailbox, ignoredEmails ...string) map[string]string {
	ignored := make(map[string]bool, len(ignoredEmails))
	for _, email := range ignoredEmails {
		if email = normalizeICloudIMAPEmail(email); email != "" {
			ignored[email] = true
		}
	}
	aliases := make(map[string]string, len(mailboxes))
	for _, mailbox := range mailboxes {
		email := normalizeICloudIMAPEmail(mailbox.Email)
		if strings.TrimSpace(mailbox.ID) != "" && email != "" && !ignored[email] {
			aliases[mailbox.ID] = email
		}
	}
	return aliases
}

func extractRecipientEmails(text string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, match := range recipientEmailPattern.FindAllString(text, -1) {
		email := strings.ToLower(strings.Trim(match, `"'<>[](),;`))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		out = append(out, email)
	}
	sort.Strings(out)
	return out
}

func normalizeICloudIMAPState(state LoginState) (LoginState, error) {
	email := normalizeICloudIMAPEmail(state.IMAPEmail)
	if email == "" {
		email = normalizeICloudIMAPEmail(state.IMAPUsername)
	}
	if email == "" {
		return LoginState{}, errCode("imap_session_missing", "未保存取码登录，请先保存 iCloud 邮箱账号和 App 专用密码", true)
	}
	appPassword := strings.TrimSpace(state.IMAPAppPassword)
	if appPassword == "" {
		return LoginState{}, errCode("imap_app_password_missing", "取码登录缺少 App 专用密码，请重新保存取码登录", false)
	}
	state.IMAPEmail = email
	state.IMAPUsername = firstNonEmpty(strings.TrimSpace(state.IMAPUsername), email)
	state.IMAPHost = firstNonEmpty(strings.TrimSpace(state.IMAPHost), defaultICloudIMAPHost)
	if state.IMAPPort == 0 {
		state.IMAPPort = defaultICloudIMAPPort
	}
	state.IMAPAppPassword = appPassword
	return state, nil
}

func imapLiteralSize(line string) (int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	start := strings.LastIndex(line, "{")
	if start < 0 || start+1 >= len(line) {
		return 0, false
	}
	value := strings.TrimSuffix(line[start+1:], "}")
	value = strings.TrimSuffix(value, "+")
	n, err := strconv.Atoi(value)
	return n, err == nil && n >= 0
}

func imapSearchUIDs(lines []string) []int {
	var out []int
	for _, line := range lines {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "* SEARCH") {
			continue
		}
		for _, part := range strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "* SEARCH"))) {
			n, err := strconv.Atoi(part)
			if err == nil && n > 0 {
				out = append(out, n)
			}
		}
	}
	return out
}

func imapFetchUIDs(lines []string) []int {
	var out []int
	for _, line := range lines {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if strings.EqualFold(strings.Trim(fields[i], "()"), "UID") {
				value := strings.Trim(fields[i+1], "()")
				if n, err := strconv.Atoi(value); err == nil && n > 0 {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func lastIntValues(values []int, limit int) []int {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return values[len(values)-limit:]
}

func firstIntValues(values []int, limit int) []int {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return values[:limit]
}

func chunkInts(values []int, size int) [][]int {
	if size <= 0 {
		size = len(values)
	}
	var out [][]int
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		out = append(out, values[start:end])
	}
	return out
}

func imapUIDSet(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func imapHeaderText(header mail.Header) string {
	var out []string
	for key, values := range header {
		for _, value := range values {
			out = append(out, key+": "+decodeMIMEHeader(value))
		}
	}
	return strings.Join(out, "\n")
}

func imapRecipientHeaderText(header mail.Header) string {
	keys := []string{
		"To", "Cc", "Bcc", "Delivered-To", "X-Original-To", "Envelope-To", "X-Envelope-To",
		"Resent-To", "Apparently-To", "X-Forwarded-To", "Original-Recipient", "X-Original-Recipient",
	}
	var out []string
	for _, key := range keys {
		for _, value := range header[textproto.CanonicalMIMEHeaderKey(key)] {
			out = append(out, key+": "+decodeMIMEHeader(value))
		}
	}
	return strings.Join(out, "\n")
}

func decodeMIMEHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decoded)
}

type iCloudIMAPDecodedBody struct {
	Text string
	HTML string
}

func decodeICloudIMAPBody(header mail.Header, body []byte) iCloudIMAPDecodedBody {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(header.Get("Content-Type")))
	}
	decoded := decodeICloudIMAPTransfer(header.Get("Content-Transfer-Encoding"), body)
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return iCloudIMAPDecodedBody{Text: string(decoded)}
		}
		reader := multipart.NewReader(bytes.NewReader(decoded), boundary)
		var textParts []string
		var htmlParts []string
		for i := 0; i < 30; i++ {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			partBody, _ := io.ReadAll(io.LimitReader(part, 4<<20))
			content := decodeICloudIMAPBody(mail.Header(part.Header), partBody)
			if strings.TrimSpace(content.Text) != "" {
				textParts = append(textParts, content.Text)
			}
			if strings.TrimSpace(content.HTML) != "" {
				htmlParts = append(htmlParts, content.HTML)
			}
		}
		return iCloudIMAPDecodedBody{Text: strings.Join(textParts, "\n"), HTML: strings.Join(htmlParts, "\n")}
	}
	switch strings.ToLower(mediaType) {
	case "text/html":
		return iCloudIMAPDecodedBody{HTML: string(decoded)}
	case "message/rfc822":
		nested, err := mail.ReadMessage(bytes.NewReader(decoded))
		if err == nil {
			nestedBody, _ := io.ReadAll(io.LimitReader(nested.Body, 4<<20))
			return decodeICloudIMAPBody(nested.Header, nestedBody)
		}
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "text/") || mediaType == "" {
		return iCloudIMAPDecodedBody{Text: string(decoded)}
	}
	return iCloudIMAPDecodedBody{}
}

func decodeICloudIMAPTransfer(encoding string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err == nil {
			return decoded
		}
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(body)))
		if err == nil {
			return decoded
		}
	}
	return body
}

func sortMessagesByReceivedAt(messages []ICloudSyncedMessage) {
	for i := 1; i < len(messages); i++ {
		for j := i; j > 0 && messages[j].ReceivedAt.After(messages[j-1].ReceivedAt); j-- {
			messages[j], messages[j-1] = messages[j-1], messages[j]
		}
	}
}
