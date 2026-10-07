package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/access"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
)

func managedHandler(t *testing.T) (*Handler, *access.Store) {
	t.Helper()
	store, err := access.Open(filepath.Join(t.TempDir(), "access.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pl := pool.New("")
	return NewHandler(Config{Access: store, ConsoleEnabled: true, DefaultRealm: realm.WB, Pool: pl, Pools: map[string]*pool.Pool{realm.WB: pl, realm.CB: pl, realm.CN: pool.New("")}, APIKey: "legacy-master-must-not-work"}), store
}
func accessRequest(h *Handler, method, path, body, token string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func adminSessionForTest(t *testing.T, h *Handler, s *access.Store) (*http.Cookie, string) {
	t.Helper()
	proof, err := os.ReadFile(s.SetupFile())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"username": "admin", "password": "memorable-password", "setup_token": strings.TrimSpace(string(proof))})
	w := accessRequest(h, "POST", "/admin/api/auth/setup", string(body), "", nil, "")
	if w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	w = accessRequest(h, "POST", "/admin/api/auth/login", `{"username":"admin","password":"memorable-password"}`, "", nil, "")
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var data struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &data)
	return w.Result().Cookies()[0], data.CSRF
}

func TestManagedAdminKeyLifecycle(t *testing.T) {
	h, s := managedHandler(t)
	for _, token := range []string{"", "legacy-master-must-not-work"} {
		if w := accessRequest(h, "GET", "/admin/api/overview", "", token, nil, ""); w.Code != 401 {
			t.Fatalf("admin accepts API key: %d", w.Code)
		}
	}
	cookie, csrf := adminSessionForTest(t, h, s)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/admin" {
		t.Fatalf("unsafe cookie %+v", cookie)
	}
	policy := `{"name":"client","channels":["workbuddy"],"default_channel":"workbuddy","rpm":10,"enabled":true,"expires_at":null}`
	if w := accessRequest(h, "POST", "/admin/api/keys", policy, "", cookie, ""); w.Code != 403 {
		t.Fatal("CSRF missing accepted")
	}
	w := accessRequest(h, "POST", "/admin/api/keys", policy, "", cookie, csrf)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var created struct {
		Key   access.Key `json:"key"`
		Token string     `json:"token"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Token == "" {
		t.Fatal("missing one-time token")
	}
	w = accessRequest(h, "GET", "/admin/api/keys", "", "", cookie, "")
	if strings.Contains(w.Body.String(), created.Token) || strings.Contains(w.Body.String(), `"hash"`) {
		t.Fatal("key list disclosed secret")
	}
	if w = accessRequest(h, "GET", "/admin/api/keys", "", created.Token, nil, ""); w.Code != 401 {
		t.Fatal("issued key has management access")
	}
	if w = accessRequest(h, "GET", "/v1/models", "", "", cookie, ""); w.Code != 401 {
		t.Fatal("admin cookie accepted as API key")
	}
	if w = accessRequest(h, "GET", "/status", "", created.Token, nil, ""); w.Code != 200 || strings.Contains(w.Body.String(), "accounts") {
		t.Fatalf("status leaks accounts: %d %s", w.Code, w.Body)
	}
	if w = accessRequest(h, "DELETE", "/admin/api/keys/"+created.Key.ID, "", "", cookie, csrf); w.Code != 200 {
		t.Fatal("revoke failed")
	}
	if w = accessRequest(h, "GET", "/v1/models", "", created.Token, nil, ""); w.Code != 401 {
		t.Fatal("revoked key accepted")
	}
	if w = accessRequest(h, "POST", "/admin/api/auth/logout", "{}", "", cookie, csrf); w.Code != 200 {
		t.Fatal("logout failed")
	}
	if w = accessRequest(h, "GET", "/admin/api/overview", "", "", cookie, ""); w.Code != 401 {
		t.Fatal("old session survived logout")
	}
}

func TestIssuedKeyChannelIsolationAllProtocols(t *testing.T) {
	h, s := managedHandler(t)
	_, token, err := s.Create(access.Policy{Name: "wb only", Channels: []string{realm.WB}, DefaultChannel: realm.WB, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"codebuddy/model","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"codebuddy/model","input":"hi"}`},
		{"/v1/messages", `{"model":"codebuddy/model","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/images/generations", `{"model":"codebuddy/model","prompt":"hi"}`},
		{"/v1/images/edits", `{"model":"codebuddy/model","prompt":"hi"}`},
		{"/v1/videos/generations", `{"model":"codebuddy/model","prompt":"hi"}`},
		{"/v1/videos/tasks", `{"model":"codebuddy/model","task_id":"1"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := accessRequest(h, "POST", tc.path, tc.body, token, nil, "")
			if w.Code != 403 {
				t.Fatalf("channel bypass %d %s", w.Code, w.Body)
			}
		})
	}
	for _, rn := range []string{realm.CN, realm.CB, "unknown"} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Realm", rn)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("X-Realm=%s status=%d", rn, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"codebuddy/model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("X-Api-Key", token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatalf("messages envelope %d %s", w.Code, w.Body)
	}
	var channels []string
	h.withAuth(func(w http.ResponseWriter, r *http.Request) {
		channels = h.realmsForCredential(r)
		if h.realmOfRequest(r) != realm.WB {
			t.Fatal("wrong default")
		}
	})(httptest.NewRecorder(), func() *http.Request {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}())
	if len(channels) != 1 || channels[0] != realm.WB {
		t.Fatalf("models expose forbidden routes %v", channels)
	}
}

func TestCrossOriginLoginAndSessionProtection(t *testing.T) {
	h, s := managedHandler(t)
	cookie, csrf := adminSessionForTest(t, h, s)
	r := httptest.NewRequest("POST", "/admin/api/auth/logout", strings.NewReader("{}"))
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	r = httptest.NewRequest("POST", "/admin/api/auth/login", strings.NewReader(`{"username":"admin","password":"memorable-password"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("login CSRF accepted")
	}
	for i := 0; i < 12; i++ {
		w = accessRequest(h, "POST", "/admin/api/auth/login", fmt.Sprintf(`{"username":"bad%d","password":"wrong"}`, i), "", nil, "")
	}
	if w.Code != 429 {
		t.Fatal("login not throttled")
	}
}

func TestAdminLogoutCancelsActiveRequest(t *testing.T) {
	h, s := managedHandler(t)
	cookie, _ := adminSessionForTest(t, h, s)
	entered := make(chan struct{})
	finished := make(chan error, 1)
	r := httptest.NewRequest("GET", "/admin/api/events", nil)
	r.AddCookie(cookie)
	go h.withAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		finished <- r.Context().Err()
	})(httptest.NewRecorder(), r)
	<-entered
	s.Logout(cookie.Value)
	select {
	case err := <-finished:
		if err != context.Canceled {
			t.Fatalf("context=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel active management stream")
	}
}

func TestSecureCookieAndKeyHTTPRateLimit(t *testing.T) {
	h, s := managedHandler(t)
	h.cfg.SecureCookie = true
	cookie, _ := adminSessionForTest(t, h, s)
	if !cookie.Secure {
		t.Fatal("secure cookie setting ignored")
	}
	_, token, err := s.Create(access.Policy{Name: "limited", Channels: []string{realm.WB}, DefaultChannel: realm.WB, Enabled: true, RPM: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w := accessRequest(h, "GET", "/status", "", token, nil, ""); w.Code != 200 {
		t.Fatal("first request failed")
	}
	if w := accessRequest(h, "GET", "/status", "", token, nil, ""); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("key rate limit not enforced over HTTP")
	}
}
