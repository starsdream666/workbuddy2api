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
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
)

func TestAdminAccountPackages(t *testing.T) {
	up := consoleUpstream("ok", nil)
	calls := 0
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer at-ai" {
			t.Fatal("wrong account selected")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[{"PackageName":"Bonus","CapacityUnit":"credits","CapacitySizePrecise":"30","CapacityRemainPrecise":"16.49","AccessToken":"private-token"}]}}}}`))}, nil
	})
	h, pl := consoleHandler(t, t.TempDir(), up, nil, nil)
	pl.SetCredits("u-ai", 8)
	pl.SetManualDisabled("u-ai", true)
	h.cfg.Pools[realm.CN].Add(&auth.Auth{UID: "u-ai", Realm: realm.CN, AccessToken: "cn-token"})
	path := "/admin/api/account-packages?realm=workbuddy&uid=u-ai"
	for _, tc := range []struct {
		path, key string
		status    int
	}{
		{path, "", 401}, {path, "wrong", 401},
		{"/admin/api/account-packages?uid=u-ai", "sk-master", 400},
		{"/admin/api/account-packages?realm=garbage&uid=u-ai", "sk-master", 400},
		{"/admin/api/account-packages?realm=workbuddy&uid=missing", "sk-master", 404},
		{"/admin/api/account-packages?realm=codebuddy&uid=u-ai", "sk-master", 404},
	} {
		status, _ := adminGet(t, h, tc.path, tc.key)
		if status != tc.status {
			t.Fatalf("%s: %d want %d", tc.path, status, tc.status)
		}
	}
	if calls != 0 {
		t.Fatal("invalid query called upstream")
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer sk-master")
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Header().Get("Cache-Control") != "no-store" || calls != 1 {
		t.Fatalf("status=%d calls=%d", rr.Code, calls)
	}
	var data map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data["uid"] != "u-ai" || data["realm"] != realm.WB || !strings.Contains(rr.Body.String(), `"remaining":"16.49"`) || strings.Contains(rr.Body.String(), "private-token") {
		t.Fatalf("bad response %s", rr.Body)
	}
	for _, st := range pl.List() {
		if st.Credits != 8 || !st.ManualDisabled {
			t.Fatal("read-only query changed account state")
		}
	}
	// Without a matching realm pool, never fall back to the default account pool.
	h.cfg.Pools = map[string]*pool.Pool{}
	status, _ := adminGet(t, h, "/admin/api/account-packages?realm=cn&uid=u-ai", "sk-master")
	if status != 404 || calls != 1 {
		t.Fatal("cross-realm fallback")
	}
}

func TestAdminAccountPackagesErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		timeout bool
		want    int
	}{
		{"unauthorized", 401, `{"msg":"private-token"}`, false, 502},
		{"malformed", 200, `{"code":0,"data":{}}`, false, 502},
		{"timeout", 0, "", true, 504},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := consoleUpstream("ok", nil)
			up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.timeout {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			h, _ := consoleHandler(t, t.TempDir(), up, nil, nil)
			req := httptest.NewRequest("GET", "/admin/api/account-packages?realm=workbuddy&uid=u-ai", nil)
			req.Header.Set("Authorization", "Bearer sk-master")
			ctx, cancel := context.WithTimeout(req.Context(), 10*time.Millisecond)
			defer cancel()
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req.WithContext(ctx))
			if rr.Code != tc.want || strings.Contains(rr.Body.String(), "private-token") {
				t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
			}
		})
	}
}
