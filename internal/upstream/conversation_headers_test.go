package upstream

import (
	"net/http/httptest"
	"regexp"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/realm"
)

// uuidV4Re 标准 UUID v4 形态（版本位 4、变体位 8/9/a/b）。
var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var conversationHeaders = []string{
	"X-Conversation-ID",
	"X-Conversation-Request-ID",
	"X-Conversation-Message-ID",
}

// TestChatHeadersCarryConversationTrio 官方会话身份三件套：chat 必带、三值互不相同、
// 都是 UUID v4 形态、每次请求新生成。
//
// 为什么必须钉住：国际网关在「首条必须是 system」之外另有一道**会话身份闸**，
// 缺三件套即 HTTP 400 + code 11128（msg 可能是 "request illegal"，也可能是
// "conversation id missing"）。本实例实测：uid=816db6e5 / f4e1d05d 不带时 2/2 被拦，
// 补上后 3/3 全部 200 —— 这一族头是「请求能不能过」的分水岭，不是可选美化。
//
// 三个值若被「优化」成模块级常量，等于拿一个 id 冒充三次会话身份，而上游拿它做的
// 正是关联，所以「每请求新生成」与「三值互不相同」两条都要单独断言。
func TestChatHeadersCarryConversationTrio(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}

	first := httptest.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(first, a)

	seen := map[string]string{}
	for _, k := range conversationHeaders {
		v := first.Header.Get(k)
		if v == "" {
			t.Fatalf("chat 缺 %s（国际网关缺它即 11128）", k)
		}
		if !uuidV4Re.MatchString(v) {
			t.Errorf("%s=%q 不是 UUID v4 形态", k, v)
		}
		if prev, dup := seen[v]; dup {
			t.Errorf("%s 与 %s 取了同一个值 %q —— 复用等于拿一个 id 冒充三次会话身份", k, prev, v)
		}
		seen[v] = k
	}

	// 每请求新生成：第二次的值必须与第一次全不同。
	second := httptest.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(second, a)
	for _, k := range conversationHeaders {
		if first.Header.Get(k) == second.Header.Get(k) {
			t.Errorf("%s 在两次请求间被复用（%q）—— 应每请求新生成", k, first.Header.Get(k))
		}
	}
}

// TestBillingHeadersNoConversationTrio 三件套只属于 chat 端点。
//
// 官方客户端调 billing / refresh 时不带这三头，实测也不影响；把它加过去只会多出
// 一个「只在 billing 端可见」的指纹差异 —— 那正是历史上长成「国际版专有、
// 国内侧完全看不见」那一类缺陷的形状。
func TestBillingHeadersNoConversationTrio(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at1", EnterpriseID: "e1"}

	req := httptest.NewRequest("POST", "https://example.invalid/v2/billing/meter/get-user-resource", nil)
	c.BillingHeaders(req, a)
	for _, k := range conversationHeaders {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("billing 不该带 %s=%q", k, v)
		}
	}

	// refresh 端点同样不带（它走 CommonHeaders + X-Refresh-Token）。
	rreq := httptest.NewRequest("POST", "https://example.invalid/v2/plugin/auth/token/refresh", nil)
	c.RefreshHeaders(rreq, a)
	for _, k := range conversationHeaders {
		if v := rreq.Header.Get(k); v != "" {
			t.Errorf("refresh 不该带 %s=%q", k, v)
		}
	}
}

// TestCommonHeadersCarryCodeBuddyRequestFlag X-CodeBuddy-Request 是官方定义的
// 安全请求头（值固定 "1"，除豁免路径外所有 API 请求必须携带），chat 与 refresh
// 都走 CommonHeaders，必须在。
func TestCommonHeadersCarryCodeBuddyRequestFlag(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}

	req := httptest.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, a)
	if got := req.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("chat X-CodeBuddy-Request=%q want \"1\"（官方要求所有 API 请求必带）", got)
	}
}

