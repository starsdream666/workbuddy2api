package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

// consoleUpstream 控制台用到的上游假实现：余额 + OAuth 三条路径。
// tokenMode 控制 /auth/token 返回 pending 还是成功。
func consoleUpstream(tokenMode string, calls *atomic.Int32) *upstream.Client {
	base := "https://fake.example"
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if calls != nil {
				calls.Add(1)
			}
			hdr := http.Header{"Content-Type": []string{"application/json"}}
			body := ""
			status := 200
			switch {
			case strings.Contains(r.URL.Path, "/billing/meter/get-user-resource"):
				if tokenMode == "zero" {
					// 余额为 0：用于验证"刷到 0 要冻结"与系统判定同口径。
					body = `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"x","CycleCapacitySize":500,"CycleCapacityRemain":0,"CycleCapacityUsed":500}]}}}}`
				} else {
					body = `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"x","CycleCapacitySize":500,"CycleCapacityRemain":321,"CycleCapacityUsed":179}]}}}}`
				}
			case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
				body = `{"code":0,"data":{"state":"st-9","authUrl":"https://login.example/st-9"}}`
			case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
				if tokenMode == "pending" {
					body = `{"code":11217,"msg":"login ing"}`
				} else {
					body = `{"code":0,"data":{"accessToken":"at-9","refreshToken":"rt-9","expiresIn":3600,"domain":"www.workbuddy.ai"}}`
				}
			case strings.HasPrefix(r.URL.Path, "/v2/plugin/login/account"):
				body = `{"code":0,"data":{"uid":"u-new","nickname":"newbie","enterpriseId":""}}`
			default:
				status, body = 404, `{"code":404,"msg":"nope"}`
			}
			return &http.Response{StatusCode: status, Header: hdr, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		RealmDefault: realm.CN,
		Profiles: map[string]realm.Profile{
			realm.WB: {Name: realm.WB, ChatBase: base, BillingBase: base, Platform: "workbuddy-ai", Origin: base},
			realm.CN: {Name: realm.CN, ChatBase: base, BillingBase: base, Platform: "CLI", Origin: base},
		},
		RealmVersions: map[string]string{},
	}
}

func consoleHandler(t *testing.T, dir string, up *upstream.Client, reload func() (map[string]int, error), keys map[string]string) (*Handler, *pool.Pool) {
	t.Helper()
	aiPool := testPoolWith(&auth.Auth{
		UID: "u-ai", AccessToken: "at-ai", RefreshToken: "rt-ai", ExpiresAt: 9999999999,
		Realm: realm.WB, Nickname: "stars",
	})
	cnPool := pool.New("")
	h := NewHandler(Config{
		Pool:           aiPool,
		Pools:          map[string]*pool.Pool{realm.CN: cnPool, realm.WB: aiPool},
		RealmKeys:      keys,
		DefaultRealm:   realm.WB,
		APIKey:         "sk-master",
		Upstream:       up,
		ConsoleEnabled: true,
		ReloadAuths:    reload,
		AuthDir:        dir,
	})
	return h, aiPool
}

