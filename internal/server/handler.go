// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/access"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/settings"
	"workbuddy2api/internal/taskqueue"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usagelog"
)

// Config handler 依赖。
type Config struct {
	Access       *access.Store // Production authentication; nil retains legacy embedding compatibility.
	SecureCookie bool
	Settings     *settings.Store
	Tasks        *taskqueue.Queue
	Maintenance  map[string]func(context.Context, string, func(taskqueue.Progress)) error
	Pool         *pool.Pool // 默认 realm 的池（兼容单线部署；Pools 未配置时全部走它）
	Upstream     *upstream.Client
	APIKey       string // 空 = 不鉴权
	// Pools 各主干线的独立账号池（"cn" / "ai"）。按 realm 分区：请求只在本 realm 池里轮转，
	// **不跨池回退**（同 token 跨线必被上游 401，跨池等于必然失败）。
	// 路线别名（codebuddy）在这里指向来源线（ai）的**同一个池对象**：账号/积分/冷却/在途共享，
	// 只有出站 base 与归因按别名线档案解析（见 upstream.Client.Route / Handler.upstreamFor）。
	// 缺省时回落 Pool。
	Pools               map[string]*pool.Pool
	PoolsSnapshot       func() map[string]*pool.Pool
	MaintenanceSnapshot func() map[string]func(context.Context, string, func(taskqueue.Progress)) error
	// RealmKeys 各 realm 的网关鉴权密钥：Bearer 命中哪个 realm 的 key，请求就走哪个池。
	// ChannelPrefixes 模型名渠道前缀 → realm 的映射（覆盖/扩展内置
	// {workbuddy: ai, codebuddy: codebuddy}）。例：`codebuddy/deepseek-v4.1-flash`
	// 走 codebuddy 路线，出站前剥离前缀。空 = 只用内置映射。
	ChannelPrefixes map[string]string
	// 未列出的 realm 用 APIKey（全局键 → DefaultRealm）。
	RealmKeys map[string]string
	// DefaultRealm 未显式指定 realm 的请求归属（X-Realm 头缺失且 Bearer 不匹配 RealmKeys 时）。
	DefaultRealm string
	// ConsoleEnabled 是否挂载控制台（GET /admin 与 /admin/api/*）。
	ConsoleEnabled bool
	// ReloadAuths 重扫凭证目录并对齐各 realm 池（控制台登录成功/手动重载时调用）。
	// nil = 不提供该能力（控制台对应接口返回 501）。
	ReloadAuths func() (map[string]int, error)
	// AuthDir 凭证目录（控制台登录成功后落盘位置；空 = ./auths）。
	AuthDir string
	// MaxRotate 单请求最多换号次数，默认 3。
	MaxRotate int
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string
	// UsageLog 调用级使用日志（nil = 关闭）。开启时每个请求出口落一条，
	// 并在响应写完后向 billing 查一次余额以计算本次消耗。
	UsageLog *usagelog.Logger
	// Events 状态变化广播总线（nil = 不推送，控制台退回定频轮询）。
	//
	// 由 *eventbus.Bus 注入；用接口而非具体类型，让 server 包不依赖该实现，
	// 测试可注入极简 fake（只需实现 Subscribe）。
	// handler 只做两件事：把 /admin/api/events 挂上、把订阅通道交给 SSE 循环。
	Events EventSource
	// Notify 状态变化通知器（nil = 不通知）。与 Events 是**同一个总线**，但职责相反：
	// Events 供 SSE 端点订阅，Notify 供本包内那些不在池路径上的变化（如用量日志写入）
	// 主动喊一声——池的状态变更由 pool.SetChangeNotifier 直接通知，不经这里。
	// 分成两个字段是为了让测试 fake 只需实现 Subscribe。
	Notify StateNotifier
	// RefreshBalanceAfter 刷新某账号余额并返回新值（用于计算消耗差值）。
	// 第一个参数是**该请求实际使用的那个池**（handler 已解析好）——刻意不传 uid
	// 让回调自己去查池：避免回调遍历 Pools 映射，而该映射会被 reloadAuths
	// 在运行时写入（并发迭代 + 写 map 是 Go 的致命错误，进程直接死）。
	// nil = 不做（积分字段一律记 null）。
	// CalibrateInterval 单号额度校准的最小间隔（0 = 每次请求都校准）。
	// 用上游绝对余额校正本地扣减的累积漂移；只刷新"刚用过的那个号"而非全量。
	// 生产默认 5m（见 NewHandler）：连续调用同一账号时不必每次都多打一次余额查询。
	CalibrateInterval   time.Duration
	RefreshBalanceAfter func(pl *pool.Pool, uid string) (int64, error)
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	runtime atomic.Pointer[RuntimeConfig]
	logins  loginAttempts
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// admin 控制台运行时状态（余额刷新时间戳 / 待完成授权会话）。
	admin *adminState
	// snap SSE 推送负载缓存（见 admin_events.go 的 sseFrame）。
	snap snapshotCache
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.CalibrateInterval <= 0 {
		// 默认 5 分钟：本地扣减已把余额维护得足够准，校准只是防漂移保险丝。
		// 注意这里**没有**"关闭校准"的取值：<=0 一律回落默认。
		// 想每次都拉权威值（更准但每请求多一次上游查询）请显式设一个极小正值（如 1ns）。
		cfg.CalibrateInterval = 5 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "passthrough" // 缺省 passthrough：网关不注入任何提示词
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), admin: newAdminState()}
	h.logins = loginAttempts{windows: map[string]loginWindow{}, gate: make(chan struct{}, 4)}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /v1/messages", h.withMessagesAuth(h.messages))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// 媒体类端点（鉴权/分流与 chat 一致；响应原样透传上游 data，见 media.go）。
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.imagesGenerations))
	h.mux.HandleFunc("POST /v1/images/edits", h.withAuth(h.imagesEdits))
	h.mux.HandleFunc("POST /v1/videos/generations", h.withAuth(h.videosGenerations))
	h.mux.HandleFunc("POST /v1/videos/tasks", h.withAuth(h.videosTasks))
	if cfg.Access != nil {
		h.mux.HandleFunc("GET /status", h.withAuth(h.keyStatus))
	} else {
		h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	}
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.ConsoleEnabled {
		h.registerAdmin()
		if cfg.Access != nil {
			h.registerAccess()
		}
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
		setAdminSecurityHeaders(w.Header())
		w.Header().Set("Cache-Control", "no-store")
	}
	h.mux.ServeHTTP(w, r)
}

