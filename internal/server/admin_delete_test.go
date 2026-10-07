package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
)

func adminDelete(t *testing.T, h *Handler, key, payload string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/accounts", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func deleteFixture(t *testing.T) (*Handler, *pool.Pool, *auth.Auth) {
	t.Helper()
	dir := t.TempDir()
	h, pl := consoleHandler(t, dir, consoleUpstream("ok", nil), nil, nil)
	a := pl.AuthByUID("u-ai")
	a.FilePath = filepath.Join(dir, "workbuddy-ai-u-ai.json")
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	return h, pl, a
}

func TestConsoleDeleteRemovesFileAndPool(t *testing.T) {
	h, pl, a := deleteFixture(t)
	cn := &auth.Auth{UID: a.UID, Realm: realm.CN, AccessToken: "other", FilePath: filepath.Join(h.cfg.AuthDir, "workbuddy-u-ai.json")}
	if err := cn.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	h.cfg.Pools[realm.CN].Add(cn)
	keep := &auth.Auth{UID: "keep", Realm: realm.AI, AccessToken: "keep"}
	pl.Add(keep)
	pl.SetCredits("keep", 17)
	pl.Acquire("u-ai") // 删除不终止已有请求；但后续租约与写回必须被阻止。
	code, body := adminDelete(t, h, "sk-master", `{"realm":"ai","uid":"u-ai"}`)
	if code != 200 || body["ok"] != true {
		t.Fatalf("code=%d body=%v", code, body)
	}
	if _, err := os.Stat(a.FilePath); !os.IsNotExist(err) {
		t.Fatalf("credential still exists: %v", err)
	}
	if pl.AuthByUID(a.UID) != nil || pl.Acquire(a.UID) {
		t.Fatal("deleted account still available")
	}
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("in-flight refresh must not recreate deleted file")
	}
	if _, err := os.Stat(cn.FilePath); err != nil || h.cfg.Pools[realm.CN].AuthByUID(cn.UID) == nil {
		t.Fatal("same UID in another realm must be untouched")
	}
	st, ok := pl.Status("keep")
	if !ok || st.Credits != 17 || pl.AuthByUID("keep") != keep {
		t.Fatal("other account state changed")
	}
	_, overview := adminGet(t, h, "/admin/api/overview", "sk-master")
	if overview["totals"].(map[string]any)["accounts"] != float64(2) {
		t.Fatalf("overview not refreshed: %v", overview)
	}
	if code, _ := adminDelete(t, h, "sk-master", `{"realm":"ai","uid":"u-ai"}`); code != 404 {
		t.Fatalf("repeat delete: %d", code)
	}
}

func TestConsoleDeleteRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name, key, payload string
		want               int
	}{
		{"no key", "", `{"realm":"ai","uid":"u-ai"}`, 401},
		{"bad key", "wrong", `{"realm":"ai","uid":"u-ai"}`, 401},
		{"invalid json", "sk-master", `{`, 400},
		{"empty uid", "sk-master", `{"realm":"ai","uid":" "}`, 400},
		{"missing realm", "sk-master", `{"uid":"u-ai"}`, 400},
		{"unknown realm", "sk-master", `{"realm":"oops","uid":"u-ai"}`, 400},
		{"wrong realm", "sk-master", `{"realm":"cn","uid":"u-ai"}`, 404},
		{"unknown uid", "sk-master", `{"realm":"ai","uid":"missing"}`, 404},
		{"path injection", "sk-master", `{"realm":"ai","uid":"u-ai","path":"../config.json"}`, 400},
		{"multiple json", "sk-master", `{"realm":"ai","uid":"u-ai"}{}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pl, a := deleteFixture(t)
			code, body := adminDelete(t, h, tc.key, tc.payload)
			if code != tc.want {
				t.Fatalf("code=%d want=%d body=%v", code, tc.want, body)
			}
			if _, err := os.Stat(a.FilePath); err != nil || pl.AuthByUID(a.UID) == nil {
				t.Fatal("rejected request changed credential")
			}
		})
	}
}

func TestConsoleDeleteUnsafeSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *Handler, *auth.Auth)
		want int
	}{
		{"outside directory", func(t *testing.T, h *Handler, a *auth.Auth) {
			a.FilePath = filepath.Join(t.TempDir(), "workbuddy-outside.json")
			if err := a.SaveAtomic(); err != nil {
				t.Fatal(err)
			}
		}, 403},
		{"not a credential name", func(t *testing.T, h *Handler, a *auth.Auth) {
			a.FilePath = filepath.Join(h.cfg.AuthDir, "config.json")
			if err := a.SaveAtomic(); err != nil {
				t.Fatal(err)
			}
		}, 403},
		{"directory", func(t *testing.T, h *Handler, a *auth.Auth) {
			a.FilePath = filepath.Join(h.cfg.AuthDir, "workbuddy-dir.json")
			if err := os.Mkdir(a.FilePath, 0o700); err != nil {
				t.Fatal(err)
			}
		}, 403},
		{"no source", func(t *testing.T, h *Handler, a *auth.Auth) { a.FilePath = "" }, 409},
		{"duplicate", func(t *testing.T, h *Handler, a *auth.Auth) {
			other := &auth.Auth{UID: a.UID, Realm: a.Realm, AccessToken: "duplicate", FilePath: filepath.Join(h.cfg.AuthDir, "workbuddy-copy.json")}
			if err := other.SaveAtomic(); err != nil {
				t.Fatal(err)
			}
		}, 409},
		{"file replaced", func(t *testing.T, h *Handler, a *auth.Auth) {
			other := &auth.Auth{UID: "another", Realm: a.Realm, AccessToken: "another", FilePath: a.FilePath}
			if err := other.SaveAtomic(); err != nil {
				t.Fatal(err)
			}
		}, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pl, a := deleteFixture(t)
			tc.edit(t, h, a)
			code, body := adminDelete(t, h, "sk-master", `{"realm":"ai","uid":"u-ai"}`)
			if code != tc.want || pl.AuthByUID(a.UID) == nil {
				t.Fatalf("code=%d want=%d body=%v", code, tc.want, body)
			}
			if a.FilePath != "" {
				if _, err := os.Stat(a.FilePath); err != nil {
					t.Fatal("source was removed")
				}
			}
		})
	}
}

func TestConsoleDeleteAlreadyMissingFile(t *testing.T) {
	h, pl, a := deleteFixture(t)
	if err := os.Remove(a.FilePath); err != nil {
		t.Fatal(err)
	}
	if code, body := adminDelete(t, h, "sk-master", `{"realm":"ai","uid":"u-ai"}`); code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	if pl.AuthByUID(a.UID) != nil {
		t.Fatal("missing file must still remove stale pool entry")
	}
}

func TestConsoleDeleteDoesNotFallbackToDefaultRealm(t *testing.T) {
	h, _, a := deleteFixture(t)
	delete(h.cfg.Pools, realm.CN)
	if code, _ := adminDelete(t, h, "sk-master", `{"realm":"cn","uid":"u-ai"}`); code != 404 {
		t.Fatalf("missing realm pool should not fallback: %d", code)
	}
	if _, err := os.Stat(a.FilePath); err != nil {
		t.Fatal(err)
	}
	h.cfg.Pools = nil
	if code, body := adminDelete(t, h, "sk-master", `{"realm":"ai","uid":"u-ai"}`); code != 200 {
		t.Fatalf("single realm deletion code=%d body=%v", code, body)
	}
}