func adminGet(t *testing.T, h *Handler, path, key string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// TestConsoleOverviewExposesCreditsKnown 控制台必须透出 credits_known。
//
// 没有它前端无法区分"未观测"与"已耗尽"：前者要显示 —（待探测），
// 后者要显示 0（真的没额度了）。两者在选号与冻结判定上完全不同，
// 显示层混为一谈会让人误判账号状态。
func TestConsoleOverviewExposesCreditsKnown(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	// 新增一个从未观测过的账号（creditsKnown=false）。
	aiPool.Add(&auth.Auth{UID: "u-fresh", AccessToken: "at-fresh", ExpiresAt: 9999999999, Realm: realm.WB, Nickname: "fresh"})
	aiPool.SetCredits("u-ai", 390)

	code, body := adminGet(t, h, "/admin/api/overview", "sk-master")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	accounts, _ := body["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(accounts))
	}
	seen := map[string]map[string]any{}
	for _, a := range accounts {
		v := a.(map[string]any)
		seen[v["uid"].(string)] = v
	}
	// 已观测：known=true，且两个口径都在。
	known := seen["u-ai"]
	if known["credits_known"] != true {
		t.Errorf("u-ai credits_known=%v want true", known["credits_known"])
	}
	if known["credits"] != float64(390) || known["credits_exact"] != float64(390) {
		t.Errorf("u-ai credits=%v exact=%v want 390/390", known["credits"], known["credits_exact"])
	}
	// 未观测：known=false——前端据此显示 — 而不是 0。
	fresh := seen["u-fresh"]
	if fresh["credits_known"] != false {
		t.Errorf("u-fresh credits_known=%v want false (must be able to tell unknown from exhausted)", fresh["credits_known"])
	}
}

func adminPost(t *testing.T, h *Handler, path, key, payload string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(payload))
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

// TestConsolePagePublic 静态页无鉴权可访问（不含数据），API 必须鉴权。
func TestConsolePagePublic(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	code, _ := adminGet(t, h, "/admin", "")
	if code != 200 {
		t.Errorf("/admin code=%d want 200", code)
	}
	code, _ = adminGet(t, h, "/admin/api/overview", "")
	if code != 401 {
		t.Errorf("overview 无 key code=%d want 401", code)
	}
}

// TestConsoleAuthAnyKey 全局 key 与 realm 专属 key 都能进控制台（管理面不按 realm 切）。
func TestConsoleAuthAnyKey(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, map[string]string{realm.WB: "sk-ai"})
	for _, key := range []string{"sk-master", "sk-ai"} {
		if code, _ := adminGet(t, h, "/admin/api/overview", key); code != 200 {
			t.Errorf("key=%s code=%d want 200", key, code)
		}
	}
	if code, _ := adminGet(t, h, "/admin/api/overview", "bad"); code != 401 {
		t.Errorf("坏 key code=%d want 401", code)
	}
}

// TestConsoleOverviewListsRealm 凭证列表带 realm 归属与积分。
func TestConsoleOverviewListsRealm(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	aiPool.SetCredits("u-ai", 88)
	code, body := adminGet(t, h, "/admin/api/overview", "sk-master")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	accounts, _ := body["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts=%v", body["accounts"])
	}
	a := accounts[0].(map[string]any)
	if a["realm"] != realm.WB || a["uid"] != "u-ai" || a["credits"] != float64(88) {
		t.Errorf("account=%v", a)
	}
	totals := body["totals"].(map[string]any)
	if totals["accounts"] != float64(1) || totals["credits"] != float64(88) {
		t.Errorf("totals=%v", totals)
	}
}

// TestConsoleOverviewAliasNotDoubleCounted 路线别名（codebuddy → workbuddy）共用同一个池对象：
// 凭证表与 totals **都不能**把同一批账号算两遍——前端是按 accounts 全表渲染的，
// 重复一次就会在页面上直接多出一半行，且额度合计翻倍。
func TestConsoleOverviewAliasNotDoubleCounted(t *testing.T) {
	aiPool := testPoolWith(&auth.Auth{
		UID: "u-wb", AccessToken: "at-wb", ExpiresAt: 9999999999, Realm: realm.WB, Nickname: "stars",
	})
	aiPool.SetCredits("u-wb", 77)
	h := NewHandler(Config{
		Pool:           aiPool,
		Pools:          map[string]*pool.Pool{realm.WB: aiPool, realm.CB: aiPool}, // 别名共享同一池
		RealmKeys:      map[string]string{realm.WB: "sk-wb", realm.CB: "sk-cb"},
		DefaultRealm:   realm.WB,
		APIKey:         "sk-master",
		Upstream:       consoleUpstream("ok", nil),
		ConsoleEnabled: true,
	})
	code, body := adminGet(t, h, "/admin/api/overview", "sk-master")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	accounts, _ := body["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts=%v want 1（别名不该把同一账号列两遍）", body["accounts"])
	}
	if got := accounts[0].(map[string]any)["realm"]; got != realm.WB {
		t.Errorf("账号归属 realm=%v want %s（别名不产生新账号）", got, realm.WB)
	}
	totals := body["totals"].(map[string]any)
	if totals["accounts"] != float64(1) || totals["healthy"] != float64(1) {
		t.Errorf("totals=%v want accounts=1 healthy=1", totals)
	}
	if totals["credits"] != float64(77) {
		t.Errorf("totals.credits=%v want 77（额度也不该翻倍）", totals["credits"])
	}
	routes, _ := body["routes"].(map[string]any)
	if routes[realm.CB] != realm.WB {
		t.Errorf("routes=%v want codebuddy → workbuddy", body["routes"])
	}
}

