// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/realm"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 仅首次迁移为可管理的 Key，后续忽略
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json
	Security  struct {
		StoreFile    string `json:"store_file"`
		SecureCookie bool   `json:"secure_cookie"`
	} `json:"security"`

	Server struct {
		// MaxBodyMB 聊天请求体大小上限（单位 MB，默认 8）。
		// 请求体超过该值直接返回 413 request_body_too_large，不再静默截断后喂给上游
		// （issue #41：截断的 JSON 让上游 unmarshal 报 unexpected EOF，网关却罚号）。
		// 0/负数视为非法 → normalize 回落默认并记录。
		MaxBodyMB int `json:"max_body_mb"`
	} `json:"server"`

	// Console 控制台（/admin）：内嵌单页 + 管理 API（凭证列表 / 积分 / 触发登录授权）。
	Console struct {
		// Enabled 是否挂载 /admin（默认 true）。关掉后相关路由不存在（404）。
		Enabled bool `json:"enabled"`
	} `json:"console"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	// Realms 各渠道的路由和维护配置；其中 api_key 仅供首次迁移。
	// 当前请求路由及渠道权限由管理页分发的 Key 策略决定。
	Realms map[string]RealmConfig `json:"realms"`

	// ModelPrefixes 模型名渠道前缀 → realm 的映射（覆盖/扩展内置
	// {workbuddy: ai, codebuddy: codebuddy}）：模型名写成 `codebuddy/deepseek-v4.1-flash`
	// 即显式指定走 codebuddy 路线，出站前剥离前缀（上游只看到裸模型名）。
	// 空 = 只用内置映射（两种前缀仍然可用）。
	ModelPrefixes map[string]string `json:"model_prefixes"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 覆盖（空 = 现状 `CLI/2.63.2 CodeBuddy/2.63.2`）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// issue #42 深挖：官网「使用端」列基于出站请求 UA 的服务端归因，官方 WorkBuddy
		// 桌面 UA 为 `WorkBuddy/<version>`。指纹净化考虑：默认值保持现状（可配而非改死），
		// 仅当用户显式配置才改写。
		UserAgent string `json:"user_agent"`
		// Realm 未标注 realm 的账号归属，同时作为未显式指定 realm 的请求默认路由。
		// "cn"（默认）= CodeBuddy CN，"ai" = WorkBuddy AI（www.workbuddy.ai），
		// "codebuddy" = CodeBuddy IDE 域（www.codebuddy.ai，ai 的路线别名）。
		Realm string `json:"realm"`
		// RealmOverrides 各 realm 的端点与出站指纹覆盖（空字段 = 内置档案）。
		// 指纹 fingerprint 取 "cli" / "workbuddy-desktop"；后者发
		// UA: WorkBuddy/<client_version> + X-Product: WorkBuddy + X-IDE-Type/Name/Version。
		RealmOverrides map[string]RealmOverride `json:"realm_overrides"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
		PromptCacheKey                bool `json:"prompt_cache_key"`
		RepairToolHistory             bool `json:"repair_tool_history"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 原样透传客户端请求体，网关不注入/不替换任何提示词；
		// custom = 用 File 指定的文本替换客户端 system/developer（File 为空则不注入，等价 passthrough）。
		Mode string `json:"mode"` // "passthrough" / "custom"
		// File 提示词文件路径；空 = 不注入任何提示词（网关不内置提示词）；
		// 路径非空但不可读 → 启动报错（fail fast）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		CreditFloor        float64 `json:"credit_floor"`
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// SelectionMode 选号策略（默认 "weighted"）：
		//   weighted        = 三因子加权随机（打散热点，各号均衡消耗）
		//   lowest_credits  = 最低额度优先（集中把一个号打光再换下一个，
		//                     使连续请求落在同一账号上，最大化上游 prompt cache 命中）
		//   highest_credits = 最高额度优先（先吃厚号，把低余额号留到后面）
		//   custom_priority = 自定义优先级（按账号级 selection_priority 降序）
		// 未知值回落 weighted（不因笔误静默改路由）。
		// 后三者都是"确定性集中"策略，会与 session_sticky 互斥（粘性自动停用）。
		SelectionMode string `json:"selection_mode"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// UsageLog 调用级使用日志（每请求一条：哪个凭证 + 消耗多少积分 + token/缓存）。
	UsageLog struct {
		// Enabled 总开关（默认 true）。关闭后不记录、不占内存、不落盘，
		// 也不会在请求出口额外查一次余额。
		Enabled bool `json:"enabled"`
		// File JSONL 落盘路径，默认 "./data/usage.jsonl"。
		// 空字符串 = 只保留内存（控制台仍可查最近记录，但重启即丢）。
		File string `json:"file"`
		// MemorySize 控制台可查的最近记录条数（环形缓冲），默认 500。
		MemorySize int `json:"memory_size"`
		// MaxSizeMB 单文件大小上限（MB），超过即轮转，默认 32。
		// 注意：**没有"不轮转"取值**——<=0 一律回落默认 32。
		// 早期版本允许 0 表示不轮转，但那正是日志爆炸的入口（沉默地无限增长），
		// 要放宽就设个大值（如 512），别留一个"关掉保险"的开关。
		MaxSizeMB int `json:"max_size_mb"`
		// MaxBackups 保留的历史备份份数（<file>.1 … <file>.N），默认 1。
		// 磁盘占用上限 = max_size_mb × (1 + max_backups)：
		//   0 = 不留备份（超限直接丢弃，占用恒为 1 个文件上限）
		//   1 = 留 1 份（默认；32MB 上限时最多 64MB）
		// 负值视为未配置 → 默认 1。
		MaxBackups int `json:"max_backups"`
		// RefreshBalance 是否允许在请求出口查询上游余额，默认 true。
		// 两个用途共用这个开关（都建立在"能查余额"之上）：
		//   1. 兜底：上游**没给** usage.credit 时，用"调用前余额 - 调用后余额"算消耗；
		//   2. 校正：单号校准（见 CalibrateInterval）拉回本地扣减的累积漂移。
		// 关闭 = 完全不做余额查询：积分只能靠上游 credit（缺失即记 null），
		// 且本地扣减永不校准（长期会漂移，故仅在明确知道上游不返回 credit 时才关）。
		// 该查询在响应写完后执行（defer 内），不占用客户端等待时间。
		RefreshBalance bool `json:"refresh_balance"`
		// CalibrateInterval 单号额度校准的最小间隔（Go duration 字符串，默认 "5m"）。
		// 背景：上游 usage.credit 给出精确消耗值，网关据此**本地即时扣减**，
		// 展示额度与选号排序当场反映消耗，不必等额度巡检。
		// 但本地扣减是累加近似（小数进位 + 上游口径假设），会缓慢漂移，
		// 故按此间隔对"刚被用过的那个号"做一次权威余额校准（单号刷新，非全量）。
		// 空 = 默认 5m；极小值（如 "1ns"）= 每次请求都校准（更准，但每请求多一次上游查询）。
		CalibrateInterval string `json:"calibrate_interval"`
	} `json:"usage_log"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	// CalibrateDur 由 UsageLog.CalibrateInterval 解析而来（见 normalize）。
	CalibrateDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Security.StoreFile = "./data/access.json"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.MaxBodyMB = 8 // 请求体上限默认 8MB
	// 排程段默认值由 internal/config 集中维护（cmd/server 与 cmd/activity 共用，
	// 消除 issue #49 的默认值漂移）。
	c.Schedule = config.DefaultSchedule()
	c.Upstream.TimeoutSeconds = 120
	c.Upstream.Realm = realm.CN // 默认产品线；ai 的账号/请求需显式标注
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	c.Features.SanitizeBlacklistFingerprints = true
	c.Features.PromptCacheKey = true
	c.Console.Enabled = true      // 控制台默认开（/admin）
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：网关不注入任何提示词（原样转发客户端 body）
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	// 使用日志默认开启（落盘到 data/usage.jsonl，保留最近 500 条，32MB 轮转）。
	c.UsageLog.Enabled = true
	c.UsageLog.File = "./data/usage.jsonl"
	c.UsageLog.MemorySize = 500
	c.UsageLog.MaxSizeMB = 32
	c.UsageLog.MaxBackups = 1
	c.UsageLog.RefreshBalance = true // 兜底：上游未返回 usage.credit 时用"调用前后余额差值"
	// 单号校准间隔默认 5m：本地扣减已足够准，校准只作防漂移保险丝。
	c.UsageLog.CalibrateInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_ACCESS_FILE"); v != "" {
		c.Security.StoreFile = v
	}
	if v := os.Getenv("WB2A_SECURE_COOKIE"); v != "" {
		c.Security.SecureCookie = v == "true" || v == "1"
	}
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.MaxBodyMB = n
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_REALM"); v != "" {
		c.Upstream.Realm = v
	}
	if v := os.Getenv("WB2A_CONSOLE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Console.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_CACHE_KEY"); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			c.Features.PromptCacheKey = enabled
		}
	}
	if v := os.Getenv("WB2A_REPAIR_TOOL_HISTORY"); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			c.Features.RepairToolHistory = enabled
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_USAGE_LOG"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.UsageLog.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_USAGE_LOG_FILE"); v != "" {
		c.UsageLog.File = v
	}
	if v := os.Getenv("WB2A_USAGE_LOG_MEMORY_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.UsageLog.MemorySize = n
		}
	}
	if v := os.Getenv("WB2A_USAGE_LOG_MAX_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.UsageLog.MaxSizeMB = n
		}
	}
	if v := os.Getenv("WB2A_USAGE_LOG_MAX_BACKUPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.UsageLog.MaxBackups = n
		}
	}
	if v := os.Getenv("WB2A_USAGE_LOG_REFRESH_BALANCE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.UsageLog.RefreshBalance = b
		}
	}
	if v := os.Getenv("WB2A_USAGE_LOG_CALIBRATE_INTERVAL"); v != "" {
		c.UsageLog.CalibrateInterval = v
	}
	if v := os.Getenv("WB2A_POOL_SELECTION_MODE"); v != "" {
		c.Pool.SelectionMode = v
	}
}

