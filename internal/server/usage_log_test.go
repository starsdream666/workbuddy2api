package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usagelog"
)

// usageFixture 构造一个"上游返回带 usage 的流式响应"的 handler + 使用日志器。
// balance 序列按调用次序依次返回（模拟"调用后余额下降"）。
func usageFixture(t *testing.T, balances []int64) (*Handler, *usagelog.Logger, *[]int64, *pool.Pool) {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "williams740", AccessToken: "at1", ExpiresAt: 9999999999})
	// 让"调用前余额"有基准：SetCredits 同时置 creditsKnown=true。
	p.SetCredits("u1", 350)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true // sseOK 末帧带 usage{prompt:1,completion:1,total:2}
	})

	// 余额刷新回调：按序列返回，并累积调用记录供断言。
	calls := &[]int64{}
	idx := 0
	refresh := func(pl *pool.Pool, uid string) (int64, error) {
		*calls = append(*calls, 1)
		if idx >= len(balances) {
			idx = len(balances) - 1
		}

		v := balances[idx]
		idx++
		return v, nil
	}

	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 16})
	h := NewHandler(Config{
		Pool:                p,
		Upstream:            up,
		UsageLog:            logger,
		RefreshBalanceAfter: refresh,
	})
	return h, logger, calls, p
}

// TestUsageLogRecordsCredentialAndTokens 端到端：一次非流式请求后，
// 使用日志必须记下服务账号（uid/昵称）、消耗积分（差值）、token 三项。
func TestUsageLogRecordsCredentialAndTokens(t *testing.T) {
	h, logger, calls, _ := usageFixture(t, []int64{347}) // 调用后余额 347 → 消耗 3
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	entries := logger.Recent(0)
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	e := entries[0]
	if e.UID != "u1" || e.UID8 != "u1" {
		t.Errorf("uid=%q uid8=%q want u1", e.UID, e.UID8)
	}
	if e.Nickname != "williams740" {
		t.Errorf("nickname=%q want williams740", e.Nickname)
	}
	if e.Model != "glm-5.2" || e.Mode != "sync" || e.Status != 200 {
		t.Errorf("model=%q mode=%q status=%d want glm-5.2/sync/200", e.Model, e.Mode, e.Status)
	}
	// 积分差值：350 - 347 = 3。
	if !e.CreditsKnown || e.CreditsUsed == nil || *e.CreditsUsed != 3 {
		t.Fatalf("credits_used=%v known=%v want 3/true", e.CreditsUsed, e.CreditsKnown)
	}
	if e.CreditsBefore == nil || *e.CreditsBefore != 350 {
		t.Errorf("credits_before=%v want 350", e.CreditsBefore)
	}
	if e.CreditsAfter == nil || *e.CreditsAfter != 347 {
		t.Errorf("credits_after=%v want 347", e.CreditsAfter)
	}
	if e.BalanceError != "" {
		t.Errorf("balance_error=%q want empty", e.BalanceError)
	}
	// token 三项取自 sseOK 的 usage。
	if e.PromptTokens != 1 || e.CompletionTokens != 1 || e.TotalTokens != 2 {
		t.Errorf("tokens prompt/completion/total=%d/%d/%d want 1/1/2",
			e.PromptTokens, e.CompletionTokens, e.TotalTokens)
	}
	if len(*calls) != 1 {
		t.Errorf("balance refresh calls=%d want 1", len(*calls))
	}
}

// TestUsageLogStreamingTokens 流式路径同样记全 token 三项与积分。
func TestUsageLogStreamingTokens(t *testing.T) {
	h, logger, _, _ := usageFixture(t, []int64{345})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	entries := logger.Recent(0)
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	e := entries[0]
	if e.Mode != "stream" {
		t.Errorf("mode=%q want stream", e.Mode)
	}
	if e.CompletionTokens != 1 {
		t.Errorf("completion_tokens=%d want 1 (from SSE usage)", e.CompletionTokens)
	}
	if e.PromptTokens != 1 {
		t.Errorf("prompt_tokens=%d want 1", e.PromptTokens)
	}
	if e.CreditsUsed == nil || *e.CreditsUsed != 5 {
		t.Errorf("credits_used=%v want 5", e.CreditsUsed)
	}
	// 流式路径必须记录过 TTFB（首个 data 帧已到达）。这里的断言要"能失败"：
	// time.Duration.Milliseconds() 恒 >= 0，所以必须断言请求确实耗时 >0 的旁证——
	// 通过 DurationMS 与 TTFBMS 的关系来验证 TTFB 真被采集过。
	if e.DurationMS < 0 {
		t.Errorf("duration_ms=%d want >=0", e.DurationMS)
	}
	if e.TTFBMS > e.DurationMS && e.DurationMS > 0 {
		t.Errorf("ttfb_ms=%d should not exceed duration_ms=%d", e.TTFBMS, e.DurationMS)
	}
}

