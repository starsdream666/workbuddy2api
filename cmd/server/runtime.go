package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/settings"
	"workbuddy2api/internal/taskqueue"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usagelog"
)

// runtimeController serializes saves and dynamic realm registration. Registry
// readers receive immutable snapshots, so adding a realm never races iteration.
type runtimeController struct {
	mu         sync.RWMutex
	current    *Config
	up         *upstream.Client
	pools      map[string]*pool.Pool
	registry   atomic.Pointer[map[string]*pool.Pool]
	schedulers map[string]*scheduler.Scheduler
	handler    *server.Handler
	session    *session.Router
	logger     *usagelog.Logger
	ctx        context.Context
}

func (m *runtimeController) poolsSnapshot() map[string]*pool.Pool {
	if published := m.registry.Load(); published != nil {
		return *published
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*pool.Pool, len(m.pools))
	for rn, p := range m.pools {
		out[rn] = p
	}
	return out
}

// publishPools is called by the registry writer under m.mu or during startup.
// Published maps never change; session callbacks can read without lock inversion.
func (m *runtimeController) publishPools() {
	next := make(map[string]*pool.Pool, len(m.pools))
	for rn, p := range m.pools {
		next[rn] = p
	}
	m.registry.Store(&next)
}

func (m *runtimeController) maintenanceSnapshot() map[string]func(context.Context, string, func(taskqueue.Progress)) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]func(context.Context, string, func(taskqueue.Progress)) error, len(m.schedulers))
	for rn, s := range m.schedulers {
		out[rn] = s.RunMaintenance
	}
	return out
}

func poolOptions(c *Config) pool.RuntimeOptions {
	return pool.RuntimeOptions{SelectionMode: c.Pool.SelectionMode, MaxInFlight: c.Pool.MaxInFlight, CreditFloor: c.Pool.CreditFloor,
		IdleWeightPerHour: c.Pool.IdleWeightPerHour, IdleWeightMax: c.Pool.IdleWeightMax, BreakerThreshold: c.Pool.BreakerThreshold,
		BreakerCooldown: c.BreakerCooldownDur, BreakerCooldownMax: c.BreakerCooldownMaxD, SoftRateMax: c.SoftRateMaxDur, FreezeMax: c.Schedule.CreditFreezeMaxDur()}
}

func scheduleOptions(c *Config, p *pool.Pool, up *upstream.Client, rn string) scheduler.Config {
	return scheduler.Config{Pool: p, Upstream: up, Realm: rn, CheckinHours: c.Schedule.CheckinHours, TravelHours: c.Schedule.TravelHours,
		ActivityHours: c.Schedule.ActivityHours, KeepaliveHours: c.Schedule.KeepaliveHours, ActivityReportCount: c.Schedule.ActivityReportCount,
		CheckinDisabled:    !c.Schedule.CheckinEnabled || !c.Schedule.RealmTaskEnabled(rn, "checkin"),
		TravelDisabled:     !c.Schedule.TravelEnabled || !c.Schedule.RealmTaskEnabled(rn, "travel"),
		ActivityDisabled:   !c.Schedule.ActivityEnabled || !c.Schedule.RealmTaskEnabled(rn, "activity"),
		KeepaliveDisabled:  !c.Schedule.KeepaliveEnabled || !c.Schedule.RealmTaskEnabled(rn, "keepalive"),
		CreditWatchEnabled: c.Schedule.CreditWatchEnabled, CreditWatchInterval: c.Schedule.CreditWatchIntervalDur(), CreditWatchScope: c.Schedule.CreditWatchScope}
}

// configWithValues projects only the validated allowlist onto the running startup
// config. External changes to ports, secrets or route topology are not hot applied.
func configWithValues(base *Config, values settings.Values) (*Config, error) {
	raw, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err = json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	for _, field := range settings.Catalog() {
		value, ok := values[field.Key]
		if !ok {
			return nil, fmt.Errorf("missing runtime setting %s", field.Key)
		}
		parts := strings.Split(field.Key, ".")
		var group map[string]json.RawMessage
		if err = json.Unmarshal(object[parts[0]], &group); err != nil {
			return nil, err
		}
		group[parts[1]], err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
		object[parts[0]], err = json.Marshal(group)
		if err != nil {
			return nil, err
		}
	}
	raw, err = json.Marshal(object)
	if err != nil {
		return nil, err
	}
	next := Default()
	if err = json.Unmarshal(raw, next); err != nil {
		return nil, err
	}
	if err = next.normalize(); err != nil {
		return nil, err
	}
	return next, nil
}

