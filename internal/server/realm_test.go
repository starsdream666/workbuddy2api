package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

// authzRecorder 记录上游收到的 Authorization、Host 与请求体，用于断言
// "请求落到哪个池""出站打哪条线的域""转发给上游的模型名（渠道前缀是否已剥离）"。
type authzRecorder struct {
	mu     sync.Mutex
	seen   []string
	hosts  []string
	models []string
}

func (r *authzRecorder) add(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, v)
}

func (r *authzRecorder) addHost(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts = append(r.hosts, v)
}

func (r *authzRecorder) addModel(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = append(r.models, v)
}

func (r *authzRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *authzRecorder) hostsAll() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

func (r *authzRecorder) modelsAll() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.models...)
}

// recorderUpstream 上游假实现：记录 Authorization/Host/请求体模型名并回 200 SSE。
func recorderUpstream(rec *authzRecorder) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec.add(r.Header.Get("Authorization"))
			rec.addHost(r.URL.Host)
			if r.Body != nil {
				if raw, err := io.ReadAll(r.Body); err == nil {
					var peek struct {
						Model string `json:"model"`
					}
					_ = json.Unmarshal(raw, &peek)
					rec.addModel(peek.Model)
				}
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		RealmDefault: realm.CN,
	}
}

// TestRealmOfRequest 请求归属解析：X-Realm 头 > realm 专属 key > DefaultRealm。
func TestRealmOfRequest(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		Upstream:     recorderUpstream(&authzRecorder{}),
		RealmKeys:    map[string]string{realm.WB: "sk-ai"},
		DefaultRealm: realm.CN,
	})
	cases := []struct {
		name   string
		header map[string]string
		want   string
	}{
		{"显式 X-Realm=ai（旧名，归一为 workbuddy）", map[string]string{"X-Realm": "ai"}, realm.WB},
		{"未知 X-Realm 被忽略", map[string]string{"X-Realm": "bogus"}, realm.CN},
		{"realm 专属 key", map[string]string{"Authorization": "Bearer sk-ai"}, realm.WB},
		{"全局 key → DefaultRealm", map[string]string{"Authorization": "Bearer other"}, realm.CN},
		{"无凭据 → DefaultRealm", map[string]string{}, realm.CN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			if got := h.realmOfRequest(req); got != tc.want {
				t.Errorf("realm=%q want %q", got, tc.want)
			}
		})
	}
}

// TestRealmScopedKeyAuth realm 专属 key 的鉴权隔离：跨 realm 用错 key 必须 401，
// 否则"分区"形同虚设（workbuddy key 能驱动 CN 池账号，反之亦然）。
func TestRealmScopedKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u-cn", AccessToken: "at-cn", ExpiresAt: 9999999999}),
		Pools:        map[string]*pool.Pool{},
		RealmKeys:    map[string]string{realm.WB: "sk-ai"},
		DefaultRealm: realm.CN,
		APIKey:       "sk-cn",
		Upstream:     recorderUpstream(&authzRecorder{}),
	})
	// 全局 key（cn）→ 200
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-cn")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("cn key: code=%d want 200", rec.Code)
	}
	// workbuddy 专属 key → 200（该 realm 的 key 有效）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-ai")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("workbuddy key: code=%d want 200", rec.Code)
	}
	// 错 key → 401
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("bad key: code=%d want 401", rec.Code)
	}
	// 全局 key + X-Realm: ai → 200（主人钥匙可进任意 realm，避免把自己锁死）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-cn")
	req.Header.Set("X-Realm", "ai")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("master key + X-Realm: code=%d want 200", rec.Code)
	}
	// realm 专属 key + 别的 realm → 401（隔离：AI key 不能驱动 CN 池）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-ai")
	req.Header.Set("X-Realm", "cn")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("realm key cross line: code=%d want 401", rec.Code)
	}
}