// TestConsoleBalanceWritesBack 刷新余额会把上游数值写回池（列表随之更新）。
func TestConsoleBalanceWritesBack(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	code, body := adminPost(t, h, "/admin/api/balance", "sk-master", "{}")
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	updated, _ := body["updated"].(map[string]any)
	if updated["u-ai"] != float64(321) {
		t.Errorf("updated=%v want u-ai=321", updated)
	}
	st, _ := aiPool.Status("u-ai")
	if st.Credits != 321 {
		t.Errorf("池内积分=%d want 321", st.Credits)
	}
	_, overview := adminGet(t, h, "/admin/api/overview", "sk-master")
	if overview["totals"].(map[string]any)["credits"] != float64(321) {
		t.Errorf("overview credits=%v", overview["totals"])
	}
}

// TestConsoleBalanceSingleUID 单号刷新：只查指定账号，绝不动其他账号。
//
// 这是控制台"↻"按钮的后端契约。全量刷新会把池里每个号都打一遍上游，
// 而用户往往只想确认"刚才那个号还剩多少"——按 uid 过滤是这个入口存在的前提。
func TestConsoleBalanceSingleUID(t *testing.T) {
	var calls atomic.Int32
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", &calls), nil, nil)
	// 再加一个号：它**不该**被查询。
	aiPool.Add(&auth.Auth{UID: "u-other", AccessToken: "at-other", ExpiresAt: 9999999999, Realm: realm.WB, Nickname: "other"})
	aiPool.SetCredits("u-other", 777)

	code, body := adminPost(t, h, "/admin/api/balance", "sk-master", `{"uid":"u-ai"}`)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	updated, _ := body["updated"].(map[string]any)
	if len(updated) != 1 || updated["u-ai"] != float64(321) {
		t.Errorf("updated=%v want only u-ai=321", updated)
	}
	if _, ok := updated["u-other"]; ok {
		t.Error("u-other must not be queried when uid is specified")
	}
	// 上游只被调用一次（单号 = 一次查询；全量会是每个号一次）。
	if n := calls.Load(); n != 1 {
		t.Errorf("upstream calls=%d want 1 (single-account refresh must not fan out)", n)
	}
	// 未指定的账号余额保持原样，不被改写。
	st, _ := aiPool.Status("u-other")
	if st.Credits != 777 {
		t.Errorf("u-other credits=%d want 777 (untouched)", st.Credits)
	}
}

// TestConsoleBalanceUnknownUID 未知 uid：不报错、不更新任何账号。
//
// 控制台的 uid 来自列表快照，而列表可能已被重扫/删除改变——
// 这种竞态不该让整个请求 500，安静地"零更新"即可。
func TestConsoleBalanceUnknownUID(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	aiPool.SetCredits("u-ai", 88)

	code, body := adminPost(t, h, "/admin/api/balance", "sk-master", `{"uid":"ghost"}`)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	updated, _ := body["updated"].(map[string]any)
	if len(updated) != 0 {
		t.Errorf("updated=%v want empty for unknown uid", updated)
	}
	if st, _ := aiPool.Status("u-ai"); st.Credits != 88 {
		t.Errorf("existing account must be untouched: credits=%d want 88", st.Credits)
	}
}