func (c *Config) normalize() error {
	if strings.TrimSpace(c.Security.StoreFile) == "" {
		c.Security.StoreFile = "./data/access.json"
	}
	if c.Pool.CreditFloor < 0 {
		return fmt.Errorf("pool.credit_floor must be nonnegative")
	}
	var err error
	// max_body_mb 非法（0/负数）直接报错：0 若被静默当成默认 8MB，用户以为"不限"，
	// 大请求又被静默 413——不如 fail fast 提示显式配大上限。
	if c.Server.MaxBodyMB <= 0 {
		return fmt.Errorf("server.max_body_mb: %d 非法（需为正整数，单位 MB）", c.Server.MaxBodyMB)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// 单号校准间隔：空 = 未配置 → 回落默认 5m（在下面 UsageLog 归一里解析）。
	// 与 RefreshSkew 不同，这里**不允许**表达"关闭校准"——校准是防漂移保险丝，
	// 关掉它本地扣减的误差就永远无法收敛。要"每次都校准"请显式写极小正值（如 "1ns"）。
	if c.UsageLog.CalibrateInterval != "" {
		if c.CalibrateDur, err = time.ParseDuration(c.UsageLog.CalibrateInterval); err != nil {
			return fmt.Errorf("usage_log.calibrate_interval: %w", err)
		}
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	// 使用日志归一：容量/上限的非正值一律回落默认——
	// 0 不再有"不轮转/不保留"的特殊含义（那正是日志爆炸的入口：
	// 配置写成 0 会被理解成"关掉限制"，而文件默默无限增长）。
	// 要"不留备份"请显式写 max_backups=0（它只影响占用上限，不解除轮转）。
	if c.UsageLog.MemorySize <= 0 {
		c.UsageLog.MemorySize = 500
	}
	if c.UsageLog.MaxSizeMB <= 0 {
		c.UsageLog.MaxSizeMB = 32
	}
	// max_backups 的 0 是显式意图（= 不留备份），只有负值回落默认 1。
	if c.UsageLog.MaxBackups < 0 {
		c.UsageLog.MaxBackups = 1
	}
	// 单号校准间隔兜底：空字符串（未配置）走默认 5m。
	// 注意 <=0 不在这里拦截——"1ns" 之类极小正值是合法的"每次校准"表达，
	// 而真正的 0 由 handler 层当默认处理，语义边界只在一处，避免两处打架。
	if c.CalibrateDur == 0 && c.UsageLog.CalibrateInterval == "" {
		c.CalibrateDur = 5 * time.Minute
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 排程段归一（空数组回落默认、ActivityReportCount 归一、小时范围校验）
	// 由 internal/config 统一实现，cmd/server 与 cmd/activity 共用同一份语义。
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	if err := c.normalizeRealms(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom 模式）。
//
// 缺省/空值 → passthrough：网关不注入任何提示词（原样透传客户端 body）；
// mode 非法（非 custom/passthrough）启动报错，避免静默回落到某一分支；
// custom 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 不注入任何文本（等价 passthrough）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// RealmConfig 单个 realm（或路线别名）的网关侧配置。
type RealmConfig struct {
	// APIKey 旧渠道 Key，仅在认证存储首次创建时导入；之后由管理页维护。
	APIKey string `json:"api_key"`
	// AuthRealm 凭证来源线（仅"路线别名"用；空 = 用 internal/realm 内置档案的默认值）。
	// 例：codebuddy 线复用 ai 的账号池 —— 配 realms.codebuddy 即启用该路线。
	AuthRealm string `json:"auth_realm"`
}

// RealmOverride 单个 realm 的上游端点/出站指纹覆盖（空字段 = 用 internal/realm 内置档案）。
type RealmOverride struct {
	ChatBase      string `json:"chat_base"`
	BillingBase   string `json:"billing_base"`
	Platform      string `json:"platform"`
	Origin        string `json:"origin"`
	Fingerprint   string `json:"fingerprint"` // cli | workbuddy-desktop
	ClientVersion string `json:"client_version"`
	UserAgent     string `json:"user_agent"`
}

// normalizeRealms 校验 realm 相关配置：未知 realm 名 / 非法指纹 / 非法凭证来源线
// 一律启动报错（fail fast，避免"配置写了但静默不生效"）；并把键归一成归一名
// （旧名 "ai" → "workbuddy"），让历史配置继续工作。
func (c *Config) normalizeRealms() error {
	if strings.TrimSpace(c.Upstream.Realm) == "" {
		c.Upstream.Realm = realm.CN
	}
	if !realm.Known(c.Upstream.Realm) {
		return fmt.Errorf("upstream.realm: %q 不是合法值（%v）", c.Upstream.Realm, realm.All())
	}
	c.Upstream.Realm = realm.Normalize(c.Upstream.Realm)
	for name, ov := range c.Upstream.RealmOverrides {
		if !realm.Known(name) {
			return fmt.Errorf("upstream.realm_overrides: 未知 realm %q（%v）", name, realm.All())
		}
		if strings.TrimSpace(ov.Fingerprint) != "" {
			if _, ok := realm.NormalizeFingerprint(ov.Fingerprint); !ok {
				return fmt.Errorf("upstream.realm_overrides.%s.fingerprint: %q 不是合法值（cli / workbuddy-desktop）", name, ov.Fingerprint)
			}
		}
	}

	// 键归一（同上）：upstream.realm_overrides 的 ai → workbuddy。
	if len(c.Upstream.RealmOverrides) > 0 {
		norm := make(map[string]RealmOverride, len(c.Upstream.RealmOverrides))
		for name, ov := range c.Upstream.RealmOverrides {
			norm[realm.Normalize(name)] = ov
		}
		c.Upstream.RealmOverrides = norm
	}
	for name, rc := range c.Realms {
		if !realm.Known(name) {
			return fmt.Errorf("realms: 未知 realm %q（%v）", name, realm.All())
		}
		// auth_realm 只对"路线别名"有意义：把该路线指向另一条线的账号池。
		if src := strings.TrimSpace(rc.AuthRealm); src != "" {
			if !realm.Known(src) {
				return fmt.Errorf("realms.%s.auth_realm: 未知 realm %q（%v）", name, src, realm.All())
			}
			rn, s := realm.Normalize(name), realm.Normalize(src)
			if rn == s {
				return fmt.Errorf("realms.%s.auth_realm: 不能等于自身（那是普通 realm，不是路线别名）", name)
			}
			if s == realm.CN {
				return fmt.Errorf("realms.%s.auth_realm: cn 不能作为来源线（cn ↔ 国际线同 token 跨线一律 401）", name)
			}
		}
	}

	// 键归一：旧名（"ai"）→ 归一名（"workbuddy"），后续一律按归一名查表，旧配置零改动继续工作。
	if len(c.Realms) > 0 {
		norm := make(map[string]RealmConfig, len(c.Realms))
		for name, rc := range c.Realms {
			norm[realm.Normalize(name)] = rc
		}
		c.Realms = norm
	}

	// model_prefixes：模型名渠道前缀 → realm（覆盖/扩展内置 {workbuddy: workbuddy, codebuddy: codebuddy}）。
	// 非法条目一律启动报错，避免"配置写了但不生效"的静默陷阱。
	for prefix, target := range c.ModelPrefixes {
		p := strings.TrimSpace(prefix)
		if p == "" {
			return fmt.Errorf("model_prefixes: 空的渠道前缀")
		}
		if strings.ContainsAny(p, "/:\\ \t") {
			return fmt.Errorf("model_prefixes.%s: 前缀不能包含分隔符 %q 或空白", prefix, realm.ChannelPrefixSeparator)
		}
		if !realm.Known(target) {
			return fmt.Errorf("model_prefixes.%s: 未知 realm %q（%v）", prefix, target, realm.All())
		}
	}
	c.ModelPrefixes = realm.NormalizeChannelPrefixes(c.ModelPrefixes)
	return nil
}

// RealmProfile 返回 realm 的内置档案叠加 RealmOverrides 后的结果（供 main 注入 upstream.Client）。
func (c *Config) RealmProfile(rn string) realm.Profile {
	rn = realm.Normalize(rn)
	p := realm.Defaults()[rn]
	if ov, ok := c.Upstream.RealmOverrides[rn]; ok {
		if ov.ChatBase != "" {
			p.ChatBase = ov.ChatBase
		}
		if ov.BillingBase != "" {
			p.BillingBase = ov.BillingBase
		}
		if ov.Platform != "" {
			p.Platform = ov.Platform
		}
		if ov.Origin != "" {
			p.Origin = ov.Origin
		}
		if fp, ok2 := realm.NormalizeFingerprint(ov.Fingerprint); ok2 {
			p.Fingerprint = fp
		}
		if ov.ClientVersion != "" {
			p.DesktopVersion = ov.ClientVersion
			p.CLIVersion = ov.ClientVersion
		}
	}
	return p
}

// RealmFingerprint realm 生效的指纹（未配置覆盖时为内置默认）。
func (c *Config) RealmFingerprint(rn string) realm.Fingerprint {
	rn = realm.Normalize(rn)
	if ov, ok := c.Upstream.RealmOverrides[rn]; ok {
		if fp, ok2 := realm.NormalizeFingerprint(ov.Fingerprint); ok2 {
			return fp
		}
	}
	return realm.Defaults()[realm.Normalize(rn)].Fingerprint
}

// RealmClientVersion realm 的客户端版本覆盖（空 = 用档案内置版本）。
func (c *Config) RealmClientVersion(rn string) string {
	rn = realm.Normalize(rn)
	if ov, ok := c.Upstream.RealmOverrides[rn]; ok {
		return ov.ClientVersion
	}
	return ""
}

// RealmUserAgent realm 的 UA 覆盖（空 = 按指纹解析）。
func (c *Config) RealmUserAgent(rn string) string {
	rn = realm.Normalize(rn)
	if ov, ok := c.Upstream.RealmOverrides[rn]; ok {
		return ov.UserAgent
	}
	return ""
}

// RealmAPIKey 返回旧渠道 Key，供历史嵌入兼容；生产请求由 Access 存储鉴权。
func (c *Config) RealmAPIKey(rn string) string {
	rn = realm.Normalize(rn)
	if rc, ok := c.Realms[rn]; ok {
		return rc.APIKey
	}
	return ""
}

// AuthRealmFor 该 realm 的凭证来源线：配置显式指定优先，其次内置档案（见 realm.AuthRealmOf）。
// 非别名线返回自身。
func (c *Config) AuthRealmFor(rn string) string {
	rn = realm.Normalize(rn)
	if rc, ok := c.Realms[rn]; ok {
		if src := strings.TrimSpace(rc.AuthRealm); src != "" {
			return realm.Normalize(src)
		}
	}
	return realm.AuthRealmOf(rn)
}

// RouteEnabled 该路线别名是否启用：realms 或 upstream.realm_overrides 里显式出现即启用。
//
// 内置档案预置的路线别名（目前只有 codebuddy → ai）默认**不启用**：什么都不配时
// 行为与改造前逐字一致（不多暴露一个路由，也不多注册一个 realm key）。
func (c *Config) RouteEnabled(rn string) bool {
	rn = realm.Normalize(rn)
	if !realm.IsRouteAlias(rn) {
		return false
	}
	if _, ok := c.Realms[rn]; ok {
		return true
	}
	if _, ok := c.Upstream.RealmOverrides[rn]; ok {
		return true
	}
	return false
}

// StateFileFor realm 的池状态文件：默认 realm 用 state_file，其余按 "<base>-<realm>.json" 派生，
// 使各池的积分/冷却/成败计数互不干扰（也便于单独备份或重置某条线）。
func (c *Config) StateFileFor(rn string) string {
	rn = realm.Normalize(rn)
	if c.StateFile == "" || rn == realm.Normalize(c.Upstream.Realm) {
		return c.StateFile
	}
	ext := filepath.Ext(c.StateFile)
	base := strings.TrimSuffix(c.StateFile, ext)
	return base + "-" + rn + ext
}

// LegacyStateFileFor realm 改名前（ai → workbuddy）的历史状态文件路径；无历史名时返回空。
//
// 仅用于启动期一次性迁移：新名状态文件不存在而历史文件存在时复制一份，
// 避免 realm 改名让积分 / 冷却 / 熔断计数凭空清零。cn 线不受影响（它一直是 state.json）。
func (c *Config) LegacyStateFileFor(rn string) string {
	if realm.Normalize(rn) != realm.WB || c.StateFile == "" {
		return ""
	}
	ext := filepath.Ext(c.StateFile)
	base := strings.TrimSuffix(c.StateFile, ext)
	return base + "-" + realm.AI + ext // realm.AI = "ai"：历史名
}