func runtimeFor(c *Config, up *upstream.Client, sess *session.Router) server.RuntimeConfig {
	r := server.RuntimeConfig{Upstream: up, MaxBodyBytes: int64(c.Server.MaxBodyMB) << 20, SoftCooldown: c.SoftRateDur, CalibrateInterval: c.CalibrateDur}
	if r.MaxBodyBytes <= 0 {
		r.MaxBodyBytes = 8 << 20
	}
	if r.SoftCooldown <= 0 {
		r.SoftCooldown = 600 * time.Second
	}
	if r.CalibrateInterval <= 0 {
		r.CalibrateInterval = 5 * time.Minute
	}
	if c.SessionSticky.Enabled {
		r.Session = sess
	}
	if c.UsageLog.Enabled && c.UsageLog.RefreshBalance {
		r.RefreshBalanceAfter = func(pl *pool.Pool, uid string) (int64, error) {
			if pl == nil {
				return 0, fmt.Errorf("account pool unavailable")
			}
			a := pl.AuthByUID(uid)
			if a == nil || strings.TrimSpace(a.AccessTokenValue()) == "" {
				return 0, fmt.Errorf("account unavailable")
			}
			remain, err := up.UserResourceWithTimeout(a, usageBalanceTimeout)
			if err != nil {
				return 0, err
			}
			pl.ReconcileCredits(uid, remain)
			return remain, nil
		}
	}
	return r
}

func (m *runtimeController) prepare(values settings.Values) (settings.Prepared, error) {
	m.mu.Lock()
	next, err := configWithValues(m.current, values)
	if err != nil {
		m.mu.Unlock()
		return settings.Prepared{}, err
	}
	applyLog, err := m.logger.PrepareRuntime(usagelog.RuntimeOptions{Enabled: next.UsageLog.Enabled, MemorySize: next.UsageLog.MemorySize, MaxSizeMB: next.UsageLog.MaxSizeMB, MaxBackups: next.UsageLog.MaxBackups})
	if err != nil {
		m.mu.Unlock()
		return settings.Prepared{}, err
	}
	up := m.up.WithRuntime(upstream.RuntimeOptions{Timeout: time.Duration(next.Upstream.TimeoutSeconds) * time.Second,
		HeaderTimeout: time.Duration(next.Upstream.HeaderTimeoutSeconds) * time.Second, IdleTimeout: time.Duration(next.Upstream.IdleTimeoutSeconds) * time.Second,
		SanitizeFingerprints: next.Features.SanitizeBlacklistFingerprints, PromptCacheKey: next.Features.PromptCacheKey, RepairToolHistory: next.Features.RepairToolHistory})
	return settings.Prepared{
		Abort: func() { up.CloseIdleConnections(); m.mu.Unlock() },
		Commit: func() {
			old := m.up
			seen := map[*pool.Pool]bool{}
			for _, p := range m.pools {
				if !seen[p] {
					p.ApplyRuntime(poolOptions(next))
					seen[p] = true
				}
			}
			m.session.ApplyRuntime(next.SessionTTL, next.SessionGCInterval)
			applyLog()
			for rn, s := range m.schedulers {
				s.ApplyRuntime(scheduleOptions(next, m.pools[rn], up, rn))
			}
			m.current, m.up = next, up
			m.handler.ApplyRuntime(runtimeFor(next, up, m.session))
			m.mu.Unlock()
			old.CloseIdleConnections()
		},
	}, nil
}

// addScheduler is called under m.mu (or before HTTP serving begins).
func (m *runtimeController) addScheduler(rn string, p *pool.Pool) {
	if m.ctx == nil || m.schedulers[rn] != nil {
		return
	}
	s := scheduler.New(scheduleOptions(m.current, p, m.up, rn))
	m.schedulers[rn] = s
	go s.Run(m.ctx)
}
