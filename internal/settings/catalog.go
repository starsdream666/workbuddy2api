// Package settings exposes an explicit, non-secret subset of gateway configuration.
package settings

type Field struct {
	Key     string   `json:"key"`
	Group   string   `json:"group"`
	Label   string   `json:"label"`
	Help    string   `json:"help"`
	Kind    string   `json:"kind"`
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Options []string `json:"options,omitempty"`
	Env     string   `json:"env,omitempty"`
}

// OptionLabels 选项取值 → 界面文案（仅展示用，配置里存的仍是 Options 的英文取值）。
//
// 为什么不塞进 Field：Catalog() 用**位置字面量**构造上百个 Field，
// 给 Field 加字段就得逐行改全部条目（且以后每次加字段都要再来一遍）。
// 文案是纯展示信息、只对少数 select 字段有意义，独立成表后
// Catalog 保持不动，前端按 key 查表即可。
var optionLabels = map[string]map[string]string{
	"pool.selection_mode": {
		"weighted":        "随机调用（打散热点，各号均衡分配）",
		"lowest_credits":  "最低额度优先（集中打光一个号再换下一个）",
		"highest_credits": "最高额度优先（先吃厚号，低额度号留后）",
		"custom_priority": "自定义优先级（按账号优先级排序）",
	},
	"schedule.credit_watch_scope": {
		"frozen": "只巡检冻结账号",
		"all":    "巡检全部账号",
	},
}

