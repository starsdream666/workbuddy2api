package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestPanelAdminSecurityHeaders(test *testing.T) {
	handler := NewHandler(Config{APIKey: "secret", ConsoleEnabled: true})
	for _, path := range []string{"/admin", "/admin/", "/admin/assets/console.js", "/admin/api/overview", "/admin/api/missing"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Header().Get("X-Frame-Options") != "DENY" || response.Header().Get("Referrer-Policy") != "no-referrer" {
			test.Errorf("missing headers: %s", path)
		}
		policy := response.Header().Get("Content-Security-Policy")
		if !strings.Contains(policy, "script-src 'self';") || !strings.Contains(policy, "frame-ancestors 'none'") {
			test.Errorf("weak CSP: %s", policy)
		}
	}
	if strings.Contains(string(consoleHTML), "onclick=") {
		test.Error("inline click handlers blocked by CSP")
	}
	if !secretEqual("key", "key") || secretEqual("key", "keys") || secretEqual("key", "kez") {
		test.Error("key comparison")
	}
}

func TestPanelCreditFloorWithoutUsageLogging(test *testing.T) {
	accountPool := testPoolWith(&auth.Auth{UID: "test"})
	accountPool.SetCreditFloor(5)
	accountPool.SetCredits("test", 6)
	handler := NewHandler(Config{Pool: accountPool})
	stat := &chatStat{start: time.Now(), uid: "test", realm: "cn", model: "paid", status: 200, hasCreditUsage: true, creditFromUsage: 1.5}
	stat.finish(handler, accountPool)
	if accountPool.PickByUIDForModel("test", "paid", "cn") != nil {
		test.Fatal("disabled usage log bypasses credit floor accounting")
	}
}

func TestPanelModelMetadataAndFreshCachedPrices(test *testing.T) {
	cache := modelsCacheFor("cn")
	cache.Lock()
	previousIDs, previousFetched, previousFailure := cache.ids, cache.fetched, cache.lastFail
	cache.ids = []upstream.ModelInfo{{ID: "paid", Credits: "2x", Vendor: "vendor", DefaultEffort: "high", Efforts: []string{"low", "high"}}}
	cache.fetched, cache.lastFail = time.Now().Add(-30*time.Minute), time.Time{}
	cache.Unlock()
	defer func() {
		cache.Lock()
		cache.ids, cache.fetched, cache.lastFail = previousIDs, previousFetched, previousFailure
		cache.Unlock()
	}()
	accountPool := testPoolWith(&auth.Auth{UID: "test"})
	accountPool.SetCredits("test", 1)
	accountPool.SetCreditFloor(5)
	handler := NewHandler(Config{Pool: accountPool})
	models := handler.modelListFor("cn")
	if len(models) != 1 || models[0]["credits"] != "2x" || models[0]["vendor"] != "vendor" || models[0]["default_effort"] != "high" {
		test.Fatalf("metadata=%+v", models)
	}
	if accountPool.PickByUIDForModel("test", "paid", "cn") != nil {
		test.Fatal("fresh CN model cache lost price evidence after 10 minutes")
	}
	if poolPortrait(accountPool)["credit_floor"] != float64(5) {
		test.Fatal("floor absent from status")
	}
}

func TestPanelRequestErrorsDoNotRotateOrPenalize(test *testing.T) {
	for _, body := range []string{`{"code":11115,"msg":"prompt is too long"}`, `{"error":{"data":{"code":11135,"msg":"invalid_image_data"}}}`} {
		calls := 0
		up := newFakeUpstream(test, func(string) (int, string, bool) { calls++; return 400, body, false })
		pool := testPoolWith(&auth.Auth{UID: "test", AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour).Unix()})
		handler := NewHandler(Config{Pool: pool, Upstream: up, MaxRotate: 3})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`)))
		if response.Code != 400 || calls != 1 {
			test.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
		}
		var payload map[string]map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			test.Fatal(err)
		}
		if payload["error"]["message"] != body || payload["error"]["gateway_hint"] == "" {
			test.Error("upstream message lost or hint absent")
		}
		state := pool.List()[0]
		if state.Disabled || state.Cooling || state.InFlight != 0 {
			test.Errorf("request fault affected pool: %+v", state)
		}
	}
}

func TestPanelCanceledChatDoesNotRotate(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		cancel()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}}
	pool := testPoolWith(&auth.Auth{UID: "test", AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	handler := NewHandler(Config{Pool: pool, Upstream: up, MaxRotate: 3})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if calls != 1 || pool.List()[0].InFlight != 0 {
		test.Error("canceled request retried or lease leaked")
	}
}

func TestPanelMalformedUIDLoginRejected(test *testing.T) {
	up := consoleUpstream("ok", nil)
	original := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Path, "/login/account") {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"uid":"../../outside"}}`))}, nil
		}
		return original.RoundTrip(request)
	})
	pool := testPoolWith()
	handler := NewHandler(Config{Pool: pool, Upstream: up, AuthDir: test.TempDir(), ConsoleEnabled: true})
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/admin/api/login/start", strings.NewReader(`{}`)))
	var session map[string]any
	_ = json.Unmarshal(start.Body.Bytes(), &session)
	state, _ := session["state"].(string)
	if state == "" {
		test.Fatalf("login start failed: %s", start.Body.String())
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/api/login/poll?state="+state, nil))
	if response.Code != http.StatusBadGateway {
		test.Fatalf("unsafe UID accepted: %d %s", response.Code, response.Body.String())
	}
}
