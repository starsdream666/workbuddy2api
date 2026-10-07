package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/upstream"
)

func waitRuntime(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("runtime update timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRuntimeResumesDisabledSchedulerAndStopsCleanly(t *testing.T) {
	var calls atomic.Int32
	srv := balanceStub(t, map[string]int64{"u": 50}, &calls)
	s := New(Config{Pool: creditWatchPool("u"), Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
		CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true, CreditWatchScope: "all", CreditWatchInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	waitRuntime(t, func() bool { return s.running.Load() })
	cfg := s.CurrentConfig()
	cfg.CreditWatchEnabled = true
	cfg.CreditWatchInterval = 40 * time.Millisecond
	s.ApplyRuntime(cfg)
	// Enabling starts a normal interval, not an eager full scan.
	time.Sleep(10 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("reload performed startup scan")
	}
	waitRuntime(t, func() bool { return calls.Load() > 0 })
	cfg.CreditWatchEnabled = false
	s.ApplyRuntime(cfg)
	s.lockOperations(context.Background())
	count := calls.Load()
	s.unlockOperations()
	time.Sleep(90 * time.Millisecond)
	if calls.Load() != count {
		t.Fatal("disabled watch still fires")
	}
	cfg.CreditWatchEnabled = true
	cfg.CreditWatchInterval = 10 * time.Millisecond
	s.ApplyRuntime(cfg)
	waitRuntime(t, func() bool { return calls.Load() > count })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler leaked loops")
	}
}

func TestRuntimeScheduleAndCachesRetained(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true})
	s.adoptTried["u"] = "today"
	s.unsupported[taskTravel] = "unsupported"
	now := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	first, _ := s.nextWake(now)
	if first.Hour() != 9 {
		t.Fatal("initial schedule")
	}
	cfg := s.CurrentConfig()
	cfg.CheckinHours = []int{10}
	s.ApplyRuntime(cfg)
	cfg.CheckinHours[0] = 12
	next, kinds := s.nextWake(now)
	if next.Hour() != 10 || len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Fatal("schedule not replaced or caller owns slice")
	}
	if s.adoptTried["u"] != "today" || s.unsupported[taskTravel] != "unsupported" {
		t.Fatal("reload cleared caches")
	}
	<-s.wake
	cfg = s.CurrentConfig()
	cfg.ActivityReportCount = 7
	s.ApplyRuntime(cfg)
	if len(s.wake) != 0 || len(s.creditWake) != 0 {
		t.Fatal("unrelated change reset timers")
	}
}
