package usagelog

import (
	"encoding/json"
	"testing"
)

// TestCreditOfRealUpstreamShape 用**线上真实响应**的 usage 形状验证提取逻辑。
// 数据取自 2026-09-17 三笔真实调用（云服务器 usage.jsonl 原始记录）。
func TestCreditOfRealUpstreamShape(t *testing.T) {
	// 逐字摘录自真实日志（字段顺序/嵌套与原样一致）。
	real := []struct {
		name string
		raw  string
		want float64
	}{
		{
			name: "真实调用1 credit=10.85",
			raw: `{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cached_tokens":0,
				"completion_thinking_tokens":895,"completion_tokens":3771,
				"credit":10.85,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":117601,
				"prompt_tokens":117601,"total_tokens":121372}`,
			want: 10.85,
		},
		{
			name: "真实调用2 credit=11（整数形式）",
			raw:  `{"credit":11,"completion_tokens":4298,"prompt_tokens":117598,"total_tokens":121896}`,
			want: 11,
		},
		{
			name: "真实调用3 credit=10.82",
			raw:  `{"credit":10.82,"completion_tokens":3661,"prompt_tokens":117655}`,
			want: 10.82,
		},
	}
	for _, tc := range real {
		t.Run(tc.name, func(t *testing.T) {
			var u map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &u); err != nil {
				t.Fatalf("fixture not json: %v", err)
			}
			got, ok := CreditOf(u)
			if !ok {
				t.Fatal("CreditOf reported unavailable — the whole point is upstream DOES report credit")
			}
			if got != tc.want {
				t.Errorf("credit=%v want %v", got, tc.want)
			}
		})
	}
}

// TestCreditOfUnavailableCases 缺失/脏数据一律视为不可用，让调用方回退差值法。
func TestCreditOfUnavailableCases(t *testing.T) {
	cases := []struct {
		name string
		u    map[string]any
	}{
		{"nil map", nil},
		{"empty map", map[string]any{}},
		{"no credit field", map[string]any{"completion_tokens": float64(10)}},
		{"negative credit", map[string]any{"credit": float64(-1)}},
		{"string credit（类型不符）", map[string]any{"credit": "10.85"}},
		{"null credit", map[string]any{"credit": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v, ok := CreditOf(tc.u); ok {
				t.Errorf("CreditOf=%v ok=true want unavailable", v)
			}
		})
	}
}

// TestCreditOfJSONNumber 支持 json.Number（调用方启用 UseNumber 解码时）。
func TestCreditOfJSONNumber(t *testing.T) {
	u := map[string]any{"credit": json.Number("10.85")}
	got, ok := CreditOf(u)
	if !ok || got != 10.85 {
		t.Errorf("CreditOf=%v ok=%v want 10.85/true", got, ok)
	}
}

// TestEntryCarriesFloatCredits 积分字段必须能承载小数——
// 早期版本用 int64 差值，会把 10.85 这类精确值抹成 0（线上实测踩过）。
func TestEntryCarriesFloatCredits(t *testing.T) {
	used := 10.85
	e := Entry{CreditsUsed: &used, CreditsKnown: true, CreditsSource: "usage"}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Entry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.CreditsUsed == nil || *back.CreditsUsed != 10.85 {
		t.Errorf("credits_used round-trip=%v want 10.85", back.CreditsUsed)
	}
	if back.CreditsSource != "usage" {
		t.Errorf("credits_source=%q want usage", back.CreditsSource)
	}
}
