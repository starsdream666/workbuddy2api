package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/settings"
)

func isolatedSettings(t *testing.T, content string) (string, *settings.Store) {
	t.Helper()
	for _, f := range settings.Catalog() {
		if f.Env != "" {
			t.Setenv(f.Env, "")
		}
	}
	for _, key := range []string{"WB2A_PROMPT_FILE", "WB2A_PROMPT_MODE", "WB2A_REALM"} {
		t.Setenv(key, "")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, newSettingsStore(path, cfg)
}
func change(key, value string) map[string]json.RawMessage {
	return map[string]json.RawMessage{key: json.RawMessage(value)}
}

func TestSettingsPreserveSecretsAndRestartState(t *testing.T) {
	path, store := isolatedSettings(t, `{"api_key":"private-legacy","upstash":{"token":"private-redis"},"pool":{"max_in_flight":3,"future_precision":9007199254740993},"extension":{"preserve":true},"schedule":{"realm_tasks":{"cn":{"travel":false}}}}`)
	before, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if before.RestartRequired {
		t.Fatal("unchanged config requires restart")
	}
	raw, _ := json.Marshal(before)
	if strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "future_precision") {
		t.Fatal("settings API leaked unrelated config")
	}
	after, err := store.Save(before.Revision, change("pool.max_in_flight", "7"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.RestartRequired || len(after.Pending) != 1 || after.Current["pool.max_in_flight"] != float64(3) || after.Values["pool.max_in_flight"] != float64(7) {
		t.Fatalf("incorrect running/pending state: %+v", after.Pending)
	}
	file, _ := os.ReadFile(path)
	for _, preserved := range []string{"private-legacy", "private-redis", "9007199254740993", `"extension"`, `"realm_tasks"`} {
		if !strings.Contains(string(file), preserved) {
			t.Fatalf("lost %s", preserved)
		}
	}
	if _, err := store.Save(before.Revision, change("pool.max_in_flight", "8")); !errors.Is(err, settings.ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	current, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newSettingsStore(path, current)
	loaded, err := restarted.Read()
	if err != nil || loaded.RestartRequired || loaded.Current["pool.max_in_flight"] != float64(7) {
		t.Fatal("restart did not apply saved configuration")
	}
}

func TestSettingsValidationIsAtomic(t *testing.T) {
	path, store := isolatedSettings(t, `{}`)
	before, _ := store.Read()
	cases := map[string]map[string]json.RawMessage{
		"unknown":           change("security.store_file", `"elsewhere"`),
		"null":              change("features.prompt_cache_key", "null"),
		"wrong type":        change("pool.max_in_flight", `"7"`),
		"fraction":          change("pool.max_in_flight", "1.1"),
		"negative":          change("pool.credit_floor", "-1"),
		"overflow":          change("server.max_body_mb", "1e50"),
		"enum":              change("pool.selection_mode", `"unknown"`),
		"duration":          change("session_sticky.ttl", `"-1m"`),
		"zero duration":     change("cooldown.soft_rate", `"0s"`),
		"hours":             change("schedule.travel_hours", "[24]"),
		"duplicate hours":   change("schedule.checkin_hours", "[9,9]"),
		"empty hours":       change("schedule.keepalive_hours", "[]"),
		"null hours":        change("schedule.keepalive_hours", "[null]"),
		"cooldown relation": change("cooldown.soft_rate_max", `"1s"`),
		"breaker relation":  change("pool.breaker_cooldown_max", `"1s"`),
		"mixed":             {"pool.max_in_flight": json.RawMessage("7"), "upstash.token": json.RawMessage(`"bad"`)},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Save(before.Revision, patch); err == nil {
				t.Fatal("invalid patch accepted")
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != "{}" {
				t.Fatal("failed validation changed config")
			}
		})
	}
}

func TestSettingsEnvLocksAndInheritedTimeout(t *testing.T) {
	path, _ := isolatedSettings(t, `{"upstream":{"timeout_seconds":100,"header_timeout_seconds":0},"features":{"prompt_cache_key":false}}`)
	t.Setenv("WB2A_PROMPT_CACHE_KEY", "true")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := newSettingsStore(path, cfg)
	before, _ := store.Read()
	if before.Locked["features.prompt_cache_key"] != "WB2A_PROMPT_CACHE_KEY" || before.Values["features.prompt_cache_key"] != true || before.RestartRequired {
		t.Fatal("environment override misreported")
	}
	if before.Values["upstream.header_timeout_seconds"] != float64(0) || before.Current["upstream.header_timeout_seconds"] != float64(100) {
		t.Fatal("inherited timeout intent lost")
	}
	if _, err := store.Save(before.Revision, change("features.prompt_cache_key", "false")); err == nil {
		t.Fatal("environment field writable")
	}
	after, err := store.Save(before.Revision, change("upstream.timeout_seconds", "200"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Pending) != 2 {
		t.Fatalf("inherited timeout not included: %+v", after.Pending)
	}
}

func TestSettingsConcurrentEditorsAndExternalChange(t *testing.T) {
	path, store := isolatedSettings(t, `{}`)
	before, _ := store.Read()
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Save(before.Revision, change("pool.max_in_flight", "6"))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, settings.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("concurrent editor overwrote config")
	}
	next, _ := store.Read()
	if err := os.WriteFile(path, []byte(`{"external":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(next.Revision, change("pool.max_in_flight", "9")); !errors.Is(err, settings.ErrConflict) {
		t.Fatal("external edit overwritten")
	}
}

func TestSettingsReadWriteFailureAndMissingFile(t *testing.T) {
	path, store := isolatedSettings(t, `{}`)
	before, _ := store.Read()
	if err := os.WriteFile(path, []byte(`{"private":"secret-corrupt"`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); !errors.Is(err, settings.ErrRead) {
		t.Fatal("corrupt config accepted")
	}
	if _, err := store.Save(before.Revision, change("pool.max_in_flight", "9")); !errors.Is(err, settings.ErrRead) {
		t.Fatal("corrupt config overwritten")
	}
	cfg := Default()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	bad := newSettingsStore(filepath.Join(t.TempDir(), "missing-parent", "config.json"), cfg)
	missing, _ := bad.Read()
	if _, err := bad.Save(missing.Revision, change("pool.max_in_flight", "9")); !errors.Is(err, settings.ErrWrite) {
		t.Fatalf("write failure not reported: %v", err)
	}
	failed, _ := bad.Read()
	if failed.RestartRequired {
		t.Fatal("failed write changed state")
	}
	fresh := newSettingsStore(filepath.Join(t.TempDir(), "config.json"), cfg)
	freshSnapshot, _ := fresh.Read()
	if _, err := fresh.Save(freshSnapshot.Revision, change("pool.max_in_flight", "9")); err != nil {
		t.Fatalf("missing config not created: %v", err)
	}
}

func TestSettingsCatalogDefaultsAndDurationEquivalence(t *testing.T) {
	_, store := isolatedSettings(t, `{}`)
	snap, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Fields) != 39 {
		t.Fatalf("review settings documentation count: %d", len(snap.Fields))
	}
	for _, f := range snap.Fields {
		if snap.Values[f.Key] == nil || snap.Defaults[f.Key] == nil || snap.Current[f.Key] == nil {
			t.Fatalf("missing config projection: %s", f.Key)
		}
	}
	after, err := store.Save(snap.Revision, change("cooldown.soft_rate", `"10m"`))
	if err != nil || after.RestartRequired {
		t.Fatalf("equivalent duration should not require restart: %v", err)
	}
}
