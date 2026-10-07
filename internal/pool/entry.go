// Package pool 账号池：单一状态机（健康/冷却/熔断）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
package pool

import (
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 冷却到次日 04:00（等签到恢复）
	CoolSoft                 // 429 → 短冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Credits  int64  `json:"credits"`
	// CreditsExact 含小数扣减的**真实可用额度** = credits - creditFrac。
	//
	// 为什么需要这个独立字段：credits 是整数余额，而本地即时扣减会把小数消耗
	// （如 10.85）攒在 creditFrac 里、满 1 才落到 credits 上（见 deduct.go 的精度说明）。
	// 于是"还剩多少"的真实值是二者相减——只报 credits 会**多报**最多 1 分。
	// 上游余额本身只有整数，这个小数完全来自本地扣减，故仅在 creditsKnown 时有意义。
	CreditsExact float64 `json:"credits_exact"`
	// CreditsKnown 余额是否被上游真实观测过。false 时 Credits 的 0 表示"未知"
	// 而非"已耗尽"——调用日志做"消耗 = 调用前 - 调用后"差值时必须据此区分。
	CreditsKnown   bool      `json:"credits_known"`
	Cooling        bool      `json:"cooling"`
	CoolKind       string    `json:"cool_kind,omitempty"`
	CoolRemaining  int64     `json:"cool_remaining_sec,omitempty"`
	Until          time.Time `json:"until,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	SoftStreak     int       `json:"soft_streak,omitempty"` // 连续软冷却次数（指数退避指数，见 entry.softStreak）
	Disabled       bool      `json:"disabled"`
	DisabledReason string    `json:"disabled_reason,omitempty"` // 仅 disabled 账号：禁用原因（运维可见）
	// ManualDisabled 人工停用（控制台开关，持久化）。与 Disabled 正交：前者是运维主动停用，
	// 后者是系统判定（12153 连续失败）。两者可同时为真，任一为真即不参与选号。
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"` // 仅手动停用账号：停用原因（运维可见）
	// SelectionExcluded / SelectionPlacement / SelectionPriority 账号级选号配置
	// （控制台展示与编辑用；语义见 selection_account.go 与 pick.go 的分组口径）。
	SelectionExcluded  bool   `json:"selection_excluded"`
	SelectionPlacement string `json:"selection_placement,omitempty"`
	SelectionPriority  int    `json:"selection_priority"`
	// Frozen 额度冻结：上游余额为 0 时冻结，**直到额度探测确认恢复才解冻**。
	// 与 disabled（session 死，需人工重登）语义不同——冻结是自动可逆的。
	Frozen          bool      `json:"frozen,omitempty"`
	FrozenReason    string    `json:"frozen_reason,omitempty"`
	SuccessCount    int64     `json:"success_count,omitempty"`
	ErrTotal        int64     `json:"err_total,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`
	// 运行态（不持久化）：在途请求数 + 熔断器状态。
	InFlight     int       `json:"in_flight"`
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}
type entry struct {
	a       *auth.Auth
	credits int64
	// creditsKnown 该账号余额是否被上游真实观测过（SetCredits / ReconcileCredits /
	// ReenableIfCredits 任一写入即置真）。未观测时 credits 的 0 是"未知"而非"耗尽"，
	// 做"本次消耗 = 调用前余额 - 调用后余额"差值计算时必须区分，
	// 否则会把"0 → 3"这种从未观测过的账号算成 -3 的消耗。
	creditsKnown bool
	// creditFrac 本地扣减累积的小数部分（< 1）。
	// 上游消耗是小数（如 10.85）而 credits 是整数，靠它满 1 进位，避免长期丢精度。
	// 运行态语义（不持久化）：重启后按最近一次权威余额重建，误差无累积意义。
	creditFrac float64
	// lastCalib 最近一次"单号额度校准"的时刻（见 CalibrateDue）。
	// 运行态语义（不持久化）：重启后首个请求即重新校准，无需跨重启记忆。
	lastCalib    time.Time
	modelCharges map[string]modelPrice
	successCount int64     // 累计成功
	errTotal     int64     // 累计错误（供成功率权重 successRate = successCount/(successCount+errTotal)，不清零）
	lastErr      time.Time // 最近一次错误时间
	lastSuccess  time.Time // 最近一次成功时间
	coolKind     CoolKind
	until        time.Time // 冷却截止（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）
	disabled     bool
	reason       string
	// manualDisabled 控制台人工停用（持久化）：停用有两层，与系统判定正交。
	//   - disabled：系统层，由连续 12153（NoteSessionDead）/ refresh 失败写入；
	//   - manualDisabled：人工层，只由控制台启停接口（SetManualDisabled）写入。
	// healthy() 认为任一生效即不可选；人工启用只摘本层，不复活被系统判死的号——
	// 系统层的清除仍只有重新登录与 ReviveDisabled 两条路径。
	manualDisabled bool
	manualReason   string
	// ── 选号策略的按账号配置（控制台写入，随 state.json 持久化）──
	//
	// selExcluded 把该账号从选号策略的**常规分组**里移出：它不再与其余账号竞争，
	// 而是按 selPlacement 落位到「优先组」或「兜底组」（分组口径见 pick.go）。
	selExcluded bool
	// selPlacement 排除后的落位：PlacementFirst（最优先使用）/ PlacementLast（最后使用）。
	// 仅在 selExcluded 为真时有意义；空值按 PlacementLast 解释（见 selectionTierOf）。
	selPlacement string
	// selPriority 自定义优先级（仅 SelectionCustomPriority 策略使用）：数值大者优先。
	selPriority int
	// frozen 额度冻结（余额耗尽）：与 until 正交——until 是"时间到了就解冻"，
	// frozen 是"必须探测到额度恢复才解冻"。没有这一层时，余额耗尽的账号在软冷却
	// 到期后会被反复选中、反复撞 429（白刷上游，还把成功率权重拖低）。
	frozen       bool
	frozenReason string
	// frozenUntil 冻结兜底截止：到期后即使探测没确认恢复也放行一次，避免探测任务
	// 被关闭/上游接口长期异常时账号被永久冻结（宁可试一次撞 429，也不要静默失效）。
	frozenUntil time.Time
	lastUsed    time.Time // 最近被选中时刻（防并发撞号）
	// breakerUntil / fails / retryCount 为熔断器运行态（不持久化）。
	// fails 是唯一的"连续失败"计数器：任何错误喂入，达到 breakerThreshold 触发熔断（指数退避），
	// 跨入口累计，成功/熔断/统一复活时清零（保留 retryCount 驱动退避指数）。
	breakerUntil time.Time // 熔断截止（指数退避）
	fails        int       // 连续失败计数（熔断用，唯一权威）
	retryCount   int       // 已熔断次数（指数退避的指数）
	// softStreak 连续软冷却次数（CoolSoft），独立于熔断器 fails 的**冷却域**计数器：
	// fails 会被熔断触发清零、且被 hard 冷却与 NoteError 污染，无法表达"连续软限流"。
	// 重置点只有两处（都是账号被证明恢复的时刻）：NoteSuccess、reviveCoolingLocked。
	// 持久化（stateAccount.SoftStreak）：重启后软限流仍在退避，不因重启回到基数。
	softStreak int
	// softRateModel 触发 6004 模型级限流时的模型名（issue #31 模型豁免）。
	// 仅当冷却由「带解析时间的 6004」触发时记录；空 = 普通软冷却（不豁免）。
	// 运行态语义（不持久化）：重启清零，退化为现状。
	softRateModel string
	// sessionDeadFails 连续 12153（ErrSessionDead）计数。12153 在真实环境会被临时性触发
	// （网络抖动/上游闪断/refresh 竞态），一次失败就永久禁用太粗暴——连续达到阈值才判死。
	// 运行态语义（不持久化，与 inFlight 同语义）：重启清零可接受——重启后首个 keepalive
	// 成功即清计数，误判号不会因重启前的历史累积被继续追杀。
	sessionDeadFails int
	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64
}

