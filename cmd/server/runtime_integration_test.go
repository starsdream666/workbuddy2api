package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/access"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/settings"
)

func TestRuntimeAdminSaveDuringActiveStream(t *testing.T) {
	m, store, _ := runtimeFixture(t)
	started, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-finish
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"last\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstreamServer.Close()
	defer once.Do(func() { close(finish) })
	m.up.Profiles = map[string]realm.Profile{realm.WB: {Name: realm.WB, ChatBase: upstreamServer.URL, BillingBase: upstreamServer.URL}}
	m.current.UsageLog.RefreshBalance = false
	// Keep this test's persisted config consistent, so finish cannot trigger a balance request.
	before, _ := store.Read()
	if _, err := store.Save(before.Revision, map[string]json.RawMessage{"usage_log.refresh_balance": json.RawMessage(`false`)}); err != nil {
		t.Fatal(err)
	}
	p := m.pools[realm.WB]
	p.Add(&auth.Auth{UID: "u", Realm: realm.WB, AccessToken: "mock", ExpiresAt: 9999999999})
	authStore, err := access.Open(filepath.Join(t.TempDir(), "access.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer authStore.Close()
	if err = authStore.ResetAdministrator("admin", "local-test-password"); err != nil {
		t.Fatal(err)
	}
	cookie, session, err := authStore.Login("admin", "local-test-password")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := authStore.Create(access.Policy{Name: "test", Channels: []string{realm.WB}, DefaultChannel: realm.WB, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h := server.NewHandler(server.Config{Access: authStore, Settings: store, ConsoleEnabled: true, Pool: p, PoolsSnapshot: m.poolsSnapshot, Upstream: m.up, DefaultRealm: realm.WB, UsageLog: m.logger})
	m.handler = h
	h.ApplyRuntime(runtimeFor(m.current, m.up, m.session))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		done <- w
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not start")
	}
	before, _ = store.Read()
	body, _ := json.Marshal(map[string]any{"revision": before.Revision, "changes": map[string]any{"upstream.idle_timeout_seconds": 1, "features.prompt_cache_key": false, "pool.max_in_flight": 1}})
	r := httptest.NewRequest("PATCH", "/admin/api/settings", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", session.CSRF)
	r.AddCookie(&http.Cookie{Name: "wb2api_session", Value: cookie})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var applied settings.Snapshot
	if err = json.Unmarshal(w.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.RestartRequired || len(applied.Pending) > 0 || applied.Current["upstream.idle_timeout_seconds"] != float64(1) {
		t.Fatal("save not applied")
	}
	if p.Acquire("u") {
		p.Release("u")
		t.Fatal("save discarded active request lease")
	}
	once.Do(func() { close(finish) })
	select {
	case response := <-done:
		if response.Code != 200 || !strings.Contains(response.Body.String(), "last") {
			t.Fatalf("stream interrupted: %d %s", response.Code, response.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream stuck")
	}
}

func TestRuntimeConcurrentReadersSavesAndRealmRegistration(t *testing.T) {
	m, s, _ := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			for _, p := range m.poolsSnapshot() {
				p.SelectionMode()
				p.CreditFloor()
				p.List()
			}
			m.handler.CurrentRuntime()
			m.session.Resolve("conversation")
			m.logger.Enabled()
			m.logger.SizeInfo()
			m.maintenanceSnapshot()
		}
	}()
	for i := 0; i < 12; i++ {
		before, err := s.Read()
		if err != nil {
			t.Fatal(err)
		}
		patch := map[string]json.RawMessage{"pool.max_in_flight": json.RawMessage("3")}
		if _, err = s.Save(before.Revision, patch); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		if m.pools[realm.CN] == nil {
			p := pool.New("")
			p.ApplyRuntime(poolOptions(m.current))
			m.pools[realm.CN] = p
			m.publishPools()
		}
		m.mu.Unlock()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime lock inversion")
	}
}

func TestRuntimeRejectsInvalidUnsubmittedFileSettings(t *testing.T) {
	m, s, path := runtimeFixture(t)
	old := m.handler.CurrentRuntime().Upstream
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	doc["cooldown"].(map[string]any)["soft_rate"] = "3h"
	doc["cooldown"].(map[string]any)["soft_rate_max"] = "1h"
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Save(before.Revision, map[string]json.RawMessage{"pool.max_in_flight": json.RawMessage(`7`)})
	var invalid *settings.Invalid
	if !errors.As(err, &invalid) || m.handler.CurrentRuntime().Upstream != old {
		t.Fatal("unsubmitted invalid settings were applied", err)
	}
}
