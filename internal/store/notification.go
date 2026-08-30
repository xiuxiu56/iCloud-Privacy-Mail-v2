package store

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	serverChanNotificationDateKey  = "server_chan_notification_date"
	serverChanNotificationCountKey = "server_chan_notification_count"
)

var chinaStandardTime = time.FixedZone("中国标准时间", 8*60*60)

// NextDailyServerChanNotificationSequence 按中国标准时间生成当日持久化的 Server 酱通知序号。
func (s *Store) NextDailyServerChanNotificationSequence(at time.Time) (int, error) {
	if at.IsZero() {
		at = time.Now()
	}
	currentDate := at.In(chinaStandardTime).Format("2006-01-02")

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	rollback := func() {
		_ = tx.Rollback()
	}

	storedDate := ""
	if err = tx.QueryRow(`SELECT value FROM metadata WHERE key = ?`, serverChanNotificationDateKey).Scan(&storedDate); err != nil && !errors.Is(err, sql.ErrNoRows) {
		rollback()
		return 0, err
	}
	count := 0
	if strings.TrimSpace(storedDate) == currentDate {
		var rawCount string
		if err = tx.QueryRow(`SELECT value FROM metadata WHERE key = ?`, serverChanNotificationCountKey).Scan(&rawCount); err != nil && !errors.Is(err, sql.ErrNoRows) {
			rollback()
			return 0, err
		}
		count, _ = strconv.Atoi(strings.TrimSpace(rawCount))
		if count < 0 {
			count = 0
		}
	}
	count++
	if _, err = tx.Exec(`INSERT INTO metadata(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, serverChanNotificationDateKey, currentDate); err != nil {
		rollback()
		return 0, err
	}
	if _, err = tx.Exec(`INSERT INTO metadata(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, serverChanNotificationCountKey, strconv.Itoa(count)); err != nil {
		rollback()
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
