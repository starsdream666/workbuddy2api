package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/realm"
)

// realmCapture 记录一次出站请求的 URL 与关键头。
type realmCapture struct {
	url     string
	ua      string
	origin  string
	referer string
	product string
	ideType string
	ideName string
	ideVer  string
	domain  string
}

func realmCaptureTransport(c *realmCapture) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		c.url = r.URL.String()
		c.ua = r.Header.Get("User-Agent")
		c.origin = r.Header.Get("Origin")
		c.referer = r.Header.Get("Referer")
		c.product = r.Header.Get("X-Product")
		c.ideType = r.Header.Get("X-IDE-Type")
		c.ideName = r.Header.Get("X-IDE-Name")
		c.ideVer = r.Header.Get("X-IDE-Version")
		c.domain = r.Header.Get("X-Domain")
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
}

// TestChatRealmDispatchBaseAndFingerprint 出站 base 与指纹按 auth.Realm 分派：
// cn+cli 与改造前逐字一致；ai+cli 走 workbuddy.ai；ai+desktop 换成桌面端官方指纹。
func TestChatRealmDispatchBaseAndFingerprint(t *testing.T) {
	cases := []struct {
		name    string
		acct    *auth.Auth
		fp      map[string]realm.Fingerprint
		wantURL string
		wantUA  string
		wantOrg string
		wantPrd string
		wantIDE bool
	}{
		{
			name:    "cn_default_cli",
			acct:    &auth.Auth{AccessToken: "at", UID: "u1"},
			wantURL: "https://copilot.tencent.com/v2/chat/completions",
			wantUA:  "CLI/2.63.2 CodeBuddy/2.63.2",
			wantOrg: "https://www.codebuddy.cn",
			wantPrd: "SaaS",
			wantIDE: false,
		},
		{
			name:    "workbuddy_cli",
			acct:    &auth.Auth{AccessToken: "at", UID: "u1", Realm: realm.WB, Domain: "www.workbuddy.ai"},
			wantURL: "https://www.workbuddy.ai/v2/chat/completions",
			wantUA:  "CLI/2.63.2 CodeBuddy/2.63.2",
			wantOrg: "https://www.workbuddy.ai",
			wantPrd: "SaaS",
			wantIDE: false,
		},
		{
			name:    "workbuddy_desktop_fingerprint",
			acct:    &auth.Auth{AccessToken: "at", UID: "u1", Realm: realm.WB, Domain: "www.workbuddy.ai"},
			fp:      map[string]realm.Fingerprint{realm.WB: realm.FingerprintDesktop},
			wantURL: "https://www.workbuddy.ai/v2/chat/completions",
			wantUA:  "WorkBuddy/5.5.2",
			wantOrg: "https://www.workbuddy.ai",
			wantPrd: "WorkBuddy",
			wantIDE: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap realmCapture
			c := &Client{
				HTTP:              &http.Client{Transport: realmCaptureTransport(&cap)},
				ChatHTTP:          &http.Client{Transport: realmCaptureTransport(&cap)},
				RealmDefault:      realm.CN,
				RealmFingerprints: tc.fp,
			}
			rc, status, _, err := c.ChatStream(tc.acct, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil || status != 200 {
				t.Fatalf("chat: status=%d err=%v", status, err)
			}
			if rc != nil {
				rc.Close()
			}
			if cap.url != tc.wantURL {
				t.Errorf("url=%q want %q", cap.url, tc.wantURL)
			}
			if cap.ua != tc.wantUA {
				t.Errorf("UA=%q want %q", cap.ua, tc.wantUA)
			}
			if cap.origin != tc.wantOrg || cap.referer != tc.wantOrg+"/" {
				t.Errorf("origin=%q referer=%q want %q", cap.origin, cap.referer, tc.wantOrg)
			}
			if cap.product != tc.wantPrd {
				t.Errorf("X-Product=%q want %q", cap.product, tc.wantPrd)
			}
			if tc.wantIDE {
				if cap.ideType != "WorkBuddy" || cap.ideName != "WorkBuddy" || cap.ideVer != "5.5.2" {
					t.Errorf("X-IDE-* = %q/%q/%q", cap.ideType, cap.ideName, cap.ideVer)
				}
			} else if cap.ideType != "" || cap.ideName != "" || cap.ideVer != "" {
				t.Errorf("cli 指纹不该带 X-IDE-*: %q/%q/%q", cap.ideType, cap.ideName, cap.ideVer)
			}
			if tc.acct.Realm == realm.WB && cap.domain != "www.workbuddy.ai" {
				t.Errorf("X-Domain=%q want www.workbuddy.ai", cap.domain)
			}
		})
	}
}