// healthy 报告账号当前是否可选（未被任一层停用、未处于任一冷却/熔断期）。
func (e *entry) healthy(now time.Time) bool {
	// 停用两层正交（系统判定 / 人工停用），任一生效即不可选。
	if e.disabled || e.manualDisabled {
		return false
	}
	// 额度冻结优先于一切时间判断：余额为 0 时"等时间"没有意义（到点也不会自己有钱），
	// 必须等额度探测确认恢复（签到/套餐周期刷新）才解冻；frozenUntil 只是安全网。
	if e.frozen && (e.frozenUntil.IsZero() || now.Before(e.frozenUntil)) {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	return true
}

// healthyForModel 报告账号对指定 model 是否可选（含模型级豁免）：
// 冷却为由 6004 触发的**模型级**软冷却（softRateModel 非空）且请求模型不同
// （softRateModel != reqModel）时，跳过冷却判定——该模型限流不代表账号在其他
// 模型下不可用（issue #31）。空 reqModel / 未记录模型 / 同模型 → 与 healthy 一致。
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	if !e.healthy(now) && reqModel != "" && e.softRateModel != "" &&
		e.coolKind == CoolSoft && e.softRateModel != reqModel {
		// 非 healthy 但属于可豁免场景：仍受两层停用/breakerUntil 约束。
		return !e.disabled && !e.manualDisabled && e.breakerUntil.IsZero()
	}
	return e.healthy(now)
}

