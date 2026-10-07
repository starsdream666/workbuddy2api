// deduct.go 本地即时扣减 + 单号额度校准。
//
// 背景：上游每次响应都在 usage.credit 里报告本次消耗（实测精确到 0.01），
// 而余额只有绝对快照一个来源、且不即时反映消耗（实测差值恒为 0）。
// 因此"展示的额度"不能干等余额刷新，应当**拿到消耗就当场扣**。
//
// 两条机制分工（关键设计决策）：
//
//   - **本地扣减**（DeductCredits）：拿到上游 credit 立即减，让展示与选号排序
//     立刻反映真实余额，不必等 30 分钟的额度巡检。
//   - **单号校准**（CalibrateDue/MarkCalibrated）：只刷新"刚被用过的那个号"，
//     而不是全量刷新。校正本地累加的漂移，代价是每号每 interval 至多一次查询。
//
// 为什么本地扣减**不直接冻结**账号：本地估算是近似值（小数累加 + 上游口径假设），
// 若偏高就会误冻健康账号；而冻结后它不再被选中 → 永远得不到校准 → 永久沉底
// （同一类死锁在选号策略上已经踩过一次）。所以冻结仍以上游权威观测为准
// （402 撞墙 / 余额查询），本地扣减只负责让排序与展示立刻变准。
package pool

import (
	"math"
	"time"
)

// DeductCredits 按上游报告的消耗做本地即时扣减，返回扣减后的余额。
//
// 精度处理：上游消耗是小数（如 10.85），而 credits 是整数。
// 用 creditFrac 累加小数部分，满 1 才落到整数余额上——保证长期累加
// 不丢精度（误差恒 < 1，不会因为连续小数扣减而永远不减）。
//
// 余额未知（creditsKnown=false）时不扣：那样会把"未知"变成"负数"，
// 比不扣更糟。这类账号由单号校准去建立基准。
func (p *Pool) DeductCredits(uid string, used float64) (after int64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, exists := p.byUID[uid]
	if !exists {
		return 0, false
	}
	if !e.creditsKnown {
		return e.credits, false
	}
	if used <= 0 || math.IsNaN(used) || math.IsInf(used, 0) {
		return e.credits, true // 无消耗/脏数据：不动余额，但也不报错
	}
	e.creditFrac += used
	if whole := math.Floor(e.creditFrac); whole >= 1 {
		e.credits -= int64(whole)
		e.creditFrac -= whole
	}
	if e.credits < 0 {
		// 钳 0：负数余额没有意义（上游最多给到 0），且会让权重计算失真。
		e.credits = 0
		e.creditFrac = 0
	}
	p.markDirty()
	return e.credits, true
}

// CreditFrac 返回该账号尚未落到整数余额上的小数部分（观测/测试用）。
func (p *Pool) CreditFrac(uid string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.creditFrac
	}
	return 0
}

// CalibrateDue 报告该账号是否到了单号校准时间。
//
// interval <= 0 视为"每次都可校准"（调用方决定是否传 0）；
// 从未校准过的账号恒为 due=true（首次即建立权威基准）。
//
// 只校准"刚用过的号"而非全量：全量刷新会把池里每个号都打一遍上游，
// 而这些号里绝大多数本就没变化——按用量付费比按存量付费更划算。
func (p *Pool) CalibrateDue(uid string, interval time.Duration) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if e.lastCalib.IsZero() {
		return true
	}
	if interval <= 0 {
		return true
	}
	return time.Since(e.lastCalib) >= interval
}

// MarkCalibrated 记录一次校准时刻（无论成功与否都调用：
// 失败也应当节流，否则上游持续报错时会对每个请求都重试一次）。
func (p *Pool) MarkCalibrated(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.lastCalib = time.Now()
	}
}
