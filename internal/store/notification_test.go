package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNextDailyServerChanNotificationSequenceResetsAtChinaMidnightAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	state, err := Open(path)
	if err != nil {
		t.Fatalf("创建测试数据库失败：%v", err)
	}
	beforeMidnight := time.Date(2026, 8, 30, 15, 59, 0, 0, time.UTC)
	for expected := 1; expected <= 2; expected++ {
		sequence, sequenceErr := state.NextDailyServerChanNotificationSequence(beforeMidnight)
		if sequenceErr != nil || sequence != expected {
			t.Fatalf("当日通知序号不正确：sequence=%d err=%v", sequence, sequenceErr)
		}
	}
	if err = state.Close(); err != nil {
		t.Fatalf("关闭测试数据库失败：%v", err)
	}

	state, err = Open(path)
	if err != nil {
		t.Fatalf("重新打开测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	sequence, err := state.NextDailyServerChanNotificationSequence(beforeMidnight)
	if err != nil || sequence != 3 {
		t.Fatalf("服务重启后未延续当日通知序号：sequence=%d err=%v", sequence, err)
	}
	afterMidnight := time.Date(2026, 8, 30, 16, 0, 0, 0, time.UTC)
	sequence, err = state.NextDailyServerChanNotificationSequence(afterMidnight)
	if err != nil || sequence != 1 {
		t.Fatalf("中国时间零点后通知序号未重置：sequence=%d err=%v", sequence, err)
	}
}