// TestConsoleBalanceZeroFreezes 刷到 0 余额要**冻结**账号，而不只是改显示数字。
//
// 与所有真实上游观测路径（请求出口刷新 / 额度巡检）同口径。
// 否则控制台显示 0、账号却仍在轮转里白撞 429/402，显示与调度状态脱节。
func TestConsoleBalanceZeroFreezes(t *testing.T) {
	// 上游返回 0 余额。
	up := consoleUpstream("zero", nil)
	h, aiPool := consoleHandler(t, t.TempDir(), up, nil, nil)
	aiPool.SetCredits("u-ai", 50)
	if aiPool.IsFrozen("u-ai") {
		t.Fatal("precondition: account should start unfrozen")
	}

	code, body := adminPost(t, h, "/admin/api/balance", "sk-master", `{"uid":"u-ai"}`)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	if updated, _ := body["updated"].(map[string]any); updated["u-ai"] != float64(0) {
		t.Errorf("updated=%v want u-ai=0", updated)
	}
	if !aiPool.IsFrozen("u-ai") {
		t.Error("account with 0 balance must be frozen (same judgement as scheduled credit watch)")
	}
	if st, _ := aiPool.Status("u-ai"); st.Credits != 0 {
		t.Errorf("credits=%d want 0", st.Credits)
	}
}

// TestConsoleLoginFlow 控制台登录：start 拿链接 → poll pending → poll done 落盘 + 热加载。
func TestConsoleLoginFlow(t *testing.T) {
	dir := t.TempDir()
	var reloaded atomic.Int32
	reload := func() (map[string]int, error) {
		reloaded.Add(1)
		return map[string]int{realm.WB: 2}, nil
	}
	h, _ := consoleHandler(t, dir, consoleUpstream("pending", nil), reload, nil)

	// 请求体里**故意**用旧名 "ai"：归一化后必须落到 workbuddy 线（legacy 兼容）。
	code, body := adminPost(t, h, "/admin/api/login/start", "sk-master", `{"realm":"ai"}`)
	if code != 200 {
		t.Fatalf("start code=%d body=%v", code, body)
	}
	if body["auth_url"] != "https://login.example/st-9" {
		t.Errorf("auth_url=%v", body["auth_url"])
	}
	state, _ := body["state"].(string)
	if state == "" {
		t.Fatal("缺少 state")
	}

	code, body = adminGet(t, h, "/admin/api/login/poll?state="+state, "sk-master")
	if code != 200 || body["status"] != "pending" {
		t.Fatalf("pending poll: code=%d body=%v", code, body)
	}
	if reloaded.Load() != 0 {
		t.Error("未完成时不该触发热加载")
	}

	// 切到成功模式，模拟用户完成登录后再次轮询。
	h.cfg.Upstream = consoleUpstream("ok", nil)
	code, body = adminGet(t, h, "/admin/api/login/poll?state="+state, "sk-master")
	if code != 200 || body["status"] != "done" {
		t.Fatalf("done poll: code=%d body=%v", code, body)
	}
	if body["uid"] != "u-new" || body["realm"] != realm.WB {
		t.Errorf("poll body=%v", body)
	}
	if reloaded.Load() != 1 {
		t.Errorf("热加载调用次数=%d want 1", reloaded.Load())
	}

	// 凭证落盘：文件名按 realm 前缀，realm 字段写入，可被 auth.Parse 直接读回。
	files, _ := filepath.Glob(filepath.Join(dir, "workbuddy-ai-u-new.json"))
	if len(files) != 1 {
		t.Fatalf("落盘文件=%v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Realm != realm.WB || a.UID != "u-new" || a.AccessToken != "at-9" {
		t.Errorf("落盘内容=%+v", a)
	}

	// 同一 state 只能消费一次。
	code, body = adminGet(t, h, "/admin/api/login/poll?state="+state, "sk-master")
	if code != 404 || body["status"] != "unknown" {
		t.Errorf("重复 poll: code=%d body=%v", code, body)
	}
}

// TestConsoleReloadEndpoint 手动重扫凭证目录。
func TestConsoleReloadEndpoint(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), func() (map[string]int, error) {
		return map[string]int{realm.WB: 1}, nil
	}, nil)
	code, body := adminPost(t, h, "/admin/api/reload", "sk-master", "{}")
	if code != 200 || body["ok"] != true {
		t.Fatalf("reload code=%d body=%v", code, body)
	}
}

