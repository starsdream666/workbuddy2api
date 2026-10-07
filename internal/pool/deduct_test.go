package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestDeductCreditsFractionAccumulates 小数累加不丢精度。
//
// 背景：上游消耗是小数（实测 10.85/11/10.82），而 credits 是整数。
// 若每次 int64(used) 截断，10.85 连续三次会三次都扣 10 分（少扣 2.55）；
// 若四舍五入，又会把长期误差随机推高。故用累加器：满 1 才落到整数余额上。
func TestDeductCreditsFractionAccumulates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	// 10.85 × 3 = 32.55 → 整数位应恰好减 32，余 0.55 挂账。
	// 10.85 × 3 = 32.55 → 整数位应恰好减 32，余 0.55 挂账。
	for i := 0; i < 3; i++ {
		if _, ok := p.DeductCredits("u1", 10.85); !ok {
			t.Fatalf("iter %d: DeductCredits reported not-ok", i)
		}
	}
	got, ok := p.CreditsOf("u1")
	if !ok {
		t.Fatal("credits should be known")
	}
	if got != 100-32 {
		t.Errorf("after 3×10.85: credits=%d want 68 (100 - 32, error must stay < 1)", got)
	}
	frac := p.CreditFrac("u1")
	if frac < 0.55-1e-9 || frac > 0.55+1e-9 {
		t.Errorf("creditFrac=%v want ~0.55 (32.55 - 32)", frac)
	}
}

// TestDeductCreditsCarriesEachStep 逐次核对进位时机（防"累加器写反"这类低级错）。
func TestDeductCreditsCarriesEachStep(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000)

	// 序列：0.6 → 余 0.6，余额 1000
	//       0.6 → 余 1.2 → 扣 1，余 0.2，余额 999
	//       0.3 → 余 0.5，余额 999
	//       0.9 → 余 1.4 → 扣 1，余 0.4，余额 998
	steps := []struct {
		used  float64
		want  int64
		frac  float64 // 期望的小数余量（容忍浮点误差）
		label string
	}{
		{0.6, 1000, 0.6, "first half-step does not carry"},
		{0.6, 999, 0.2, "second step carries 1"},
		{0.3, 999, 0.5, "no carry"},
		{0.9, 998, 0.4, "carries again"},
	}
	for i, s := range steps {
		after, ok := p.DeductCredits("u1", s.used)
		if !ok {
			t.Fatalf("step %d (%s): not ok", i, s.label)
		}
		if after != s.want {
			t.Errorf("step %d (%s): after=%d want %d", i, s.label, after, s.want)
		}
		if f := p.CreditFrac("u1"); f < s.frac-1e-9 || f > s.frac+1e-9 {
			t.Errorf("step %d (%s): frac=%v want %v", i, s.label, f, s.frac)
		}
	}
}

// TestDeductCreditsClampsAtZero 余额不越零。
//
// 负数余额没有意义（上游最多给到 0），且会让权重/排序计算失真。
func TestDeductCreditsClampsAtZero(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 5)

	after, ok := p.DeductCredits("u1", 999)
	if !ok {
		t.Fatal("should be ok")
	}
	if after != 0 {
		t.Errorf("after=%d want 0 (clamped)", after)
	}
	if f := p.CreditFrac("u1"); f != 0 {
		t.Errorf("frac=%v want 0 (reset alongside clamp)", f)
	}
	// 再扣一次仍然稳定停在 0（不产生负数、也不因钳位残留状态）。
	if after, _ := p.DeductCredits("u1", 3.5); after != 0 {
		t.Errorf("second drain: after=%d want 0", after)
	}
}

// TestDeductCreditsSkipsUnknownBalance 余额未知时不扣。
//
// 未知（creditsKnown=false）的 0 表示"没观测过"，不是"耗尽"。
// 若在此扣减，会把"未知"变成"负数"，比不扣更糟——这类账号由单号校准建立基准。
func TestDeductCreditsSkipsUnknownBalance(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"}) // 未 SetCredits → 余额未知

	after, ok := p.DeductCredits("u1", 12.5)
	if ok {
		t.Errorf("ok=true want false (balance unknown must not be deducted)")
	}
	if after != 0 {
		t.Errorf("after=%d want 0 (untouched)", after)
	}
	if p.CreditsKnownOf("u1") {
		t.Error("creditsKnown must stay false — deduction must not fabricate knowledge")
	}
	if f := p.CreditFrac("u1"); f != 0 {
		t.Errorf("frac=%v want 0 (no accumulation on unknown balance)", f)
	}
}

// TestDeductCreditsRejectsDirtyValues NaN/Inf/负消耗不改变余额。
func TestDeductCreditsRejectsDirtyValues(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 50)

	for _, used := range []float64{-1, 0, 0.0} {
		after, ok := p.DeductCredits("u1", used)
		if !ok {
			t.Errorf("used=%v: ok=false want true (known balance, no-op)", used)
		}
		if after != 50 {
			t.Errorf("used=%v: after=%d want 50 (unchanged)", used, after)
		}
	}
}

