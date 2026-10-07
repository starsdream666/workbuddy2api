package upstream

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/realm"
)

// TestRefreshTokenPrimaryPathSucceeds 主路径（桌面/旧协议）正常时不回落：只打一次。
func TestRefreshTokenPrimaryPathSucceeds(t *testing.T) {
	var paths []string
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return jsonResp(http.StatusOK, `{"code":0,"data":{"accessToken":"at-new","expiresIn":3600}}`), nil
	})}, RealmDefault: realm.WB}
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", Realm: realm.WB}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "at-new" {
		t.Errorf("accessToken=%q want at-new", a.AccessToken)
	}
	if len(paths) != 1 || paths[0] != refreshPath {
		t.Errorf("paths=%v want 只打主路径 %s", paths, refreshPath)
	}
}

// TestRefreshTokenFallsBackToCLIPath 主路径 404/405（上游下线旧路径）→ 回落官方 CLI 的
// /v2/auth/token/refresh，并按 CLI 约定覆盖 X-Auth-Refresh-Source: plugin。
func TestRefreshTokenFallsBackToCLIPath(t *testing.T) {
	var paths []string
	var fallbackSource, fallbackRefreshToken string
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case refreshPath:
			return jsonResp(http.StatusNotFound, `{"code":404,"msg":"not found"}`), nil
		case refreshPathV2:
			fallbackSource = r.Header.Get("X-Auth-Refresh-Source")
			fallbackRefreshToken = r.Header.Get("X-Refresh-Token")
			return jsonResp(http.StatusOK, `{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`), nil
		}
		return jsonResp(http.StatusNotFound, `{"code":404}`), nil
	})}, RealmDefault: realm.WB}
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", Realm: realm.WB}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "at-new" || a.RefreshToken != "rt-new" {
		t.Errorf("token=%q/%q want at-new/rt-new", a.AccessToken, a.RefreshToken)
	}
	if len(paths) != 2 || paths[0] != refreshPath || paths[1] != refreshPathV2 {
		t.Fatalf("paths=%v want [%s %s]", paths, refreshPath, refreshPathV2)
	}
	if fallbackSource != "plugin" {
		t.Errorf("X-Auth-Refresh-Source=%q want plugin", fallbackSource)
	}
	if fallbackRefreshToken != "rt-old" {
		t.Errorf("X-Refresh-Token=%q want rt-old", fallbackRefreshToken)
	}
}

// TestRefreshTokenNoFallbackOnCredentialError 401/403 说明路径在、是凭证问题：不该回落（避免打两次）。
func TestRefreshTokenNoFallbackOnCredentialError(t *testing.T) {
	var calls int
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(http.StatusUnauthorized, `{"code":12153,"msg":"session dead"}`), nil
	})}, RealmDefault: realm.WB}
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", Realm: realm.WB}
	if err := c.RefreshToken(a); err == nil {
		t.Fatal("401 应返回错误")
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1（凭证类错误不回落）", calls)
	}
}

// TestRefreshDoMissingRefreshToken 没有 refreshToken 时直接报错，不打上游。
func TestRefreshDoMissingRefreshToken(t *testing.T) {
	var calls int
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(http.StatusOK, `{"code":0,"data":{}}`), nil
	})}, RealmDefault: realm.WB}
	if err := c.RefreshToken(&auth.Auth{UID: "u1", AccessToken: "at", Realm: realm.WB}); err == nil {
		t.Fatal("缺 refreshToken 应报错")
	}
	if calls != 0 {
		t.Errorf("calls=%d want 0", calls)
	}
}
