package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/settings"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usagelog"
)

func runtimeFixture(t *testing.T) (*runtimeController, *settings.Store, string) {
	t.Helper()
	for _, f := range settings.Catalog() {
		if f.Env != "" {
			t.Setenv(f.Env, "")
		}
	}
	c := Default()
	c.UsageLog.File = filepath.Join(t.TempDir(), "usage.jsonl")
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u"})
	p.SetCredits("u", 100)
	up := upstream.New()
	sess := session.New(session.Config{TTL: c.SessionTTL, GCInterval: c.SessionGCInterval, Available: func() []string { return []string{"u"} }})
	l := usagelog.New(usagelog.Config{Enabled: c.UsageLog.Enabled, File: c.UsageLog.File, MemorySize: c.UsageLog.MemorySize})
	h := server.NewHandler(server.Config{Pool: p, Upstream: up, UsageLog: l, Session: sess})
	m := &runtimeController{current: c, up: up, pools: map[string]*pool.Pool{"workbuddy": p, "codebuddy": p}, schedulers: map[string]*scheduler.Scheduler{}, handler: h, session: sess, logger: l}
	m.publishPools()
	m.schedulers["workbuddy"] = scheduler.New(scheduleOptions(c, p, up, "workbuddy"))
	h.ApplyRuntime(runtimeFor(c, up, sess))
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := newSettingsStore(path, c)
	s.SetApplier(m.prepare)
	t.Cleanup(func() { m.up.CloseIdleConnections() })
	return m, s, path
}