// TestDeductCreditsUnknownUID 不存在的账号安全返回。
func TestDeductCreditsUnknownUID(t *testing.T) {
	p := New("")
	if _, ok := p.DeductCredits("ghost", 5); ok {
		t.Error("ok=true for missing uid")
	}
	if f := p.CreditFrac("ghost"); f != 0 {
		t.Errorf("frac=%v want 0 for missing uid", f)
	}
}

// TestCalibrateDueNeverCalibrated 从未校准过的账号恒为 due。
//
// 语义：本地扣减只是相对累加，必须至少有一次权威基准（否则展示值纯属推算）。
func TestCalibrateDueNeverCalibrated(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if !p.CalibrateDue("u1", time.Hour) {
		t.Error("never-calibrated account must be due even with a long interval")
	}
}

// TestCalibrateDueThrottles 同一 interval 内不重复校准。
//
// 这是"单号校准"的成本控制：连续调用同一账号时不该每次多打一次余额查询。
func TestCalibrateDueThrottles(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.MarkCalibrated("u1")
	if p.CalibrateDue("u1", time.Hour) {
		t.Error("just-calibrated account must not be due within the interval")
	}
	// 极短 interval → 到期（供"每次都要权威值"的场景）。
	// 先睡过 interval 再断言：Windows 时钟分辨率可达毫秒级，
	// 刚写入 lastCalib 就立刻读回可能得到 0 间隔（与既有的时钟精度失败同类）。
	time.Sleep(20 * time.Millisecond)
	if !p.CalibrateDue("u1", time.Millisecond) {
		t.Error("tiny interval should make it due once elapsed")
	}
}

// TestCalibrateDueZeroIntervalMeansAlways 0 间隔表示每次都校准。
func TestCalibrateDueZeroIntervalMeansAlways(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkCalibrated("u1")

	if !p.CalibrateDue("u1", 0) {
		t.Error("interval=0 must mean always due")
	}
	if !p.CalibrateDue("u1", -time.Second) {
		t.Error("negative interval must mean always due")
	}
}

// TestCalibrateDueUnknownUID 不存在的账号不触发校准（避免无谓的上游查询）。
func TestCalibrateDueUnknownUID(t *testing.T) {
	p := New("")
	if p.CalibrateDue("ghost", time.Hour) {
		t.Error("unknown uid must not be due")
	}
}

// TestReconcileCreditsResetsFraction 权威写入必须清零小数余量。
//
// 这是校准正确性的关键副作用：校准 = 用上游绝对余额覆盖本地累加估算。
// 若残留零头，下一轮扣减会挂在新基准上从错误基数起算，且永远不会自然消失。
func TestReconcileCreditsResetsFraction(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	// 制造一个小数余量：0.9 不触发进位。
	p.DeductCredits("u1", 0.9)
	if f := p.CreditFrac("u1"); f < 0.9-1e-9 {
		t.Fatalf("setup: frac=%v want ~0.9", f)
	}

	p.ReconcileCredits("u1", 500)
	if f := p.CreditFrac("u1"); f != 0 {
		t.Errorf("frac=%v after ReconcileCredits want 0 (authoritative value resets the accumulator)", f)
	}
	got, _ := p.CreditsOf("u1")
	if got != 500 {
		t.Errorf("credits=%d want 500", got)
	}
}

// TestSetCreditsResetsFraction SetCredits 同为权威写入，也必须清零余量。
func TestSetCreditsResetsFraction(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.DeductCredits("u1", 0.75)

	p.SetCredits("u1", 200)
	if f := p.CreditFrac("u1"); f != 0 {
		t.Errorf("frac=%v after SetCredits want 0", f)
	}
}

// TestDeductThenCalibrateTogether 扣减与校准协作：本地估算被权威值纠正。
func TestDeductThenCalibrateTogether(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	// 本地扣两个 10.85：整数位减 21，余 0.70。
	p.DeductCredits("u1", 10.85)
	p.DeductCredits("u1", 10.85)
	if got, _ := p.CreditsOf("u1"); got != 79 {
		t.Fatalf("local estimate=%d want 79", got)
	}

	// 上游权威值 78（比本地估算多扣了 1，模拟延迟计费等漂移）。
	p.ReconcileCredits("u1", 78)
	got, _ := p.CreditsOf("u1")
	if got != 78 {
		t.Errorf("after calibration=%d want 78 (authority wins)", got)
	}
	if f := p.CreditFrac("u1"); f != 0 {
		t.Errorf("frac=%v want 0 (drift corrected, accumulator cleared)", f)
	}
}
