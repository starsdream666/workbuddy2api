// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因 issue #49：cmd/activity 一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与 cmd/server 主程序缺省 5 条漂移。把 Schedule 段 + 默认值/归一化抽到本包，
// 两个命令共用同一份定义，消除漂移源头。
package config

import (
	"fmt"
	"time"

	"workbuddy2api/internal/realm"
)

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 四类独立排程：签到 / 活跃上报 / 猫猫旅行 / token keepalive。
// cmd/server 与 cmd/activity 共用本结构，默认值由 DefaultSchedule 填充、
// 缺省归一由 Normalize 完成——两命令走同一份语义，不再各自复制。
type Schedule struct {
	CheckinHours   []int `json:"checkin_hours"`   // [9,21]
	TravelHours    []int `json:"travel_hours"`    // [9,21]
	ActivityHours  []int `json:"activity_hours"`  // [10]
	KeepaliveHours []int `json:"keepalive_hours"` // [22]
	// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled 显式禁用开关（缺省 true）。
	//
	// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
	//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
	//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
	//     则对老配置零影响，向后完全兼容。
	//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
	//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
	CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
	TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
	ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
	KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int `json:"activity_report_count"`
	// 猫猫旅行已退役 travel_interval_minutes：旅行现为独立排程（travel_hours）。
	// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。

	// CreditWatchEnabled 额度巡检开关（缺省 true）：定期查上游余额，
	// 余额为 0 → 冻结账号；余额恢复 → 解冻。
	CreditWatchEnabled bool `json:"credit_watch_enabled"`
	// CreditWatchInterval 巡检间隔（默认 "30m"）。只对冻结账号巡检时开销极小
	// （正常情况 0 个），全量范围则是「账号数 / 间隔」的上游查询频率。
	CreditWatchInterval string `json:"credit_watch_interval"`
	// CreditWatchScope 巡检范围：
	//   "frozen"（默认）只查冻结中的账号——只做「解冻」判断，开销随冻结数变化；
	//   "all" 全量账号——能主动发现余额为 0 的号并冻结，代价是每轮查全部账号。
	// 无论哪种范围，进程启动时都会全量巡检一次（让开局状态正确）。
	CreditWatchScope string `json:"credit_watch_scope"`
	// CreditFreezeMax 冻结兜底时长（默认 "72h"）：到期后即使巡检没确认恢复也放行一次，
	// 避免巡检被关闭/上游接口长期异常时账号被永久冻结。
	CreditFreezeMax string `json:"credit_freeze_max"`

	// RealmTasks 按产品线（realm）覆盖任务开关，用于"某条线没有该运营功能"的场景。
	//
	// 缺省（键缺席 / 为 null）= 该 realm 全部继承全局开关（与单线部署逐字一致）；
	// 只有确知不支持时才显式关掉。运行时若某路径返回 404（路径不存在），
	// scheduler 会自动标记 unsupported 并在本进程内跳过，不必写配置。
	RealmTasks map[string]RealmTaskFlags `json:"realm_tasks"`
}

// RealmTaskFlags 单个 realm 的任务开关；nil = 继承全局 *_enabled。
type RealmTaskFlags struct {
	Checkin   *bool `json:"checkin"`
	Travel    *bool `json:"travel"`
	Activity  *bool `json:"activity"`
	Keepalive *bool `json:"keepalive"`
}

// 任务名常量（与 RealmTaskEnabled 的 task 参数一一对应）。
const (
	TaskCheckin   = "checkin"
	TaskTravel    = "travel"
	TaskActivity  = "activity"
	TaskKeepalive = "keepalive"
)

// RealmTaskEnabled 报告某 realm 的某任务是否启用：未配置 = true（继承全局开关）。
// 未知 realm / 未知任务名一律返回 true（不阻碍既有行为；配置合法性由 Normalize 校验）。
//
// 查询键先按已知 realm 归一（旧名 "ai" → "workbuddy"），与 validateRealmTasks 的键归一同一口径：
// 少了这一步，旧配置 schedule.realm_tasks.ai 里的显式 false 会被静默忽略。
// 未知名保持原样，避免被归一成 cn 后误继承 cn 的开关。
func (s *Schedule) RealmTaskEnabled(realmName, task string) bool {
	if realm.Known(realmName) {
		realmName = realm.Normalize(realmName)
	}
	flags, ok := s.RealmTasks[realmName]
	if !ok {
		return true
	}
	var v *bool
	switch task {
	case TaskCheckin:
		v = flags.Checkin
	case TaskTravel:
		v = flags.Travel
	case TaskActivity:
		v = flags.Activity
	case TaskKeepalive:
		v = flags.Keepalive
	default:
		return true
	}
	if v == nil {
		return true
	}
	return *v
}

