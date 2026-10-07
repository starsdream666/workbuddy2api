// 额度冻结：上游余额为 0 时把账号冻结，直到**额度探测**确认恢复才解冻。
//
// 与既有冷却的分工：
//   - Cooldown/until：时间语义——"到点了就解冻"。适合限流（429）、熔断这类会自动好的状态。
//   - frozen：条件语义——"必须观测到额度恢复才解冻"。余额耗尽不是等时间能好的
//     （签到/套餐周期刷新才有额度），用时间冷却会出现「到期解冻 → 被选中 → 又 429」的循环。
//
// 触发路径两条：
//  1. 请求实撞上游额度耗尽（ErrHardCredit）→ 立即冻结；
//  2. 额度探测任务（scheduler.CreditWatch）定期查余额 → 余额 0 则冻结、余额 > 0 则解冻。
package pool

import "time"

// FreezeNoCredits 因额度耗尽冻结账号。
//
// 会清掉时间冷却（until/coolKind）：额度冻结已表达"不可用"，再叠一层时间冷却只会
// 让解冻逻辑要同时满足两个条件（探测恢复 + 时间到期），徒增不一致。软冷却计数
// softStreak 保留不动（它衡量的是限流历史，与额度无关）。
func (p *Pool) FreezeNoCredits(uid string, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	p.freezeLocked(e, reason)
	p.markDirty()
}

// freezeLocked 置冻结态（调用方必须已持 p.mu）。幂等：已冻结时只刷新原因。
func (p *Pool) freezeLocked(e *entry, reason string) {
	e.frozen = true
	e.frozenReason = reason
	e.frozenUntil = time.Now().Add(p.freezeMaxOr())
	e.until = time.Time{}
	e.coolKind = 0
	e.softRateModel = ""
}

// unfreezeLocked 清冻结态（调用方必须已持 p.mu）。
func (p *Pool) unfreezeLocked(e *entry) {
	e.frozen = false
	e.frozenReason = ""
	e.frozenUntil = time.Time{}
}

// ReconcileCredits 用一次余额观测调和冻结状态，返回是否发生了状态翻转。
//
//   - credits <= 0 且未冻结 → 冻结（主动发现，不必等请求撞 429）
//   - credits > 0 且已冻结 → 解冻（签到/套餐刷新后恢复）
//
// 同时刷新 credits 缓存（与 SetCredits 同效）。未冻结且额度 > 0 时无状态翻转，
// 只更新额度缓存。
func (p *Pool) ReconcileCredits(uid string, credits int64) (froze bool, unfroze bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false, false
	}
	e.credits = credits
	e.creditsKnown = true
	// 校准 = 用上游绝对余额覆盖本地累加估算 → 必须同时清零小数余量。
	// 否则残留的零头会挂在新基准上，下一轮扣减从错误基数起算（且永远不会自然消失）。
	e.creditFrac = 0
	p.markDirty()
	if credits <= 0 {
		if !e.frozen {
			p.freezeLocked(e, "额度耗尽（探测）")
			return true, false
		}
		return false, false
	}
	if e.frozen {
		p.unfreezeLocked(e)
		// 额度恢复即视为"账号已恢复"：清软冷却退避计数（与签到解冻同口径）。
		e.softStreak = 0
		return false, true
	}
	return false, false
}

// FrozenUIDs 返回当前处于额度冻结的账号 uid（稳定顺序，供探测任务逐个复查）。
func (p *Pool) FrozenUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, 4)
	for uid, e := range p.byUID {
		if e.frozen {
			out = append(out, uid)
		}
	}
	return out
}

// IsFrozen 报告账号是否处于额度冻结。
func (p *Pool) IsFrozen(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	return ok && e.frozen
}

// SetFreezeMax 注入冻结兜底时长（main 从 config 解析后调用）；非正值保留原值。
func (p *Pool) SetFreezeMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.freezeMax = d
	}
}

// freezeMaxOr 返回生效的冻结兜底时长（未注入时用默认值）。调用方须持 p.mu。
func (p *Pool) freezeMaxOr() time.Duration {
	if p.freezeMax > 0 {
		return p.freezeMax
	}
	return defaultFreezeMax
}

// defaultFreezeMax 冻结兜底默认时长：3 天。取值理由：签到活动按天恢复额度、
// 套餐周期通常 14 天，3 天足够探测任务（默认 30 分钟一轮）发现恢复；真出现连续
// 3 天额度为 0 的账号，放行一次让它撞真实响应比继续静默更利于排查。
const defaultFreezeMax = 72 * time.Hour
