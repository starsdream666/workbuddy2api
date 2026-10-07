package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestSetManualDisabledBlocksPick 人工停用的账号不参与选号（即便它是唯一/最高分账号），
// 启用后回到池子。
func TestSetManualDisabledBlocksPick(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	if !p.SetManualDisabled("u1", true) {
		t.Fatal("SetManualDisabled 对已存在账号应返回 true")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("停用账号不可被选中, got %+v", got)
	}
	st, ok := p.Status("u1")
	if !ok || !st.ManualDisabled || st.ManualReason != manualDisabledReason {
		t.Fatalf("status 应透出人工停用与原因: ok=%v %+v", ok, st)
	}

	p.SetManualDisabled("u1", false)
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("启用后账号应回到池子, got %+v", got)
	}
	st, _ = p.Status("u1")
	if st.ManualDisabled || st.ManualReason != "" {
		t.Fatalf("启用应清掉人工层与原因: %+v", st)
	}
}

// TestSetManualDisabledPrefersHealthyPeer 停用高分号后，流量应落到其余健康账号。
func TestSetManualDisabledPrefersHealthyPeer(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 50000)
	p.SetCredits("u2", 10)
	p.SetManualDisabled("u1", true)
	for i := 0; i < 20; i++ {
		if got := p.Pick(); got == nil || got.UID != "u2" {
			t.Fatalf("停用高分号后应选 u2, got %+v", got)
		}
	}
}

// TestSetManualDisabledUnknownUID 不存在的 uid 返回 false（控制台据此回 404，
// 避免"点了成功但什么都没发生"）。
func TestSetManualDisabledUnknownUID(t *testing.T) {
	p := New("")
	if p.SetManualDisabled("nope", true) {
		t.Fatal("未知 uid 应返回 false")
	}
	if p.ManualDisabled("nope") {
		t.Fatal("未知 uid 不应报告为已停用")
	}
}

// TestManualDisabledOrthogonalToSystemDisabled 两层互不覆盖：
// 启用只摘人工层，系统层（12153 判死）依旧生效；ReviveDisabled 才恢复系统层。
func TestManualDisabledOrthogonalToSystemDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", sessionDeadReason)
	p.SetManualDisabled("u1", true)

	st, _ := p.Status("u1")
	if !st.Disabled || !st.ManualDisabled {
		t.Fatalf("precondition: 两层应同时为真: %+v", st)
	}

	p.SetManualDisabled("u1", false) // 只摘人工层
	st, _ = p.Status("u1")
	if st.ManualDisabled {
		t.Fatalf("启用应清人工层: %+v", st)
	}
	if !st.Disabled || st.DisabledReason != sessionDeadReason {
		t.Fatalf("系统层不该被人工启用清掉: %+v", st)
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("系统判死的号在人工启用后仍不可选, got %+v", got)
	}

	p.ReviveDisabled("u1") // 清系统层
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("复活系统层后应可选, got %+v", got)
	}
}

// TestManualDisabledExcludedFromFallback 全冷却兜底同样跳过人工停用号
// （否则运维停掉的号会在无健康账号时被当兜底捞出来调）。
func TestManualDisabledExcludedFromFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429")
	p.Cooldown("u2", CoolSoft, time.Hour, "429")
	p.SetManualDisabled("u1", true)

	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("兜底应选未停用的 u2, got %+v", got)
	}

	p.SetManualDisabled("u2", true)
	if got := p.Pick(); got != nil {
		t.Fatalf("全部停用（含冷却）应返回 nil, got %+v", got)
	}
}

// TestManualDisabledPersists 停用/启用都要落盘，重启后不回退。
func TestManualDisabledPersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetManualDisabled("u1", true)
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.ManualDisabled || st.ManualReason != manualDisabledReason {
		t.Fatalf("停用应持久化, ok=%v %+v", ok, st)
	}
	if p2.Pick() != nil {
		t.Fatal("重启后停用号仍不可选")
	}

	p2.SetManualDisabled("u1", false)
	p2.Flush()
	p3 := New(fp)
	p3.Add(&auth.Auth{UID: "u1"})
	if st, _ := p3.Status("u1"); st.ManualDisabled || st.ManualReason != "" {
		t.Fatalf("启用应持久化（重启后不回退）: %+v", st)
	}
}

// TestManualDisabledStateJSONField 落盘字段名是契约（控制台/Redis 快照/运维手工改
// state.json 都依赖它），固定为 manual_disabled / manual_reason。
func TestManualDisabledStateJSONField(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetManualDisabled("u1", true)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	var sf struct {
		Accounts map[string]struct {
			ManualDisabled bool   `json:"manual_disabled"`
			ManualReason   string `json:"manual_reason"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("解析 state.json: %v", err)
	}
	acc, ok := sf.Accounts["u1"]
	if !ok {
		t.Fatalf("state.json 缺 u1: %s", raw)
	}
	if !acc.ManualDisabled || acc.ManualReason != manualDisabledReason {
		t.Fatalf("state.json 字段不符: %+v", acc)
	}
}

// TestManualDisabledSurvivesRescan 重扫凭证目录（SyncToDir）不该丢人工层：
// 账号还在时保留状态，这正是"停用后重扫/重新授权仍停用"的保证。
func TestManualDisabledSurvivesRescan(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetManualDisabled("u1", true)

	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "at-new"}})
	if !p.ManualDisabled("u1") {
		t.Fatal("重扫后人工停用应保留")
	}
	if p.Pick() != nil {
		t.Fatal("重扫后停用号仍不可选")
	}
}

// TestManuallyDisabledUIDsAndCounts 列表查询 + /status 计数口径：
// 人工停用计入 disabled（对外"不参与调度"），不该落进 cooling。
func TestManuallyDisabledUIDsAndCounts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetManualDisabled("u2", true)

	uids := p.ManuallyDisabledUIDs()
	if len(uids) != 1 || uids[0] != "u2" {
		t.Fatalf("ManuallyDisabledUIDs=%v want [u2]", uids)
	}
	if !p.ManualDisabled("u2") || p.ManualDisabled("u1") {
		t.Fatalf("ManualDisabled 判定不符: u2=%v u1=%v", p.ManualDisabled("u2"), p.ManualDisabled("u1"))
	}
	total, healthy, cooling, disabled, _ := p.CountsDetailed()
	if total != 2 || healthy != 1 || cooling != 0 || disabled != 1 {
		t.Fatalf("计数口径 total=%d healthy=%d cooling=%d disabled=%d want 2/1/0/1", total, healthy, cooling, disabled)
	}
}

// TestStoppedCoversBothLayers 调度器跳过口径（Status.Stopped）必须覆盖两层：
// 只看 st.Disabled 会让运维停掉的号继续被签到/旅行/活跃/保活刷。
func TestStoppedCoversBothLayers(t *testing.T) {
	cases := []struct {
		system bool
		manual bool
		want   bool
	}{
		{false, false, false},
		{true, false, true},
		{false, true, true},
		{true, true, true},
	}
	for _, c := range cases {
		st := Status{Disabled: c.system, ManualDisabled: c.manual}
		if got := st.Stopped(); got != c.want {
			t.Errorf("Stopped(system=%v manual=%v)=%v want %v", c.system, c.manual, got, c.want)
		}
	}
}