// apiKeyFor 返回该 realm 的网关鉴权密钥：realm 专属键优先，否则全局键（空 = 不鉴权）。
func (h *Handler) apiKeyFor(rn string) string {
	if k, ok := h.cfg.RealmKeys[rn]; ok && strings.TrimSpace(k) != "" {
		return k
	}
	return h.cfg.APIKey
}

// bearerToken 取 Authorization: Bearer <token> 的 token 部分；不匹配时返回空串。
func bearerToken(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(authz, "Bearer ")
}

// realmOfRequest 解析本次请求归属的上游产品线：
//  1. X-Realm 头显式指定（已知值才采纳，未知值忽略）
//  2. Bearer 命中某个 realm 的专属键（RealmKeys）
//  3. DefaultRealm（空 = cn）
//
// 分流依据是"用哪个 key"而不是"猜模型名"：两条线的模型/额度/账号互不相通，
// 隐式猜测会在模型同名但本线无该模型时静默失败，显式分流最可预测。
//
// 注意：全局 api_key 是"主人钥匙"——它命中 RealmKeys 之外时归 DefaultRealm，
// 配合 X-Realm 也可进入任意 realm（见 authorized）。
func (h *Handler) realmOfRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Realm")); v != "" && realm.Known(v) {
		return realm.Normalize(v)
	}
	if key, ok := r.Context().Value(accessContextKey{}).(access.Key); ok {
		return key.DefaultChannel
	}
	if token := bearerToken(r); token != "" {
		for _, rn := range realm.All() {
			if k, ok := h.cfg.RealmKeys[rn]; ok && k != "" && secretEqual(k, token) {
				return rn
			}
		}
	}
	return realm.Normalize(h.cfg.DefaultRealm)
}

// poolFor 取该 realm 的账号池；未分池部署（Pools 为空）时回落 Pool。
func (h *Handler) poolFor(rn string) *pool.Pool {
	if p, ok := h.pools()[rn]; ok && p != nil {
		return p
	}
	return h.cfg.Pool
}

