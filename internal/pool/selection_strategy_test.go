package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newSelectionPool 建一个无落盘、关掉防撞号窗口的池（选号断言需要确定性）。
func newSelectionPool(t *testing.T, mode string, uids ...string) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.SetSelectionMode(mode)
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid})
	}
	return p
}

// TestPickHighestCreditsDrainsOneAccount 最高额度优先：始终选中余额最多的健康账号，
// 与 lowest_credits 完全镜像。把它打光（降到 0）后才顺位到次高者。
func TestPickHighestCreditsDrainsOneAccount(t *testing.T) {
	p := newSelectionPool(t, SelectionHighestCredits, "u1", "u2", "u3")
	p.SetCredits("u1", 50)
	p.SetCredits("u2", 900) // 最多 → 应被持续选中
	p.SetCredits("u3", 300)

	for i := 0; i < 20; i++ {
		if got := p.Pick(); got == nil || got.UID != "u2" {
			t.Fatalf("iter %d: pick=%v want u2 (highest credits)", i, got)
		}
	}

	// 模拟 u2 被打光：余额降到 0 后应顺位到次高的 u3。
	p.SetCredits("u2", 0)
	if got := p.Pick(); got == nil || got.UID != "u3" {
		t.Fatalf("after u2 exhausted: pick=%v want u3 (next highest)", got)
	}
}

// TestPickHighestCreditsUnobservedFirst 最高额度优先同样必须优先探测未观测账号，
// 否则未观测号永远沉底（余额恒为 0）→ 永远刷不到真实余额 → 观测死锁。
// 这是 lessCredits/greaterCredits 共有的第一档，与"要高还是要低"无关。
func TestPickHighestCreditsUnobservedFirst(t *testing.T) {
	p := newSelectionPool(t, SelectionHighestCredits, "exhausted", "unknown", "has")
	p.SetCredits("exhausted", 0) // 已观测且 <=0：真·耗尽，应排最后
	p.SetCredits("has", 7)       // 已观测正余额

	// unknown 未观测 → 必须先被选中（探测它）。
	if got := p.Pick(); got == nil || got.UID != "unknown" {
		t.Fatalf("pick=%v want unknown (unobserved must be probed first)", got)
	}

	// 让 unknown 变成"已观测且更高"，此后应稳定选它。
	p.SetCredits("unknown", 500)
	for i := 0; i < 3; i++ {
		if got := p.Pick(); got == nil || got.UID != "unknown" {
			t.Fatalf("iter %d: pick=%v want unknown (highest positive balance)", i, got)
		}
	}

	// 全员已观测且 <=0：退化为 uid 升序最小者（确定性）。
	p.SetCredits("has", 0)
	p.SetCredits("unknown", 0)
	if got := p.Pick(); got == nil || got.UID != "exhausted" {
		t.Fatalf("all non-positive: pick=%v want exhausted (smallest uid)", got)
	}
}

// TestPickHighestCreditsSkipsCoolingAndFull 策略不改变健康/在途口径：
// 冷却中的号与占满在途的号同样被跳过（与加权、lowest_credits 路径一致）。
func TestPickHighestCreditsSkipsCoolingAndFull(t *testing.T) {
	p := newSelectionPool(t, SelectionHighestCredits, "cooling", "full", "ok")
	p.SetCredits("cooling", 9000) // 额度最高但在冷却 → 不可选
	p.SetCredits("full", 8000)    // 次高但占满在途 → 不可选
	p.SetCredits("ok", 10)

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

// TestPickCustomPriorityOrdersByPriority 自定义优先级：数值大者先被选中，
// 与余额完全无关（这是本策略与两个 credits 策略的本质差异）。
func TestPickCustomPriorityOrdersByPriority(t *testing.T) {
	p := newSelectionPool(t, SelectionCustomPriority, "low", "high", "mid")
	p.SetCredits("low", 9999) // 余额最高，但优先级最低
	p.SetCredits("high", 1)
	p.SetCredits("mid", 500)

	set := func(uid string, priority int) {
		if !p.SetAccountSelection(uid, AccountSelection{Priority: priority}) {
			t.Fatalf("SetAccountSelection(%s) failed", uid)
		}
	}
	set("low", 1)
	set("high", 100)
	set("mid", 50)

	if got := p.Pick(); got == nil || got.UID != "high" {
		t.Fatalf("pick=%v want high (largest priority)", got)
	}
}

// TestPickCustomPriorityDefaultsToUIDOrder 未配置优先级（全 0）时退化为 uid 升序：
// 保证"没配优先级"的池行为可复现，且与改造前一致。
func TestPickCustomPriorityDefaultsToUIDOrder(t *testing.T) {
	p := newSelectionPool(t, SelectionCustomPriority, "b", "a", "c")
	// 余额刻意配成与 uid 序相反，证明本策略不看余额。
	p.SetCredits("a", 1)
	p.SetCredits("b", 500)
	p.SetCredits("c", 100)

	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("pick=%v want a (smallest uid at equal priority 0)", got)
	}
}

