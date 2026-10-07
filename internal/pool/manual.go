// 人工启停：控制台对单个凭证的手动开/关（持久化到 state.json）。
//
// 停用分两层，互不覆盖：
//   - 系统层（entry.disabled）：连续 12153 判死、refresh 失败等自动写入，只有重新登录
//     或 ReviveDisabled 能清；
//   - 人工层（entry.manualDisabled）：运维在控制台点的开关，本文件写入。
//
// healthy() 认为两层任一生效即不可选，但两层各自独立清除——所以"启用"一个被系统判死的
// 账号不会让它忽然回到轮转里（session 依然是死的，只会白撞一次 12153 再被判死）。
// 人工层同样不碰冷却/冻结/熔断：那些是系统观测到的真实限制，运维开关不该替它们做决定。
package pool

import "sort"

// manualDisabledReason 人工停用的持久化原因（控制台展示用）。写死而非自由文本：
// 停用是开关不是工单，固定文案免去前端输入的转义面。
const manualDisabledReason = "手动停用"

// SetManualDisabled 控制台人工启停入口：置/清手动停用层，返回 false = uid 不存在
// （控制台据此回 404，避免"静默成功"）。
//
// 幂等：重复置位/清除只刷新原因字段。只改人工层，不碰系统层 disabled/reason，
// 也不碰 until/coolKind/frozen/breaker（见文件头说明）。变更置 dirty，由后台 flusher
// 落盘（默认 5s 一次），重启后仍生效。
func (p *Pool) SetManualDisabled(uid string, disabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if disabled {
		e.manualDisabled = true
		e.manualReason = manualDisabledReason
	} else {
		e.manualDisabled = false
		e.manualReason = ""
	}
	p.markDirty()
	return true
}

// ManualDisabled 报告账号是否处于人工停用层。供调度器/运维接口区分"谁停的"：
// 两层的停用原因不同，处置方式也不同（人工层点一下就好，系统层要重登）。
func (p *Pool) ManualDisabled(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	return ok && e.manualDisabled
}

// ManuallyDisabledUIDs 返回处于人工停用层的 uid 列表（稳定顺序，同 FrozenUIDs）。
func (p *Pool) ManuallyDisabledUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, 4)
	for uid, e := range p.byUID {
		if e.manualDisabled {
			out = append(out, uid)
		}
	}
	sort.Strings(out)
	return out
}

// Stopped 报告账号当前是否被任一层停用（系统判定或人工停用）。
// 调度器（签到/旅行/活跃/保活）用这个口径跳过停用号——只看 st.Disabled 会漏掉人工层，
// 让运维停掉的号继续被后台任务刷。与选号口径（healthy 里两层并罚）保持一致。
func (s Status) Stopped() bool { return s.Disabled || s.ManualDisabled }