// activePools 当前启用的池，按池指针去重。
//
// 路线别名（codebuddy）与来源线（ai）共用同一个池对象：聚合口径必须去重，
// 否则同一批账号会被算两次（/status 的 total、/healthz 的 healthy 都会翻倍）。
func (h *Handler) activePools() []*pool.Pool {
	seen := map[*pool.Pool]bool{}
	var out []*pool.Pool
	for _, rn := range h.activeRealms() {
		p := h.poolFor(rn)
		if p == nil || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// upstreamFor 取该 route realm 对应的上游 client。
//
//   - 非别名线（含单线部署）→ 原样返回基础 client，行为零变化；
//   - 别名线（codebuddy）→ 返回钉在该 realm 档案上的浅拷贝：HTTP 连接池、Profiles
//     覆盖表与 effort 能力缓存全部共享，账号池仍是来源线（ai）的那一个。
func (h *Handler) upstreamFor(rn string) *upstream.Client {
	return h.CurrentRuntime().upstreamFor(rn)
}

func (c RuntimeConfig) upstreamFor(rn string) *upstream.Client {
	if c.Upstream == nil {
		return nil
	}
	if !realm.IsRouteAlias(rn) {
		return c.Upstream
	}
	return c.Upstream.Route(rn)
}

// channelPrefixes 渠道前缀映射（配置为空时用内置 {workbuddy: ai, codebuddy: codebuddy}）。
func (h *Handler) channelPrefixes() map[string]string {
	if len(h.cfg.ChannelPrefixes) > 0 {
		return h.cfg.ChannelPrefixes
	}
	return realm.DefaultChannelPrefixes()
}

// prefixAllowed 报告"前缀切到的线"能否用本次凭据访问：
//   - 全局 api_key（主人钥匙）→ 任意前缀；
//   - 该前缀对应的线未配专属 key → 放行（与既有"未配 key 即不鉴权"一致）；
//   - 前缀线与凭据所属线**共用同一份凭证来源**（AuthRealm 相同，如 codebuddy ↔ ai）→ 放行；
//   - 其余情况拒绝：不许用一条线的 key 借前缀跨池提权（例如 ai 的 key + `cn/` 前缀）。
func (h *Handler) prefixAllowed(keyRealm, prefixRealm, token string) bool {
	if h.cfg.Access != nil {
		key, err := h.cfg.Access.Lookup(token)
		return err == nil && key.Allows(prefixRealm)
	}
	if h.cfg.APIKey != "" && secretEqual(token, h.cfg.APIKey) {
		return true
	}
	if h.apiKeyFor(prefixRealm) == "" {
		return true
	}
	return realm.AuthRealmOf(prefixRealm) == realm.AuthRealmOf(keyRealm)
}

// modelWithChannel 给模型名加上渠道前缀（prefix 为空时原样返回）。
func modelWithChannel(prefix, model string) string {
	if prefix == "" || model == "" {
		return model
	}
	return prefix + realm.ChannelPrefixSeparator + model
}

// withChannelPrefix 给兜底表的每行 id 加上渠道前缀（prefix 为空或表为空时原样返回）。
// 返回新切片，不修改入参（兜底表是包级变量，可能被其他调用方共享）。
func withChannelPrefix(prefix string, rows []map[string]any) []map[string]any {
	if prefix == "" || len(rows) == 0 {
		return rows
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		cp := make(map[string]any, len(row)+2)
		for k, v := range row {
			cp[k] = v
		}
		if id, ok := cp["id"].(string); ok {
			cp["id"] = modelWithChannel(prefix, id)
			cp["channel"] = prefix
			cp["base_id"] = id
		}
		out = append(out, cp)
	}
	return out
}

// stripModelPrefix 把请求体里的 model 换成剥离渠道前缀后的裸模型名。
//
// 与 prompt.Rewrite 同样的策略：解析失败一律原样返回 —— 出站改写绝不阻塞请求转发。
func stripModelPrefix(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["model"]; !ok {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// activeRealms 当前启用的 realm 列表（保序）：未分池部署退化为单 realm
// （DefaultRealm，空 = cn），此时各接口行为与改造前的单线部署逐字一致。
func (h *Handler) activeRealms() []string {
	if len(h.pools()) == 0 {
		return []string{realm.Normalize(h.cfg.DefaultRealm)}
	}
	var out []string
	for _, rn := range realm.All() {
		if p, ok := h.pools()[rn]; ok && p != nil {
			out = append(out, rn)
		}
	}
	if len(out) == 0 {
		out = append(out, realm.Normalize(h.cfg.DefaultRealm))
	}
	return out
}

// authorized 鉴权：该 realm 的专属键、或全局 api_key（主人钥匙，可进任意 realm）
// 任一匹配即放行；realm 未配 key 且全局 key 为空时视为不鉴权。
// 两条线的账号/额度互不相通，但"拿全局 key 显式指定 X-Realm"是被允许的运维入口；
// 反过来 realm 专属 key 不能进别的 realm（隔离防误用）。
func (h *Handler) authorized(rn, token string) bool {
	key := h.apiKeyFor(rn)
	if key == "" {
		return true
	}
	if secretEqual(token, key) {
		return true
	}
	return h.cfg.APIKey != "" && secretEqual(token, h.cfg.APIKey)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	if h.cfg.Access != nil {
		return h.withIssuedKey(next)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(h.realmOfRequest(r), bearerToken(r)) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy := 0, 0
	servable := false
	// 按池去重：路线别名（codebuddy）与来源线（ai）共用同一个池对象，
	// 不去重会把同一批账号算两次。
	for _, p := range h.activePools() {
		if p == nil {
			continue
		}
		t, hh, _, _, _ := p.CountsDetailed()
		total += t
		healthy += hh
		// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
		// 否则负载均衡器会把流量持续打进无法受理的实例。多池时任一池可服务即 200。
		if p.ServableNow() {
			servable = true
		}
	}
	status := http.StatusOK
	if !servable {
		status = http.StatusServiceUnavailable
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": ServiceName,
	})
}

// poolPortrait 单个池的状态画像（/status 的 per-realm 明细）。
func poolPortrait(p *pool.Pool) map[string]any {
	if p == nil {
		return map[string]any{"accounts": []any{}, "total": 0, "healthy": 0}
	}
	total, healthy, cooling, disabled, inFlightFull := p.CountsDetailed()
	return map[string]any{
		"accounts":       p.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		"credit_floor":   p.CreditFloor(),
	}
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	realms := h.activeRealms()
	// 单 realm（未分池 / 只有一个 realm 有账号）：结构与改造前逐字一致。
	if len(realms) == 1 {
		p := h.poolFor(realms[0])
		body := poolPortrait(p)
		body["sticky_sessions"] = sticky
		body["redis_mode"] = redisMode
		body["realm"] = realms[0]
		writeJSON(w, http.StatusOK, body)
		return
	}
	// 多 realm：顶层给聚合口径，per-realm 明细挂 realms 字段。
	//
	// 去重口径：路线别名（codebuddy）与来源线（ai）**共用同一个池对象**（同一批账号、
	// 同一份状态），按池指针去重后再聚合，否则同一批账号会被算两次；
	// 别名线单独列在 routes 里（别名 → 来源线），客户端据此知道该路由存在。
	byRealm := map[string]any{}
	routes := map[string]string{}
	total, healthy, cooling, disabled, inFlightFull := 0, 0, 0, 0, 0
	accounts := []any{}
	for _, rn := range realms {
		if src := realm.AuthRealmOf(rn); src != rn {
			routes[rn] = src
			continue
		}
		portrait := poolPortrait(h.poolFor(rn))
		portrait["realm"] = rn
		byRealm[rn] = portrait
		total += portrait["total"].(int)
		healthy += portrait["healthy"].(int)
		cooling += portrait["cooling"].(int)
		disabled += portrait["disabled"].(int)
		inFlightFull += portrait["in_flight_full"].(int)
		if list, ok := portrait["accounts"].([]any); ok {
			accounts = append(accounts, list...)
		}
	}
	body := map[string]any{
		"accounts":        accounts,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		"realms":          byRealm,
	}
	if len(routes) > 0 {
		body["routes"] = routes
	}
	writeJSON(w, http.StatusOK, body)
}

// 冷启动兜底模型表：**只在"从未成功拿到过服务端名单"时使用**。
//
// 正常运行的模型表一律来自服务端下发（cn → /console/enterprises/personal/models，
// 国际线 ai / codebuddy → /v3/config），由 FetchModels 拉取并缓存；
// 缓存过期后拉取失败时沿用上一次成功下发的名单（见 fetchDynamicModelsFor）。
//
// cn 线：api-reference §5 的快照，动态接口可用，兜底几乎不会命中。
// 国际线：**不提供手工快照**。桌面端 product.json 里的静态表已与服务端漂移
// （如 gemini-3.1-flash-lite / gpt-5.3-codex 等 id 已不在服务端名单中，请求会
// 直接 422/11102），宁可返回空列表让客户端重试，也不返回过期假模型。
// 图片/视频模型（tags 含 text-to-image / image-to-image / text-to-video /
// image-to-video）不进 chat 列表。
var staticModelsCN = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// staticModelsFor 返回该 realm 的冷启动兜底表（见上方注释）；
// 国际线返回空列表（非 nil，保持 "data": [] 的响应形状）。
func staticModelsFor(rn string) []map[string]any {
	if realm.IsIntlLine(rn) {
		return []map[string]any{}
	}
	return staticModelsCN
}

// modelsCache 动态模型缓存（按 realm 独立：两条线的模型集不同，不可共享）。
type modelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

// dynamicModelsCache cn 线（默认 realm）的动态模型缓存。
// 保持包级变量与原字段名：单线部署与既有测试直接操作它。
var dynamicModelsCache modelsCache

// dynamicModelsCacheByRealm 其他 realm 的动态缓存（realm → *modelsCache）。
var dynamicModelsCacheByRealm sync.Map

// modelsCacheFor 取该 realm 的动态缓存（realm 名先归一，旧名 "ai" 与 workbuddy 共用同一份缓存）。
func modelsCacheFor(rn string) *modelsCache {
	rn = realm.Normalize(rn)
	if rn == realm.CN {
		return &dynamicModelsCache
	}
	if v, ok := dynamicModelsCacheByRealm.Load(rn); ok {
		return v.(*modelsCache)
	}
	actual, _ := dynamicModelsCacheByRealm.LoadOrStore(rn, &modelsCache{})
	return actual.(*modelsCache)
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
	// productConfigTTL 国际线（/v3/config 产品配置）变化较快：新模型上架、限时免费促销都会改它，
	// 故用比 CN 控制台接口更短的缓存。
	productConfigTTL = 10 * time.Minute
)

// dynamicModelsTTLFor 该 realm 的动态模型缓存时长（国际线走服务端产品配置，TTL 更短）。
func dynamicModelsTTLFor(rn string) time.Duration {
	if realm.IsIntlLine(rn) {
		return productConfigTTL
	}
	return dynamicModelsTTL
}

// models 返回模型列表：**聚合本次凭据可访问的所有渠道**，内容以服务端下发为准
// （见 fetchDynamicModelsFor；冷启动才用兜底表）。
//
// 每条 id 带各自渠道前缀（cn 无前缀），所以聚合后不会重名：客户端一次就能看到
// `workbuddy/<模型>` 与 `codebuddy/<模型>`，直接拿去选渠道即可。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelsForRequest(r),
	})
}

// modelsForRequest 本次凭据可访问的全部渠道的模型列表（按 realm.All 顺序、各带前缀）。
//
// 覆盖范围 = **与凭据所属线共用同一份凭证来源**的所有启用线（ai 的 key → ai + codebuddy），
// modelsForRequest 本次凭据可见的渠道的模型列表（每条 id 各带渠道前缀）。
//
// 口径（两条线的专属 key 必须**各看各的**，否则分开两条密钥就毫无意义）：
//   - 全局 api_key（主人钥匙）→ 聚合**全部启用线**；
//   - 某条线的专属 key → **只列该条线**（workbuddy 的 key 看不到 codebuddy 的模型，反之亦然）。
func (h *Handler) modelsForRequest(r *http.Request) []map[string]any {
	realms := h.realmsForCredential(r)
	out := make([]map[string]any, 0, len(realms)*16)
	for _, rn := range realms {
		out = append(out, h.modelListFor(rn)...)
	}
	return out
}

// realmsForCredential 本次凭据可见的渠道（保序：按 realm.All() 顺序）。
func (h *Handler) realmsForCredential(r *http.Request) []string {
	if h.cfg.Access != nil {
		out := []string{}
		for _, rn := range h.activeRealms() {
			if h.issuedChannelAllowed(r, rn) {
				out = append(out, rn)
			}
		}
		return out
	}
	keyRealm := h.realmOfRequest(r)
	if h.cfg.APIKey != "" && secretEqual(bearerToken(r), h.cfg.APIKey) {
		return h.activeRealms() // 主人钥匙：全部启用线
	}
	return []string{keyRealm} // 专属 key（或未配 key）：只有它自己那条线
}

// modelList 默认 realm 的模型列表（保留入口，供既有调用方/测试使用）。
func (h *Handler) modelList() []map[string]any {
	return h.modelListFor(realm.Normalize(h.cfg.DefaultRealm))
}

// modelListFor 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
//
// 模型 id 会带上该 realm 的**渠道前缀**（如 `codebuddy/deepseek-v4.1-flash`），
// 让客户端显式选渠道、日志与账单也能一眼分辨走的是哪条线；前缀是可选的装饰 ——
// 不带前缀的裸模型名仍按 key / X-Realm 分流（既有行为不变）。
func (h *Handler) modelListFor(rn string) []map[string]any {
	prefix := realm.ChannelPrefixFor(rn, h.channelPrefixes())
	if infos := h.fetchDynamicModelsFor(rn); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":                modelWithChannel(prefix, mi.ID),
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    mi.ContextWindow,
				"max_output_tokens": mi.MaxTokens,
			}
			if prefix != "" {
				entry["channel"] = prefix
				entry["base_id"] = mi.ID
			}
			if mi.Credits != "" {
				entry["credits"] = mi.Credits
				// rate：解析后的倍率数值（免费模型为 0），方便客户端直接排序 / 判免费。
				if rate, known := upstream.ParseModelRate(mi.Credits); known {
					entry["rate"] = rate
				}
			}
			if mi.Vendor != "" {
				entry["vendor"] = mi.Vendor // 上游原始字段（内部单字母枚举）
			}
			if name := upstream.ModelVendor(mi.ID); name != "" {
				entry["vendor_name"] = name // 可读厂商名（由模型 id 推导）
			}
			if mi.DefaultEffort != "" {
				entry["default_effort"] = mi.DefaultEffort
			}
			// supported_efforts：实际可选档位。上游未下发 supportedEfforts 但模型带推理时，
			// 回退官方全档枚举（否则客户端会误以为"只支持一个档"）。
			if efforts := upstream.EffectiveEfforts(mi.Efforts, mi.DefaultEffort); len(efforts) > 0 {
				entry["supported_efforts"] = efforts
			}
			// 媒体模型（图片 / 视频）：没有上下文概念，不打 131072 兜底；
			// 改为透出 kind / tags，客户端据此区分「对话模型」与「该调 images/videos 的模型」。
			if kind := upstream.MediaKind(mi.Tags); kind != "" {
				entry["kind"] = kind
				entry["tags"] = mi.Tags
			} else if mi.ContextWindow == 0 {
				entry["context_length"] = 131072 // 兜底
			}
			out = append(out, entry)
		}
		return out
	}
	return withChannelPrefix(prefix, staticModelsFor(rn))
}

