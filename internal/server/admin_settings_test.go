package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/access"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/settings"
)

func TestAdminSettingsAuthenticationAndSave(t *testing.T) {
	h, s := managedHandler(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	resolve := func(raw []byte) (settings.Values, settings.Values, error) {
		var c struct {
			Pool struct {
				Max int `json:"max_in_flight"`
			} `json:"pool"`
		}
		c.Pool.Max = 3
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, nil, err
		}
		v := settings.Values{"pool.max_in_flight": float64(c.Pool.Max)}
		return v, v, nil
	}
	initial := settings.Values{"pool.max_in_flight": float64(3)}
	h.cfg.Settings = settings.New(path, initial, initial, nil, resolve)
	_, token, err := s.Create(access.Policy{Name: "api", Channels: []string{realm.WB}, DefaultChannel: realm.WB, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{"", token} {
		for _, method := range []string{"GET", "PATCH"} {
			if w := accessRequest(h, method, "/admin/api/settings", `{}`, credential, nil, ""); w.Code != 401 {
				t.Fatalf("unauthorized settings access: %d", w.Code)
			}
		}
	}
	cookie, csrf := adminSessionForTest(t, h, s)
	w := accessRequest(h, "GET", "/admin/api/settings", "", "", cookie, "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("settings not available to administrator")
	}
	var snapshot settings.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"revision": snapshot.Revision, "changes": map[string]int{"pool.max_in_flight": 5}})
	if w := accessRequest(h, "PATCH", "/admin/api/settings", string(body), "", cookie, ""); w.Code != 403 {
		t.Fatal("settings write without CSRF accepted")
	}
	w = accessRequest(h, "PATCH", "/admin/api/settings", string(body), "", cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if w := accessRequest(h, "PATCH", "/admin/api/settings", string(body), "", cookie, csrf); w.Code != 409 {
		t.Fatal("conflict not HTTP 409")
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"revision": snapshot.Revision, "changes": map[string]int{"server.max_body_mb": 0}})
	if w := accessRequest(h, "PATCH", "/admin/api/settings", string(body), "", cookie, csrf); w.Code != 400 {
		t.Fatal("validation not HTTP 400")
	}
	if w := accessRequest(h, "PATCH", "/admin/api/settings", `{"revision":"x","changes":{},"unexpected":true}`, "", cookie, csrf); w.Code != 400 {
		t.Fatal("unknown request field accepted")
	}
	h.cfg.Settings = nil
	if w := accessRequest(h, "GET", "/admin/api/settings", "", "", cookie, ""); w.Code != 503 {
		t.Fatal("missing service not HTTP 503")
	}
}