// TestConsoleDisabled 关闭控制台时路由不存在（404）。
func TestConsoleDisabled(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: consoleUpstream("ok", nil)})
	if code, _ := adminGet(t, h, "/admin", ""); code != 404 {
		t.Errorf("/admin code=%d want 404（未启用控制台）", code)
	}
	if code, _ := adminGet(t, h, "/admin/api/overview", "k"); code != 404 {
		t.Errorf("api code=%d want 404", code)
	}
}

// TestAdminModelsAggregatesRates /admin/api/models 聚合全部启用线的模型并透出上游倍率。
// 倍率来自上游下发的 credits 字段：数字 / "2x" 都要解析出 rate，null 则无 rate。
func TestAdminModelsAggregatesRates(t *testing.T) {
	// 模型缓存是包级共享的：测试拉取成功会写入 cn/wb 缓存，污染后续依赖冷启动
	// 回落静态表的用例（如 TestModelsEndpoint）。前后都要清。
	resetModelsCache(t, realm.CN)
	resetModelsCache(t, realm.WB)
	resetModelsCache(t, realm.CB)
	t.Cleanup(func() {
		resetModelsCache(t, realm.CN)
		resetModelsCache(t, realm.WB)
		resetModelsCache(t, realm.CB)
	})

	const cnModels = `{"code":0,"data":{"models":[{"id":"cn-a","maxInputTokens":100,"maxOutputTokens":10,"credits":"x0.00"},{"id":"cn-b","maxInputTokens":100,"maxOutputTokens":10,"credits":"x2.00 credits"}],"agents":[{"name":"cli","models":["cn-a","cn-b"]}]}}`
	const intlModels = `{"code":0,"data":{"models":[{"id":"gpt-6-astra","maxInputTokens":200,"maxOutputTokens":20,"credits":"x1.50","vendor":"e","reasoning":{"effort":"high"}}],"agents":[{"name":"cli","models":["gpt-6-astra"]}]}}`

	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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
	wbPool := testPoolWith(&auth.Auth{UID: "u-wb", AccessToken: "at-wb", ExpiresAt: 9999999999, Realm: realm.WB})
	h := NewHandler(Config{
		Pool:           cnPool,
		Pools:          map[string]*pool.Pool{realm.CN: cnPool, realm.WB: wbPool, realm.CB: wbPool},
		DefaultRealm:   realm.CN,
		APIKey:         "sk-master",
		Upstream:       up,
		ConsoleEnabled: true,
	})

	code, body := adminGet(t, h, "/admin/api/models", "sk-master")
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	rows, _ := body["data"].([]any)
	if len(rows) == 0 {
		t.Fatalf("模型列表为空: %v", body)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		byID[m["id"].(string)] = m
	}
	// cn 线：免费模型 "x0.00" → rate=0（可判免费）；"x2.00 credits" → rate=2。
	if v, ok := byID["cn-a"]["rate"]; !ok || v != float64(0) {
		t.Errorf("cn-a rate=%v want 0", byID["cn-a"]["rate"])
	}
	if v := byID["cn-b"]; v["rate"] != float64(2) || v["credits"] != "x2.00 credits" {
		t.Errorf("cn-b=%v want rate=2 credits=x2.00 credits", v)
	}
	// 国际线：倍率 "x1.50" → rate=1.5；vendor 由模型 id 推导为可读名，原始字母进 raw_vendor；
	// 无 supportedEfforts 但带 reasoning.effort → 档位回退官方全档（6 个）。
	v := byID["gpt-6-astra"]
	if v["rate"] != float64(1.5) || v["vendor"] != "OpenAI" || v["raw_vendor"] != "e" {
		t.Errorf("gpt-6-astra=%v want rate=1.5 vendor=OpenAI raw_vendor=e", v)
	}
	if v["realm"] != realm.WB && v["realm"] != realm.CB {
		t.Errorf("gpt-6-astra realm=%v want workbuddy|codebuddy", v["realm"])
	}
	if eff, _ := v["supported_efforts"].([]any); len(eff) != 6 {
		t.Errorf("gpt-6-astra supported_efforts=%v want 全档 6 个（无 supportedEfforts 时回退）", v["supported_efforts"])
	}
}