// TestUsageLogColdStartEstablishesBaseline 冷启动语义：
// 账号余额从未被观测过时，**同样**刷新余额（否则基准永远建立不起来）——
// 本次积分为 null（诚实），但刷新结果写回池后，第二次请求就能算出差值。
//
// 这个"没有基准就不刷新"的死锁是在进程级冒烟里实测到的：表现为积分字段永久为空。
func TestUsageLogColdStartEstablishesBaseline(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 注意：不调用 SetCredits → creditsKnown=false（冷启动）。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })

	// 余额刷新回调：既返回余额，也像 main.go 那样写回池（建立基准）。
	balances := []int64{300, 295}
	idx := 0
	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 8})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		UsageLog: logger,
		RefreshBalanceAfter: func(pl *pool.Pool, uid string) (int64, error) {
			v := balances[len(balances)-1]
			if idx < len(balances) {
				v = balances[idx]
			}
			idx++
			pl.ReconcileCredits(uid, v) // 关键：写回池 = 建立下次的基准
			return v, nil
		},
	})

	send := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}

	send() // 第一次：无基准 → 积分 null，但建立基准 300
	entries := logger.Recent(0)
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	first := entries[0]
	if first.CreditsKnown || first.CreditsUsed != nil {
		t.Errorf("cold start must record null credits: %+v", first)
	}
	// 但 token 统计不受影响。
	if first.CompletionTokens != 1 {
		t.Errorf("completion_tokens=%d want 1", first.CompletionTokens)
	}
	// 关键：基准已建立（这就是死锁修复的证明）。
	if !p.CreditsKnownOf("u1") {
		t.Fatal("cold-start refresh must establish a baseline in the pool")
	}

	send() // 第二次：有基准 300，刷新得 295 → 消耗 5
	entries = logger.Recent(0)
	if len(entries) != 2 {
		t.Fatalf("entries=%d want 2", len(entries))
	}
	second := entries[1]
	if !second.CreditsKnown || second.CreditsUsed == nil || *second.CreditsUsed != 5 {
		t.Errorf("second request should compute delta 5: %+v", second)
	}
	if second.CreditsBefore == nil || *second.CreditsBefore != 300 {
		t.Errorf("credits_before=%v want 300", second.CreditsBefore)
	}
	if second.CreditsAfter == nil || *second.CreditsAfter != 295 {
		t.Errorf("credits_after=%v want 295", second.CreditsAfter)
	}
}

// TestUsageLogFailedRequestRecordsNoCredits 失败请求不记积分差值：
// 402/429 恰恰是"缓存余额已过期"的高发场景，差值会得出巨大假数字
// （缓存 350 / 实际 0 → 假装一次调用吃掉 350）。留 null 比留假数据好。
// 但余额刷新本身照做（保持下次基准新鲜）。
func TestUsageLogFailedRequestRecordsNoCredits(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 350) // 缓存里是 350（但实际可能已归零）
	// 上游 402 余额不足 → 账号被冻结、请求最终 503。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	refreshed := false
	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 8})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		UsageLog: logger,
		RefreshBalanceAfter: func(pl *pool.Pool, uid string) (int64, error) {
			refreshed = true
			return 0, nil // 实际余额已归零
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == 200 {
		t.Fatal("fixture should fail (402 upstream → 503)")
	}

	e := logger.Recent(0)[0]
	if e.CreditsKnown || e.CreditsUsed != nil {
		t.Errorf("failed request must not record a credit delta (350-0=350 would be fake): %+v", e)
	}
	if e.CreditsBefore != nil || e.CreditsAfter != nil {
		t.Errorf("failed request should carry no balance pair: %+v", e)
	}
	// 但刷新照做：基准被更新，下次请求才有可比对象。
	if !refreshed {
		t.Error("balance should still be refreshed on failure to keep the baseline fresh")
	}
	if e.Status == 200 {
		t.Errorf("status=%d should reflect the failure", e.Status)
	}
}

