package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestPickLowestCreditsDrainsOneAccount 最少余额优先：始终选中余额最少的健康账号，
// 把它打光（余额降到 0 或更低）后才顺位到下一个。
// 这正"集中压号以提升上游缓存命中"的行为定义。
func TestPickLowestCreditsDrainsOneAccount(t *testing.T) {
	withNoPickGap(t) // 本策略有意跳过防撞号窗口，但仍清掉它以免干扰断言
	p := New("")
	p.SetSelectionMode(SelectionLowestCredits)
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetCredits("u1", 500)
	p.SetCredits("u2", 10) // 最少 → 应被持续选中
	p.SetCredits("u3", 300)

	// 连续选中必须稳定落在 u2 上（确定性是策略目的，不是随机）。
	for i := 0; i < 20; i++ {
		if got := p.Pick(); got == nil || got.UID != "u2" {
			t.Fatalf("iter %d: pick=%v want u2 (lowest credits)", i, got)
		}
	}

	// 模拟 u2 被打光：余额降到 0 后应顺位到次少的 u3。
	p.SetCredits("u2", 0)
	if got := p.Pick(); got == nil || got.UID != "u3" {
		t.Fatalf("after u2 exhausted: pick=%v want u3 (next lowest)", got)
	}
}

// TestPickLowestCreditsUnobservedFirst 未观测账号优先于"已观测且 <=0"，
// 且已观测的正余额（无论多小）优先于真·耗尽号。
//
// 修正背景：早期版本把"未观测"与"已耗尽"一并排到最后，导致未观测账号被永久饿死
// （不选中 → 观测不到 → 永远沉底）。现按三档排序：
//
//	未观测  >  已观测正余额（小者优先）  >  已观测且 <=0
func TestPickLowestCreditsUnobservedFirst(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetSelectionMode(SelectionLowestCredits)
	p.Add(&auth.Auth{UID: "exhausted"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.Add(&auth.Auth{UID: "has"})
	// exhausted：已观测余额 0（真·耗尽）→ 应排最后。
	p.SetCredits("exhausted", 0)
	// has：已观测正余额 → 应优先于 exhausted，但**让位**于未观测的 unknown。
	p.SetCredits("has", 7)
	// unknown：从未观测 → 应最先被选中（探测它）。

	if got := p.Pick(); got == nil || got.UID != "unknown" {
		t.Fatalf("pick=%v want unknown (unobserved must be probed first)", got)
	}

	// 把 unknown 也标记为已观测且额度更低（3 < 7）→ 此后应选 unknown（余额更小）。
	p.SetCredits("unknown", 3)
	for i := 0; i < 3; i++ {
		if got := p.Pick(); got == nil || got.UID != "unknown" {
			t.Fatalf("iter %d: pick=%v want unknown (lowest positive balance)", i, got)
		}
	}

	// 全员已观测且 <=0：退化为 uid 升序最小者（确定性）。
	// "exhausted" < "has" < "unknown"。
	p.SetCredits("has", 0)
	p.SetCredits("unknown", 0)
	if got := p.Pick(); got == nil || got.UID != "exhausted" {
		t.Fatalf("all non-positive: pick=%v want exhausted (smallest uid)", got)
	}
}

// TestPickLowestCreditsSkipsCoolingAndFull 选号策略不改变健康/在途口径：
// 冷却中的账号与占满在途的账号同样被跳过（与加权路径一致）。
func TestPickLowestCreditsSkipsCoolingAndFull(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetSelectionMode(SelectionLowestCredits)
	p.Add(&auth.Auth{UID: "cooling"})
	p.Add(&auth.Auth{UID: "full"})
	p.Add(&auth.Auth{UID: "ok"})
	p.SetCredits("cooling", 1) // 余额最少但在冷却 → 不可选
	p.SetCredits("full", 2)    // 余额次少但占满在途 → 不可选
	p.SetCredits("ok", 3)

	p.Cooldown("cooling", CoolSoft, time.Hour, "test")
	p.SetMaxInFlight(1)
	if !p.Acquire("full") {
		t.Fatal("acquire full failed")
	}
	defer p.Release("full")

	if got := p.Pick(); got == nil || got.UID != "ok" {
		t.Fatalf("pick=%v want ok (cooling/full must be skipped)", got)
	}
}

// TestSetSelectionModeUnknownFallsBackToWeighted 未知策略名回落加权：
// 配置笔误不该静默改变路由行为。
func TestSetSelectionModeUnknownFallsBackToWeighted(t *testing.T) {
	p := New("")
	p.SetSelectionMode("lowest_credits")
	p.SetSelectionMode("bogus")
	p.mu.RLock()
	got := p.selectionMode
	p.mu.RUnlock()
	if got != SelectionWeighted {
		t.Fatalf("selectionMode=%q want %q", got, SelectionWeighted)
	}
}

// TestCreditsKnownTracking 余额"是否被观测过"的标记：
// SetCredits / ReconcileCredits / ReenableIfCredits 三条写入路径都要置真，
// 因为使用日志要靠它区分"余额 0 = 耗尽"与"余额 0 = 从未观测"。
func TestCreditsKnownTracking(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.CreditsKnownOf("u1") {
		t.Error("fresh account must report creditsKnown=false")
	}

	p.SetCredits("u1", 0) // 观测到余额 0（真实观测，不是未知）
	if !p.CreditsKnownOf("u1") {
		t.Error("SetCredits must mark creditsKnown=true")
	}
	if c, ok := p.CreditsOf("u1"); !ok || c != 0 {
		t.Errorf("CreditsOf=%d ok=%v want 0/true", c, ok)
	}

	// 未观测过的账号：CreditsOf 返回存在但值为 0，CreditsKnownOf=false。
	p.Add(&auth.Auth{UID: "u2"})
	if p.CreditsKnownOf("u2") {
		t.Error("u2 must stay unobserved")
	}

	// 不存在的账号：两个查询都不成立。
	if _, ok := p.CreditsOf("nope"); ok {
		t.Error("CreditsOf on unknown uid should report !ok")
	}
	if p.CreditsKnownOf("nope") {
		t.Error("CreditsKnownOf on unknown uid should be false")
	}
}

// TestCreditsKnownPersisted 余额观测标记随 state.json 往返：
// 重启后不该把"已观测的 0 余额"退化为"未知"。
func TestCreditsKnownPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 0)
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if !p2.CreditsKnownOf("u1") {
		t.Error("creditsKnown must survive reload (observed zero balance)")
	}
}