// fetchDynamicModels 默认 realm 的动态拉取入口（保留供既有调用方/测试使用）。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	return h.fetchDynamicModelsFor(realm.Normalize(h.cfg.DefaultRealm))
}

// fetchDynamicModelsFor 从该 realm 池中任一健康账号拉模型列表（国际线缓存 10min，cn 1h）。
//
// 「模型表以服务端下发的为准」的落实：
//   - 拉取成功 → 覆盖缓存，TTL 内直接命中；
//   - TTL 过期后拉取失败 → **继续沿用上一次成功下发的名单**（不再退回手工静态表），
//     并进入 5min 负缓存，避免反复打上游；
//   - 从未成功拉到过（冷启动 + 上游不可达）→ 返回 nil，由 modelListFor 走冷启动兜底表；
//   - 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败的号。
func (h *Handler) fetchDynamicModelsFor(rn string) []upstream.ModelInfo {
	cache := modelsCacheFor(rn)
	cache.RLock()
	fresh := len(cache.ids) > 0 && time.Since(cache.fetched) < dynamicModelsTTLFor(rn)
	cached := cache.ids
	fetched := cache.fetched
	failCooldown := !cache.lastFail.IsZero() && time.Since(cache.lastFail) < modelsFetchFailCooldown
	cache.RUnlock()
	if fresh || failCooldown {
		if fresh {
			h.recordModelRates(rn, cached, fetched)
		}
		return cached
	}

	pl := h.poolFor(rn)
	if pl == nil {
		return cached
	}
	acct := pl.Pick()
	if acct == nil {
		return cached
	}
	infos, err := h.upstreamFor(rn).FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 模型表拉取失败**不等于账号有问题**：DNS 黑洞 / 连不上 / 超时都属于上游可达性故障，
		// 记到账号上会误伤健康号（3 次就熔断 30 分钟，而它聊天完全正常）。只有确实拿到
		// 上游 HTTP 响应（401/403/code!=0/空配置）的错误才计入账号健康度。
		if !isTransportErr(err) {
			pl.NoteError(acct.UID)
		}
		cache.Lock()
		cache.lastFail = time.Now()
		cache.Unlock()
		reason := "上游返回空模型表"
		if err != nil {
			reason = err.Error()
		}
		// 冷启动（无历史名单）过去是**静默**返回空列表：排障时只能靠猜。现在一律写明原因。
		if len(cached) > 0 {
			log.Printf("WARN: [server] models realm=%s: 拉取失败（%s），继续沿用上一次服务端下发的 %d 个模型", rn, reason, len(cached))
		} else {
			log.Printf("WARN: [server] models realm=%s: 拉取失败（%s），且无历史名单 → 该渠道本次返回空列表；%s 内不再重试（uid=%s）", rn, reason, modelsFetchFailCooldown, logfmt.UID8(acct.UID))
		}
		return cached
	}
	cache.Lock()
	cache.ids = infos
	fetched = time.Now()
	cache.fetched = fetched
	cache.lastFail = time.Time{} // 成功则清空负缓存
	cache.Unlock()
	h.recordModelRates(rn, infos, fetched)
	return infos
}