// TestPickCustomPriorityIgnoresCreditsObservation 自定义优先级**不区分**未观测/已观测：
// 优先级是运维手写的意图值，不该被"未观测要先探测"的规则顶掉。
// （credits 两个策略里那条规则是为打破观测死锁，本策略排序键不是余额，不存在该死锁。）
func TestPickCustomPriorityIgnoresCreditsObservation(t *testing.T) {
	p := newSelectionPool(t, SelectionCustomPriority, "unobserved", "observed")
	// observed 未观测（creditsKnown=false）；observed 已观测且额度很高。
	p.SetCredits("observed", 9999)
	if !p.SetAccountSelection("observed", AccountSelection{Priority: 5}) {
		t.Fatal("set priority failed")
	}
	if !p.SetAccountSelection("unobserved", AccountSelection{Priority: 50}) {
		t.Fatal("set priority failed")
	}

	if got := p.Pick(); got == nil || got.UID != "unobserved" {
		t.Fatalf("pick=%v want unobserved (priority wins over observation state)", got)
	}
}

// TestPickCustomPriorityNegativePriority 负数优先级合法（表示"比默认更低"）：
// 0（默认）应当排在 -5 之前，且不做非负钳制。
func TestPickCustomPriorityNegativePriority(t *testing.T) {
	p := newSelectionPool(t, SelectionCustomPriority, "below", "default")
	if !p.SetAccountSelection("below", AccountSelection{Priority: -5}) {
		t.Fatal("set priority failed")
	}
	if got := p.Pick(); got == nil || got.UID != "default" {
		t.Fatalf("pick=%v want default (0 > -5)", got)
	}
}

// TestSelectionExcludedFirstPreemptsStrategy 排除 + 落位「最优先使用」：
// 只要有它在，就永远先选它——**完全压过**常规组的策略排序，
// 哪怕常规组里有额度更高/优先级更高的号。
func TestSelectionExcludedFirstPreemptsStrategy(t *testing.T) {
	// 用 lowest_credits 做对照：常规组里 rich 额度最低本会被优先，但被排除优先组压过。
	p := newSelectionPool(t, SelectionLowestCredits, "vip", "rich")
	p.SetCredits("vip", 9000) // 额度最高
	p.SetCredits("rich", 1)   // 常规组里最低 → 策略本会选它
	if !p.SetAccountSelection("vip", AccountSelection{Excluded: true, Placement: PlacementFirst}) {
		t.Fatal("set selection failed")
	}

	for i := 0; i < 10; i++ {
		if got := p.Pick(); got == nil || got.UID != "vip" {
			t.Fatalf("iter %d: pick=%v want vip (excluded+first must preempt strategy)", i, got)
		}
	}
}

// TestSelectionExcludedLastYieldsToNormal 排除 + 落位「最后使用」：
// 常规组非空时永远不选它；常规组全不可用时才轮到它。
func TestSelectionExcludedLastYieldsToNormal(t *testing.T) {
	p := newSelectionPool(t, SelectionLowestCredits, "backup", "normal")
	p.SetCredits("backup", 1) // 额度最低：若参与策略竞争本会被一直选中
	p.SetCredits("normal", 500)
	if !p.SetAccountSelection("backup", AccountSelection{Excluded: true, Placement: PlacementLast}) {
		t.Fatal("set selection failed")
	}

	for i := 0; i < 10; i++ {
		if got := p.Pick(); got == nil || got.UID != "normal" {
			t.Fatalf("iter %d: pick=%v want normal (excluded+last must yield to normal group)", i, got)
		}
	}

	// 常规组不可用（冷却）→ 才轮到兜底组。
	p.Cooldown("normal", CoolSoft, time.Hour, "test")
	if got := p.Pick(); got == nil || got.UID != "backup" {
		t.Fatalf("normal cooling: pick=%v want backup (last-placement used only when normal empty)", got)
	}
}