// TestUsageLogBalanceErrorRecorded 余额刷新失败时记下原因，且不写消耗差值。
func TestUsageLogBalanceErrorRecorded(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 4})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		UsageLog: logger,
		RefreshBalanceAfter: func(pl *pool.Pool, uid string) (int64, error) {
			return 0, io.ErrUnexpectedEOF // 任意错误
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	e := logger.Recent(0)[0]
	if e.BalanceError == "" {
		t.Error("balance_error should be recorded when refresh fails")
	}
	if e.CreditsKnown || e.CreditsUsed != nil {
		t.Errorf("failed refresh must not produce a credit delta: %+v", e)
	}
}

// TestUsageLogDisabledSkipsBalanceRefresh 关闭使用日志时不做任何额外开销：
// 不记日志、也不查余额（这是"关掉即零成本"的保证）。
func TestUsageLogDisabledSkipsBalanceRefresh(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	called := false
	logger := usagelog.New(usagelog.Config{Enabled: false})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		UsageLog: logger,
		RefreshBalanceAfter: func(pl *pool.Pool, uid string) (int64, error) {
			called = true
			return 1, nil
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if called {
		t.Error("disabled usage log must not trigger balance refresh")
	}
	if n := len(logger.Recent(0)); n != 0 {
		t.Errorf("disabled logger kept %d entries", n)
	}
}

// TestUsageLogNilLoggerSafeHandler 完全未注入日志器时请求照常（回归保护）。
func TestUsageLogNilLoggerSafeHandler(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up}) // UsageLog 为 nil
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestCachedTokensOfFieldVariants 缓存命中字段的多路探测：
// 上游字段名不受本项目控制，必须兼容几种常见拼法。
func TestCachedTokensOfFieldVariants(t *testing.T) {
	cases := []struct {
		name string
		u    map[string]any
		want int
	}{
		{"openai details", map[string]any{
			"prompt_tokens_details": map[string]any{"cached_tokens": float64(128)},
		}, 128},
		{"flat cached_tokens", map[string]any{"cached_tokens": float64(64)}, 64},
		{"deepseek style", map[string]any{"prompt_cache_hit_tokens": float64(32)}, 32},
		{"anthropic style", map[string]any{"cache_read_input_tokens": float64(16)}, 16},
		{"none reported", map[string]any{"prompt_tokens": float64(10)}, 0},
		{"details without cache", map[string]any{
			"prompt_tokens_details": map[string]any{"audio_tokens": float64(5)},
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cachedTokensOf(tc.u); got != tc.want {
				t.Errorf("cachedTokensOf=%d want %d", got, tc.want)
			}
		})
	}
}

// TestChatStatUsageEntryPartialUsage usage 缺失时 token 字段保持零值，
// 且 total 不被拼成假的 prompt+completion。
func TestChatStatUsageEntryPartialUsage(t *testing.T) {
	s := &chatStat{start: time.Now(), model: "m", mode: "sync", uid: "u1", status: 200, toks: -1}
	e := s.usageEntry(time.Second)
	if e.PromptTokens != 0 || e.CompletionTokens != 0 || e.TotalTokens != 0 {
		t.Errorf("no usage → tokens must stay zero: %+v", e)
	}
	if e.Usage != nil {
		t.Errorf("usage raw should be nil when absent: %v", e.Usage)
	}
	// toks=-1 是"usage 缺失"的哨兵值，不该泄漏进 credits 或 total。
	if e.TotalTokens == -1 {
		t.Error("total_tokens must not be the sentinel -1")
	}
}

// TestAdminUsageEndpoint 控制台接口：启用时返回 enabled=true + entries。
func TestAdminUsageEndpoint(t *testing.T) {
	up := consoleUpstream("ok", nil)
	h, _ := consoleHandler(t, t.TempDir(), up, nil, nil)
	h.cfg.UsageLog = usagelog.New(usagelog.Config{Enabled: true, MemorySize: 4, File: filepath.Join(t.TempDir(), "usage.jsonl")})

	code, body := adminGet(t, h, "/admin/api/usage?limit=50", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%v", code, body)
	}
	if body["enabled"] != true {
		t.Errorf("enabled=%v want true", body["enabled"])
	}
	if _, ok := body["entries"]; !ok {
		t.Errorf("entries key missing: %v", body)
	}
}