// TestRealmPoolIsolation 池分区：请求只在本 realm 池里选号，绝不跨池回退。
func TestRealmPoolIsolation(t *testing.T) {
	rec := &authzRecorder{}
	cnPool := testPoolWith(&auth.Auth{UID: "u-cn", AccessToken: "at-cn", ExpiresAt: 9999999999})
	aiPool := testPoolWith(&auth.Auth{UID: "u-ai", AccessToken: "at-ai", ExpiresAt: 9999999999, Realm: realm.WB})
	h := NewHandler(Config{
		Pool:         cnPool,
		Pools:        map[string]*pool.Pool{realm.CN: cnPool, realm.WB: aiPool},
		RealmKeys:    map[string]string{realm.WB: "sk-ai"},
		DefaultRealm: realm.CN,
		APIKey:       "sk-cn",
		Upstream:     recorderUpstream(rec),
	})

	// workbuddy key → 必须只用 workbuddy 池账号
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"default-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-ai")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := rec.all(); len(got) != 1 || got[0] != "Bearer at-ai" {
		t.Fatalf("ai 请求出站凭据=%v want [Bearer at-ai]（禁止跨池回退到 cn 号）", got)
	}

	// 全局 key → cn 池
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-cn")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	got := rec.all()
	if len(got) != 2 || got[1] != "Bearer at-cn" {
		t.Fatalf("cn 请求出站凭据=%v", got)
	}
}

// TestModelsPerRealm 模型表按 realm 分派，且**内容来自服务端下发**：
//   - cn 线走控制台接口（/console/enterprises/personal/models）
//   - 国际线（workbuddy / codebuddy）走 /v3/config：codebuddy 与 workbuddy 同后端，拿到的是同一份名单
//   - 每条线的专属 key **只列本线**；只有主人钥匙（全局 api_key）才聚合全部渠道
//
// （改造前这里断言的是手工静态表；模型表已改为以服务端下发为准，故改为断言动态分派。）
func TestModelsPerRealm(t *testing.T) {
	resetModelsCache(t, realm.CN)
	resetModelsCache(t, realm.WB)
	resetModelsCache(t, realm.CB)

	const cnModels = `{"code":0,"data":{"models":[{"id":"cn-dyn-1","maxInputTokens":111,"maxOutputTokens":22}],"agents":[{"name":"cli","models":["cn-dyn-1"]}]}}`
	const intlModels = `{"code":0,"data":{"models":[{"id":"intl-dyn-1","maxInputTokens":333,"maxOutputTokens":44},{"id":"gpt-image-2.5-sunburst","name":"GPT-Image-2.5-Sunburst","tags":["text-to-image","image-to-image"]}],"agents":[{"name":"cli","models":["intl-dyn-1"]}]}}`

	var mu sync.Mutex
	seenPaths := map[string]int{}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			seenPaths[r.URL.Path]++
			mu.Unlock()
			body := `{"code":404,"msg":"not found"}`
			switch r.URL.Path {
			case "/console/enterprises/personal/models":
				body = cnModels
			case "/v3/config":
				body = intlModels
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		RealmDefault: realm.CN,
	}
	cnPool := testPoolWith(&auth.Auth{UID: "u-cn", AccessToken: "at-cn", ExpiresAt: 9999999999})
	aiPool := testPoolWith(&auth.Auth{UID: "u-ai", AccessToken: "at-ai", ExpiresAt: 9999999999, Realm: realm.WB})
	h := NewHandler(Config{
		Pool:         cnPool,
		Pools:        map[string]*pool.Pool{realm.CN: cnPool, realm.WB: aiPool, realm.CB: aiPool}, // 别名共享同一池
		RealmKeys:    map[string]string{realm.WB: "sk-ai", realm.CB: "sk-cb"},
		DefaultRealm: realm.CN,
		APIKey:       "sk-cn",
		Upstream:     up,
	})

	// 主人钥匙（全局 api_key）→ 聚合**全部启用渠道**：cn 裸 id + 两条国际线前缀 id。
	masterBody := modelsFor(t, h, "sk-cn")
	for _, want := range []string{`"id":"cn-dyn-1"`, "workbuddy/intl-dyn-1", "codebuddy/intl-dyn-1"} {
		if !strings.Contains(masterBody, want) {
			t.Errorf("主人钥匙的聚合模型表缺 %s: %s", want, truncateBody(masterBody))
		}
	}
	// cn 没有渠道前缀映射（内置只有 workbuddy/codebuddy）→ 保持裸 id，不硬加前缀。
	if strings.Contains(masterBody, "cn/cn-dyn-1") {
		t.Errorf("cn 不该被硬加渠道前缀: %s", truncateBody(masterBody))
	}

	// workbuddy 的**专属 key** → 只列本线渠道（看不到 codebuddy，也看不到 cn）。
	wbBody := modelsFor(t, h, "sk-ai")
	if !strings.Contains(wbBody, "workbuddy/intl-dyn-1") {
		t.Errorf("workbuddy key 的模型表缺 workbuddy/intl-dyn-1: %s", truncateBody(wbBody))
	}
	for _, banned := range []string{"codebuddy/", "cn-dyn-1"} {
		if strings.Contains(wbBody, banned) {
			t.Errorf("workbuddy key 不该看到 %s: %s", banned, truncateBody(wbBody))
		}
	}

	// 媒体模型（图片 / 视频）必须一并出现，并带 kind 分类——客户端才知道该去调 images / videos 端点。
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(wbBody), &list); err != nil {
		t.Fatalf("模型表不是合法 JSON: %v", err)
	}
	foundMedia := false
	for _, e := range list.Data {
		if e["id"] != "workbuddy/gpt-image-2.5-sunburst" {
			continue
		}
		foundMedia = true
		if e["kind"] != "image" {
			t.Errorf("媒体模型 kind=%v want image", e["kind"])
		}
		if cl, _ := e["context_length"].(float64); cl != 0 {
			t.Errorf("媒体模型不该打 131072 上下文兜底: context_length=%v", e["context_length"])
		}
	}
	if !foundMedia {
		t.Errorf("workbuddy key 的模型表缺媒体模型: %s", truncateBody(wbBody))
	}

	// codebuddy 的专属 key → 只列 codebuddy 线（同池同源，但两条密钥分开就必须各看各的）。
	cbBody := modelsFor(t, h, "sk-cb")
	if !strings.Contains(cbBody, "codebuddy/intl-dyn-1") {
		t.Errorf("codebuddy key 的模型表缺 codebuddy/intl-dyn-1: %s", truncateBody(cbBody))
	}
	for _, banned := range []string{"workbuddy/", "cn-dyn-1"} {
		if strings.Contains(cbBody, banned) {
			t.Errorf("codebuddy key 不该看到 %s: %s", banned, truncateBody(cbBody))
		}
	}

	mu.Lock()
	paths := map[string]int{}
	for k, v := range seenPaths {
		paths[k] = v
	}
	mu.Unlock()
	if paths["/v3/config"] == 0 || paths["/console/enterprises/personal/models"] == 0 {
		t.Errorf("模型接口分派不对（期望 cn 走控制台、国际线走 /v3/config）: %v", paths)
	}
}

