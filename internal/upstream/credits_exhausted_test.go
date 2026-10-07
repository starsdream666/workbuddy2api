package upstream

import (
	"net/http"
	"testing"
)

// creditsExhaustedBody 上游「额度耗尽」的真实响应（2026-09-15 实测，WorkBuddy AI 线）。
//
// 两个坑，都是这次修复的起因：
//  1. 状态码是 **429**（不是 402），只看状态码会掉进限流兜底；
//  2. 文案是复数 "Credits exhausted"，而原 hardMarkers 只有单数 "credit exhausted"，
//     子串匹配不到（"credits exhausted" 不含 "credit exhausted"）。
//
// 语义：账号当天额度用完，次日签到会恢复 30 额度 → 应硬冷却到次日 04:00 等签到解冻，
// 而不是软冷却 600s 后反复重试（既白刷 429，也不会在签到时解冻）。
const creditsExhaustedBody = `{"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and get more credits: https://www.codebuddy.ai/pro"}}}`

// TestClassifyCreditsExhausted 额度耗尽（429 + 复数文案 + code 14018）必须判 hard_credit。
func TestClassifyCreditsExhausted(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"真实响应样本", creditsExhaustedBody},
		{"仅业务码", `{"code":14018}`},
		{"业务码带空格", `{"code" : 14018 }`},
		{"复数文案单独出现", `Credits exhausted`},
		{"复数措辞 insufficient credits", `insufficient credits`},
		{"复数措辞 out of credits", `out of credits`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(http.StatusTooManyRequests, tc.body); got != ErrHardCredit {
				t.Errorf("Classify(429, %.60q) = %v, want ErrHardCredit", tc.body, got)
			}
		})
	}
}

// TestClassifyReal429StillSoftRate 回归保护：真限流不能被新规则误判成额度耗尽
// （误判会把还有额度的号停到次日 04:00）。
func TestClassifyReal429StillSoftRate(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空 body 的 429", ``},
		{"限流文案", `{"code":11140,"msg":"The model provider is rate-limiting requests."}`},
		{"相近但不同的业务码", `{"code":1401}`},
		{"业务码更长（词边界必须生效）", `{"code":140180}`},
		{"业务码前缀", `{"code":140`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(http.StatusTooManyRequests, tc.body); got != ErrSoftRate {
				t.Errorf("Classify(429, %q) = %v, want ErrSoftRate", tc.body, got)
			}
		})
	}
}

// TestHardCreditCodeRegexBoundary 业务码正则必须有词边界，
// 否则 "140180" 这类更长数字会被误命中（把真限流当额度耗尽 → 账号被停到次日）。
func TestHardCreditCodeRegexBoundary(t *testing.T) {
	hit := []string{`"code":14018`, `"code" : 14018`, `{"code":14018,`, `{"code": 14018}`}
	miss := []string{`"code":140180`, `"code":1401`, `"code":1402}`, `code:14018`}
	for _, s := range hit {
		if !hardCreditCodeRe.MatchString(s) {
			t.Errorf("应命中但未命中: %s", s)
		}
	}
	for _, s := range miss {
		if hardCreditCodeRe.MatchString(s) {
			t.Errorf("不应命中但命中了: %s", s)
		}
	}
}