// TestSelectionExcludedGroupsAreDeterministic 排除组内按 uid 升序取第一个（不走策略排序）：
// 这样"最优先使用"才钉得住号，而不是变成"大概率用它"。
func TestSelectionExcludedGroupsAreDeterministic(t *testing.T) {
	p := newSelectionPool(t, SelectionWeighted, "b-first", "a-first", "c-normal")
	p.SetCredits("b-first", 9999) // 若走加权，额度高者会被压倒性选中
	p.SetCredits("a-first", 1)
	for _, uid := range []string{"a-first", "b-first"} {
		if !p.SetAccountSelection(uid, AccountSelection{Excluded: true, Placement: PlacementFirst}) {
			t.Fatalf("set selection failed for %s", uid)
		}
	}

	for i := 0; i < 20; i++ {
		if got := p.Pick(); got == nil || got.UID != "a-first" {
			t.Fatalf("iter %d: pick=%v want a-first (uid order inside excluded group, no strategy)", i, got)
		}
	}
}

// TestSelectionTierPriorityOrder 三组并存时的组间优先级：
// 优先组 > 常规组 > 兜底组。逐组清空以观察顺位下移。
func TestSelectionTierPriorityOrder(t *testing.T) {
	p := newSelectionPool(t, SelectionWeighted, "first", "normal", "last")
	if !p.SetAccountSelection("first", AccountSelection{Excluded: true, Placement: PlacementFirst}) {
		t.Fatal("set first failed")
	}
	if !p.SetAccountSelection("last", AccountSelection{Excluded: true, Placement: PlacementLast}) {
		t.Fatal("set last failed")
	}

	if got := p.Pick(); got == nil || got.UID != "first" {
		t.Fatalf("pick=%v want first (tier 1 wins)", got)
	}
	p.Cooldown("first", CoolSoft, time.Hour, "test")
	if got := p.Pick(); got == nil || got.UID != "normal" {
		t.Fatalf("pick=%v want normal (tier 2 after first unavailable)", got)
	}
	p.Cooldown("normal", CoolSoft, time.Hour, "test")
	if got := p.Pick(); got == nil || got.UID != "last" {
		t.Fatalf("pick=%v want last (tier 3 only when others empty)", got)
	}
}

// TestSelectionExcludedDoesNotBypassAvailability 排除**不**放宽可用性判定：
// 被排除的号若在冷却/冻结/占满在途，照样不会被选中，下一顺位顶上。
// 这是本功能最关键的边界——"排除"只改排序，不是"绕过健康检查"的开关。
func TestSelectionExcludedDoesNotBypassAvailability(t *testing.T) {
	p := newSelectionPool(t, SelectionWeighted, "vip-cooling", "vip-frozen", "vip-full", "normal")
	for _, uid := range []string{"vip-cooling", "vip-frozen", "vip-full"} {
		if !p.SetAccountSelection(uid, AccountSelection{Excluded: true, Placement: PlacementFirst}) {
			t.Fatalf("set selection failed for %s", uid)
		}
	}
	p.Cooldown("vip-cooling", CoolSoft, time.Hour, "test")
	p.SetCredits("vip-frozen", 0) // 冻结路径由额度巡检/签到触发，这里直接走 FreezeNoCredits
	p.FreezeNoCredits("vip-frozen", "额度耗尽")
	p.SetMaxInFlight(1)
	if !p.Acquire("vip-full") {
		t.Fatal("acquire vip-full failed")
	}
	defer p.Release("vip-full")

	if got := p.Pick(); got == nil || got.UID != "normal" {
		t.Fatalf("pick=%v want normal (excluded-but-unavailable must be skipped)", got)
	}
}

// TestSelectionExcludedPriorityStillAppliesToCustomPriority 排除与自定义优先级正交：
// 优先级在**组内**仍然生效——优先组里优先级高者先被选中。
func TestSelectionExcludedPriorityStillAppliesToCustomPriority(t *testing.T) {
	p := newSelectionPool(t, SelectionCustomPriority, "a", "b")
	if !p.SetAccountSelection("a", AccountSelection{Excluded: true, Placement: PlacementFirst}) {
		t.Fatal("set a failed")
	}
	if !p.SetAccountSelection("b", AccountSelection{Excluded: true, Placement: PlacementFirst}) {
		t.Fatal("set b failed")
	}
	// 排除组内按 uid 升序 → 默认会选 a。但优先级只对常规组生效（排除组不走策略），
	// 故即便给 b 更高的优先级，结果仍是 a —— 这证明排除组确实"绕开了策略"。
	if !p.SetAccountSelection("b", AccountSelection{Excluded: true, Placement: PlacementFirst, Priority: 999}) {
		t.Fatal("set b priority failed")
	}
	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("pick=%v want a (excluded group ignores strategy priority, uses uid order)", got)
	}
}

