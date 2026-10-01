package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/store"
)

const maxEvents = 200

type Config struct {
	AccountIDs                []string `json:"account_ids"`
	Label                     string   `json:"label"`
	Note                      string   `json:"note"`
	CreateChannel             string   `json:"create_channel"`
	IntervalMinMinutes        int      `json:"interval_min_minutes,omitempty"`
	IntervalMaxMinutes        int      `json:"interval_max_minutes,omitempty"`
	IntervalMinSeconds        int      `json:"interval_min_seconds,omitempty"`
	IntervalMaxSeconds        int      `json:"interval_max_seconds,omitempty"`
	AccountIntervalMinSeconds int      `json:"account_interval_min_seconds,omitempty"`
	AccountIntervalMaxSeconds int      `json:"account_interval_max_seconds,omitempty"`
}

type Event struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	AccountID string    `json:"account_id,omitempty"`
	MailboxID string    `json:"mailbox_id,omitempty"`
	Email     string    `json:"email,omitempty"`
	Label     string    `json:"label,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type State struct {
	Running                       bool      `json:"running"`
	Status                        string    `json:"status"`
	AccountIDs                    []string  `json:"account_ids"`
	FailedAccountIDs              []string  `json:"failed_account_ids"`
	Label                         string    `json:"label"`
	Note                          string    `json:"note"`
	CreateChannel                 string    `json:"create_channel"`
	CurrentIntervalSeconds        int       `json:"current_interval_seconds,omitempty"`
	IntervalMinSeconds            int       `json:"interval_min_seconds,omitempty"`
	IntervalMaxSeconds            int       `json:"interval_max_seconds,omitempty"`
	AccountIntervalMinSeconds     int       `json:"account_interval_min_seconds,omitempty"`
	AccountIntervalMaxSeconds     int       `json:"account_interval_max_seconds,omitempty"`
	CurrentAccountIntervalSeconds int       `json:"current_account_interval_seconds,omitempty"`
	BatchIndex                    int       `json:"batch_index"`
	Success                       int       `json:"success"`
	Failed                        int       `json:"failed"`
	StartedAt                     time.Time `json:"started_at,omitempty"`
	LastRunAt                     time.Time `json:"last_run_at,omitempty"`
	NextRunAt                     time.Time `json:"next_run_at,omitempty"`
	StoppedAt                     time.Time `json:"stopped_at,omitempty"`
	LastError                     string    `json:"last_error,omitempty"`
	Events                        []Event   `json:"events"`
}

type mailboxCreator interface {
	Create(context.Context, string, string, string, string) (domain.Mailbox, error)
}

type Service struct {
	mu         sync.Mutex
	store      *store.Store
	mailbox    mailboxCreator
	state      State
	cancel     context.CancelFunc
	nextID     int64
	generation uint64
}

func NewService(state *store.Store, mailbox mailboxCreator) *Service {
	service := &Service{store: state, mailbox: mailbox, state: State{Status: "idle", Events: []Event{}}}
	if state != nil {
		var persisted State
		if found, err := state.LoadRuntimeState("scheduler", &persisted); err == nil && found {
			service.state = persisted
			if service.state.Events == nil {
				service.state.Events = []Event{}
			}
			for _, event := range service.state.Events {
				if event.ID > service.nextID {
					service.nextID = event.ID
				}
			}
		}
	}
	return service
}

// Resume 恢复数据库中尚未结束的调度任务。
func (s *Service) Resume(parent context.Context) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Running || s.cancel != nil || s.mailbox == nil || s.store == nil {
		return s.snapshotLocked()
	}
	cfg := Config{
		AccountIDs: append([]string(nil), s.state.AccountIDs...), Label: s.state.Label, Note: s.state.Note,
		CreateChannel:             s.state.CreateChannel,
		IntervalMinSeconds:        s.state.IntervalMinSeconds,
		IntervalMaxSeconds:        s.state.IntervalMaxSeconds,
		AccountIntervalMinSeconds: s.state.AccountIntervalMinSeconds,
		AccountIntervalMaxSeconds: s.state.AccountIntervalMaxSeconds,
	}
	cfg = s.normalizeConfig(cfg)
	s.state.IntervalMinSeconds = cfg.IntervalMinSeconds
	s.state.IntervalMaxSeconds = cfg.IntervalMaxSeconds
	s.state.AccountIntervalMinSeconds = cfg.AccountIntervalMinSeconds
	s.state.AccountIntervalMaxSeconds = cfg.AccountIntervalMaxSeconds
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.generation++
	generation := s.generation
	s.state.Status = "recovering"
	s.addEventLocked(Event{Type: "resumed", Message: "已从数据库恢复定时创建任务"})
	resumeAt := s.state.NextRunAt
	go s.run(ctx, cfg, generation, resumeAt)
	return s.snapshotLocked()
}

func (s *Service) Start(parent context.Context, cfg Config) (State, error) {
	cfg = s.normalizeConfig(cfg)
	if len(cfg.AccountIDs) == 0 {
		return State{}, errors.New("请至少选择一个 Apple 账号")
	}

	s.mu.Lock()
	if s.state.Running {
		s.mu.Unlock()
		return State{}, errors.New("定时创建已经在运行，请先停止")
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.generation++
	generation := s.generation
	intervalMinSeconds, intervalMaxSeconds := s.intervalRange(cfg)
	s.state = State{
		Running:                       true,
		Status:                        "running",
		AccountIDs:                    append([]string(nil), cfg.AccountIDs...),
		FailedAccountIDs:              []string{},
		Label:                         cfg.Label,
		Note:                          cfg.Note,
		CreateChannel:                 cfg.CreateChannel,
		CurrentIntervalSeconds:        intervalMinSeconds,
		IntervalMinSeconds:            intervalMinSeconds,
		IntervalMaxSeconds:            intervalMaxSeconds,
		AccountIntervalMinSeconds:     cfg.AccountIntervalMinSeconds,
		AccountIntervalMaxSeconds:     cfg.AccountIntervalMaxSeconds,
		CurrentAccountIntervalSeconds: cfg.AccountIntervalMinSeconds,
		StartedAt:                     time.Now(),
		Events:                        []Event{},
	}
	s.addEventLocked(Event{Type: "started", Message: "定时创建已启动"})
	out := s.snapshotLocked()
	s.mu.Unlock()

	go s.run(ctx, cfg, generation, time.Time{})
	return out, nil
}

func (s *Service) Stop(message string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.generation++
	if s.state.Running {
		s.state.Running = false
		s.state.Status = "stopped"
		s.state.StoppedAt = time.Now()
		if strings.TrimSpace(message) == "" {
			message = "定时创建已停止"
		}
		s.addEventLocked(Event{Type: "stopped", Message: message})
	}
	return s.snapshotLocked()
}

func (s *Service) ClearEvents() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Events = []Event{}
	s.publishLocked()
	return s.snapshotLocked()
}

func (s *Service) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Service) RecordManualSuccess(accountID string, mailbox domain.Mailbox) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Success++
	s.state.LastRunAt = time.Now()
	s.state.LastError = ""
	s.addEventLocked(Event{Type: "created", AccountID: accountID, MailboxID: mailbox.ID, Email: mailbox.Email, Label: mailbox.Label, Message: "单次创建隐私邮箱成功"})
	return s.snapshotLocked()
}

func (s *Service) RecordManualFailure(accountID, label string, createErr error) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Failed++
	s.state.LastRunAt = time.Now()
	s.state.LastError = createErr.Error()
	s.addEventLocked(Event{Type: "failed", AccountID: accountID, Label: strings.TrimSpace(label), Message: "单次创建隐私邮箱失败", Error: createErr.Error()})
	return s.snapshotLocked()
}

func (s *Service) run(ctx context.Context, cfg Config, generation uint64, resumeAt time.Time) {
	if !resumeAt.IsZero() && time.Until(resumeAt) > 0 {
		timer := time.NewTimer(time.Until(resumeAt))
		select {
		case <-ctx.Done():
			timer.Stop()
			s.finish(generation)
			return
		case <-timer.C:
		}
	}
	for {
		if ctx.Err() != nil {
			s.finish(generation)
			return
		}
		s.runRound(ctx, cfg, generation)
		if ctx.Err() != nil {
			s.finish(generation)
			return
		}
		s.mu.Lock()
		if generation != s.generation || !s.state.Running {
			s.mu.Unlock()
			return
		}
		intervalSeconds := randomBetween(s.intervalRange(cfg))
		interval := time.Duration(intervalSeconds) * time.Second
		s.state.Status = "waiting"
		s.state.CurrentIntervalSeconds = intervalSeconds
		s.state.NextRunAt = time.Now().Add(interval)
		s.addEventLocked(Event{Type: "waiting", Message: fmt.Sprintf("等待 %s 后开始下一轮", interval)})
		s.mu.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.finish(generation)
			return
		case <-timer.C:
		}
	}
}

func (s *Service) runRound(ctx context.Context, cfg Config, generation uint64) {
	s.mu.Lock()
	if generation != s.generation || !s.state.Running {
		s.mu.Unlock()
		return
	}
	s.state.BatchIndex++
	batch := s.state.BatchIndex
	s.state.Status = "creating"
	s.state.LastRunAt = time.Now()
	s.state.NextRunAt = time.Time{}
	s.addEventLocked(Event{Type: "round_started", Message: fmt.Sprintf("开始第 %d 轮定时创建", batch)})
	failed := make(map[string]bool, len(s.state.FailedAccountIDs))
	for _, accountID := range s.state.FailedAccountIDs {
		failed[accountID] = true
	}
	s.mu.Unlock()

	activeIDs := make([]string, 0, len(cfg.AccountIDs))
	for _, accountID := range cfg.AccountIDs {
		if !failed[accountID] {
			activeIDs = append(activeIDs, accountID)
		}
	}
	if len(activeIDs) == 0 {
		s.mu.Lock()
		if generation == s.generation && s.state.Running {
			s.stopAfterAllAccountsFailedLocked()
		}
		s.mu.Unlock()
		return
	}

	for index, accountID := range activeIDs {
		if ctx.Err() != nil {
			return
		}
		mailbox, err := s.mailbox.Create(ctx, accountID, cfg.Label, cfg.Note, cfg.CreateChannel)
		s.mu.Lock()
		if generation != s.generation || !s.state.Running {
			s.mu.Unlock()
			return
		}
		if err != nil {
			s.state.Failed++
			s.state.LastError = err.Error()
			s.state.FailedAccountIDs = append(s.state.FailedAccountIDs, accountID)
			s.addEventLocked(Event{Type: "failed", AccountID: accountID, Label: cfg.Label, Message: "创建隐私邮箱失败，该账号已退出自动创建", Error: err.Error()})
			if len(s.state.FailedAccountIDs) >= len(s.state.AccountIDs) {
				s.stopAfterAllAccountsFailedLocked()
				s.mu.Unlock()
				return
			}
		} else {
			s.state.Success++
			s.addEventLocked(Event{Type: "created", AccountID: accountID, MailboxID: mailbox.ID, Email: mailbox.Email, Label: mailbox.Label, Message: "已创建隐私邮箱 " + mailbox.Email})
		}
		s.mu.Unlock()
		if index < len(activeIDs)-1 && cfg.AccountIntervalMaxSeconds > 0 {
			accountIntervalSeconds := randomBetween(cfg.AccountIntervalMinSeconds, cfg.AccountIntervalMaxSeconds)
			s.mu.Lock()
			if generation == s.generation && s.state.Running {
				s.state.CurrentAccountIntervalSeconds = accountIntervalSeconds
			}
			s.mu.Unlock()
			timer := time.NewTimer(time.Duration(accountIntervalSeconds) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func (s *Service) stopAfterAllAccountsFailedLocked() {
	s.state.Running = false
	s.state.Status = "stopped"
	s.state.NextRunAt = time.Time{}
	s.state.StoppedAt = time.Now()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.generation++
	s.addEventLocked(Event{Type: "stopped", Message: "所有参与账号创建失败，自动创建已停止"})
}

func (s *Service) finish(generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Running || generation != s.generation {
		return
	}
	s.state.Running = false
	s.state.Status = "stopped"
	s.state.StoppedAt = time.Now()
	s.cancel = nil
	s.addEventLocked(Event{Type: "stopped", Message: "定时创建已停止"})
}

func (s *Service) normalizeConfig(cfg Config) Config {
	seen := map[string]bool{}
	ids := make([]string, 0, len(cfg.AccountIDs))
	for _, id := range cfg.AccountIDs {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	cfg.AccountIDs = ids
	cfg.Label = strings.TrimSpace(cfg.Label)
	cfg.Note = strings.TrimSpace(cfg.Note)
	cfg.CreateChannel = strings.ToLower(strings.TrimSpace(cfg.CreateChannel))
	switch cfg.CreateChannel {
	case "apple_account", "icloud_web":
	default:
		cfg.CreateChannel = "auto"
	}
	if cfg.IntervalMinMinutes <= 0 && cfg.IntervalMinSeconds <= 0 {
		cfg.IntervalMinMinutes = 60
	}
	if cfg.IntervalMaxMinutes <= 0 && cfg.IntervalMaxSeconds <= 0 {
		cfg.IntervalMaxMinutes = cfg.IntervalMinMinutes
	}
	if cfg.AccountIntervalMinSeconds <= 0 {
		cfg.AccountIntervalMinSeconds = 5
	}
	if cfg.AccountIntervalMaxSeconds <= 0 {
		cfg.AccountIntervalMaxSeconds = cfg.AccountIntervalMinSeconds
	}
	if cfg.AccountIntervalMinSeconds > cfg.AccountIntervalMaxSeconds {
		cfg.AccountIntervalMinSeconds, cfg.AccountIntervalMaxSeconds = cfg.AccountIntervalMaxSeconds, cfg.AccountIntervalMinSeconds
	}
	intervalMinSeconds, intervalMaxSeconds := s.intervalRange(cfg)
	cfg.IntervalMinSeconds = intervalMinSeconds
	cfg.IntervalMaxSeconds = intervalMaxSeconds
	return cfg
}

func (s *Service) intervalRange(cfg Config) (int, int) {
	minimum := cfg.IntervalMinSeconds
	maximum := cfg.IntervalMaxSeconds
	if minimum <= 0 {
		minimum = cfg.IntervalMinMinutes * 60
	}
	if maximum <= 0 {
		maximum = cfg.IntervalMaxMinutes * 60
	}
	if minimum <= 0 {
		minimum = 60 * 60
	}
	if maximum <= 0 {
		maximum = minimum
	}
	if minimum > maximum {
		minimum, maximum = maximum, minimum
	}
	return minimum, maximum
}

func randomBetween(minimum, maximum int) int {
	if minimum <= 0 {
		minimum = 1
	}
	if maximum <= minimum {
		return minimum
	}
	return minimum + rand.IntN(maximum-minimum+1)
}

func (s *Service) addEventLocked(event Event) {
	s.nextID++
	event.ID = s.nextID
	if event.At.IsZero() {
		event.At = time.Now()
	}
	s.state.Events = append(s.state.Events, event)
	if len(s.state.Events) > maxEvents {
		s.state.Events = append([]Event(nil), s.state.Events[len(s.state.Events)-maxEvents:]...)
	}
	s.publishLocked()
}

func (s *Service) publishLocked() {
	if s.store == nil {
		return
	}
	_ = s.store.SaveRuntimeState("scheduler", s.state, true)
}

func (s *Service) snapshotLocked() State {
	out := s.state
	out.AccountIDs = append([]string(nil), s.state.AccountIDs...)
	out.FailedAccountIDs = append([]string(nil), s.state.FailedAccountIDs...)
	out.Events = append([]Event(nil), s.state.Events...)
	return out
}

func DefaultConfig(state *store.Store) Config {
	settings := state.CreateSettings()
	accountIDs := append([]string(nil), settings.AccountIDs...)
	if len(accountIDs) == 0 {
		for _, account := range state.AppleAccounts() {
			if account.Status == domain.StatusActive {
				accountIDs = append(accountIDs, account.ID)
				break
			}
		}
	}
	return Config{
		AccountIDs:                accountIDs,
		Label:                     settings.Label,
		Note:                      settings.Note,
		CreateChannel:             settings.SchedulerCreateChannel,
		IntervalMinMinutes:        settings.SchedulerIntervalMinMinutes,
		IntervalMaxMinutes:        settings.SchedulerIntervalMaxMinutes,
		AccountIntervalMinSeconds: settings.SchedulerAccountIntervalMinSeconds,
		AccountIntervalMaxSeconds: settings.SchedulerAccountIntervalMaxSeconds,
	}
}
