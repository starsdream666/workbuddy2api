// 选号：Pick 簇（排除落位分组 + 三因子加权 Top5 短名单 + 加权随机 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math/rand/v2"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按三因子权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "")
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "")
}

func (p *Pool) PickExcludingForRoute(tried map[string]bool, model, route string) *auth.Auth {
	return p.pick(tried, model, route)
}

// pick 在 healthy 候选集中按三因子权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// 候选集是 top5 近似：先按三因子权重（weightOf）降序取前 5（credits 只是权重的一个因子，
// 闲置补偿与成功率同样决定谁进短名单），再在 top5 内做防撞号过滤。
// 并发防雪崩：跳过 lastUsed 距今 < minPickGap 的账号（除非 top5 全部刚被用过，
// 此时退回最近最少使用 LRU 账号），迫使高并发请求发散，而不是全部撞同一高分账号。
// minPickGap=0（测试用）时过滤恒通过，退化为纯加权随机。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效；PickExcluding 传 ""）。
// 注意：模型豁免只进 normal 选号（候选 healthy 判定）；全冷却兜底不参与模型豁免——
// 兜底本来就是在"无任何 direct 可用"时的降级，切模型可用性已在 normal 阶段体现。
//
// 排除落位（选号策略排除）：候选按账号级配置分三组，组间优先级固定为
// 「优先组 > 常规组 > 兜底组」（见 selectionTierOf）。选中第一个非空组后：
//   - 常规组：按 pool.selection_mode 走对应策略（weighted / lowest_credits /
//     highest_credits / custom_priority）；
//   - 优先组 / 兜底组：**不走策略排序**，按 uid 升序取第一个（确定性）——
//     这两组本就是"排除在策略之外"的号，策略不该再对它们排序。
//
// 硬过滤与排除落位无关：tried / healthy / floorBlocked / inFlightFull 四道过滤
// 对所有分组一视同仁。排除只影响"策略如何排序"，绝不放宽可用性判定——
// 一个被排除的号若在冷却/冻结/占满在途，照样不会被选中（下一顺位顶上）。
func (p *Pool) pick(tried map[string]bool, reqModel, route string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	healthyOf := func(e *entry) bool { return e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !healthyOf(e) || p.floorBlocked(e, route, reqModel, now) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryForModelLocked(tried, now, route, reqModel)
	}

	// 排除落位分组：组间优先级 优先组 > 常规组 > 兜底组，取第一个非空组。
	//
	// 先扫一遍探"有没有任何账号被排除"，全都没配就直接走原路径——
	// 这既省掉一次分组分配（Pick 是每请求一次的热路径），也让
	// "没配排除的池行为与改造前逐字一致"成为代码层面的显然事实，而不是靠推理。
	hasExcluded := false
	for _, e := range cands {
		if e.selExcluded {
			hasExcluded = true
			break
		}
	}
	if hasExcluded {
		var tiers [3][]*entry
		for _, e := range cands {
			t := selectionTierOf(e)
			tiers[t] = append(tiers[t], e)
		}
		switch {
		case len(tiers[tierFirst]) > 0:
			// 优先组：不走策略排序，按 uid 升序取第一个（确定性）。
			// 与 lowest_credits 同理有意跳过 minPickGap——本组语义就是"最优先用它"，
			// 打散窗口会让它名不副实；并发上限仍由 maxInFlight 兜底。
			return p.pickByUIDOrderLocked(tiers[tierFirst], now)
		case len(tiers[tierNormal]) > 0:
			cands = tiers[tierNormal] // 常规组：按策略排序（下面的逻辑）
		default:
			// 只剩兜底组：常规组与优先组都空，此时才轮到"最后使用"的号。
			return p.pickByUIDOrderLocked(tiers[tierLast], now)
		}
	}
	// 常规组：按选号策略排序。三个"确定性集中"策略都不走加权抽签。
	// 见各自 *Locked 的注释说明为什么这里有意跳过 minPickGap。
	switch p.selectionMode {
	case SelectionLowestCredits:
		return p.pickLowestCreditsLocked(cands, now)
	case SelectionHighestCredits:
		return p.pickHighestCreditsLocked(cands, now)
	case SelectionCustomPriority:
		return p.pickCustomPriorityLocked(cands, now)
	}
	// top5 短名单按三因子权重降序截断（而非 credits 单纯降序）：否则闲置补偿 + 成功率
	// 根本进不了短名单决策，低 credits 但高成功率/久置的账号会永远排不进 top5。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// 权重只算一次：顶 5 截断要排序，若在 sort 比较器里现算 weightOf 会翻成 O(n log n) 次
	// 冗余浮点计算（46 账号约 500 次）。先做 O(n) 预计算，再按 (权重, uid) 排序。
	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, len(cands))
	for i, e := range cands {
		ws[i] = weighted{e: e, w: p.weightOf(e, maxCredits, now)}
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	if len(cands) > 5 {
		cands = cands[:5]
	}
	eligible := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，维持发散且不 starve 任一候选。
		oldest := []*entry{cands[0]}
		for _, c := range cands[1:] {
			if c.lastUsed.Before(oldest[0].lastUsed) {
				oldest = []*entry{c}
			} else if c.lastUsed.Equal(oldest[0].lastUsed) {
				oldest = append(oldest, c)
			}
		}
		// 同一时钟刻度内的时间戳可能完全相同，固定取 UID 最小者会形成热点。
		// 只在最早时间的平局候选中抽签，保留 LRU 优先级。
		e = p.pickWeighted(oldest)
	} else {
		e = p.pickWeighted(eligible) // eligible 保序 = top5 降序子集
	}
	e.lastUsed = time.Now()
	return e.a
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time) *auth.Auth {
	return p.pickEarliestExpiryForModelLocked(tried, now, "", "")
}

