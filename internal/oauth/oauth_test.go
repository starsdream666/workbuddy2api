package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/realm"
)

// stubUpstream 模拟上游三条 OAuth 路径；tokenMode 控制 /auth/token 的行为。
func stubUpstream(t *testing.T, tokenMode string) (*httptest.Server, *realm.Profile) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			if r.Method != http.MethodPost {
				t.Errorf("auth/state method=%s want POST", r.Method)
			}
			if got := r.URL.Query().Get("platform"); got == "" {
				t.Error("auth/state 缺少 platform 参数")
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"st-1","authUrl":"https://login.example/?state=st-1"}}`))
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
			switch tokenMode {
			case "pending":
				_, _ = w.Write([]byte(`{"code":11217,"msg":"11217:login ing..."}`))
			case "server_error":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":500,"msg":"internal server error"}`))
			default:
				_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":3600,"domain":"www.workbuddy.ai"}}`))
			}
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/login/account"):
			if auth := r.Header.Get("Authorization"); auth != "Bearer at-1" {
				t.Errorf("account Authorization=%q want Bearer at-1", auth)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"uid":"u-1","nickname":"nick","enterpriseId":"e-1"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	prof := &realm.Profile{Name: realm.WB, ChatBase: srv.URL, Platform: "workbuddy-ai", Origin: srv.URL}
	return srv, prof
}

// TestStart 取授权链接：state 与 authUrl 都要回传，platform 随 realm 变化。
func TestStart(t *testing.T) {
	_, prof := stubUpstream(t, "ok")
	sess, err := Start(*prof)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if sess.State != "st-1" || sess.AuthURL != "https://login.example/?state=st-1" {
		t.Errorf("session=%+v", sess)
	}
	if sess.Realm != realm.WB {
		t.Errorf("realm=%q", sess.Realm)
	}
}

// TestPollPending 未完成：上游业务 code 非 0 → ErrPending（调用方应继续轮询而非报错）。
func TestPollPending(t *testing.T) {
	_, prof := stubUpstream(t, "pending")
	_, err := Poll(*prof, "st-1")
	if !errors.Is(err, ErrPending) {
		t.Fatalf("err=%v want ErrPending", err)
	}
}

// TestPollServerError 5xx 是上游故障，不该被当成"还没登录"。
func TestPollServerError(t *testing.T) {
	_, prof := stubUpstream(t, "server_error")
	_, err := Poll(*prof, "st-1")
	if err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("err=%v want 非 pending 的真实错误", err)
	}
}

// TestPollSuccess 完成：返回 token + 账号信息，realm 标注正确。
func TestPollSuccess(t *testing.T) {
	_, prof := stubUpstream(t, "ok")
	b, err := Poll(*prof, "st-1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if b.AccessToken != "at-1" || b.RefreshToken != "rt-1" || b.ExpiresIn != 3600 {
		t.Errorf("bundle=%+v", b)
	}
	if b.UID != "u-1" || b.Nickname != "nick" || b.EnterpriseID != "e-1" {
		t.Errorf("account=%+v", b)
	}
	if b.Realm != realm.WB {
		t.Errorf("realm=%q", b.Realm)
	}
}

// TestPollEmptyState 空 state 直接报错，不打上游。
func TestPollEmptyState(t *testing.T) {
	_, prof := stubUpstream(t, "ok")
	if _, err := Poll(*prof, ""); err == nil {
		t.Fatal("空 state 应报错")
	}
}

// TestEnvelopeParse 非 JSON 响应要报 parse failed（而不是静默当成 pending）。
func TestEnvelopeParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()
	prof := realm.Profile{Name: realm.CN, ChatBase: srv.URL, Platform: "CLI"}
	if _, err := Start(prof); err == nil || !strings.Contains(err.Error(), "parse failed") {
		t.Fatalf("err=%v want parse failed", err)
	}
}

var _ = json.Marshal