// DefaultSchedule 返回排程段的默认值。
//
// 开关「缺省 true」靠这里实现：调用方先取 DefaultSchedule 再用 json.Unmarshal 覆盖，
// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
// ActivityReportCount 默认 5：领猫前置需 5 次对话，5 连发刷满 chat_5。
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:        []int{9, 21},
		TravelHours:         []int{9, 21},
		ActivityHours:       []int{10},
		KeepaliveHours:       []int{22},
		CheckinEnabled:      true,
		TravelEnabled:       true,
		ActivityEnabled:     true,
		KeepaliveEnabled:    true,
		ActivityReportCount: 5, // 领猫前置需 5 次对话，5 连发刷满 chat_5
		CreditWatchEnabled:  true,
		CreditWatchInterval: "30m",
		CreditWatchScope:    "frozen",
		CreditFreezeMax:     "72h",
	}
}

// Normalize 归一化排程段：空数组/null 回落默认小时，ActivityReportCount 归一，校验小时范围。
//
// 空数组与 null 反序列化后覆盖掉 DefaultSchedule 的排程值（键缺席才保留），在此补齐。
// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
//
// ActivityReportCount：0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
// 注意这是「显式配 0 = 旧行为」的兼容语义，与 scheduler.New 的 <=0 → 1 归一一致；
// 「缺省 = 5」由 DefaultSchedule 在 Unmarshal 前置入，是另一条路径，两者不合并。
func (s *Schedule) Normalize() error {
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = []int{9, 21}
	}
	if len(s.TravelHours) == 0 {
		s.TravelHours = []int{9, 21}
	}
	if len(s.ActivityHours) == 0 {
		s.ActivityHours = []int{10}
	}
	if len(s.KeepaliveHours) == 0 {
		s.KeepaliveHours = []int{22}
	}
	// 0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if s.ActivityReportCount <= 0 {
		s.ActivityReportCount = 1
	}
	if err := s.validateRealmTasks(); err != nil {
		return err
	}
	if err := s.normalizeCreditWatch(); err != nil {
		return err
	}
	return s.validateHours()
}

// validateHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", s.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", s.ActivityHours); err != nil {
		return err
	}
	return checkHourRange("schedule.keepalive_hours", "keepalive_enabled", s.KeepaliveHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}

// validateRealmTasks 校验 realm_tasks 的 realm 名合法性（未知 realm 启动报错，
// 避免"配置写了但不生效"的静默陷阱）；并把键归一成归一名（旧名 "ai" → "workbuddy"）。
//
// 路线别名（codebuddy）不在其列：它与来源线共用账号池、不单独起调度器，
// 在这里配任务只会静默不生效 —— 直接报错并指向来源线。
func (s *Schedule) validateRealmTasks() error {
	for name := range s.RealmTasks {
		if !realm.Known(name) {
			return fmt.Errorf("schedule.realm_tasks: 未知 realm %q（%v）", name, realm.All())
		}
		if src := realm.AuthRealmOf(name); src != realm.Normalize(name) {
			return fmt.Errorf("schedule.realm_tasks.%s: %q 是路线别名（复用 %s 的账号池、不单独调度），请改为在 schedule.realm_tasks.%s 上配置",
				name, realm.Normalize(name), src, src)
		}
	}
	// 键归一：旧名 "ai" → "workbuddy"。RealmTaskEnabled 等按归一名查表，
	// 不归一的话旧配置（realm_tasks.ai）会静默失效。
	if len(s.RealmTasks) > 0 {
		norm := make(map[string]RealmTaskFlags, len(s.RealmTasks))
		for name, flags := range s.RealmTasks {
			norm[realm.Normalize(name)] = flags
		}
		s.RealmTasks = norm
	}
	return nil
}

// normalizeCreditWatch 归一额度巡检配置：空值回落默认，非法值启动报错（fail fast，
// 避免定时任务按一个没读懂的间隔静默跑）。
func (s *Schedule) normalizeCreditWatch() error {
	if s.CreditWatchInterval == "" {
		s.CreditWatchInterval = "30m"
	}
	if d, err := time.ParseDuration(s.CreditWatchInterval); err != nil || d <= 0 {
		return fmt.Errorf("schedule.credit_watch_interval: %q 非法（需为正时长，如 30m）", s.CreditWatchInterval)
	}
	if s.CreditWatchScope == "" {
		s.CreditWatchScope = "frozen"
	}
	if s.CreditWatchScope != "frozen" && s.CreditWatchScope != "all" {
		return fmt.Errorf("schedule.credit_watch_scope: %q 非法（frozen / all）", s.CreditWatchScope)
	}
	if s.CreditFreezeMax == "" {
		s.CreditFreezeMax = "72h"
	}
	if d, err := time.ParseDuration(s.CreditFreezeMax); err != nil || d <= 0 {
		return fmt.Errorf("schedule.credit_freeze_max: %q 非法（需为正时长，如 72h）", s.CreditFreezeMax)
	}
	return nil
}

// CreditWatchIntervalDur 解析后的巡检间隔。
func (s *Schedule) CreditWatchIntervalDur() time.Duration {
	d, _ := time.ParseDuration(s.CreditWatchInterval)
	if d <= 0 {
		return 30 * time.Minute
	}
	return d
}

// CreditFreezeMaxDur 解析后的冻结兜底时长。
func (s *Schedule) CreditFreezeMaxDur() time.Duration {
	d, _ := time.ParseDuration(s.CreditFreezeMax)
	if d <= 0 {
		return 72 * time.Hour
	}
	return d
}
