package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestStatusCreditsExactIncludesFraction 真实额度必须是 credits - creditFrac。
//
// 背景：上游余额接口只给整数，而本地即时扣减会把小数消耗（10.85）攒在
// creditFrac 里、满 1 才落到 credits 上。于是"还剩多少"的真实值是二者相减。
// 只暴露 credits 会**多报**最多 1 分——前端就显示不出这个差值（用户报的问题）。
func TestStatusCreditsExactIncludesFraction(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 390)

	// 一次 10.85 的消耗：整数位不减（0.85 < 1），小数挂账。
	// 一次 10.85 的消耗：creditFrac 累加 0→10.85，Floor=10 立即落到整数位，
	// 余 0.85 挂账（误差恒 < 1，见 deduct.go 的精度说明）。
	p.DeductCredits("u1", 10.85)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status not found")
	}
	if st.Credits != 380 {
		t.Errorf("credits=%d want 380 (390 - floor(10.85))", st.Credits)
	}
	// 精确值 = 390 - 10.85 = 379.15
	if diff := st.CreditsExact - 379.15; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("credits_exact=%v want 379.15 (390 - 10.85)", st.CreditsExact)
	}
	if !st.CreditsKnown {
		t.Error("credits_known must be true after SetCredits")
	}
}

// TestStatusCreditsExactFractionOnly 小数不足 1 时整数位不动，精确值体现挂账。
//
// 0.4 的消耗：creditFrac 0→0.4，Floor=0 不落整数位 → credits 仍是 390，
// 而精确值是 389.6。这正是"只显示整数会多报"的最小复现。
func TestStatusCreditsExactFractionOnly(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 390)
	p.DeductCredits("u1", 0.4)

	st, _ := p.Status("u1")
	if st.Credits != 390 {
		t.Errorf("credits=%d want 390 (0.4 < 1，整数位不动)", st.Credits)
	}
	if diff := st.CreditsExact - 389.6; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("credits_exact=%v want 389.6 (390 - 0.4)", st.CreditsExact)
	}
}

// TestStatusCreditsExactAccumulates 多次小数消耗的精确值持续反映累加。
//
// 10.85 × 3 = 32.55 → 整数位减 32，余 0.55 → 精确值 = 390-32-0.55 = 357.45。
func TestStatusCreditsExactAccumulates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 390)
	for i := 0; i < 3; i++ {
		p.DeductCredits("u1", 10.85)
	}
	st, _ := p.Status("u1")
	if st.Credits != 358 {
		t.Errorf("credits=%d want 358 (390 - 32)", st.Credits)
	}
	if diff := st.CreditsExact - 357.45; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("credits_exact=%v want 357.45 (358 - 0.55)", st.CreditsExact)
	}
}

// TestStatusCreditsExactUnknownStaysZero 余额未知时精确值必须是 0，不能是 -frac。
//
// 这是关键语义边界：creditsKnown=false 时 credits 的 0 意思是"从没观测过"，
// 不是"耗尽"。若此时算出 -creditFrac，就会把"未知"伪装成负余额，
// 而两者在选号与冻结判定上完全不同（未知要优先探测，0 是耗尽）。
func TestStatusCreditsExactUnknownStaysZero(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"}) // 未 SetCredits → 未知

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status not found")
	}
	if st.CreditsKnown {
		t.Fatal("precondition: credits must be unknown")
	}
	if st.CreditsExact != 0 {
		t.Errorf("credits_exact=%v want 0 for unknown balance (must not fake a value)", st.CreditsExact)
	}
}

// TestStatusCreditsExactUnknownNotDeducted 未知余额即便收到消耗也不该变成负数。
func TestStatusCreditsExactUnknownNotDeducted(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 未知余额时 DeductCredits 拒绝扣减（ok=false），故 frac 不增长。
	if _, ok := p.DeductCredits("u1", 9.9); ok {
		t.Error("DeductCredits on unknown balance must report not-ok")
	}
	st, _ := p.Status("u1")
	if st.CreditsExact != 0 {
		t.Errorf("credits_exact=%v want 0 (unknown balance untouched)", st.CreditsExact)
	}
}

// TestStatusCreditsExactNeverNegative 精确值不出现负数（浮点误差也要钳住）。
//
// 显示层的意义：-0.00 这种值出现在控制台上很刺眼，且会让"额度耗尽的号"
// 看起来像出了 bug 而不是正常耗尽。
func TestStatusCreditsExactNeverNegative(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 5)
	p.DeductCredits("u1", 999) // 远超余额 → 钳 0

	st, _ := p.Status("u1")
	if st.CreditsExact < 0 {
		t.Errorf("credits_exact=%v must never be negative", st.CreditsExact)
	}
	if st.CreditsExact != 0 {
		t.Errorf("credits_exact=%v want 0 (clamped)", st.CreditsExact)
	}
}

// TestStatusCreditsExactClearedByCalibration 校准清零小数余量后精确值回到整数。
//
// 校准 = 用上游权威余额覆盖本地估算，故 credits_exact 应当正好等于权威值
// （而不是权威值再减去已经作废的旧余量）。
func TestStatusCreditsExactClearedByCalibration(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.DeductCredits("u1", 0.75) // 挂账 0.75

	if st, _ := p.Status("u1"); st.CreditsExact == 100 {
		t.Fatal("precondition: fraction should already reduce the exact value")
	}

	p.ReconcileCredits("u1", 200)
	st, _ := p.Status("u1")
	if st.CreditsExact != 200 {
		t.Errorf("credits_exact=%v want 200 (calibration resets fraction)", st.CreditsExact)
	}
}