// TestNewUUIDv4ShapeAndUniqueness newUUIDv4 的形态与唯一性（三件套的地基）。
func TestNewUUIDv4ShapeAndUniqueness(t *testing.T) {
	got := map[string]bool{}
	for i := 0; i < 200; i++ {
		v := newUUIDv4()
		if !uuidV4Re.MatchString(v) {
			t.Fatalf("newUUIDv4()=%q 不是 UUID v4 形态", v)
		}
		if got[v] {
			t.Fatalf("newUUIDv4() 生成重复值 %q", v)
		}
		got[v] = true
	}
}

// TestRequestIDMatchesMessageID 官方客户端把 X-Request-ID 发成与
// X-Conversation-Message-ID **同一个值**（application-manifest.js 逆向原文：
// 「the message and request ids are the same one, as WorkBuddy's client has them」）。
// 我们此前完全没发 X-Request-ID，是「少了」的那一项。
func TestRequestIDMatchesMessageID(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}
	req := httptest.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, a)

	reqID := req.Header.Get("X-Request-ID")
	msgID := req.Header.Get("X-Conversation-Message-ID")
	if reqID == "" {
		t.Fatal("chat 缺 X-Request-ID（官方客户端必发，且与 Message-ID 同值）")
	}
	if reqID != msgID {
		t.Errorf("X-Request-ID=%q 应等于 X-Conversation-Message-ID=%q", reqID, msgID)
	}
}

// TestXNoHeadersOnlyForCLIFingerprint X-No-* 是 CLI 的「缺省字段声明」约定；
// 官方桌面端在凭据缺失时**什么都不发**（application-manifest.js：
// 「A personal account has none and sends none」）。此前两条线共用一套 X-No-*，
// 桌面指纹因此多发了官方客户端从不发的头。
func TestXNoHeadersOnlyForCLIFingerprint(t *testing.T) {
	// 故意不带 EnterpriseID / Domain，触发「缺省字段」分支。
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}

	cli := New()
	cli.RealmDefault = realm.CN
	reqCLI := httptest.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	cli.ChatHeaders(reqCLI, a)
	if got := reqCLI.Header.Get("X-No-Enterprise-Id"); got != "1" {
		t.Errorf("CLI 指纹应发 X-No-Enterprise-Id=1（缺省字段声明），got %q", got)
	}

	desk := New()
	desk.RealmDefault = realm.CN
	desk.RealmFingerprints = map[string]realm.Fingerprint{realm.CN: realm.FingerprintDesktop}
	reqDesk := httptest.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	desk.ChatHeaders(reqDesk, a)
	for _, k := range []string{"X-No-Enterprise-Id", "X-No-Department-Info", "X-No-User-Id", "X-No-Authorization"} {
		if v := reqDesk.Header.Get(k); v != "" {
			t.Errorf("桌面指纹不该发 %s=%q（官方桌面端个人号不发任何 X-No-*）", k, v)
		}
	}
}

// TestEnterpriseAccountSendsBothIDHeaders 企业号：官方同时发 X-Enterprise-Id
// 与 X-Tenant-Id（同值，application-manifest.js 的 auth interceptor）。
// 此前只发了前者 —— 这是「少了」的另一项。
func TestEnterpriseAccountSendsBothIDHeaders(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at1", EnterpriseID: "e1"}
	req := httptest.NewRequest("POST", "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, a)

	if got := req.Header.Get("X-Enterprise-Id"); got != "e1" {
		t.Errorf("X-Enterprise-Id=%q want e1", got)
	}
	if got := req.Header.Get("X-Tenant-Id"); got != "e1" {
		t.Errorf("X-Tenant-Id=%q want e1（官方两个都发且同值）", got)
	}
	if got := req.Header.Get("X-No-Enterprise-Id"); got != "" {
		t.Errorf("企业号不该发 X-No-Enterprise-Id，got %q", got)
	}
}
