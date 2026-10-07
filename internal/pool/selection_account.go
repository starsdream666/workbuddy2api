// 账号级选号配置：选号策略排除 + 排除后的落位（最优先使用 / 最后使用）+ 自定义优先级。
//
// 与「人工停用」（manual.go）的分工：
//   - 人工停用是**可用性**开关：停用的号根本不进选号，也不跑后台任务；
//   - 本文件的配置是**排序**开关：账号照常健康、照常参与调度，只是不再按选号策略
//     与其它号竞争，而是被钉到「优先组」或「兜底组」（见 pick.go 的分组口径）。
//
// 之所以做成按账号配置而非全局名单：排除是"针对某个号"的运维意志，配置写在账号上
// 才不会出现"名单里的 uid 属于哪条产品线"、"uid 打错了静默不生效"这类歧义；
// 且与人工停用共用同一条持久化路径（state.json），重启后仍生效。
package pool

// AccountSelection 单个账号的选号配置（控制台读写）。
//
// 三个字段互相独立，组合语义见 selectionTierOf：
//   - Excluded 为 false 时 Placement 无意义（该号按策略正常参与竞争）；
//   - Priority 只在 pool.selection_mode = custom_priority 时参与排序。
type AccountSelection struct {
	// Excluded 是否排除在选号策略之外（从常规组移出）。
	Excluded bool `json:"excluded"`
	// Placement 排除后的落位：PlacementFirst / PlacementLast（空值按 PlacementLast 解释）。
	Placement string `json:"placement,omitempty"`
	// Priority 自定义优先级（数值大者优先；默认 0；仅 custom_priority 策略使用）。
	Priority int `json:"priority"`
}

// ValidPlacement 报告落位取值是否合法（空值合法：表示"未指定"，按最后使用解释）。
// 供控制台接口在写库前挡下拼写错误——静默回落会让"我配了但没生效"无从排查。
func ValidPlacement(s string) bool {
	return s == "" || s == PlacementFirst || s == PlacementLast
}

// normalizePlacement 把落位归一为确定的两个取值之一。
// 空值/未知值 → PlacementLast：排除一个号最常见的意图是"别优先用它"，
// 而把未知值当 first 会让一个手滑写入的配置把某号顶到最前（更危险的错向）。
func normalizePlacement(s string) string {
	if s == PlacementFirst {
		return PlacementFirst
	}
	return PlacementLast
}

// SetAccountSelection 写入账号级选号配置，返回 false = uid 不存在。
//
// 幂等：重复写入同一份配置只刷新字段。落位做归一化后存储（见 normalizePlacement），
// 使 state.json 里的值永远是确定的两取值之一——读取端（selectionTierOf）无需再兜。
// 变更置 dirty，由后台 flusher 落盘（默认 5s 一次），重启后仍生效。
func (p *Pool) SetAccountSelection(uid string, sel AccountSelection) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.selExcluded = sel.Excluded
	e.selPriority = sel.Priority
	if sel.Excluded {
		e.selPlacement = normalizePlacement(sel.Placement)
	} else {
		// 未排除时清掉落位：让 state.json 不残留一个语义上无效的值
		// （否则"看着配了 first，实际因为没排除而无效"会持续误导运维）。
		// Priority 不受影响——它与排除无关，custom_priority 策略下照常生效。
		e.selPlacement = ""
	}
	p.markDirty()
	return true
}

// AccountSelection 读取账号级选号配置；第二个返回值为 false 表示 uid 不存在。
func (p *Pool) AccountSelection(uid string) (AccountSelection, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return AccountSelection{}, false
	}
	return AccountSelection{
		Excluded:  e.selExcluded,
		Placement: e.selPlacement,
		Priority:  e.selPriority,
	}, true
}