func (h *Handler) recordModelRates(rn string, infos []upstream.ModelInfo, fetched time.Time) {
	pl := h.poolFor(rn)
	if pl == nil {
		return
	}
	rates := make(map[string]float64)
	for _, info := range infos {
		if rate, known := upstream.ParseModelRate(info.Credits); known {
			rates[info.ID] = rate
		}
	}
	pl.SetModelRates(rn, rates, fetched.Add(dynamicModelsTTLFor(rn)))
}

// isTransportErr 报告错误是否来自"连接层"（DNS 解析失败 / 拨号失败 / 超时 / TLS 失败）——
// 即根本没跟上游建立起一次 HTTP 事务。这类故障与账号本身无关，不该记到账号健康度上：
// 否则一个域名被 DNS 黑洞（例如从海外解析 www.codebuddy.ai 拿到 0.0.0.1），
// 每次拉模型表都会给健康账号记一次错误 + 熔断失败，几轮下来把能聊天的号提前熔断。
func isTransportErr(err error) bool {
	var uerr *url.Error
	return errors.As(err, &uerr)
}
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	h.chat(w, r, "")
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	h.chat(w, r, "responses")
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	h.chat(w, r, "messages")
}

func (h *Handler) chat(w http.ResponseWriter, r *http.Request, protocol string) {
	runtime := h.CurrentRuntime()
	// realm 分流：本次请求归属的产品线决定用哪个账号池（不跨池回退）。
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	limit := runtime.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	var responseRequest map[string]any
	if protocol != "" {
		body, responseRequest, err = protocolToChat(body, protocol)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
				"type": "invalid_request_error", "code": "invalid_request", "message": err.Error(),
			}})
			return
		}
	} else if err := validateChatRequest(body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// ── 路线解析：渠道前缀优先 ──
	// 模型名形如 `codebuddy/<模型>` 时按前缀选线（把调用渠道写进模型名，日志/账单一眼可辨），
	// 否则沿用 X-Realm / realm 专属 key / DefaultRealm（既有行为，逐字不变）。
	// 命中前缀时出站前剥离，上游只看到裸模型名；裸模型名同时是选号与模型级冷却的键。
	reqRealm := h.realmOfRequest(r)
	channel, bareModel, prefixRealm := realm.SplitChannelPrefix(peek.Model, h.channelPrefixes())
	if channel != "" {
		if h.cfg.Access != nil && !h.issuedChannelAllowed(r, prefixRealm) {
			writeOpenAIError(w, 403, "channel_not_allowed", "该密钥不允许访问模型指定的渠道")
			return
		}
		if h.prefixAllowed(reqRealm, prefixRealm, bearerToken(r)) {
			reqRealm = prefixRealm
		} else {
			log.Printf("WARN: [server] 模型前缀 %q 指向 %s，但本次凭据属于 %s（两条线凭证来源不同）→ 仍按 %s 处理",
				channel, prefixRealm, reqRealm, reqRealm)
		}
		body = stripModelPrefix(body, bareModel)
		peek.Model = bareModel
	}
	// realm 在这一刻定格（后面不再变）：它决定账号池、出站档案与使用日志标注的产品线。
	if h.cfg.Access != nil && !h.issuedChannelAllowed(r, reqRealm) {
		writeOpenAIError(w, 403, "channel_not_allowed", "该密钥的渠道未授权或未启用")
		return
	}
	pl := h.poolFor(reqRealm)
	// 路线别名（codebuddy）在这里钉住出站档案：账号仍取自 ai 池（同一份凭证/同一份额度），
	// 但 base / Origin / 归因按别名线解析。非别名线时与原来逐字一致。
	routeUp := runtime.upstreamFor(reqRealm)
	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	st.runtime = &runtime
	st.realm = reqRealm
	if h.cfg.UsageLog.Enabled() {
		st.seq = h.cfg.UsageLog.NextSeq()
	}
	defer st.finish(h, pl)

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	//
	// 确定性选号策略（lowest_credits / highest_credits / custom_priority）下**跳过粘性**：
	// 两者目标相反——粘性按会话把请求散到不同账号（hash 分配、30m TTL），而这三个策略
	// 要按余额或优先级把请求集中到确定的账号上。若同时生效，粘性会先按 hash 定号、
	// 选号策略根本轮不到，功能静默失效（这是 code review 抓到的：
	// 默认配置下 session_sticky.enabled=true）。判定口径统一在 pool.IsDeterministicSelection，
	// 新增确定性策略时只改那一处，这里不会漏。
	sessKey := ""
	stickyUID := ""
	if runtime.Session != nil && !pool.IsDeterministicSelection(pl.SelectionMode()) {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := runtime.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			pl.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			pl.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			runtime.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。网关**不内置任何提示词**：
	//   - custom：仅当运维配了 prompt.file（PromptText 非空）时才用那份文本替换客户端
	//     system/developer；未配置则与 passthrough 等价（不注入）。
	//   - passthrough：透传客户端原始 system（不改写）。
	//   - 降级期（内容拦截误报）：**剥离** system/developer，不注入任何文案。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "passthrough" && h.degrade.Active() {
		body = prompt.StripSystem(body)
		degradedApplied = true
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		if r.Context().Err() != nil {
			st.status = 499
			return
		}
		// 选号：粘性号优先（PickByUID 已校验 health + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = pl.PickByUIDForModel(stickyUID, peek.Model, reqRealm)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满）→ 解绑，本次回落普通轮换。
				unbindSticky()
			}
		}
		if acct == nil {
			// 模型感知选号：请求携带 model 时启用 6004 模型级冷却豁免
			// （PickExcludingForModel 内部当 model 为空时即退化为 PickExcluding）。
			acct = pl.PickExcludingForRoute(tried, peek.Model, reqRealm)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		st.nname = acct.Nickname
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !pl.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := routeUp.RefreshTokenContext(r.Context(), acct); err != nil {
				if r.Context().Err() != nil {
					st.status = 499
					return
				}
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					pl.Disable(acct.UID, "refresh session dead")
				} else {
					pl.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh uid=%s: save auth failed: %v", logfmt.UID8(acct.UID), err)
			}
		}

		rc, status, respBody, terr := routeUp.ChatStreamContext(r.Context(), acct, body, r.Header.Get("X-Conversation-ID"))
		if terr != nil {
			if r.Context().Err() != nil {
				st.status = 499
				return
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			if kind == upstream.ErrPromptTooLong || kind == upstream.ErrImageInvalid {
				writeJSON(w, status, map[string]any{"error": map[string]any{
					"message":      string(respBody),
					"type":         "invalid_request_error",
					"code":         kind.String(),
					"gateway_hint": upstream.RequestErrorHint(kind),
				}})
				return
			}
			// 内容拦截误报（passthrough 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，**剥离** system/developer 后同请求内重试
			// （网关不注入任何自有提示词）。
			// 第二次仍被拦（用户内容本身触发审核）→ 走既有错误路径返回客户端。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.StripSystem(body)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> strip-system retry")
				continue
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicyOn(pl, acct.UID, kind, string(respBody), peek.Model)
			fail(acct.UID)
			continue
		}
		pl.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && runtime.Session != nil {
			runtime.Session.Bind(sessKey, acct.UID)
		}
		if protocol != "" || peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			var streamErr error
			if protocol != "" {
				streamErr = serveProtocol(w, stats, responseRequest, protocol, peek.Stream)
			} else {
				streamErr = upstream.StreamWithModel(w, stats, peek.Model)
			}
			if streamErr != nil {
				st.status = http.StatusBadGateway
			}
			if r.Context().Err() != nil {
				st.status = 499
			}
			st.ttfb = stats.TTFB()
			st.toks, st.hasUse = stats.Tokens()
			if st.hasUse {
				st.prompt = stats.Prompt()
				st.cached = stats.Cached()
				st.usageRaw = stats.Raw()
				// 上游报告的积分消耗（首选来源，见 chatStat.finish 的优先级说明）。
				if c, ok := stats.Credit(); ok {
					st.creditFromUsage, st.hasCreditUsage = c, true
				}
			}
			rc.Close()
			return
		}
		resp, err := upstream.AggregateWithToolValidation(rc, routeUp.RepairToolHistory)
		rc.Close()
		if r.Context().Err() != nil {
			st.status = 499
			return
		}
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		if stringOf(resp["model"]) == "" {
			resp["model"] = peek.Model
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		if st.toks >= 0 {
			// Aggregate 保留了上游 usage 对象：三项 token + 原始副本一次取全。
			st.hasUse = true
			st.prompt = promptTokens(resp)
			st.cached = cachedTokensOf(usageRawOf(resp))
			st.usageRaw = usageRawOf(resp)
			// 上游报告的积分消耗（首选来源，见 chatStat.finish 的优先级说明）。
			if c, ok := usagelog.CreditOf(st.usageRaw); ok {
				st.creditFromUsage, st.hasCreditUsage = c, true
			}
		}
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 七条路径，各司其职：
//   - ErrHardCredit → FreezeNoCredits：额度冻结（条件解冻——等额度巡检探测到余额恢复）。
//   - ErrSoftRate → 默认 Cooldown(CoolSoft, soft_rate) 连续触发指数退避（封顶 soft_rate_max）；
//     若上游 body 为模型级 6004 且带重置时间 → CooldownSoftForModel（until=重置墙钟，
//     封顶 soft_rate_max，记录触发模型供切模型豁免）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError），passthrough 模式走降级重试。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
// applyErrorPolicy 默认 realm 池的处置入口（保留原签名，供既有调用方与测试使用）。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	h.applyErrorPolicyOn(h.poolFor(realm.Normalize(h.cfg.DefaultRealm)), uid, kind, body, model)
}