// TestBillingBaseRealmDispatch billing 域同样按 realm 分派；UA 仍保持"默认不设"。
func TestBillingBaseRealmDispatch(t *testing.T) {
	var cap realmCapture
	c := &Client{
		HTTP:         &http.Client{Transport: realmCaptureTransport(&cap)},
		RealmDefault: realm.CN,
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Realm: realm.WB}
	if _, err := c.UserResource(a); err != nil {
		t.Fatalf("user resource: %v", err)
	}
	if !strings.HasPrefix(cap.url, "https://www.workbuddy.ai/v2/billing/meter/get-user-resource") {
		t.Errorf("url=%q", cap.url)
	}
	if cap.ua != "" {
		t.Errorf("billing 默认不该设 UA（保持既有指纹），got %q", cap.ua)
	}

	// 显式配置指纹后，billing 也带该 realm 的 UA（否则同一账号两条路径指纹不一致）。
	var cap2 realmCapture
	c2 := &Client{
		HTTP:              &http.Client{Transport: realmCaptureTransport(&cap2)},
		RealmDefault:      realm.CN,
		RealmFingerprints: map[string]realm.Fingerprint{realm.WB: realm.FingerprintDesktop},
	}
	if _, err := c2.UserResource(a); err != nil {
		t.Fatalf("user resource: %v", err)
	}
	if cap2.ua != "WorkBuddy/5.5.2" {
		t.Errorf("billing UA=%q want WorkBuddy/5.5.2", cap2.ua)
	}
}

// TestRealmDefaultFallback 无 realm 标注的旧凭证按 RealmDefault 走（空 = cn）。
func TestRealmDefaultFallback(t *testing.T) {
	var cap realmCapture
	c := &Client{
		HTTP:         &http.Client{Transport: realmCaptureTransport(&cap)},
		ChatHTTP:     &http.Client{Transport: realmCaptureTransport(&cap)},
		RealmDefault: realm.WB,
	}
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"m","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if rc != nil {
		rc.Close()
	}
	if !strings.HasPrefix(cap.url, "https://www.workbuddy.ai/") {
		t.Errorf("url=%q want workbuddy.ai（RealmDefault=workbuddy）", cap.url)
	}
}

// TestEnsureLeadingSystem workbuddy 线首条必须是 system（上游 400 code=11128），缺失即补齐。
func TestEnsureLeadingSystem(t *testing.T) {
	t.Run("inject_when_missing", func(t *testing.T) {
		out := EnsureLeadingSystem([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		var obj struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(obj.Messages) != 2 || obj.Messages[0]["role"] != "system" {
			t.Fatalf("messages=%v", obj.Messages)
		}
		if obj.Messages[1]["role"] != "user" {
			t.Errorf("原消息顺序被破坏: %v", obj.Messages)
		}
	})
	t.Run("keep_existing_system", func(t *testing.T) {
		src := []byte(`{"messages":[{"role":"system","content":"人格"},{"role":"user","content":"hi"}]}`)
		if out := string(EnsureLeadingSystem(src)); out != string(src) {
			t.Errorf("已有 system 应原样返回, got %s", out)
		}
	})
	t.Run("non_json_passthrough", func(t *testing.T) {
		src := []byte(`not json`)
		if out := string(EnsureLeadingSystem(src)); out != string(src) {
			t.Errorf("非 JSON 应原样返回, got %s", out)
		}
	})
}

// TestPrepareBodyForRealm：workbuddy 线（含旧名 ai）出站 body 补齐 system，cn 线不动。
func TestPrepareBodyForRealm(t *testing.T) {
	c := &Client{SanitizeFingerprints: false, RealmDefault: realm.CN}
	src := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	cnOut := c.prepareBodyFor(&auth.Auth{UID: "u1"}, src)
	if strings.Contains(string(cnOut), `"system"`) {
		t.Errorf("cn 线不该注入 system: %s", cnOut)
	}
	// 旧名 "ai" 仍必须享受国际线待遇（历史 auth 文件零改动）。
	legacyOut := c.prepareBodyFor(&auth.Auth{UID: "u1", Realm: realm.AI}, src)
	if !strings.Contains(string(legacyOut), `"system"`) {
		t.Errorf("旧名 ai 也应注入 system: %s", legacyOut)
	}
}

// TestLegacyAIRealmAlias 旧名 realm "ai"（历史 auth 文件 / 旧配置）必须仍按 workbuddy 线出站：
// 改名之后旧部署零改动继续工作，这条链路要锁住（读旧 realm → 打 workbuddy 域）。
func TestLegacyAIRealmAlias(t *testing.T) {
	var cap realmCapture
	c := &Client{
		HTTP:         &http.Client{Transport: realmCaptureTransport(&cap)},
		ChatHTTP:     &http.Client{Transport: realmCaptureTransport(&cap)},
		RealmDefault: realm.CN,
	}
	rc, status, _, err := c.ChatStream(
		&auth.Auth{AccessToken: "at", UID: "u1", Realm: realm.AI, Domain: "www.workbuddy.ai"},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
	)
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if rc != nil {
		rc.Close()
	}
	if !strings.HasPrefix(cap.url, "https://www.workbuddy.ai/") {
		t.Fatalf("旧名 ai 的出站 url=%q want www.workbuddy.ai", cap.url)
	}
	if cap.domain != "www.workbuddy.ai" {
		t.Errorf("X-Domain=%q want www.workbuddy.ai", cap.domain)
	}
}
