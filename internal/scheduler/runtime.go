package scheduler

import "slices"

// CurrentConfig returns the immutable configuration for the next operation.
// Pool/Realm never change. Callers must not mutate the returned schedule slices.
func (s *Scheduler) CurrentConfig() Config {
	if cfg := s.runtime.Load(); cfg != nil {
		return *cfg
	}
	return s.cfg
}

// ApplyRuntime retains operation serialization, capability caches and daily
// attempt history. Notifications interrupt timers even when all tasks are off.
func (s *Scheduler) ApplyRuntime(cfg Config) {
	old := s.CurrentConfig()
	cfg.Pool, cfg.Realm = s.cfg.Pool, s.cfg.Realm
	cfg.CheckinHours = append([]int(nil), cfg.CheckinHours...)
	cfg.TravelHours = append([]int(nil), cfg.TravelHours...)
	cfg.ActivityHours = append([]int(nil), cfg.ActivityHours...)
	cfg.KeepaliveHours = append([]int(nil), cfg.KeepaliveHours...)
	s.runtime.Store(&cfg)
	if !sameSchedule(old, cfg) {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	if !sameCreditSchedule(old, cfg) {
		select {
		case s.creditWake <- struct{}{}:
		default:
		}
	}
}

func sameSchedule(a, b Config) bool {
	return a.CheckinDisabled == b.CheckinDisabled && a.TravelDisabled == b.TravelDisabled &&
		a.ActivityDisabled == b.ActivityDisabled && a.KeepaliveDisabled == b.KeepaliveDisabled &&
		slices.Equal(a.CheckinHours, b.CheckinHours) && slices.Equal(a.TravelHours, b.TravelHours) &&
		slices.Equal(a.ActivityHours, b.ActivityHours) && slices.Equal(a.KeepaliveHours, b.KeepaliveHours)
}

func sameCreditSchedule(a, b Config) bool {
	return a.CreditWatchEnabled == b.CreditWatchEnabled && a.CreditWatchInterval == b.CreditWatchInterval && a.CreditWatchScope == b.CreditWatchScope
}
