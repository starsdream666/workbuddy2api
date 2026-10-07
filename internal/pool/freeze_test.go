package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func freezeTestPool() *Pool {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999})
	return p
}

// TestFreezeNoCreditsBlocksPick 冻结后账号不可选：Pick 永远落到未冻结的那个。
func TestFreezeNoCreditsBlocksPick(t *testing.T) {
	p := freezeTestPool()
	p.FreezeNoCredits("u1", "余额不足")

	if !p.IsFrozen("u1") {
		t.Fatal("u1 应处于额度冻结")
	}
	st, ok := p.Status("u1")
	if !ok || !st.Frozen || st.FrozenReason == "" {
		t.Errorf("Status 未暴露冻结信息: %+v", st)
	}
	if st.Cooling {
		t.Error("冻结不该表现为时间冷却（否则会到点自动解冻，与本语义冲突）")
	}
	for i := 0; i < 20; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatal("仍有 u2 可用，不该返回 nil")
		}
		if a.UID != "u2" {
			t.Fatalf("Pick 选中了冻结账号 %s", a.UID)
		}
	}
}

// TestFrozenAccountExcludedFromFallback 全池都不可用时，冻结号也不参与"最早到期"兜底
// （否则冻结形同虚设）。这里用"冻结 + 熔断"组合覆盖最容易漏的那条路径。
func TestFrozenAccountExcludedFromFallback(t *testing.T) {
	p := freezeTestPool()
	p.SetBreaker(1, time.Minute, time.Minute)                 // 阈值 1：下一次失败立刻熔断
	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit") // 制造 breakerUntil
	p.FreezeNoCredits("u1", "余额不足")
	p.FreezeNoCredits("u2", "余额不足")

	if a := p.Pick(); a != nil {
		t.Fatalf("两个号都冻结时不该兜底选出账号，got %s", a.UID)
	}
}

// TestReconcileCreditsFreezeAndUnfreeze 巡检核心闭环：
// 余额 0 → 冻结；余额恢复 → 解冻（并清软冷却退避计数）。
func TestReconcileCreditsFreezeAndUnfreeze(t *testing.T) {
	p := freezeTestPool()

	froze, unfroze := p.ReconcileCredits("u1", 0)
	if !froze || unfroze {
		t.Fatalf("余额 0 应冻结: froze=%v unfroze=%v", froze, unfroze)
	}
	if !p.IsFrozen("u1") {
		t.Fatal("u1 未被冻结")
	}
	if st, _ := p.Status("u1"); st.Credits != 0 {
		t.Errorf("额度缓存未更新: %d", st.Credits)
	}

	// 重复观测到 0：幂等，不再报"新冻结"
	if froze, _ := p.ReconcileCredits("u1", 0); froze {
		t.Error("已在冻结态时重复观测不该再报 froze")
	}

	// 余额恢复 → 解冻
	froze, unfroze = p.ReconcileCredits("u1", 30)
	if froze || !unfroze {
		t.Fatalf("余额恢复应解冻: froze=%v unfroze=%v", froze, unfroze)
	}
	if p.IsFrozen("u1") {
		t.Fatal("解冻后不应仍为冻结")
	}
	if a := p.Pick(); a == nil {
		t.Fatal("解冻后应可被选中")
	}

	// 余额充足且未冻结：只更新额度，不算状态翻转
	if froze, unfroze := p.ReconcileCredits("u2", 100); froze || unfroze {
		t.Errorf("无状态变化时不该报翻转: froze=%v unfroze=%v", froze, unfroze)
	}
	if st, _ := p.Status("u2"); st.Credits != 100 {
		t.Errorf("额度缓存未更新: %d", st.Credits)
	}
}

// TestFreezeClearsTimeCooldown 冻结会清掉时间冷却：解冻逻辑不必同时满足两个条件。
func TestFreezeClearsTimeCooldown(t *testing.T) {
	p := freezeTestPool()
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Fatal("前置条件：u1 应处于软冷却")
	}
	p.FreezeNoCredits("u1", "余额不足")
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Error("冻结应清掉时间冷却，避免解冻要同时满足两个条件")
	}
	if !st.Frozen {
		t.Error("冻结标记应生效")
	}
}

// TestFrozenUntilSafetyNet 兜底放行：冻结超过兜底时长后即使没探测到恢复也放行一次
// （防止巡检被关闭导致账号永久冻结）。
func TestFrozenUntilSafetyNet(t *testing.T) {
	p := freezeTestPool()
	p.SetFreezeMax(50 * time.Millisecond)
	p.FreezeNoCredits("u1", "余额不足")
	if p.IsFrozen("u1") && p.byUID["u1"].healthy(time.Now()) {
		t.Fatal("兜底期内不应放行")
	}
	time.Sleep(80 * time.Millisecond)
	if !p.byUID["u1"].healthy(time.Now()) {
		t.Error("超过兜底时长后应放行（宁可撞一次也不要永久静默失效）")
	}
}

// TestFreezePersistsAcrossReload 冻结状态随 state.json 持久化，重启后不丢失
// （否则重启即解冻，余额为 0 的号又会去撞 429）。
func TestFreezePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 0)
	p.FreezeNoCredits("u1", "余额不足")
	p.Flush()

	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("state.json 未落盘: %v", err)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	if !p2.IsFrozen("u1") {
		t.Fatal("重启后冻结状态丢失")
	}
	st, _ := p2.Status("u1")
	if st.FrozenReason == "" {
		t.Error("冻结原因未持久化")
	}
}

// TestFrozenUIDsList 巡检任务靠它拿到待复查清单。
func TestFrozenUIDsList(t *testing.T) {
	p := freezeTestPool()
	p.FreezeNoCredits("u2", "余额不足")
	got := p.FrozenUIDs()
	if len(got) != 1 || got[0] != "u2" {
		t.Errorf("FrozenUIDs=%v want [u2]", got)
	}
}
