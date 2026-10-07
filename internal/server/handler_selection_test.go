package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
)

// chatOnceForSelection 发一次带会话键的 chat 请求，返回响应码与**实际上游用到的账号**。
//
// 为什么用 Authorization 头而不是粘性绑定判定用了哪个号：确定性策略下粘性被跳过时
// sessKey 为空，处理链根本不会写绑定（handler.go 的 Bind 有 `sessKey != ""` 前置），
// 于是"读绑定"读到的可能是测试自己预先写入的陈旧值——那会把"粘性被跳过"误判成"粘性生效"。
// 要判断策略是否真的生效，只能看请求实际打到哪个账号。
func chatOnceForSelection(t *testing.T, h *Handler, used *string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	_ = used
	return rec.Code
}

// TestChatStickySkippedForDeterministicSelection 会话粘性在三个"确定性集中"策略下必须自动停用：
// 粘性按会话 hash 散号，而这三个策略要按余额/优先级把请求集中到确定的账号上；
// 若粘性先定号，选号策略就静默失效了。
//
// 构造：会话已绑定 sticky 号；策略语义指向 target 号。断言请求真的打到 target。
func TestChatStickySkippedForDeterministicSelection(t *testing.T) {
	for _, mode := range []string{
		pool.SelectionLowestCredits,
		pool.SelectionHighestCredits,
		pool.SelectionCustomPriority,
	} {
		t.Run(mode, func(t *testing.T) {
			st := newBindStore()
			sess := session.New(session.Config{
				TTL:       time.Minute,
				Store:     st,
				Available: func() []string { return []string{"sticky", "target"} },
			})
			p := testPoolWith(
				&auth.Auth{UID: "sticky", AccessToken: "at-sticky", ExpiresAt: 9999999999},
				&auth.Auth{UID: "target", AccessToken: "at-target", ExpiresAt: 9999999999},
			)
			p.SetSelectionMode(mode)
			// 按策略语义安排额度/优先级，让"策略生效"与"粘性生效"给出不同答案。
			switch mode {
			case pool.SelectionLowestCredits:
				p.SetCredits("sticky", 900)
				p.SetCredits("target", 1)
			case pool.SelectionHighestCredits:
				p.SetCredits("sticky", 1)
				p.SetCredits("target", 900)
			case pool.SelectionCustomPriority:
				p.SetCredits("sticky", 900)
				p.SetCredits("target", 1)
				if !p.SetAccountSelection("target", pool.AccountSelection{Priority: 50}) {
					t.Fatal("set priority failed")
				}
			}
			var used string
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				used = authz
				return 200, sseOK, true
			})
			h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
			sess.Bind("conv-1", "sticky")

			if code := chatOnceForSelection(t, h, &used); code != 200 {
				t.Fatalf("code=%d", code)
			}
			if used != "Bearer at-target" {
				t.Fatalf("粘性未被停用：实际使用 %q want \"Bearer at-target\"（策略应生效，而非粘性号 sticky）", used)
			}
		})
	}
}

// TestChatStickyActiveForWeightedSelection 加权策略下粘性照常生效（回归护栏）：
// 停用粘性只针对确定性策略，不能顺手把默认行为也改掉。
func TestChatStickyActiveForWeightedSelection(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"sticky", "other"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "sticky", AccessToken: "at-sticky", ExpiresAt: 9999999999},
		&auth.Auth{UID: "other", AccessToken: "at-other", ExpiresAt: 9999999999},
	)
	p.SetSelectionMode(pool.SelectionWeighted)
	// 把 other 的额度拉到极高：若走加权选号它会压倒性胜出；粘性生效才会落到 sticky。
	p.SetCredits("sticky", 1)
	p.SetCredits("other", 100000)
	var used string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = authz
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "sticky")

	if code := chatOnceForSelection(t, h, &used); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if used != "Bearer at-sticky" {
		t.Fatalf("加权策略下粘性应生效：实际使用 %q want \"Bearer at-sticky\"", used)
	}
}