// applyErrorPolicyOn 指定账号池的处置入口（多 realm 部署下按请求所属池处置）。
func (h *Handler) applyErrorPolicyOn(pl *pool.Pool, uid string, kind upstream.ErrKind, body, model string) {
	softCooldown := h.CurrentRuntime().SoftCooldown
	switch kind {
	case upstream.ErrHardCredit:
		// 余额耗尽 → **冻结**（条件解冻：必须额度探测确认恢复）。
		// 换成时间冷却（到次日 04:00）会有个空窗：04:00 解冻后余额仍是 0（签到在 09:00），
		// 这期间账号被选中只会反复撞 429，白刷上游还把成功率权重拖低。
		pl.FreezeNoCredits(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 模型级 6004 且带「将在 … 重置」时间（issue #31）：冷却到上游明说的重置墙钟
		// （封顶 soft_rate_max），记录触发模型 → 该账号对**其他模型**请求可豁免冷却。
		// 解析失败（无时间文案 / 非 6004）→ 退回既有 600s 基数 + 指数退避现况。
		if upstream.IsModelRateLimit(body) {
			if resetAt, ok := upstream.ParseSoftRateReset(body); ok {
				pl.CooldownSoftForModel(uid, softCooldown, resetAt, model, "6004 model rate limit")
				return
			}
		}
		// 其余 soft_rate：软冷却基数来自 soft_rate（默认 600s）；同一账号连续触发时
		// pool 内部按 softStreak 指数退避并封顶 soft_rate_max。
		pl.Cooldown(uid, pool.CoolSoft, softCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		pl.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		pl.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		pl.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	typ := "api_error"
	if status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge {
		typ = "invalid_request_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"code":    code,
		},
	})
}
