package main

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/settings"
)

func settingsValues(c *Config) settings.Values {
	raw, _ := json.Marshal(c)
	// Top-level scalar settings are intentionally excluded from the projection.
	var root map[string]json.RawMessage
	_ = json.Unmarshal(raw, &root)
	out := settings.Values{}
	for _, field := range settings.Catalog() {
		parts := strings.Split(field.Key, ".")
		var group map[string]json.RawMessage
		_ = json.Unmarshal(root[parts[0]], &group)
		var value any
		_ = json.Unmarshal(group[parts[1]], &value)
		out[field.Key] = value
	}
	if out["pool.selection_mode"] == "" {
		out["pool.selection_mode"] = "weighted"
	}
	return out
}

func resolveSettings(raw []byte) (settings.Values, settings.Values, error) {
	c := Default()
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, nil, err
	}
	configured := settingsValues(c)
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, nil, err
	}
	effective := effectiveSettingsValues(c)
	for _, field := range settings.Catalog() {
		// Empty schedules and optional durations mean defaults in the loader.
		// Show those defaults instead of creating a dirty, empty form on load.
		if field.Kind == "hours" {
			if hours, _ := configured[field.Key].([]any); len(hours) == 0 {
				configured[field.Key] = effective[field.Key]
			}
		}
		if field.Kind == "duration" && configured[field.Key] == "" {
			configured[field.Key] = effective[field.Key]
		}
	}
	return configured, effective, nil
}

// Mirror startup fallbacks applied by the pool, handler and session router after
// Config.normalize, without mutating the running configuration or its resources.
func effectiveSettingsValues(c *Config) settings.Values {
	v := settingsValues(c)
	// 选号策略：只有四个合法取值能生效，其余（含空值/笔误）在池内一律回落 weighted。
	// 口径与 pool.SetSelectionMode 的 default 分支一致——控制台"当前生效值"
	// 必须反映池的真实行为，否则页面显示的策略名与路由行为会不一致。
	switch c.Pool.SelectionMode {
	case pool.SelectionLowestCredits, pool.SelectionHighestCredits, pool.SelectionCustomPriority:
		// 合法且非默认：原样透出。
	default:
		v["pool.selection_mode"] = pool.SelectionWeighted
	}
	if c.Pool.MaxInFlight < 0 {
		v["pool.max_in_flight"] = float64(0)
	}
	for key, fallback := range map[string]string{
		"cooldown.soft_rate": "600s", "cooldown.soft_rate_max": "2h",
		"pool.breaker_cooldown": "30m", "pool.breaker_cooldown_max": "6h",
		"session_sticky.ttl": "30m", "session_sticky.gc_interval": "5m",
		"usage_log.calibrate_interval": "5m",
	} {
		d, err := time.ParseDuration(v[key].(string))
		if err != nil || d <= 0 {
			v[key] = fallback
		}
	}
	return v
}

func newSettingsStore(path string, current *Config) *settings.Store {
	locked := map[string]string{}
	for _, field := range settings.Catalog() {
		if field.Env != "" && os.Getenv(field.Env) != "" {
			locked[field.Key] = field.Env
		}
	}
	return settings.New(path, effectiveSettingsValues(current), settingsValues(Default()), locked, resolveSettings)
}