// TestLowestCreditsUnobservedAccountsStarve 复现真实缺陷（用户线上报告的）：
// 新加入/从未观测过余额的账号 creditsKnown=false、credits=0，
// 而 lessCredits 把 "credits<=0" 排到最后 —— 于是它们**永远不被选中**，
// 也就永远观测不到自己的真实余额，形成死锁。
//
// 现场：6 个 ai 账号里 3 个是低额度，但只有 2 个被观测过（350/207），
// 低额度的始终选不中 → 用户看到"永远用高额度号"。
func TestLowestCreditsUnobservedAccountsStarve(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetSelectionMode(SelectionLowestCredits)
	// 两个"已观测"的高额度号。
	p.Add(&auth.Auth{UID: "known-high-a"})
	p.Add(&auth.Auth{UID: "known-high-b"})
	p.SetCredits("known-high-a", 350)
	p.SetCredits("known-high-b", 207)
	// 一个"未观测"的低额度号（真实额度 90，但池里是 0/unknown）。
	p.Add(&auth.Auth{UID: "unobserved-low"})

	// 连续 20 次选择：未观测号必须有机会被选中，否则永远观测不到它。
	saw := map[string]int{}
	for i := 0; i < 20; i++ {
		if a := p.Pick(); a != nil {
			saw[a.UID]++
		}
	}
	if saw["unobserved-low"] == 0 {
		t.Fatalf("unobserved account starved: never picked in 20 tries (saw=%v) — "+
			"its real balance can never be discovered", saw)
	}
}