func (p *Pool) pickEarliestExpiryForModelLocked(tried map[string]bool, now time.Time, route, model string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if e.disabled || e.manualDisabled || p.floorBlocked(e, route, model, now) {
			continue // 停用的账号永不参与兜底（系统判定与人工停用两层并罚）
		}
		// 额度冻结号不参与兜底：余额为 0，选中也只是白撞一次 429/402。
		// 必须显式判断——冻结会清掉 until（不再有时间冷却），若该号另有未过期的
		// breakerUntil，expiry 落在熔断上就不为零，会绕过下面的零值过滤被捞出来。
		if e.frozen && (e.frozenUntil.IsZero() || now.Before(e.frozenUntil)) {
			continue
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 历史 hard 冷却号（未走冻结路径的旧状态）同理不参与兜底
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry uid=%s until=%s kind=%s", logfmt.UID8(best.a.UID), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（max=0 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读 p.maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	if p.maxInFlight <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(p.maxInFlight)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 三因子加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight + successRate × 3
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//	  - successRate = successCount/(successCount+errTotal)；无请求记录给 1.5（中性偏信任）
//
// credits 全 0 时仍按 idle+successRate 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry) *entry {
	now := time.Now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.weightOf(e, maxCredits, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的三因子权重。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3. 成功率 ×3。
	totalReq := e.successCount + e.errTotal
	if totalReq > 0 {
		w += float64(e.successCount) / float64(totalReq) * 3
	} else {
		w += 1.5 // 无请求记录 → 中性偏信任
	}
	return w
}

// 选号策略取值（对应 config 的 pool.selection_mode）。
const (
	// SelectionWeighted 三因子加权随机（默认）：打散热点，避免永远打同一个账号。
	SelectionWeighted = "weighted"
	// SelectionLowestCredits 最少余额优先：始终挑当前余额最少的健康账号，
	// 把它打光（余额归零 → 冻结/排除）后再顺位到下一个。
	// 目的：把流量**集中**在同一个账号上连续调用，最大化上游侧 prompt cache 命中
	// （上游缓存按账号维度隔离，跨号即冷启动）。
	SelectionLowestCredits = "lowest_credits"
	// SelectionHighestCredits 最高余额优先：始终挑当前余额最多的健康账号，
	// 与 SelectionLowestCredits 镜像相反——先吃厚号，把低余额号留到后面。
	// 适用：想让"快过期的号/临时号"先消耗，或故意保留低余额号做备用。
	// 同样是**确定性**选择（余额最大者，同余额按 uid 升序），不做随机抽签。
	SelectionHighestCredits = "highest_credits"
	// SelectionCustomPriority 自定义优先级：按账号级 selection_priority 降序
	// （数值大者优先），同优先级按 uid 升序。优先级为 0（默认）的账号彼此等价，
	// 于是"只给少数几个号设优先级"就能把它们排到最前，其余保持 uid 序。
	// 也是**确定性**选择：同优先级下结果可复现，便于把指定号钉在指定位置。
	SelectionCustomPriority = "custom_priority"
)

// pickLowestCreditsLocked 最少余额优先选号。调用方必须已持有 p.mu。
//
// 与加权路径的三点差异：
//  1. 不做随机抽签——同一候选集内选择结果确定（余额最小者），
//     确定性正是本策略的目的：连续请求稳定落在同一个账号上。
//  2. **有意跳过 minPickGap 防撞号窗口**。那个窗口的作用是"打散并发热点"，
//     与本策略"集中压号"的目标正好相反；并发上限仍由 maxInFlight 在
//     Acquire/Release 处兜底（默认 3），不会把单号打爆。
//  3. healthy / tried / inFlightFull 过滤与加权路径完全一致（已在 pick 内完成）。
//
// 排序规则（关键，曾因顺序错误导致真实缺陷）：
//
//  1. **未观测余额的账号优先**（creditsKnown=false）
//  2. 已观测且余额为正：余额小者优先
//  3. 已观测但余额 <=0（真·耗尽）：排最后
//
// 为什么未观测必须排最前：未观测意味着池里的 credits 是初始值 0，而 0 既可能
// 对应"真的耗尽"也可能是"从没问过"。若不优先探测，这类账号永远不被选中 →
// 永远刷不到自己的真实余额 → 被当成 <=0 永远沉底，形成**死锁**。
// 现场报告：6 个 ai 账号里多个未观测（真实额度远低于已观测的 350/207），
// 结果"永远用高额度号"——正是本死锁。多花一两次调用把余额探明，
// 换来此后全程正确的低额度优先，是划算的；也符合"优先用低额度"的总目标。
//
// 若候选全为"已观测且 <=0"，退化为按 uid 升序取最小者（保持确定性）。
func (p *Pool) pickLowestCreditsLocked(cands []*entry, now time.Time) *auth.Auth {
	best := cands[0]
	for _, e := range cands[1:] {
		if lessCredits(e, best) {
			best = e
		}
	}
	best.lastUsed = now
	return best.a
}

// lessCredits 报告 a 是否应比 b 优先选中。三档优先级（见 pickLowestCreditsLocked）：
//
//	未观测  >  已观测且正余额（小者优先）  >  已观测且 <=0
//
// 同档内：余额小者优先，再按 uid 升序（保证结果可复现）。
func lessCredits(a, b *entry) bool {
	// 1. 未观测者优先探测（打破"不选中就观测不到"的死锁）。
	if a.creditsKnown != b.creditsKnown {
		return !a.creditsKnown
	}
	// 2. 已观测：正余额优先于 <=0（<=0 是真的耗尽，探测已无意义）。
	if (a.credits > 0) != (b.credits > 0) {
		return a.credits > 0
	}
	// 3. 同为正（或同为非正）：余额小者优先；相等则 uid 升序。
	if a.credits != b.credits {
		return a.credits < b.credits
	}
	return a.a.UID < b.a.UID
}

// SetCredits 更新账号余额。

// 排除落位分组（账号级配置 selection_excluded / selection_placement 的派生结果）。
// 组间优先级固定为 tierFirst > tierNormal > tierLast，且这一顺序**不随策略变化**：
// 排除是"运维对账号的显式意志"，必须比策略排序更硬。
const (
	// tierFirst 优先组：被排除 + 落位「最优先使用」。有它时永远先选它。
	tierFirst = iota
	// tierNormal 常规组：未被排除的账号，按 pool.selection_mode 排序。
	tierNormal
	// tierLast 兜底组：被排除 + 落位「最后使用」。仅当优先组与常规组都空时才用。
	tierLast
)

// 排除落位取值（对应账号级配置 selection_placement）。
const (
	// PlacementFirst 最优先使用：该号排在所有未排除账号之前。
	PlacementFirst = "first"
	// PlacementLast 最后使用：该号排在所有未排除账号之后。
	PlacementLast = "last"
)

// selectionTierOf 由账号级配置推导其落位分组。
//
// 边界口径（重要，避免"配了但语义含糊"）：
//   - 未排除（selExcluded=false）：无论 placement 配了什么，一律常规组。
//     placement 只在排除时才有意义，不排除就该按策略正常参与竞争。
//   - 已排除 + placement="first"：优先组。
//   - 已排除 + 其它任何值（含空串、"last"、拼写错误）：兜底组。
//     空值按「最后使用」解释是有意选择：排除一个号最常见的意图是"别优先用它"，
//     而把空值当 first 会让一个手滑写入的排除配置把某号顶到最前（更危险的错向）。
func selectionTierOf(e *entry) int {
	if !e.selExcluded {
		return tierNormal
	}
	if e.selPlacement == PlacementFirst {
		return tierFirst
	}
	return tierLast
}

// pickByUIDOrderLocked 在排除组内按 uid 升序取第一个。调用方必须已持有 p.mu。
//
// 为什么排除组不走选号策略：这两组的存在意义就是"绕开策略的排序逻辑"。
// 若还让策略（尤其加权随机）在组内排序，"最优先使用"就会变成"大概率用它"，
// 运维钉不住号。确定性取 uid 最小者是本组唯一自洽的口径。
//
// 有意跳过 minPickGap 防撞号窗口：本组语义是"优先/最后用它"，打散窗口会让
// 语义名不副实；并发上限仍由 maxInFlight 在 Acquire/Release 处兜底。
func (p *Pool) pickByUIDOrderLocked(cands []*entry, now time.Time) *auth.Auth {
	best := cands[0]
	for _, e := range cands[1:] {
		if e.a.UID < best.a.UID {
			best = e
		}
	}
	best.lastUsed = now
	return best.a
}

// pickHighestCreditsLocked 最高余额优先选号。调用方必须已持有 p.mu。
//
// 与 pickLowestCreditsLocked 完全镜像，差异只在排序方向（lessCredits → greaterCredits）：
// 同样确定性、同样有意跳过 minPickGap、健康/在途过滤口径一致（已在 pick 内完成）。
// 三档排序见 greaterCredits。
func (p *Pool) pickHighestCreditsLocked(cands []*entry, now time.Time) *auth.Auth {
	best := cands[0]
	for _, e := range cands[1:] {
		if greaterCredits(e, best) {
			best = e
		}
	}
	best.lastUsed = now
	return best.a
}

// greaterCredits 报告 a 是否应比 b 优先选中。三档优先级（lessCredits 的镜像）：
//
//	未观测  >  已观测且正余额（**大者**优先）  >  已观测且 <=0
//
// 未观测仍排最前，理由与 lessCredits 完全一致：那是打破"不选中就观测不到"死锁的
// 必要条件，与"要高余额还是要低余额"无关。一个未观测号若不先探一次，
// 它在排序里永远是 0，也就永远沉底——两种策略下都是同一个死锁。
//
// 差异只在第二档：本策略要吃厚号，故余额大者优先。
// 同档内：余额大者优先，再按 uid 升序（保证结果可复现）。
func greaterCredits(a, b *entry) bool {
	// 1. 未观测者优先探测（与 lessCredits 同因：打破观测死锁）。
	if a.creditsKnown != b.creditsKnown {
		return !a.creditsKnown
	}
	// 2. 已观测：正余额优先于 <=0（<=0 是真的耗尽，探测已无意义）。
	if (a.credits > 0) != (b.credits > 0) {
		return a.credits > 0
	}
	// 3. 同为正（或同为非正）：余额**大者**优先；相等则 uid 升序。
	if a.credits != b.credits {
		return a.credits > b.credits
	}
	return a.a.UID < b.a.UID
}

// pickCustomPriorityLocked 自定义优先级选号。调用方必须已持有 p.mu。
//
// 与另外两个确定性策略同样跳过 minPickGap（运维要求"先走这个号"时，打散窗口
// 会让连续请求被推开，与意图冲突）；健康/在途过滤已在 pick 内完成。
// 排序规则见 greaterPriority。
func (p *Pool) pickCustomPriorityLocked(cands []*entry, now time.Time) *auth.Auth {
	best := cands[0]
	for _, e := range cands[1:] {
		if greaterPriority(e, best) {
			best = e
		}
	}
	best.lastUsed = now
	return best.a
}

// greaterPriority 报告 a 是否应比 b 优先选中。按自定义优先级降序：
//
//	优先级大者  >  优先级小者；相等按 uid 升序
//
// 与 credits 两个策略的关键差异：**不区分"未观测/已观测"**。优先级是运维手写的
// 意图值，不依赖任何上游观测——把未观测号排到前面会直接违背"我指定 A 号优先"，
// 而优先级策略本身也不存在那个观测死锁（排序键不是余额，无需靠选中去"刷"出来）。
//
// 默认优先级 0：未显式配置的账号彼此等价，退化为 uid 升序，行为与改造前一致。
// 负数优先级合法（表示"比默认更低"），故不做非负钳制。
func greaterPriority(a, b *entry) bool {
	if a.selPriority != b.selPriority {
		return a.selPriority > b.selPriority
	}
	return a.a.UID < b.a.UID
}
