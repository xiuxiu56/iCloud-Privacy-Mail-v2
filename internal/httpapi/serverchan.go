package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/serverchan"
)

type loginStateHealth struct {
	CheckedAt time.Time
	OK        bool
	Message   string
}

func (s *Server) handleServerChanTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SendKey *string `json:"send_key"`
		HideIP  *bool   `json:"hide_ip"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	settings := s.serverChanSettings()
	if body.SendKey != nil {
		if value := strings.TrimSpace(*body.SendKey); value != "" {
			settings.ServerChanSendKey = value
		}
	}
	if body.HideIP != nil {
		settings.ServerChanHideIP = *body.HideIP
	}
	if err := validateServerChanSettings(settings); err != nil {
		writeError(w, http.StatusBadRequest, "server_chan_settings_invalid", err.Error())
		return
	}
	if strings.TrimSpace(settings.ServerChanSendKey) == "" {
		writeError(w, http.StatusBadRequest, "server_chan_not_configured", "请先输入 Server 酱 SendKey")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	message := s.numberServerChanMessage(serverchan.Message{
		Title: "iCloud Privacy Mail 测试通知",
		Desp:  fmt.Sprintf("这是一条 Server 酱配置测试消息。\n\n- 发送时间：%s\n- 结果：后端已成功提交推送任务", time.Now().Format("2006-01-02 15:04:05")),
		Short: "Server 酱配置测试成功",
	})
	result, err := s.serverChan.Send(ctx, serverChanOptions(settings), message)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_chan_test_failed", err.Error())
		return
	}
	_ = s.store.RecordEvent("info", "notification", "Server 酱测试推送已入队")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"message": "测试推送已加入 Server 酱队列",
		"push_id": result.PushID,
	}})
}

func (s *Server) notifyAdminLogin(r *http.Request, admin domain.Admin) {
	settings := s.serverChanSettings()
	if !settings.NotifyAdminLogin || strings.TrimSpace(settings.ServerChanSendKey) == "" {
		return
	}
	address := clientAddress(r)
	userAgent := strings.TrimSpace(r.UserAgent())
	if userAgent == "" {
		userAgent = "未提供"
	}
	s.sendServerChanAsync(settings, serverchan.Message{
		Title: "后台管理员已登录",
		Desp:  fmt.Sprintf("管理后台发生了一次成功登录。\n\n- 管理员：%s\n- 时间：%s\n- 访问地址：%s\n- 浏览器：%s", admin.Username, time.Now().Format("2006-01-02 15:04:05"), address, truncateText(userAgent, 240)),
		Short: fmt.Sprintf("管理员 %s 登录成功", admin.Username),
	})
}

func (s *Server) sendServerChanAsync(settings domain.Settings, message serverchan.Message) {
	sender := s.serverChan
	if sender == nil {
		return
	}
	message = s.numberServerChanMessage(message)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := sender.Send(ctx, serverChanOptions(settings), message); err != nil {
			s.log.Warn("Server 酱推送失败", "错误", err)
			_ = s.store.RecordEvent("warning", "notification", "Server 酱推送失败："+err.Error())
			return
		}
		s.log.Info("Server 酱推送已入队", "标题", message.Title)
		_ = s.store.RecordEvent("info", "notification", "Server 酱推送已入队："+message.Title)
	}()
}

func (s *Server) numberServerChanMessage(message serverchan.Message) serverchan.Message {
	sequence, err := s.store.NextDailyServerChanNotificationSequence(time.Now())
	if err != nil {
		s.log.Warn("Server 酱通知序号生成失败", "错误", err)
		return message
	}
	sequenceLabel := fmt.Sprintf("第%d次通知", sequence)
	message.Short = sequenceLabel + "｜" + firstNonEmptyText(message.Short, message.Title)
	message.Desp = fmt.Sprintf("- 通知序号：%s\n\n%s", sequenceLabel, strings.TrimSpace(message.Desp))
	return message
}

func (s *Server) startAccountLoginStateNotifications(ctx context.Context) {
	changes, unsubscribe := s.store.SubscribeChanges(32)
	previous := loginStateHealthSnapshot(s.store.ICloudSessions())
	go func() {
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case change, open := <-changes:
				if !open {
					return
				}
				if change.Resource == "settings" {
					previous = loginStateHealthSnapshot(s.store.ICloudSessions())
					continue
				}
				if change.Resource != "apple-session" {
					continue
				}
				currentSessions := s.store.ICloudSessions()
				current := loginStateHealthSnapshot(currentSessions)
				settings := s.serverChanSettings()
				if settings.NotifyAccountLoginStateOffline && strings.TrimSpace(settings.ServerChanSendKey) != "" {
					s.notifyOfflineTransitions(settings, previous, current, currentSessions)
				}
				previous = current
			}
		}
	}()
}

func (s *Server) notifyOfflineTransitions(settings domain.Settings, previous, current map[string]loginStateHealth, sessions []domain.ICloudSession) {
	byAccount := make(map[string][]string)
	for key, after := range current {
		if after.CheckedAt.IsZero() || after.OK {
			continue
		}
		before, existed := previous[key]
		if existed && !before.OK && !before.CheckedAt.IsZero() {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		line := fmt.Sprintf("  - %s：%s", loginStateDisplayName(parts[1]), firstNonEmptyText(after.Message, "登录态检测失败"))
		byAccount[parts[0]] = append(byAccount[parts[0]], line)
	}
	if len(byAccount) == 0 {
		return
	}
	labels := make(map[string]string, len(sessions))
	for _, session := range sessions {
		labels[session.AccountID] = firstNonEmptyText(session.AppleID, session.AccountID, "未知账号")
	}
	accountIDs := make([]string, 0, len(byAccount))
	for accountID := range byAccount {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Strings(accountIDs)
	incidentSections := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		label := labels[accountID]
		incidentSections = append(incidentSections, fmt.Sprintf("- 掉线账号：%s\n%s", label, strings.Join(byAccount[accountID], "\n")))
	}
	primaryLabel := labels[accountIDs[0]]
	title := fmt.Sprintf("%s｜Apple 登录态掉线", primaryLabel)
	if len(accountIDs) > 1 {
		title = fmt.Sprintf("%s 等账号｜Apple 登录态掉线", primaryLabel)
	}
	s.sendServerChanAsync(settings, serverchan.Message{
		Title: title,
		Desp:  fmt.Sprintf("检测到以下 Apple 账号的登录态从正常转为异常。\n\n%s\n\n- 时间：%s\n\n请在后台的 Apple 账号详情中重新检测或登录。", strings.Join(incidentSections, "\n"), time.Now().Format("2006-01-02 15:04:05")),
		Short: title,
	})
}

func loginStateHealthSnapshot(sessions []domain.ICloudSession) map[string]loginStateHealth {
	out := make(map[string]loginStateHealth)
	for _, session := range sessions {
		accountID := firstNonEmptyText(session.AccountID, strings.ToLower(strings.TrimSpace(session.AppleID)), "default")
		for _, state := range session.LoginStates {
			kind := strings.TrimSpace(state.Kind)
			if kind == "" {
				continue
			}
			out[accountID+"\x00"+kind] = loginStateHealth{CheckedAt: state.LastCheckedAt, OK: state.LastCheckOK, Message: state.LastStatusMessage}
		}
	}
	return out
}

func serverChanOptions(settings domain.Settings) serverchan.Options {
	return serverchan.Options{
		SendKey: settings.ServerChanSendKey,
		HideIP:  settings.ServerChanHideIP,
	}
}

func (s *Server) serverChanSettings() domain.Settings {
	settings := s.store.Settings()
	if strings.TrimSpace(settings.ServerChanSendKey) != "" || strings.TrimSpace(s.cfg.ServerChanSendKey) == "" {
		return settings
	}
	settings.ServerChanSendKey = s.cfg.ServerChanSendKey
	settings.ServerChanHideIP = s.cfg.ServerChanHideIP
	settings.NotifyAdminLogin = s.cfg.ServerChanNotifyAdminLogin
	settings.NotifyAccountLoginStateOffline = s.cfg.ServerChanNotifyLoginStateOffline
	return settings
}

func validateServerChanSettings(settings domain.Settings) error {
	sendKey := strings.TrimSpace(settings.ServerChanSendKey)
	if len(sendKey) > 180 {
		return errors.New("Server 酱 SendKey 格式不正确")
	}
	for _, character := range sendKey {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return errors.New("Server 酱 SendKey 格式不正确")
		}
	}
	return nil
}

func maskServerChanSendKey(sendKey string) string {
	sendKey = strings.TrimSpace(sendKey)
	if sendKey == "" {
		return ""
	}
	runes := []rune(sendKey)
	if len(runes) <= 10 {
		return strings.Repeat("•", len(runes))
	}
	return string(runes[:6]) + strings.Repeat("•", 8) + string(runes[len(runes)-4:])
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	if value := strings.TrimSpace(r.RemoteAddr); value != "" {
		return value
	}
	return "未知"
}

func loginStateDisplayName(kind string) string {
	switch kind {
	case domain.LoginStateAppleAccount:
		return "Apple Account"
	case domain.LoginStateICloudWeb:
		return "iCloud Web"
	case domain.LoginStateICloudIMAP:
		return "IMAP 取码"
	default:
		return kind
	}
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func truncateText(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
