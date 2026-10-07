package server

import (
	"testing"

	"workbuddy2api/internal/pool"
)

// TestConsoleAccountSelection 账号级选号配置的控制台闭环：
// 写入 → 池内状态生效 → 选号行为改变 → overview 透出。
func TestConsoleAccountSelection(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)

	code, body := adminPatch(t, h, "/admin/api/accounts", "sk-master",
		`{"realm":"ai","uid":"u-ai","selection":{"excluded":true,"placement":"first"}}`)
	if code != 200 || body["ok"] != true {
		t.Fatalf("排除 code=%d body=%v", code, body)
	}
	sel, ok := aiPool.AccountSelection("u-ai")
	if !ok || !sel.Excluded || sel.Placement != pool.PlacementFirst {
		t.Fatalf("池内选号配置不符: %+v ok=%v", sel, ok)
	}
	// 被排除的号仍然健康、仍可被选中（排除只改排序，不是停用）。
	if aiPool.Pick() == nil {
		t.Fatal("被排除的号仍应可被选中（排除只改排序）")
	}

	code, ov := adminGet(t, h, "/admin/api/overview", "sk-master")
	if code != 200 {
		t.Fatalf("overview code=%d", code)
	}
	accounts, _ := ov["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts=%v", ov["accounts"])
	}
	a := accounts[0].(map[string]any)
	if a["selection_excluded"] != true || a["selection_placement"] != "first" {
		t.Fatalf("overview 未透出选号配置: %v", a)
	}
	// 与停用正交：排除不应让账号显示成停用/不健康。
	if a["healthy"] != true || a["manual_disabled"] != false {
		t.Fatalf("排除不应影响可用性与停用状态: %v", a)
	}
}

// TestConsoleAccountSelectionPartialUpdate 部分更新语义：
// 只传 priority 时不该清掉已有的排除配置，反之亦然。
func TestConsoleAccountSelectionPartialUpdate(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)

	if code, body := adminPatch(t, h, "/admin/api/accounts", "sk-master",
		`{"realm":"ai","uid":"u-ai","selection":{"excluded":true,"placement":"last","priority":9}}`); code != 200 {
		t.Fatalf("初次写入 code=%d body=%v", code, body)
	}

	// 只改优先级：排除与落位必须保留。
	if code, body := adminPatch(t, h, "/admin/api/accounts", "sk-master",
		`{"realm":"ai","uid":"u-ai","selection":{"priority":42}}`); code != 200 {
		t.Fatalf("只改优先级 code=%d body=%v", code, body)
	}
	sel, _ := aiPool.AccountSelection("u-ai")
	if !sel.Excluded || sel.Placement != pool.PlacementLast || sel.Priority != 42 {
		t.Fatalf("部分更新串改了其它字段: %+v", sel)
	}

	// 只改启停：选号配置必须原样保留（这是指针字段存在的理由）。
	if code, _ := adminPatch(t, h, "/admin/api/accounts", "sk-master",
		`{"realm":"ai","uid":"u-ai","enabled":false}`); code != 200 {
		t.Fatalf("只改启停失败 code=%d", code)
	}
	sel, _ = aiPool.AccountSelection("u-ai")
	if !sel.Excluded || sel.Priority != 42 {
		t.Fatalf("启停请求串改了选号配置: %+v", sel)
	}
	if !aiPool.ManualDisabled("u-ai") {
		t.Fatal("启停请求应生效")
	}
}

// TestConsoleAccountSelectionValidation 选号配置的请求体检：
// 非法落位、空壳请求、未知字段、未知 uid 都要在动手之前挡下来。
func TestConsoleAccountSelectionValidation(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	cases := []struct {
		name    string
		key     string
		payload string
		want    int
	}{
		{"非法落位", "sk-master", `{"realm":"ai","uid":"u-ai","selection":{"excluded":true,"placement":"FIRST"}}`, 400},
		{"非法落位（中文）", "sk-master", `{"realm":"ai","uid":"u-ai","selection":{"placement":"最优先"}}`, 400},
		{"空壳请求", "sk-master", `{"realm":"ai","uid":"u-ai"}`, 400},
		{"selection 内未知字段", "sk-master", `{"realm":"ai","uid":"u-ai","selection":{"excluded":true,"extra":1}}`, 400},
		{"不存在 uid", "sk-master", `{"realm":"ai","uid":"ghost","selection":{"excluded":true}}`, 404},
		{"无 key", "", `{"realm":"ai","uid":"u-ai","selection":{"excluded":true}}`, 401},
	}
	for _, c := range cases {
		code, body := adminPatch(t, h, "/admin/api/accounts", c.key, c.payload)
		if code != c.want {
			t.Errorf("%s: code=%d want %d body=%v", c.name, code, c.want, body)
		}
	}
	// 被拒绝的请求不该改动任何状态。
	sel, _ := aiPool.AccountSelection("u-ai")
	if sel.Excluded || sel.Priority != 0 {
		t.Fatalf("被拒绝的请求改动了状态: %+v", sel)
	}
	// 空落位合法（表示未指定，按最后使用解释）。
	if code, body := adminPatch(t, h, "/admin/api/accounts", "sk-master",
		`{"realm":"ai","uid":"u-ai","selection":{"excluded":true,"placement":""}}`); code != 200 {
		t.Fatalf("空落位应合法: code=%d body=%v", code, body)
	}
	sel, _ = aiPool.AccountSelection("u-ai")
	if sel.Placement != pool.PlacementLast {
		t.Fatalf("空落位应归一为 last, got %q", sel.Placement)
	}
}

// TestConsoleAccountSelectionRealmIsolation 跨产品线隔离：拿 ai 的 uid 打 cn 的 realm
// 必须 404（不能命中别的池，更不能回退默认池）。
func TestConsoleAccountSelectionRealmIsolation(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	if code, body := adminPatch(t, h, "/admin/api/accounts", "sk-master",
		`{"realm":"cn","uid":"u-ai","selection":{"excluded":true}}`); code != 404 {
		t.Fatalf("跨产品线应 404: code=%d body=%v", code, body)
	}
}