func TestRuntimeAppliesEveryCatalogFieldAndPersists(t *testing.T) {
	m, s, path := runtimeFixture(t)
	values := map[string]any{
		"server.max_body_mb": 9, "upstream.timeout_seconds": 31, "upstream.header_timeout_seconds": 13, "upstream.idle_timeout_seconds": 17,
		"pool.selection_mode": "highest_credits", "pool.max_in_flight": 7, "pool.credit_floor": 9, "pool.idle_weight_per_hour": 0.75, "pool.idle_weight_max": 8,
		"cooldown.soft_rate": "30s", "cooldown.soft_rate_max": "1h", "pool.breaker_threshold": 4, "pool.breaker_cooldown": "1m", "pool.breaker_cooldown_max": "2h",
		"session_sticky.enabled": false, "session_sticky.ttl": "12m", "session_sticky.gc_interval": "2m",
		"features.sanitize_blacklist_fingerprints": false, "features.prompt_cache_key": false, "features.repair_tool_history": true,
		"schedule.checkin_enabled": false, "schedule.checkin_hours": []int{1, 2}, "schedule.travel_enabled": false, "schedule.travel_hours": []int{3, 4},
		"schedule.activity_enabled": false, "schedule.activity_hours": []int{5, 6}, "schedule.activity_report_count": 7, "schedule.keepalive_enabled": false, "schedule.keepalive_hours": []int{7, 8},
		"schedule.credit_watch_enabled": false, "schedule.credit_watch_interval": "45m", "schedule.credit_watch_scope": "all", "schedule.credit_freeze_max": "24h",
		"usage_log.enabled": false, "usage_log.memory_size": 17, "usage_log.max_size_mb": 3, "usage_log.max_backups": 2, "usage_log.refresh_balance": false, "usage_log.calibrate_interval": "2m",
	}
	if len(values) != len(settings.Catalog()) {
		t.Fatal("update runtime coverage when catalog changes")
	}
	patch := map[string]json.RawMessage{}
	for _, f := range settings.Catalog() {
		v, ok := values[f.Key]
		if !ok {
			t.Fatalf("uncovered setting %s", f.Key)
		}
		patch[f.Key], _ = json.Marshal(v)
	}
	m.session.Bind("conversation", "u")
	before, _ := s.Read()
	after, err := s.Save(before.Revision, patch)
	if err != nil {
		t.Fatal(err)
	}
	if after.RestartRequired || !after.HotReload || len(after.Pending) != 0 || after.AppliedVersion != 1 {
		t.Fatalf("not applied: %+v", after)
	}
	want := settings.Values{}
	raw, _ := json.Marshal(values)
	json.Unmarshal(raw, &want)
	if !reflect.DeepEqual(after.Current, want) {
		t.Fatalf("current mismatch: %+v", after.Current)
	}
	if !reflect.DeepEqual(effectiveSettingsValues(m.current), want) {
		t.Fatal("controller omitted fields")
	}
	r := m.handler.CurrentRuntime()
	if r.Session != nil || r.MaxBodyBytes != 9<<20 || r.SoftCooldown != 30*time.Second || r.CalibrateInterval != 2*time.Minute || r.RefreshBalanceAfter != nil {
		t.Fatal("handler not updated")
	}
	if r.Upstream.HTTP.Timeout != 31*time.Second || r.Upstream.HeaderTimeout != 13*time.Second || r.Upstream.IdleTimeout != 17*time.Second || r.Upstream.SanitizeFingerprints || r.Upstream.PromptCacheKey || !r.Upstream.RepairToolHistory {
		t.Fatal("upstream not updated")
	}
	p := m.pools["workbuddy"]
	if p.SelectionMode() != "highest_credits" || p.CreditFloor() != 9 {
		t.Fatal("pool not updated")
	}
	for i := 0; i < 7; i++ {
		if !p.Acquire("u") {
			t.Fatal("wrong concurrency limit")
		}
	}
	if p.Acquire("u") {
		t.Fatal("concurrency not enforced")
	}
	for i := 0; i < 7; i++ {
		p.Release("u")
	}
	if m.session.Count() != 1 {
		t.Fatal("disabled session lost binding")
	}
	sch := m.schedulers["workbuddy"].CurrentConfig()
	if !sch.CheckinDisabled || !sch.TravelDisabled || !sch.ActivityDisabled || !sch.KeepaliveDisabled || sch.CreditWatchEnabled || sch.CreditWatchScope != "all" || sch.CreditWatchInterval != 45*time.Minute || sch.ActivityReportCount != 7 || sch.Upstream != r.Upstream {
		t.Fatal("scheduler not updated")
	}
	if !reflect.DeepEqual(sch.CheckinHours, []int{1, 2}) || !reflect.DeepEqual(sch.TravelHours, []int{3, 4}) || !reflect.DeepEqual(sch.ActivityHours, []int{5, 6}) || !reflect.DeepEqual(sch.KeepaliveHours, []int{7, 8}) {
		t.Fatal("hours omitted")
	}
	si := m.logger.SizeInfo()
	if m.logger.Enabled() || si.MemoryEntries != 17 || si.MaxBytes != 3<<20 || si.MaxBackups != 2 {
		t.Fatal("logger not updated")
	}
	restarted, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(effectiveSettingsValues(restarted), want) {
		t.Fatal("restart lost saved settings")
	}
	// Re-enabling uses the same stateful router and reinstates calibration.
	after, err = s.Save(after.Revision, map[string]json.RawMessage{"session_sticky.enabled": json.RawMessage(`true`), "usage_log.enabled": json.RawMessage(`true`), "usage_log.refresh_balance": json.RawMessage(`true`)})
	if err != nil || m.handler.CurrentRuntime().Session != m.session || !m.logger.Enabled() || m.handler.CurrentRuntime().RefreshBalanceAfter == nil {
		t.Fatal("re-enable failed", err)
	}
}

func TestRuntimePrepareFailurePreservesDiskAndLive(t *testing.T) {
	m, s, path := runtimeFixture(t)
	before, _ := s.Read()
	old := m.handler.CurrentRuntime().Upstream
	// Replace the log directory with a file to force a preparation failure.
	bad := filepath.Join(t.TempDir(), "not-a-directory")
	os.WriteFile(bad, []byte("x"), 0600)
	m.logger = usagelog.New(usagelog.Config{File: filepath.Join(bad, "usage.jsonl")})
	_, err := s.Save(before.Revision, map[string]json.RawMessage{"pool.max_in_flight": json.RawMessage(`9`)})
	if err == nil {
		t.Fatal("bad log path accepted")
	}
	after, _ := s.Read()
	if before.Revision != after.Revision || m.handler.CurrentRuntime().Upstream != old {
		t.Fatal("failure partly applied")
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}