// TestStatusAggregatesRealms 多池时 /status 顶层为聚合口径并附 per-realm 明细。
func TestStatusAggregatesRealms(t *testing.T) {
	cnPool := testPoolWith(&auth.Auth{UID: "u-cn", AccessToken: "at-cn", ExpiresAt: 9999999999})
	aiPool := testPoolWith(
		&auth.Auth{UID: "u-ai1", AccessToken: "at", ExpiresAt: 9999999999, Realm: realm.WB},
		&auth.Auth{UID: "u-ai2", AccessToken: "at", ExpiresAt: 9999999999, Realm: realm.WB},
	)
	h := NewHandler(Config{
		Pool:         cnPool,
		Pools:        map[string]*pool.Pool{realm.CN: cnPool, realm.WB: aiPool},
		DefaultRealm: realm.CN,
		Upstream:     recorderUpstream(&authzRecorder{}),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status json: %v", err)
	}
	if body["total"] != float64(3) {
		t.Errorf("total=%v want 3（聚合两池）", body["total"])
	}
	perRealm, ok := body["realms"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 per-realm 明细: %v", body)
	}
	if len(perRealm) != 2 {
		t.Errorf("realms=%v want cn+workbuddy", perRealm)
	}
	if p, ok := perRealm[realm.WB].(map[string]any); !ok || p["total"] != float64(2) {
		t.Errorf("workbuddy 池明细=%v", perRealm[realm.WB])
	}
}

// TestHealthzAggregatesRealms 任一池可服务即 200（避免"workbuddy 有号但探活 503"）。
func TestHealthzAggregatesRealms(t *testing.T) {
	emptyCN := pool.New("")
	aiPool := testPoolWith(&auth.Auth{UID: "u-ai", AccessToken: "at", ExpiresAt: 9999999999, Realm: realm.WB})
	h := NewHandler(Config{
		Pool:         emptyCN,
		Pools:        map[string]*pool.Pool{realm.CN: emptyCN, realm.WB: aiPool},
		DefaultRealm: realm.CN,
		Upstream:     recorderUpstream(&authzRecorder{}),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200（ai 池有健康账号）", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["total"] != float64(1) || body["healthy"] != float64(1) {
		t.Errorf("healthz=%v", body)
	}
}

// ---- helpers ----

func resetModelsCache(t *testing.T, rn string) {
	t.Helper()
	c := modelsCacheFor(rn)
	c.Lock()
	c.ids = nil
	c.fetched = time.Time{}
	c.lastFail = time.Time{}
	c.Unlock()
}

func modelsFor(t *testing.T, h *Handler, key string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("models code=%d", rec.Code)
	}
	return rec.Body.String()
}

func truncateBody(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// TestCodebuddyRouteReusesAIPool 路线别名（codebuddy）：账号取自 workbuddy 池（同一份凭证、
// 同一份额度），但出站 host 必须是 www.codebuddy.ai —— 只有档案解析换线，池不换。
func TestCodebuddyRouteReusesAIPool(t *testing.T) {
	rec := &authzRecorder{}
	cnPool := testPoolWith(&auth.Auth{UID: "u-cn", AccessToken: "at-cn", ExpiresAt: 9999999999})
	aiPool := testPoolWith(&auth.Auth{UID: "u-ai", AccessToken: "at-ai", ExpiresAt: 9999999999, Realm: realm.WB})
	h := NewHandler(Config{
		Pool:         cnPool,
		Pools:        map[string]*pool.Pool{realm.CN: cnPool, realm.WB: aiPool, realm.CB: aiPool},
		RealmKeys:    map[string]string{realm.WB: "sk-ai", realm.CB: "sk-cb"},
		DefaultRealm: realm.CN,
		APIKey:       "sk-cn",
		Upstream:     recorderUpstream(rec),
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"default-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-cb") // 路线专属 key → X-Realm 之外的显式分流
	req.Header.Set("X-Realm", realm.CB)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := rec.all(); len(got) != 1 || got[0] != "Bearer at-ai" {
		t.Fatalf("codebuddy 路线出站凭据=%v want [Bearer at-ai]（必须复用 ai 池账号）", got)
	}
	hosts := rec.hostsAll()
	if len(hosts) != 1 || hosts[0] != "www.codebuddy.ai" {
		t.Fatalf("codebuddy 路线出站 host=%v want [www.codebuddy.ai]（档案必须按别名线解析）", hosts)
	}

	// 对照：同池走 ai 路线时 host 必须是 www.workbuddy.ai。
	req = httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"default-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-ai")
	req.Header.Set("X-Realm", realm.WB)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	hosts = rec.hostsAll()
	if len(hosts) != 2 || hosts[1] != "www.workbuddy.ai" {
		t.Fatalf("ai 路线出站 host=%v want 第二跳 www.workbuddy.ai", hosts)
	}
}

// TestModelPrefixRouting 模型名的渠道前缀决定路线，且出站前被剥离：
//   - `workbuddy/<模型>` → workbuddy 线（www.workbuddy.ai）
//   - `codebuddy/<模型>` → codebuddy 路线（www.codebuddy.ai），凭据仍复用 workbuddy 池
//   - 越权前缀（`cn/` + workbuddy 凭据：两条线凭证来源不同）→ 不跨池，按凭据所属线走并剥离前缀
//   - 未知前缀 → 整串透传，不做任何改写
func TestModelPrefixRouting(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		key       string
		wantHost  string
		wantModel string
	}{
		{"workbuddy 前缀 → workbuddy 线", "workbuddy/default-model", "sk-ai", "www.workbuddy.ai", "default-model"},
		{"codebuddy 前缀 → codebuddy 路线（复用 workbuddy 凭据）", "codebuddy/default-model", "sk-ai", "www.codebuddy.ai", "default-model"},
		{"越权前缀不跨池：cn/ + workbuddy 凭据仍走 workbuddy，只剥离前缀", "cn/glm-5.2", "sk-ai", "www.workbuddy.ai", "glm-5.2"},
		{"未知前缀整串透传", "foo/bar", "sk-ai", "www.workbuddy.ai", "foo/bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &authzRecorder{}
			cnPool := testPoolWith(&auth.Auth{UID: "u-cn", AccessToken: "at-cn", ExpiresAt: 9999999999})
			aiPool := testPoolWith(&auth.Auth{UID: "u-ai", AccessToken: "at-ai", ExpiresAt: 9999999999, Realm: realm.WB})
			h := NewHandler(Config{
				Pool:  cnPool,
				Pools: map[string]*pool.Pool{realm.CN: cnPool, realm.WB: aiPool, realm.CB: aiPool},
				// cn 也配一把专属 key：这样"越权前缀"用例才会走到 AuthRealm 比对分支，
				// 而不是被"该线未配 key → 放行"规则短路。
				RealmKeys: map[string]string{realm.CN: "sk-cn-realm", realm.WB: "sk-ai"},
				// 显式声明三条前缀（含 cn），用来验证越权保护。
				ChannelPrefixes: map[string]string{"workbuddy": realm.WB, "codebuddy": realm.CB, "cn": realm.CN},
				DefaultRealm:    realm.CN,
				APIKey:          "sk-master",
				Upstream:        recorderUpstream(rec),
			})
			body := `{"model":"` + tc.model + `","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tc.key)
			req.Header.Set("Content-Type", "application/json")
			h.ServeHTTP(httptest.NewRecorder(), req)

			hosts := rec.hostsAll()
			models := rec.modelsAll()
			if len(hosts) != 1 {
				t.Fatalf("出站次数=%d want 1（hosts=%v）", len(hosts), hosts)
			}
			if hosts[0] != tc.wantHost {
				t.Errorf("出站 host=%q want %q", hosts[0], tc.wantHost)
			}
			if len(models) != 1 || models[0] != tc.wantModel {
				t.Errorf("转发模型名=%v want [%s]（渠道前缀必须剥离）", models, tc.wantModel)
			}
		})
	}
}

// TestModelsFetchFailureAccounting 模型表拉取失败的记账口径：
//   - 连接层故障（DNS 黑洞 / 连不上 / 超时）→ 该渠道返回空列表，但**不记账号错误**。
//     否则每次拉模型表都给健康号记一次 errTotal + 熔断失败，几轮下来把能聊天的号提前熔断
//     （真实事故：海外机解析 www.codebuddy.ai 拿到 0.0.0.1，codebuddy 渠道永远是空列表）；
//   - 上游确实回了非 200 → 这是账号/凭据问题，照旧记账号错误。
func TestModelsFetchFailureAccounting(t *testing.T) {
	cases := []struct {
		name      string
		transport bool
		wantErrs  int64
	}{
		{"连接层失败不罚号", true, 0},
		{"上游 500 记账号错误", false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetModelsCache(t, realm.WB)
			pl := testPoolWith(&auth.Auth{UID: "u-wb", AccessToken: "at", ExpiresAt: 9999999999, Realm: realm.WB})
			up := &upstream.Client{
				HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if tc.transport {
						return nil, &url.Error{
							Op:  "Get",
							URL: r.URL.String(),
							Err: errors.New("dial tcp 0.0.0.1:443: connect: connection refused"),
						}
					}
					return &http.Response{
						StatusCode: 500,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"code":500,"msg":"boom"}`)),
					}, nil
				})},
				RealmDefault: realm.WB,
			}
			h := NewHandler(Config{
				Pool:         pl,
				Pools:        map[string]*pool.Pool{realm.WB: pl},
				DefaultRealm: realm.WB,
				APIKey:       "sk-master",
				Upstream:     up,
			})
			body := modelsFor(t, h, "sk-master")
			if !strings.Contains(body, `"data":[]`) {
				t.Errorf("拉取失败时该渠道应返回空列表: %s", truncateBody(body))
			}
			st, ok := pl.Status("u-wb")
			if !ok {
				t.Fatal("账号丢了")
			}
			if st.ErrTotal != tc.wantErrs {
				t.Errorf("err_total=%d want %d（连接层故障不该记到账号健康度上）", st.ErrTotal, tc.wantErrs)
			}
		})
	}
	resetModelsCache(t, realm.WB)
}
