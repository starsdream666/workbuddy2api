// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// freezeMax 额度冻结的兜底时长（SetFreezeMax 注入；默认 defaultFreezeMax）。
	freezeMax time.Duration
	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// selectionMode 选号策略（SetSelectionMode 注入；默认 SelectionWeighted）。
	// weighted = 三因子加权随机（打散热点）；lowest_credits = 最少余额优先
	// （把流量压在同一个账号上连续打光额度，最大化上游 prompt cache 命中）；
	// highest_credits = 最高余额优先（镜像，先吃厚号）；custom_priority = 按账号级
	// 自定义优先级降序（把指定号钉在指定位置）。取值与语义见 pick.go。
	selectionMode string
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	creditFloor float64
	modelRates  map[string]modelPrice
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// notify 状态变化通知（nil = 不通知，如测试或未接控制台）。
	//
	// 用 atomic.Pointer 而非普通字段 + 锁，原因是一个**会导致进程死锁**的陷阱：
	// 大量状态变更（Cooldown/Reconcile/Disable/NoteError…）本身就在 p.mu.Lock()
	// 之内，而 Go 的 RWMutex 不可重入——在持写锁时再去 RLock 读该字段会永久死锁。
	// 原子指针让读取完全无锁，于是"持锁中通知"与"无锁路径通知"都安全。
	//
	// 存成 interface 而非 *eventbus.Bus：pool 是底层包，不该知道"控制台/SSE"
	// 这类上层概念；测试注入 fake 也更容易。
	//
	// 调用约定（热路径契约）：notify 永不阻塞。eventbus.Notify 满足这一点
	// （非阻塞投递 + 独立广播 goroutine），见 eventbus 包注释。
	notify atomic.Pointer[notifierBox]
}

// notifierBox 包一层 interface：atomic.Pointer 需要具体类型指针。
type notifierBox struct{ n ChangeNotifier }

// ChangeNotifier 状态变化通知口（由 *eventbus.Bus 实现）。
//
// 只暴露一个方法：pool 不关心有没有订阅者、有几个、怎么广播。
type ChangeNotifier interface {
	Notify()
}

// SetChangeNotifier 注入状态变化通知器（main 接线；nil = 关闭通知）。
func (p *Pool) SetChangeNotifier(n ChangeNotifier) {
	if n == nil {
		p.notify.Store(nil)
		return
	}
	p.notify.Store(&notifierBox{n: n})
}

// notifyChange 报告状态有变。无锁、永不阻塞、可在持锁中安全调用。
func (p *Pool) notifyChange() {
	if b := p.notify.Load(); b != nil && b.n != nil {
		b.n.Notify()
	}
}

// markDirty 标记"内存有变更待落盘"，并顺带通知控制台。
//
// 把两件事合成一个入口是有意的：它们**语义完全重合**——凡是需要落盘的状态变更，
// 也正是控制台该立刻看到的变化（额度/冷却/冻结/禁用/在途计数…）。
// 分散写 p.dirty.Store(true) 的地方有 14 处，逐个补通知必然遗漏；
// 统一走这里，新增状态变更只要照抄既有写法就自动获得实时推送。
//
// 注意：调用方通常**持有写锁**，故本方法内不得再取任何锁
// （notifyChange 走原子指针读取，满足这一约束）。
func (p *Pool) markDirty() {
	p.dirty.Store(true)
	p.notifyChange()
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetSelectionMode 注入选号策略（main 从 config 解析后调用）。
// 未知/空值回落 SelectionWeighted —— 保持既有三因子加权行为，
// 不因配置笔误静默改变路由策略。
func (p *Pool) SetSelectionMode(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch mode {
	case SelectionLowestCredits, SelectionHighestCredits, SelectionCustomPriority:
		p.selectionMode = mode
	default:
		p.selectionMode = SelectionWeighted
	}
}

// SelectionMode 返回当前选号策略（weighted / lowest_credits / highest_credits /
// custom_priority 之一）。
// handler 用它判断是否需要跳过会话粘性——确定性策略与粘性目标相反（粘性按会话散号，
// 确定性策略集中压号），同时生效会让策略失效（见 IsDeterministicSelection）。
func (p *Pool) SelectionMode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.selectionMode == "" {
		return SelectionWeighted
	}
	return p.selectionMode
}

// IsDeterministicSelection 报告该策略是否为"确定性集中"策略：
// lowest_credits / highest_credits / custom_priority 三者都不做随机抽签，
// 目的是让连续请求稳定落在同一账号（或同一优先序）上。
//
// 为什么单独开一个判定口：会话粘性（按会话 hash 把请求散到不同账号）与它们
// 目标相反，同时生效会让选号策略静默失效——粘性先按 hash 定号，选号根本轮不到。
// 三者的这个冲突是同一条，故共用一处口径，新增确定性策略时只改这里。
func IsDeterministicSelection(mode string) bool {
	switch mode {
	case SelectionLowestCredits, SelectionHighestCredits, SelectionCustomPriority:
		return true
	}
	return false
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	limit := p.maxInFlight
	p.mu.RUnlock()
	if !ok {
		return false
	}
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		p.notifyChange()
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			// 只在计数**真的**加了才通知：被拒（满额）不改变状态，无需唤醒控制台。
			p.notifyChange()
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			// 同上：只有真的减了才通知（幂等分支的 no-op 不唤醒）。
			p.notifyChange()
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// startFlusher 每 flushInterval 检查 dirty 标志，有变更则 saveLocked 落盘。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// Remove 立即剔除指定账号并持久化，不重建其他账号的凭证或运行状态。
// 已在途请求可自行结束，后续请求不会再选中该账号。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byUID[uid]; ok {
		delete(p.byUID, uid)
		p.saveLocked()
	}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