// OptionLabels 返回全部 select 字段的选项文案表（供控制台把机器取值渲染成中文）。
// 返回值是深拷贝：调用方（settings 快照）不该能改到包级表。
func OptionLabels() map[string]map[string]string {
	out := make(map[string]map[string]string, len(optionLabels))
	for key, m := range optionLabels {
		cp := make(map[string]string, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out[key] = cp
	}
	return out
}

// Catalog is shared by server-side validation and the generated console form.
// Paths, credentials, upstream endpoints and console/authentication switches are
// deliberately excluded. Editing this allowlist changes the public settings API.
func Catalog() []Field {
	return []Field{
		{"server.max_body_mb", "请求与超时", "请求体上限（MB）", "超出上限返回 413；建议仅按客户端需要增加。", "integer", 1, 1024, nil, "WB2A_MAX_BODY_MB"},
		{"upstream.timeout_seconds", "请求与超时", "短请求超时（秒）", "用于刷新令牌、余额查询和模型列表等请求。", "integer", 1, 3600, nil, "WB2A_TIMEOUT_SECONDS"},
		{"upstream.header_timeout_seconds", "请求与超时", "响应头超时（秒）", "聊天流等待上游响应头的上限；0 表示跟随短请求超时。", "integer", 0, 3600, nil, "WB2A_HEADER_TIMEOUT_SECONDS"},
		{"upstream.idle_timeout_seconds", "请求与超时", "流空闲超时（秒）", "仅在流持续无数据时计时；0 使用默认 300 秒。", "integer", 0, 86400, nil, "WB2A_IDLE_TIMEOUT_SECONDS"},
		{"pool.selection_mode", "账号池", "选号策略", "随机调用打散热点；三种「优先」策略为确定性选择，会自动跳过会话保持。", "select", 0, 0, []string{"weighted", "lowest_credits", "highest_credits", "custom_priority"}, "WB2A_POOL_SELECTION_MODE"},
		{"pool.max_in_flight", "账号池", "单账号并发上限", "0 表示不限；较小的值可减少单账号过载。", "integer", 0, 10000, nil, ""},
		{"pool.credit_floor", "账号池", "积分保底", "0 关闭；余额达到保底后暂停选号。", "number", 0, 1000000000, nil, ""},
		{"pool.idle_weight_per_hour", "账号池", "每小时闲置补偿权重", "仅加权选号使用，需大于 0。", "number", 0.001, 10000, nil, ""},
		{"pool.idle_weight_max", "账号池", "闲置补偿权重上限", "限制闲置账号获得的额外权重，需大于 0。", "number", 0.001, 100000, nil, ""},
		{"cooldown.soft_rate", "冷却与熔断", "限流冷却基数", "时长格式，例如 10m、600s；连续限流按指数退避。", "duration", 0, 0, nil, "WB2A_SOFT_RATE"},
		{"cooldown.soft_rate_max", "冷却与熔断", "限流冷却上限", "不得小于限流冷却基数，例如 2h。", "duration", 0, 0, nil, "WB2A_SOFT_RATE_MAX"},
		{"pool.breaker_threshold", "冷却与熔断", "连续失败熔断阈值", "账号连续失败达到此次数后暂时退出选号。", "integer", 1, 1000, nil, ""},
		{"pool.breaker_cooldown", "冷却与熔断", "基础熔断时长", "时长格式，例如 30m。", "duration", 0, 0, nil, ""},
		{"pool.breaker_cooldown_max", "冷却与熔断", "熔断时长上限", "不得小于基础熔断时长，例如 6h。", "duration", 0, 0, nil, ""},
		{"session_sticky.enabled", "会话保持", "启用会话保持", "尽量将同一会话分配给同一账号；低余额优先策略会跳过会话保持。", "boolean", 0, 0, nil, ""},
		{"session_sticky.ttl", "会话保持", "会话绑定有效期", "时长格式，例如 30m。", "duration", 0, 0, nil, ""},
		{"session_sticky.gc_interval", "会话保持", "过期会话清理间隔", "时长格式，例如 5m。", "duration", 0, 0, nil, ""},
		{"features.sanitize_blacklist_fingerprints", "兼容性", "请求指纹脱敏", "清理请求体中的已知黑名单指纹。", "boolean", 0, 0, nil, "WB2A_SANITIZE_FINGERPRINTS"},
		{"features.prompt_cache_key", "兼容性", "提示词缓存标识", "为上游请求附加稳定缓存标识。", "boolean", 0, 0, nil, "WB2A_PROMPT_CACHE_KEY"},
		{"features.repair_tool_history", "兼容性", "工具调用历史修复", "修补不完整的工具历史；会改变发送给上游的工具消息。", "boolean", 0, 0, nil, "WB2A_REPAIR_TOOL_HISTORY"},
		{"schedule.checkin_enabled", "定时任务", "自动签到", "以下开关为全局配置；各渠道已有的任务覆盖仍然适用。", "boolean", 0, 0, nil, ""},
		{"schedule.checkin_hours", "定时任务", "签到时间（小时）", "使用服务器本地时区，逗号分隔 0–23，例如 9,21。", "hours", 0, 23, nil, ""},
		{"schedule.travel_enabled", "定时任务", "猫猫旅行", "定时领养、派出和领奖，仅适用于上游支持的渠道。", "boolean", 0, 0, nil, ""},
		{"schedule.travel_hours", "定时任务", "旅行时间（小时）", "使用服务器本地时区，逗号分隔 0–23。", "hours", 0, 23, nil, ""},
		{"schedule.activity_enabled", "定时任务", "活跃上报", "定时执行上游活跃上报。", "boolean", 0, 0, nil, ""},
		{"schedule.activity_hours", "定时任务", "活跃上报时间（小时）", "使用服务器本地时区，逗号分隔 0–23。", "hours", 0, 23, nil, ""},
		{"schedule.activity_report_count", "定时任务", "每账号活跃上报条数", "每次执行的条数；默认 5。", "integer", 1, 100, nil, ""},
		{"schedule.keepalive_enabled", "定时任务", "令牌保活", "定时刷新账号令牌。", "boolean", 0, 0, nil, ""},
		{"schedule.keepalive_hours", "定时任务", "保活时间（小时）", "使用服务器本地时区，逗号分隔 0–23。", "hours", 0, 23, nil, ""},
		{"schedule.credit_watch_enabled", "额度巡检", "启用额度巡检", "定期查询上游余额并处理冻结状态。", "boolean", 0, 0, nil, ""},
		{"schedule.credit_watch_interval", "额度巡检", "额度巡检间隔", "时长格式，例如 30m；频率越高，上游查询越多。", "duration", 0, 0, nil, ""},
		{"schedule.credit_watch_scope", "额度巡检", "额度巡检范围", "frozen 只巡检冻结账号；all 巡检全部账号。", "select", 0, 0, []string{"frozen", "all"}, ""},
		{"schedule.credit_freeze_max", "额度巡检", "冻结兜底时长", "到期后允许账号尝试一次，避免永久冻结，例如 72h。", "duration", 0, 0, nil, ""},
		{"usage_log.enabled", "用量日志", "记录调用用量", "关闭后不记录新日志，也不会在请求结束时额外查询余额。", "boolean", 0, 0, nil, "WB2A_USAGE_LOG"},
		{"usage_log.memory_size", "用量日志", "内存保留记录条数", "控制台可查询的最近记录容量。", "integer", 1, 100000, nil, "WB2A_USAGE_LOG_MEMORY_SIZE"},
		{"usage_log.max_size_mb", "用量日志", "单个日志文件上限（MB）", "超过上限轮转；不可设为 0。", "integer", 1, 4096, nil, "WB2A_USAGE_LOG_MAX_MB"},
		{"usage_log.max_backups", "用量日志", "日志备份份数", "0 不保留备份；总占用上限约为单文件上限 ×（1 + 份数）。", "integer", 0, 100, nil, "WB2A_USAGE_LOG_MAX_BACKUPS"},
		{"usage_log.refresh_balance", "用量日志", "调用后余额校准", "用于消耗积分的兜底计算和余额校正；关闭后缺失的消耗保持未知。", "boolean", 0, 0, nil, "WB2A_USAGE_LOG_REFRESH_BALANCE"},
		{"usage_log.calibrate_interval", "用量日志", "单账号余额校准间隔", "时长格式，例如 5m；仅在用量记录及余额校准均开启时生效。", "duration", 0, 0, nil, "WB2A_USAGE_LOG_CALIBRATE_INTERVAL"},
	}
}