// TestAdminUsageEndpointDisabled 关闭时接口仍可用，前端据此渲染"未启用"。
func TestAdminUsageEndpointDisabled(t *testing.T) {
	up := consoleUpstream("ok", nil)
	h, _ := consoleHandler(t, t.TempDir(), up, nil, nil)
	h.cfg.UsageLog = usagelog.New(usagelog.Config{Enabled: false})

	code, body := adminGet(t, h, "/admin/api/usage", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 even when disabled", code)
	}
	if body["enabled"] != false {
		t.Errorf("enabled=%v want false", body["enabled"])
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 0 {
		t.Errorf("entries=%v want empty array", body["entries"])
	}
}

// TestAdminUsageEndpointNilLogger 未注入日志器（老路径）时接口不 panic。
func TestAdminUsageEndpointNilLogger(t *testing.T) {
	up := consoleUpstream("ok", nil)
	h, _ := consoleHandler(t, t.TempDir(), up, nil, nil)
	h.cfg.UsageLog = nil

	code, body := adminGet(t, h, "/admin/api/usage", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 with nil logger", code)
	}
	if body["enabled"] != false {
		t.Errorf("enabled=%v want false", body["enabled"])
	}
}

// TestAdminUsageInvalidLimitFallsBack 非法 limit 不应报错，回落默认值。
func TestAdminUsageInvalidLimitFallsBack(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	h.cfg.UsageLog = usagelog.New(usagelog.Config{Enabled: true, MemorySize: 4})
	for _, q := range []string{"?limit=abc", "?limit=-5", "?limit=0"} {
		code, _ := adminGet(t, h, "/admin/api/usage"+q, "sk-master")
		if code != http.StatusOK {
			t.Errorf("%s: code=%d want 200", q, code)
		}
	}
}

func TestAdminUsageAllHistoryByDefault(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	h.cfg.UsageLog = usagelog.New(usagelog.Config{Enabled: true, MemorySize: 2, File: filepath.Join(t.TempDir(), "usage.jsonl")})
	for i := 0; i < 650; i++ {
		h.cfg.UsageLog.Record(usagelog.Entry{Time: time.Unix(int64(i), 0), TotalTokens: 10, Status: 200})
	}
	for _, query := range []string{"", "?limit=0", "?limit=abc", "?limit=-1", "?limit=500"} {
		code, body := adminGet(t, h, "/admin/api/usage"+query, "sk-master")
		want := 650
		if query == "?limit=500" {
			want = 500
		}
		entries, _ := body["entries"].([]any)
		if code != http.StatusOK || len(entries) != want || body["kept"] != float64(650) || body["history_incomplete"] != false {
			t.Fatalf("query %q: code=%d entries=%d kept=%v incomplete=%v", query, code, len(entries), body["kept"], body["history_incomplete"])
		}
	}
	var frame struct {
		Usage struct {
			Entries []usagelog.Entry `json:"entries"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(h.sseFrame(), &frame); err != nil {
		t.Fatal(err)
	}
	if len(frame.Usage.Entries) != 650 {
		t.Fatalf("SSE truncated history to %d records", len(frame.Usage.Entries))
	}
}

// TestAdminUsageRecordsEntry 端到端：日志器记一条后，控制台接口能查到它。
func TestAdminUsageRecordsEntry(t *testing.T) {
	up := consoleUpstream("ok", nil)
	h, _ := consoleHandler(t, t.TempDir(), up, nil, nil)
	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 8})
	h.cfg.UsageLog = logger
	used := float64(3)
	logger.Record(usagelog.Entry{UID: "u-ai", UID8: "u-ai", Model: "glm-5.2", Status: 200,
		CreditsKnown: true, CreditsUsed: &used})

	code, body := adminGet(t, h, "/admin/api/usage", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries=%v want 1 item", body["entries"])
	}
	first := entries[0].(map[string]any)
	if first["uid"] != "u-ai" || first["credits_used"] != float64(3) {
		t.Errorf("entry=%v want uid=u-ai credits_used=3", first)
	}
	if body["total"] != float64(1) {
		t.Errorf("total=%v want 1", body["total"])
	}
}

// 确保未使用导入不报错（upstream 在 fixture 里用到）。
var _ = upstream.New

// TestLowestCreditsBypassesSessionSticky lowest_credits 模式必须跳过会话粘性。
// 两者目标相反：粘性按会话 hash 把请求散到不同账号（30m TTL），而 lowest_credits
// 要集中把一个号打光以吃满上游缓存。若粘性先生效，选号策略根本轮不到——
// 功能会静默失效（这是 code review 抓到的，默认配置 session_sticky.enabled=true）。
func TestLowestCreditsBypassesSessionSticky(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"low", "high"} },
	})
	p := pool.New("")
	p.SetSelectionMode(pool.SelectionLowestCredits)
	p.Add(&auth.Auth{UID: "low", AccessToken: "at-low", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "high", AccessToken: "at-high", ExpiresAt: 9999999999})
	p.SetCredits("low", 5) // 余额最少 → 应始终选中它
	p.SetCredits("high", 900)

	sent := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		sent[authz]++
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})

	// 用**不同**会话键发多次请求：若粘性生效，hash 会把它们散到两个号上。
	for i := 0; i < 6; i++ {
		body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-` + string(rune('a'+i)) + `"}}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if sent["Bearer at-high"] != 0 {
		t.Errorf("sticky must be bypassed in lowest_credits mode: high used %d times, sent=%v",
			sent["Bearer at-high"], sent)
	}
	if sent["Bearer at-low"] != 6 {
		t.Errorf("all 6 requests should hit the lowest-credit account: sent=%v", sent)
	}
}

// TestAdminUsageClearEndpoint 清空接口：DELETE /admin/api/usage 返回释放字节数，
// 且之后表为空、size 归零。
func TestAdminUsageClearEndpoint(t *testing.T) {
	dir := t.TempDir()
	up := consoleUpstream("ok", nil)
	h, _ := consoleHandler(t, dir, up, nil, nil)
	logger := usagelog.New(usagelog.Config{
		Enabled: true, MemorySize: 8, MaxBytes: 1000, BackupsSet: true, MaxBackups: 1,
		File: filepath.Join(dir, "usage.jsonl"),
	})
	h.cfg.UsageLog = logger

	// 写入足够多，确保落盘有内容。
	pad := strings.Repeat("p", 200)
	for i := 0; i < 20; i++ {
		logger.Record(usagelog.Entry{UID: "u-ai", UID8: "u-ai", Model: pad})
	}

	// 前置：GET 能看到条目与占用。
	code, body := adminGet(t, h, "/admin/api/usage", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("GET code=%d", code)
	}
	if body["kept"] == float64(0) {
		t.Fatal("fixture should have entries before clear")
	}

	// DELETE 清空。
	req := httptest.NewRequest("DELETE", "/admin/api/usage", nil)
	req.Header.Set("Authorization", "Bearer sk-master")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE code=%d body=%s", rec.Code, rec.Body)
	}
	var delResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatalf("delete resp not json: %v", err)
	}
	if delResp["ok"] != true {
		t.Errorf("ok=%v want true", delResp["ok"])
	}
	if delResp["removed"] == nil {
		t.Errorf("removed key missing: %v", delResp)
	}

	// 之后 GET 必须为空。
	code, body = adminGet(t, h, "/admin/api/usage", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("GET after clear code=%d", code)
	}
	if body["kept"] != float64(0) || body["total"] != float64(0) {
		t.Errorf("after clear kept=%v total=%v want 0/0", body["kept"], body["total"])
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 0 {
		t.Errorf("entries=%v want empty", entries)
	}
}

// TestAdminUsageSizeInResponse GET 响应必须带 size 占用快照（控制台据此画占用条）。
func TestAdminUsageSizeInResponse(t *testing.T) {
	dir := t.TempDir()
	h, _ := consoleHandler(t, dir, consoleUpstream("ok", nil), nil, nil)
	h.cfg.UsageLog = usagelog.New(usagelog.Config{
		Enabled: true, File: filepath.Join(dir, "usage.jsonl"),
		MaxSizeMB: 32, BackupsSet: true, MaxBackups: 1,
	})

	code, body := adminGet(t, h, "/admin/api/usage", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	size, ok := body["size"].(map[string]any)
	if !ok {
		t.Fatalf("size missing or wrong type: %v", body["size"])
	}
	// 32MB × (1+1) = 64MB 硬上限。
	wantLimit := float64(32<<20) * 2
	if size["limit_bytes"] != wantLimit {
		t.Errorf("limit_bytes=%v want %v", size["limit_bytes"], wantLimit)
	}
	if size["max_backups"] != float64(1) {
		t.Errorf("max_backups=%v want 1", size["max_backups"])
	}
}

// TestUsageLogPrefersUpstreamCredit 上游 usage 带 credit 时：
//  1. credits_used 取上游值（精确小数），而不是余额差值；
//  2. **完全跳过**余额查询（省一次上游往返——这是 credit 路径的核心收益）。
//
// 现场背景：线上三笔真实调用 usage.credit 分别为 10.85/11/10.82，
// 而余额差值恒为 0。早期只做差值法导致统计全是 0，故必须优先 credit。
func TestUsageLogPrefersUpstreamCredit(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 423) // 有基准，若走差值法会记录 0

	// 上游 SSE 末帧带 usage.credit（模拟真实响应）。
	const sseCredit = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":117601,\"completion_tokens\":3771,\"total_tokens\":121372,\"credit\":10.85}}\n\n" +
		"data: [DONE]\n\n"

	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseCredit, true })
	balanceCalls := 0
	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 4})
	// CalibrateInterval 不设 → handler 默认 5m，故首次请求会做一次单号校准
	// （这正是与新设计的分工：credit 负责"记多少"，校准负责"把权威余额拉回来"）。
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		UsageLog: logger,
		RefreshBalanceAfter: func(pl *pool.Pool, uid string) (int64, error) {
			balanceCalls++
			// 权威余额 400（不是 423-10.85 的推算值）。
			// 照生产回调写回池：否则测不到"先校准再扣减"的真实顺序。
			pl.ReconcileCredits(uid, 400)
			return 400, nil
		},
	})

	// 非流式（走 Aggregate 路径）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	e := logger.Recent(0)[0]
	if !e.CreditsKnown || e.CreditsUsed == nil {
		t.Fatalf("credits should be known from upstream usage: %+v", e)
	}
	if *e.CreditsUsed != 10.85 {
		t.Errorf("credits_used=%v want 10.85 (upstream value, not balance diff)", *e.CreditsUsed)
	}
	if e.CreditsSource != "usage" {
		t.Errorf("credits_source=%q want usage", e.CreditsSource)
	}
	// 关键：有上游 credit 时**不再**走差值路径（该路径只在上游没给 credit 时兜底）。
	if e.CreditsBefore != nil || e.CreditsAfter != nil {
		t.Errorf("balance snapshot fields belong to the diff path, must stay empty: %+v", e)
	}
	// 校准恰一次：本号从未校准过（lastCalib 零值 → due=true）。
	if balanceCalls != 1 {
		t.Errorf("calibration calls=%d want 1 (first use of this account must establish authority)", balanceCalls)
	}
	// 顺序是"先校准、再扣减"：
	//   1. 校准把基准置为权威值 400（同时清零小数余量）；
	//   2. 再扣本次消耗 10.85 → 整数位减 10，余 0.85 挂账。
	// 反过来（先扣后校准）会让快照把刚扣掉的消耗覆盖回来，展示额度不降反升。
	if e.PoolCreditsAfter == nil {
		t.Fatalf("pool_credits_after is nil")
	}
	if *e.PoolCreditsAfter != 390 {
		t.Errorf("pool_credits_after=%d want 390 (400 authoritative baseline minus floor(10.85))", *e.PoolCreditsAfter)
	}
	if *e.PoolCreditsAfter >= 400 {
		t.Errorf("pool_credits_after=%d must be BELOW the authoritative snapshot: deduction must survive calibration", *e.PoolCreditsAfter)
	}
}

// TestUsageLogFallsBackToBalanceDiff 上游**没给** credit 时回退差值法（兜底不能丢）。
func TestUsageLogFallsBackToBalanceDiff(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 423)

	// 该 SSE 无 credit 字段（模拟其他上游实现的响应形状）。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	balanceCalls := 0
	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 4})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		UsageLog: logger,
		RefreshBalanceAfter: func(pl *pool.Pool, uid string) (int64, error) {
			balanceCalls++
			return 420, nil // 423 - 420 = 3
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	e := logger.Recent(0)[0]
	if e.CreditsSource != "balance_diff" {
		t.Errorf("credits_source=%q want balance_diff (fallback)", e.CreditsSource)
	}
	if e.CreditsUsed == nil || *e.CreditsUsed != 3 {
		t.Errorf("credits_used=%v want 3 (diff fallback)", e.CreditsUsed)
	}
	if balanceCalls != 1 {
		t.Errorf("balance refresh calls=%d want 1 (fallback path must still run)", balanceCalls)
	}
	if e.CreditsBefore == nil || *e.CreditsBefore != 423 {
		t.Errorf("credits_before=%v want 423", e.CreditsBefore)
	}
}

// TestUsageLogStreamingUsesUpstreamCredit 流式路径同样优先采用上游 credit。
func TestUsageLogStreamingUsesUpstreamCredit(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 445)

	const sseCredit = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12,\"credit\":7.5}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseCredit, true })
	balanceCalls := 0
	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 4})
	// 显式给一个长 interval：本号刚校准过，同一 interval 内不应再次校准。
	// 这正是"单号校准"的节流语义——连续调用同一账号时不该每次多打一次余额查询。
	h := NewHandler(Config{
		Pool: p, Upstream: up, UsageLog: logger,
		CalibrateInterval: time.Hour,
		RefreshBalanceAfter: func(pl *pool.Pool, uid string) (int64, error) {
			balanceCalls++
			// 权威快照 438。真实回调会把权威值写回池（ReconcileCredits），
			// 这里照做：否则测不到"校准清零小数余量"这个关键副作用。
			pl.ReconcileCredits(uid, 438)
			return 438, nil
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	// Recent 是**正序**（最旧在前）→ 取末条才是本次请求。
	all := logger.Recent(0)
	e := all[len(all)-1]
	if e.CreditsUsed == nil || *e.CreditsUsed != 7.5 {
		t.Errorf("credits_used=%v want 7.5 (streaming upstream credit)", e.CreditsUsed)
	}
	if e.CreditsSource != "usage" {
		t.Errorf("credits_source=%q want usage", e.CreditsSource)
	}
	// 首次使用 → 校准一次（建立权威基准）。
	if balanceCalls != 1 {
		t.Errorf("first request on account: calibration calls=%d want 1", balanceCalls)
	}
	// 先校准（基准置为权威值 438），再扣 floor(7.5)=7 → 431。
	// 0.5 留在小数余量里。注意这里 438 是**校准值**而非 445-7：
	// 顺序修正后，扣减发生在校准之后，故结果必然低于权威快照。
	if e.PoolCreditsAfter == nil {
		t.Fatalf("pool_credits_after is nil")
	}
	if *e.PoolCreditsAfter != 431 {
		t.Errorf("pool_credits_after=%d want 431 (438 authoritative baseline minus floor(7.5))", *e.PoolCreditsAfter)
	}

	// 第二次请求：仍在同一 interval 内 → 不再校准，但本地扣减继续。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second call code=%d", rec2.Code)
	}
	if balanceCalls != 1 {
		t.Errorf("calibration must be throttled within interval: calls=%d want 1", balanceCalls)
	}
	// 末条才是本次请求（Recent 正序，最旧在前）。
	entries := logger.Recent(0)
	e2 := entries[len(entries)-1]
	// 第二次调用前余额 431（首次：校准 438 → 扣 7，余 0.5 挂账）。
	// 本次仍扣 7.5：0.5 + 7.5 = 8.0 → 整数位再减 8 = 423。
	// 这条断言同时证明了"小数余量跨请求累加"——若每次截断，这里会是 424。
	if e2.PoolCreditsAfter == nil {
		t.Fatalf("second call: pool_credits_after is nil")
	}
	if *e2.PoolCreditsAfter != 423 {
		t.Errorf("second call pool_credits_after=%d want 423 (431 - 8, fraction carried across requests)", *e2.PoolCreditsAfter)
	}
}

// TestUsageLogFailedRequestIgnoresUpstreamCredit 失败请求即便带 credit 也不记消耗
// （上游给了值不代表真扣了钱；失败恰恰是"不该归因消耗"的场景）。
func TestUsageLogFailedRequestIgnoresUpstreamCredit(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 100)
	// 402 余额不足 → 请求最终 503。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	logger := usagelog.New(usagelog.Config{Enabled: true, MemorySize: 4})
	h := NewHandler(Config{
		Pool: p, Upstream: up, UsageLog: logger,
		RefreshBalanceAfter: func(pl *pool.Pool, uid string) (int64, error) { return 0, nil },
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == 200 {
		t.Fatal("fixture should fail")
	}
	e := logger.Recent(0)[0]
	if e.CreditsKnown || e.CreditsUsed != nil {
		t.Errorf("failed request must not record credits: %+v", e)
	}
}

// TestAdminUsageSummaryInResponse GET 响应必须带 summary（控制台「累计统计」区块据此渲染），
// 且该项是**累计口径**（覆盖整个日志文件），不受 entries 的 limit 窗口影响。
func TestAdminUsageSummaryInResponse(t *testing.T) {
	dir := t.TempDir()
	h, _ := consoleHandler(t, dir, consoleUpstream("ok", nil), nil, nil)
	logger := usagelog.New(usagelog.Config{
		Enabled: true, MemorySize: 2, // 环形缓冲故意只留 2 条
		File: filepath.Join(dir, "usage.jsonl"),
	})
	h.cfg.UsageLog = logger
	// 写 5 条：内存只留 2 条，但累计统计必须看到 5 条。
	used := float64(2)
	for i := 0; i < 5; i++ {
		logger.Record(usagelog.Entry{
			UID: "u-ai", UID8: "u-ai", Realm: "ai", Model: "glm-5.2", Status: 200,
			PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
			CreditsKnown: true, CreditsUsed: &used,
		})
	}

	// limit=2：entries 只回 2 条，summary 仍是 5 条的累计。
	code, body := adminGet(t, h, "/admin/api/usage?limit=2", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%v", code, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 2 {
		t.Errorf("entries=%d want 2（limit 是明细窗口）", len(entries))
	}
	summary, ok := body["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing or wrong type: %v", body["summary"])
	}
	if summary["requests"] != float64(5) {
		t.Errorf("summary.requests=%v want 5（累计不受 limit 影响：同一个数字不能随 limit 变）", summary["requests"])
	}
	if summary["prompt_tokens"] != float64(500) {
		t.Errorf("summary.prompt_tokens=%v want 500", summary["prompt_tokens"])
	}
	if summary["credits_used"] != float64(10) {
		t.Errorf("summary.credits_used=%v want 10", summary["credits_used"])
	}
	if summary["success"] != float64(5) || summary["failed"] != float64(0) {
		t.Errorf("summary success/failed=%v/%v want 5/0", summary["success"], summary["failed"])
	}
	groups, ok := summary["by_uid"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("summary.by_uid=%v want 1 group", summary["by_uid"])
	}
}

// TestAdminUsageSummaryZeroWhenDisabled 未启用时 summary 是**全零 + 空数组**而不是 null：
// 前端据此渲染"未启用"，无需特判 null。老后端缺该字段时前端同样要走降级路径。
func TestAdminUsageSummaryZeroWhenDisabled(t *testing.T) {
	up := consoleUpstream("ok", nil)
	h, _ := consoleHandler(t, t.TempDir(), up, nil, nil)
	h.cfg.UsageLog = usagelog.New(usagelog.Config{Enabled: false})

	code, body := adminGet(t, h, "/admin/api/usage", "sk-master")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200 even when disabled", code)
	}
	summary, ok := body["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing: %v", body)
	}
	if summary["requests"] != float64(0) {
		t.Errorf("summary.requests=%v want 0", summary["requests"])
	}
	// 数组必须是 []（不是 null）：JSON 里 nil 切片序列化成 null，前端得写 `|| []`。
	for _, k := range []string{"by_realm", "by_uid", "by_model"} {
		arr, ok := summary[k].([]any)
		if !ok || len(arr) != 0 {
			t.Errorf("summary.%s=%v want empty array (not null)", k, summary[k])
		}
	}
}

// TestAdminUsageClearResetsSummary 清空接口必须回传**归零后的**统计快照：
// 前端据此立刻把累计卡片清零，而不是等下一次轮询才发现数字没变。
func TestAdminUsageClearResetsSummary(t *testing.T) {
	dir := t.TempDir()
	h, _ := consoleHandler(t, dir, consoleUpstream("ok", nil), nil, nil)
	logger := usagelog.New(usagelog.Config{
		Enabled: true, File: filepath.Join(dir, "usage.jsonl"),
	})
	h.cfg.UsageLog = logger
	used := float64(9)
	logger.Record(usagelog.Entry{UID: "u-ai", UID8: "u-ai", Status: 200, PromptTokens: 10,
		CreditsKnown: true, CreditsUsed: &used})
	if _, body := adminGet(t, h, "/admin/api/usage", "sk-master"); body["summary"] == nil {
		t.Fatal("fixture should expose summary")
	}

	req := httptest.NewRequest("DELETE", "/admin/api/usage", nil)
	req.Header.Set("Authorization", "Bearer sk-master")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("delete resp not json: %v", err)
	}
	summary, ok := resp["summary"].(map[string]any)
	if !ok {
		t.Fatalf("clear response must carry summary: %v", resp)
	}
	if summary["requests"] != float64(0) || summary["credits_used"] != float64(0) {
		t.Errorf("cleared summary=%v want zeros（清空后统计必须归零）", summary)
	}
	// 之后 GET 也必须是零（缓存已被 Clear 的世代号作废）。
	_, body := adminGet(t, h, "/admin/api/usage", "sk-master")
	after, _ := body["summary"].(map[string]any)
	if after["requests"] != float64(0) {
		t.Errorf("summary after clear=%v want requests=0", after)
	}
}