// TestAccountSelectionNormalizesPlacement 写入端归一化：
//   - 排除 + 未知/空落位 → 存成 PlacementLast（危险错向不放大）；
//   - 未排除 → 落位被清空（不残留语义上无效的值），但优先级保留。
func TestAccountSelectionNormalizesPlacement(t *testing.T) {
	p := newSelectionPool(t, SelectionWeighted, "u1")

	if !p.SetAccountSelection("u1", AccountSelection{Excluded: true, Placement: "bogus", Priority: 7}) {
		t.Fatal("set failed")
	}
	sel, ok := p.AccountSelection("u1")
	if !ok || sel.Placement != PlacementLast {
		t.Fatalf("unknown placement must normalize to last, got %+v", sel)
	}
	if sel.Priority != 7 {
		t.Fatalf("priority must be preserved, got %d", sel.Priority)
	}

	if !p.SetAccountSelection("u1", AccountSelection{Excluded: true, Placement: PlacementFirst}) {
		t.Fatal("set failed")
	}
	if sel, _ := p.AccountSelection("u1"); sel.Placement != PlacementFirst {
		t.Fatalf("placement=%q want first", sel.Placement)
	}

	// 取消排除 → 落位清空，优先级保留（优先级与排除无关）。
	if !p.SetAccountSelection("u1", AccountSelection{Excluded: false, Placement: PlacementFirst, Priority: 7}) {
		t.Fatal("set failed")
	}
	sel, _ = p.AccountSelection("u1")
	if sel.Excluded || sel.Placement != "" {
		t.Fatalf("un-excluded must clear placement, got %+v", sel)
	}
	if sel.Priority != 7 {
		t.Fatalf("priority must survive un-exclusion, got %d", sel.Priority)
	}
}

// TestAccountSelectionUnknownUID 不存在的 uid：读写都报 false（控制台据此回 404，
// 不能让界面拿到一个"成功"却什么都没发生）。
func TestAccountSelectionUnknownUID(t *testing.T) {
	p := newSelectionPool(t, SelectionWeighted, "u1")
	if p.SetAccountSelection("ghost", AccountSelection{Excluded: true}) {
		t.Fatal("SetAccountSelection on unknown uid must report false")
	}
	if _, ok := p.AccountSelection("ghost"); ok {
		t.Fatal("AccountSelection on unknown uid must report false")
	}
}

// TestUnconfiguredAccountsBehaveAsBefore 向后兼容：没有任何账号级配置时，
// 三种策略下的选号结果与改造前完全一致（排除机制不参与决策）。
func TestUnconfiguredAccountsBehaveAsBefore(t *testing.T) {
	p := newSelectionPool(t, SelectionLowestCredits, "u1", "u2")
	p.SetCredits("u1", 500)
	p.SetCredits("u2", 10)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2 (unchanged lowest_credits behavior)", got)
	}
	// Status 里的选号配置字段应为零值，不因新增字段改变既有账号的对外视图。
	st, ok := p.Status("u1")
	if !ok || st.SelectionExcluded || st.SelectionPlacement != "" || st.SelectionPriority != 0 {
		t.Fatalf("unconfigured account must report zero-value selection config: %+v", st)
	}
}

// TestSelectionModeSettersAndDeteminism 策略注入口径：
// 四个合法值原样生效，未知值回落 weighted；确定性判定与注入结果一致。
func TestSelectionModeSettersAndDeteminism(t *testing.T) {
	p := New("")
	for _, mode := range []string{SelectionWeighted, SelectionLowestCredits, SelectionHighestCredits, SelectionCustomPriority} {
		p.SetSelectionMode(mode)
		if got := p.SelectionMode(); got != mode {
			t.Fatalf("SelectionMode=%q want %q", got, mode)
		}
		want := mode != SelectionWeighted
		if got := IsDeterministicSelection(p.SelectionMode()); got != want {
			t.Fatalf("IsDeterministicSelection(%q)=%v want %v", mode, got, want)
		}
	}
	p.SetSelectionMode("bogus")
	if got := p.SelectionMode(); got != SelectionWeighted {
		t.Fatalf("unknown mode must fall back to weighted, got %q", got)
	}
	if IsDeterministicSelection("") || IsDeterministicSelection("bogus") {
		t.Fatal("non-deterministic/unknown modes must report false")
	}
}

// TestValidPlacement 落位取值校验：空串合法（未指定），first/last 合法，其余非法。
func TestValidPlacement(t *testing.T) {
	for _, ok := range []string{"", PlacementFirst, PlacementLast} {
		if !ValidPlacement(ok) {
			t.Errorf("ValidPlacement(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"FIRST", "First", "last ", "most", "0"} {
		if ValidPlacement(bad) {
			t.Errorf("ValidPlacement(%q) = true, want false", bad)
		}
	}
}
