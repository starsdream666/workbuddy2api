package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/pool"
)

// adminPatch 发一个 PATCH 管理请求（人工启停端点用；GET/POST 的 helper 在 admin_test.go）。
func adminPatch(t *testing.T, h *Handler, path, key, payload string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("PATCH", path, strings.NewReader(payload))
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

// TestConsoleSetAccountEnabled 人工停用/启用闭环：池状态、选号、overview 三处都要同步。
func TestConsoleSetAccountEnabled(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)

	code, body := adminPatch(t, h, "/admin/api/accounts", "sk-master", `{"realm":"ai","uid":"u-ai","enabled":false}`)
	if code != 200 || body["ok"] != true || body["manual_disabled"] != true {
		t.Fatalf("停用 code=%d body=%v", code, body)
	}
	if !aiPool.ManualDisabled("u-ai") {
		t.Fatal("停用应落到池的人工层")
	}
	if aiPool.Pick() != nil {
		t.Fatal("停用号不应再被选中")
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
	if a["manual_disabled"] != true || a["healthy"] != false {
		t.Fatalf("overview 账号视图不符: %v", a)
	}
	totals := ov["totals"].(map[string]any)
	if totals["manual_disabled"] != float64(1) || totals["disabled"] != float64(0) || totals["healthy"] != float64(0) {
		t.Fatalf("overview 汇总不符: %v", totals)
	}

	code, body = adminPatch(t, h, "/admin/api/accounts", "sk-master", `{"realm":"ai","uid":"u-ai","enabled":true}`)
	if code != 200 || body["manual_disabled"] != false {
		t.Fatalf("启用 code=%d body=%v", code, body)
	}
	if aiPool.Pick() == nil {
		t.Fatal("启用后账号应回到池子")
	}
}

// TestConsoleSetAccountEnabledValidation 请求体检：缺 enabled、未知字段、非法 realm、
// 不存在的 uid、无 key —— 全部要在动手之前挡下来。
func TestConsoleSetAccountEnabledValidation(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	cases := []struct {
		name    string
		key     string
		payload string
		want    int
	}{
		{"缺 enabled", "sk-master", `{"realm":"ai","uid":"u-ai"}`, 400},
		{"未知字段", "sk-master", `{"realm":"ai","uid":"u-ai","enabled":false,"extra":1}`, 400},
		{"非法 realm", "sk-master", `{"realm":"zz","uid":"u-ai","enabled":false}`, 400},
		{"空 uid", "sk-master", `{"realm":"ai","uid":"","enabled":false}`, 400},
		{"不存在的 uid", "sk-master", `{"realm":"ai","uid":"ghost","enabled":false}`, 404},
		{"无 key", "", `{"realm":"ai","uid":"u-ai","enabled":false}`, 401},
		{"坏 key", "nope", `{"realm":"ai","uid":"u-ai","enabled":false}`, 401},
	}
	for _, c := range cases {
		code, body := adminPatch(t, h, "/admin/api/accounts", c.key, c.payload)
		if code != c.want {
			t.Errorf("%s: code=%d want %d body=%v", c.name, code, c.want, body)
		}
	}
	if aiPool.ManualDisabled("u-ai") {
		t.Fatal("被拒绝的请求不该改动任何状态")
	}
}

// TestConsoleTotalsManualDisabledExclusiveWithSystem disabled 与 manual_disabled 互斥
// （两层同时为真只计 disabled），前端拿到的两数相加才是停用总数。
func TestConsoleTotalsManualDisabledExclusiveWithSystem(t *testing.T) {
	h, aiPool := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	aiPool.Disable("u-ai", "12153 session dead")
	if code, body := adminPatch(t, h, "/admin/api/accounts", "sk-master", `{"realm":"ai","uid":"u-ai","enabled":false}`); code != 200 {
		t.Fatalf("停用 code=%d body=%v", code, body)
	}

	_, ov := adminGet(t, h, "/admin/api/overview", "sk-master")
	totals := ov["totals"].(map[string]any)
	if totals["disabled"] != float64(1) || totals["manual_disabled"] != float64(0) {
		t.Fatalf("两层同时为真应只计 disabled: %v", totals)
	}
	accounts, _ := ov["accounts"].([]any)
	a := accounts[0].(map[string]any)
	if a["disabled"] != true || a["manual_disabled"] != true {
		t.Fatalf("账号视图应两层都透出: %v", a)
	}
}

// TestConsoleSetAccountEnabledDisabledConsole 未启用控制台时该路由整体 404。
func TestConsoleSetAccountEnabledDisabledConsole(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: consoleUpstream("ok", nil)})
	if code, _ := adminPatch(t, h, "/admin/api/accounts", "k", `{"realm":"cn","uid":"u","enabled":false}`); code != 404 {
		t.Errorf("code=%d want 404", code)
	}
}