// expiry 返回账号当前仍在生效的最近冷却/熔断截止时间（两个截止取较早者）；不在冷却期返回零值。
// 供全冷却兜底选取"最早到期"账号用。
func (e *entry) expiry(now time.Time) time.Time {
	var t time.Time
	if !e.until.IsZero() && now.Before(e.until) {
		t = e.until
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if t.IsZero() || e.breakerUntil.Before(t) {
			t = e.breakerUntil
		}
	}
	return t
}

// fallbackKind 报告兜底账号属于哪一类冷却（soft：即时软冷却；breaker：熔断期）。
// 只对参与兜底的账号调用（CoolHard 已被 pickEarliestExpiryLocked 排除）。判定口径：
// 若熔断截止是当前生效的最近截止（含"仅有熔断无软冷却"），记为 breaker；否则记为 soft。
func (e *entry) fallbackKind(now time.Time) string {
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if e.until.IsZero() || !now.Before(e.until) || e.breakerUntil.Before(e.until) {
			return "breaker"
		}
	}
	return "soft"
}

// stateAccount 单个账号的持久化状态（JSON tag 全小写下划线，向后兼容：缺字段零值）。
type stateAccount struct {
	Credits  int64 `json:"credits"`
	Disabled bool  `json:"disabled"`
	// CreditsKnown 余额是否被上游真实观测过（旧文件缺此字段 → 见 applyAccountsLocked 的兼容推断）。
	CreditsKnown bool   `json:"credits_known,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// ManualDisabled 人工停用层。旧 state.json 缺此字段 → false（该号正常参与轮转，向后兼容）。
	ManualDisabled bool   `json:"manual_disabled,omitempty"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// SelectionExcluded / SelectionPlacement / SelectionPriority 选号策略的按账号配置。
	// 旧 state.json 缺这些字段 → 零值（不排除、优先级 0），该号行为与改造前完全一致（向后兼容）。
	SelectionExcluded  bool      `json:"selection_excluded,omitempty"`
	SelectionPlacement string    `json:"selection_placement,omitempty"`
	SelectionPriority  int       `json:"selection_priority,omitempty"`
	Until              time.Time `json:"until,omitempty"`
	CoolKind           CoolKind  `json:"cool_kind"`
	SuccessCount       int64     `json:"success_count,omitempty"`
	// err_total 累计错误计数。旧版 err_count（连续错误）仍可读：加载时映射到 err_total，
	// 仅作一次性迁移，不再回写 err_count。
	ErrTotal    int64     `json:"err_total,omitempty"`
	ErrCount    int       `json:"err_count,omitempty"` // 兼容旧文件的迁移源，仅读取
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr     time.Time `json:"last_err,omitempty"`
	// SoftStreak 连续软冷却次数（软退避指数）。旧 state.json 缺此字段 → 零值，
	// 退避从基数重新开始（向后兼容）。
	SoftStreak int `json:"soft_streak,omitempty"`
	// CreditFrozen 额度冻结。旧 state.json 缺此字段 → false（该号正常参与轮转，向后兼容）。
	CreditFrozen       bool      `json:"credit_frozen,omitempty"`
	CreditFrozenReason string    `json:"credit_frozen_reason,omitempty"`
	CreditFrozenUntil  time.Time `json:"credit_frozen_until,omitempty"`
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval 后台落盘周期。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax 软冷却指数退避的默认封顶：softRateMax 未注入（<=0）时按此值算，
// 避免测试/裸用池时退避无上限。
const defaultSoftRateMax = 2 * time.Hour

// sessionDeadThreshold 连续 ErrSessionDead（12153）达到该次数才永久禁用。
// 12153 会被临时性触发（网络抖动/上游闪断/refresh 竞态），一次失败即禁用的旧行为
// 会误杀健康账号（P0-1：13 个 disabled 号全是误判）。3 次连续才判死：容忍偶发抖动，
// 又不会让真正的死 session 留在池里反复被选中。
const sessionDeadThreshold = 3

// sessionDeadReason 12153 判定为 session 死亡时的持久化 reason。
const sessionDeadReason = "12153 session dead"

// SessionDeadThreshold 暴露连续 12153 的禁用阈值（供 scheduler 日志/运维文档引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

// softStreakShiftMax 软冷却退避的最大左移位数（防 1<<streak 溢出成负数/零）。
// 无论 streak 累积多少，封顶逻辑总会先生效，此值只是溢出兜底。
const softStreakShiftMax = 16

// StoreSnapshotter 池状态快照镜像的最小接口（redisstore.Store 满足；Noop 空实现安全）。
// 与本地 state.json 并存，作启动恢复备份：快照比本地新才采用，否则本地优先。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
