// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/realm"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	ErrClient                        // 其他 4xx / 业务错误
	ErrPromptTooLong
	ErrImageInvalid
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrClient:
		return "client"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrImageInvalid:
		return "image_invalid"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	// 复数形式：上游实际文案是 "Credits exhausted. Please visit the link below to
	// purchase add-on packs..."（HTTP 429 + code 14018），单数 marker 匹配不到复数，
	// 会掉进 429 兜底被误判为软限流（软冷却 600s 反复重试，而非等签到解冻）。
	"credits exhausted", "insufficient credits", "out of credits", "credits running out",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// hardCreditCodeRe 计费额度耗尽的业务码。
//
// 上游把「额度耗尽」也发成 HTTP 429（而非 402），只靠状态码会误判为限流：
//
//	{"error":{"data":{"code":14018,"msg":"Credits exhausted. ..."}}}   // 实测 2026-09-15
//
// 按业务码识别比抠文案稳（文案会变，code 稳定）。容忍 "code":14018 / "code" : 14018。
var hardCreditCodeRe = regexp.MustCompile(`"code"\s*:\s*(14018)\b`)

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// badParamsMarkers 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsMarkerMsg = "Unmarshal chat params failed"
var badParamsMarkerCode = `"code":11101`

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetRe 匹配「将在 … 重置」，捕获中间的时间串。
const softRateResetRe = `将在 (.+?) 重置`

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	re := regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	return re.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
// 内部先判 IsModelRateLimit：非模型级限流（非 6004）即使带"重置"字样也不返回——该重置
// 无冷却语义（如 11140 的通用限流提示），解析出来反而会错误收窄冷却。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	re := regexp.MustCompile(softRateResetRe)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  1. 402 / hardMarkers —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit，本次保持不变
//     （issue #28 已记录该反向误判风险，待上游原始响应确认后再定）。
//  2. sessionDeadMarkers —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时在此短路，
//     结果同为 soft_rate，与下一层一致。
//  4. status==429 —— body 无文案时的兜底识别。
//  5. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
func Classify(status int, body string) ErrKind {
	if kind := classifyRequestError(status, body); kind != ErrNone {
		return kind
	}
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	// 业务码兜底：文案换成别的措辞也仍能识别（见 hardCreditCodeRe 注释）。
	if hardCreditCodeRe.MatchString(body) {
		return ErrHardCredit
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
		// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
		// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
		// 归 ErrBadParams：不冷却/不熔断/不计错，但**仍然轮转**（不同账号可能有不同的
		// 模型权限，值得再试一次）。
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 指针持有（自带锁）：Client 可被安全浅拷贝，见 Route。
	efforts *effortCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool
	PromptCacheKey       bool
	RepairToolHistory    bool

	// UserAgent 出站 User-Agent 覆盖（空 = 按 realm 指纹解析）。
	// 全部出站请求生效：chat / refresh / checkin / balance(含 report/travel) / FetchModels。
	// issue #42 深挖：官网「使用端」列基于出站请求的 UA/X-Product 服务端归因，
	// 官方 WorkBuddy 桌面 UA 为 `WorkBuddy/<version>`（桌面端 AuthService 出站头实测口径）。
	// 默认保持现状（指纹净化考虑），仅当用户显式配置才改写。
	UserAgent string

	// ChatBaseCN / BillingBaseCN CN 线的 base 覆盖（历史字段，测试与旧调用方仍可注入）。
	// 空 = 用 realm 内置档案（copilot.tencent.com / www.codebuddy.cn）。
	ChatBaseCN    string
	BillingBaseCN string

	// Profiles 各 realm 的档案覆盖（chat/billing base、origin、platform、指纹、版本）。
	// 缺省用 internal/realm 内置档案；CN 的 base 仍可走 ChatBaseCN/BillingBaseCN 覆盖。
	Profiles map[string]realm.Profile
	// RealmFingerprints 各 realm 的指纹风格覆盖（"cli" / "workbuddy-desktop"）。
	// 未列出 = 用该 realm 档案的默认指纹。
	RealmFingerprints map[string]realm.Fingerprint
	// RealmVersions 各 realm 的客户端版本覆盖（cli 指纹拼 UA，desktop 指纹拼 UA/X-IDE-Version）。
	RealmVersions map[string]string
	// RealmUserAgents 各 realm 的 UA 直接覆盖（优先级高于指纹解析，低于全局 UserAgent）。
	RealmUserAgents map[string]string
	// RealmDefault 未标注 realm 的账号归属（空 = cn），兼容改造前的单线部署。
	RealmDefault string
	// RouteRealm 本次请求的"路线 realm"（空 = 按账号自带 realm 解析）。
	// 非空时优先于账号 realm：别名线（codebuddy）借用来源线（ai）的账号时用它指定出站档案。
	RouteRealm string
}

// effortCache 各模型 supportedEfforts 缓存（FetchModels 刷新），供请求体 effort 降级。
//
// 独立指针 + 自带锁：Client 需要能被安全浅拷贝（见 Route），而 sync.RWMutex
// 一旦作为值字段就会让整个结构不可复制（copylocks）。
type effortCache struct {
	mu sync.RWMutex
	m  map[string][]string
}

func newEffortCache() *effortCache { return &effortCache{} }

// get 返回能力表副本；nil 表示尚未拉到（调用方透传、不做 effort 降级）。
func (e *effortCache) get() map[string][]string {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(e.m) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(e.m))
	for k, v := range e.m {
		cp[k] = v
	}
	return cp
}

// set 覆盖能力表（FetchModels 成功后调用）。
func (e *effortCache) set(m map[string][]string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.m = m
	e.mu.Unlock()
}

// Route 返回钉在 rn 上的浅拷贝：HTTP/Transport、Profiles 覆盖表与 effort 缓存全部共享，
// 只有"档案按哪个 realm 解析"变了。
//
// 用途：路线别名（codebuddy）复用来源线（ai）的账号池时，账号自带 Realm 仍是 ai，
// 但出站 base/Origin/归因必须按别名线的档案走——所以按"本次请求的路线 realm"钉住。
// rn 为空时原样返回（单线部署行为零变化）。
func (c *Client) Route(rn string) *Client {
	if c == nil || strings.TrimSpace(rn) == "" {
		return c
	}
	cp := *c
	cp.RouteRealm = realm.Normalize(rn)
	return &cp
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: true,
		PromptCacheKey:       true,
		efforts:              newEffortCache(),
		// base 不再硬编码：由 internal/realm 的档案按账号 realm 解析
		// （cn → copilot.tencent.com / www.codebuddy.cn，ai → www.workbuddy.ai，
		// codebuddy → www.codebuddy.ai，复用 ai 的账号池）。
		RealmDefault: realm.CN,
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// chatBase 返回聊天域 base（chat/completions、token refresh、OAuth、模型列表）。
func (c *Client) chatBase(a *auth.Auth) string {
	return c.profileOf(a).ChatBase
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
func (c *Client) prepareBody(body []byte) []byte {
	return PrepareBodyOptWithEfforts(body, c.SanitizeFingerprints, c.effortsSnapshot())
}

// prepareBodyFor 按出站 realm 组装出站请求体：在 prepareBody 基础上补 realm 专属改写
// （国际线 ai / codebuddy 都要求首条消息为 system，见 EnsureLeadingSystem）。
func (c *Client) prepareBodyFor(a *auth.Auth, body []byte) []byte {
	out := c.prepareBody(body)
	if c.RepairToolHistory {
		out = repairToolHistory(out)
	}
	if realm.IsIntlLine(c.realmOf(a)) {
		return EnsureLeadingSystem(out)
	}
	return out
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot() map[string][]string {
	return c.effortCacheRef().get()
}

// billingBase 返回 billing 域 base（签到 / 余额 / 活跃上报）。
func (c *Client) billingBase(a *auth.Auth) string {
	return c.profileOf(a).BillingBase
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath = "/v2/billing/meter/get-user-resource"
	dailyCheckinPath = "/v2/billing/meter/daily-checkin"
)

// doJSON 响应体量上限：常规短 RPC 与媒体两类（媒体响应可远超 1MB）。
const (
	doJSONMaxBody  = 1 << 20  // 常规短 RPC 响应上限（保持既有行为）
	doJSONMaxMedia = 24 << 20 // 媒体类响应上限：b64 图片 / 视频任务详情可远超 1MB
)

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	return c.doJSONLimit(req, doJSONMaxBody)
}

// doJSONLimit 带响应体量上限的 doJSON：媒体端点（b64 图片）需要更大上限，
// 否则合法响应会被 1MB 截断成 "parse failed"。
func (c *Client) doJSONLimit(req *http.Request, max int64) (json.RawMessage, error) {
	if max <= 0 {
		max = doJSONMaxBody
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, max))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// 刷新路径：桌面/旧协议 vs 官方 CLI 2.x（两条并存，旧路径失效时回落新的）。
const (
	refreshPath   = "/v2/plugin/auth/token/refresh"
	refreshPathV2 = "/v2/auth/token/refresh"
)

// refreshDo 用给定路径发一次刷新请求；source 非空时覆盖 X-Auth-Refresh-Source。
func (c *Client) refreshDo(ctx context.Context, a *auth.Auth, path, source string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+path, nil)
	if err != nil {
		return nil, err
	}
	c.RefreshHeaders(req, a)
	if source != "" {
		req.Header.Set("X-Auth-Refresh-Source", source)
	}
	return c.doJSON(req)
}

// isPathMissing 判断错误是否表示"该路径不存在"（404/405）。只在这种情况回落备用刷新路径；
// 401/403/余额类错误不回落 —— 那说明路径在、是凭证或权限问题。
func isPathMissing(err error) bool {
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Status == http.StatusNotFound || ue.Status == http.StatusMethodNotAllowed
	}
	return false
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。网络刷新串行执行，凭证读写通过快照与短锁保持一致。
func (c *Client) RefreshToken(a *auth.Auth) error {
	return c.RefreshTokenContext(context.Background(), a)
}

func (c *Client) RefreshTokenContext(ctx context.Context, account *auth.Auth) error {
	return account.Refresh(ctx, func(a *auth.Auth) error {
		if strings.TrimSpace(a.RefreshToken) == "" {
			return fmt.Errorf("no refreshToken")
		}
		// 刷新路径：桌面/旧协议 /v2/plugin/auth/token/refresh 优先；官方 CLI 2.x 用
		// /v2/auth/token/refresh（X-Auth-Refresh-Source: plugin）。旧路径 404/405（路径下线）
		// 时自动回落新路径，避免整条线因为上游换了刷新路径而集体掉线。
		data, err := c.refreshDo(ctx, a, refreshPath, "workbuddy")
		if err != nil && isPathMissing(err) {
			log.Printf("WARN: [upstream] refresh %s 不可用（%v）→ 回落 %s", refreshPath, err, refreshPathV2)
			data, err = c.refreshDo(ctx, a, refreshPathV2, "plugin")
		}
		if err != nil {
			return err
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
			return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
		}
		a.AccessToken = tok.AccessToken
		if tok.RefreshToken != "" {
			a.RefreshToken = tok.RefreshToken
		}
		if tok.Domain != "" {
			a.Domain = tok.Domain
		}
		// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
		if tok.ExpiresIn > 0 {
			a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
		}
		return nil
	})
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, "")
}

func (c *Client) ChatStreamContext(parent context.Context, a *auth.Auth, body []byte, conversationID string) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if err := parent.Err(); err != nil {
		return nil, 0, nil, err
	}
	a = a.Snapshot()
	prepared := c.prepareBodyFor(a, body)
	if c.PromptCacheKey {
		prepared = injectPromptCacheKey(prepared, c.realmOf(a), a.UID, conversationID)
	}
	url := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a)
	ctx, cancel := context.WithCancel(parent)
	req = req.WithContext(ctx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		if parent.Err() == nil {
			c.chatHTTP().CloseIdleConnections()
		}
		log.Printf("ERR: [upstream] chat_stream uid=%s: transport error: %v", logfmt.UID8(a.UID), err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			return nil, resp.StatusCode, nil, readErr
		}
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] chat_stream uid=%s: upstream %d %s body=%s",
			logfmt.UID8(a.UID), resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：Close 始终取消请求；启用空闲监控时额外监测流中停顿。
	return monitorBody(&cancelBody{ReadCloser: resp.Body, cancel: cancel}, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	Credits       string
	Vendor        string
	DefaultEffort string
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
	Tags          []string // 服务端能力标签（text-to-image / image-to-image / text-to-video / image-to-video …）
}

// mediaTags 服务端用来标注图片 / 视频模型的能力标签。
// 取值照官方 CLI 的模型发现逻辑——它就是在产品配置的 models 上按这些 tag 过滤出图片 / 视频模型。
var mediaTags = []string{"text-to-image", "image-to-image", "text-to-video", "image-to-video"}

// HasMediaTag 标签里是否含图片 / 视频能力标签。
func HasMediaTag(tags []string) bool {
	for _, t := range tags {
		for _, mt := range mediaTags {
			if t == mt {
				return true
			}
		}
	}
	return false
}

// MediaKind 媒体模型归类："image" / "video"；非媒体模型返回 ""。
// 同一模型可能同时标 text-to-image 与 image-to-image（如 gpt-image-2.5-sunburst），按 image 归类。
func MediaKind(tags []string) string {
	for _, t := range tags {
		if t == "text-to-image" || t == "image-to-image" {
			return "image"
		}
	}
	for _, t := range tags {
		if t == "text-to-video" || t == "image-to-video" {
			return "video"
		}
	}
	return ""
}

// FetchModels 按出站 realm 调上游动态模型接口：
//
//	cn → /console/enterprises/personal/models（控制台接口，返回 models + cli agent 名单）
//	ai / codebuddy → /v3/config（服务端下发的产品配置，与桌面端 UI 同源；codebuddy 域返回同一份）
//
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	if realm.IsIntlLine(c.realmOf(a)) {
		return c.fetchProductConfigModels(a)
	}
	return c.fetchConsoleModels(a)
}

// fetchConsoleModels CN 线动态模型接口。
func (c *Client) fetchConsoleModels(a *auth.Auth) ([]ModelInfo, error) {
	a = a.Snapshot()
	url := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string   `json:"id"`
				Name            string   `json:"name"`
				MaxInputTokens  int64    `json:"maxInputTokens"`
				MaxOutputTokens int64    `json:"maxOutputTokens"`
				Disabled        bool     `json:"disabled"`
				Tags            []string `json:"tags"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]struct {
		ID              string
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
		Efforts         []string
		Tags            []string
	}, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = struct {
			ID              string
			Name            string
			MaxInputTokens  int64
			MaxOutputTokens int64
			Disabled        bool
			Efforts         []string
			Tags            []string
		}{m.ID, m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled, m.Reasoning.SupportedEfforts, m.Tags}
	}
	out := make([]ModelInfo, 0, len(cliIDs)+len(env.Data.Models))
	seen := make(map[string]bool, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Efforts,
			Tags:          m.Tags,
		})
	}
	// 追加媒体模型（若该接口也下发 tags；cn 侧未实测，无 tags 时自然为空）。
	for _, m := range env.Data.Models {
		if m.Disabled || seen[m.ID] || !HasMediaTag(m.Tags) {
			continue
		}
		seen[m.ID] = true
		out = append(out, ModelInfo{ID: m.ID, Name: m.Name, Tags: m.Tags})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortCacheRef().set(cache)
	enrichModelCatalog(raw, out)
	return out, nil
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	return c.UserResourceContext(context.Background(), a)
}

func (c *Client) UserResourceContext(ctx context.Context, a *auth.Auth) (remain int64, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingJSONContext(ctx, a, http.MethodPost, billingMeterPath, body)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

// UserResourceWithTimeout 与 UserResource 相同，但用独立的短超时 client。
// 供"响应写完后的余额快照"这类**非关键路径**调用使用：迟到的观测没有价值，
// 快速失败（调用方按失败记 null）比占住连接更合适。
// timeout<=0 或未配 HTTP 时回落 UserResource 的原行为。
func (c *Client) UserResourceWithTimeout(a *auth.Auth, timeout time.Duration) (int64, error) {
	if timeout <= 0 || c.HTTP == nil {
		return c.UserResource(a)
	}
	// 浅拷贝 Client 只换 HTTP：其余字段（base / 出站头 / realm 档案）全部沿用，
	// 避免为一次查询再构造一份配置。Transport 复用同一个连接池。
	clone := *c
	clone.HTTP = &http.Client{Timeout: timeout, Transport: c.HTTP.Transport}
	return clone.UserResource(a)
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.billingJSON(a, http.MethodPost, dailyCheckinPath, map[string]any{})
	return err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// fetchProductConfigModels WorkBuddy AI 线：GET /v3/config 取服务端下发的产品配置。
//
// 关键（实测）：服务端按**出站指纹**区分配置版本，同一端点返回的名单不同——
//   - CLI 指纹（X-Product: SaaS + CLI/<v> UA）→ cli agent 名单 17 个
//   - 桌面指纹（X-Product: WorkBuddy + X-IDE-Type/Name/Version）→ 20 个（与桌面端 UI 一致，
//     含 hy4-preview-f / hy3 / deepseek-v4.1-flash / gpt-6-astra 等）
//
// 因此这里复用 CommonHeaders + applyClientIdentity（即与请求聊天时同一套身份），
// 保证"模型列表 = 出站身份对应的那份"；要拿到桌面端 UI 一致的列表，
// 把该 realm 的指纹配成 workbuddy-desktop 即可（见 upstream.realm_overrides）。
func (c *Client) fetchProductConfigModels(a *auth.Auth) ([]ModelInfo, error) {
	a = a.Snapshot()
	url := c.chatBase(a) + "/v3/config"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
	c.applyClientIdentity(req, a) // X-Product / X-IDE-*（仅桌面指纹带 X-IDE-*）

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("product config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string   `json:"id"`
				Name            string   `json:"name"`
				MaxInputTokens  int64    `json:"maxInputTokens"`
				MaxOutputTokens int64    `json:"maxOutputTokens"`
				Disabled        bool     `json:"disabled"`
				Tags            []string `json:"tags"`
				Reasoning       struct {
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("product config parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("product config code=%d", env.Code)
	}
	// 未鉴权时上游给空配置（models=null）→ 明确报错，由调用方回落静态表。
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("product config: no cli agent models")
	}
	idx := make(map[string]int, len(env.Data.Models))
	for i, m := range env.Data.Models {
		idx[m.ID] = i
	}
	out := make([]ModelInfo, 0, len(cliIDs)+len(env.Data.Models))
	seen := make(map[string]bool, len(cliIDs))
	for _, id := range cliIDs {
		i, ok := idx[id]
		if !ok {
			// cli 名单里的 id 未出现在 models 数组时仍透出（拿不到上下文就留 0）。
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, ModelInfo{ID: id, Name: id})
			continue
		}
		m := env.Data.Models[i]
		if m.Disabled || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Reasoning.SupportedEfforts,
			Tags:          m.Tags,
		})
	}
	// 追加**媒体模型**（图片 / 视频）：它们不在 cli agent 名单里，但服务端用 tags 标了能力，
	// 一并透出客户端才能发现文生图 / 图生图 / 视频该用哪个模型名。
	for _, m := range env.Data.Models {
		if m.Disabled || seen[m.ID] || !HasMediaTag(m.Tags) {
			continue
		}
		seen[m.ID] = true
		out = append(out, ModelInfo{ID: m.ID, Name: m.Name, Tags: m.Tags})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("product config returned empty cli model list")
	}
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortCacheRef().set(cache)
	enrichModelCatalog(raw, out)
	return out, nil
}

// fallbackEfforts 包级兜底能力表。
//
// 手工构造（未走 New）的 Client 没有 efforts 实例：若写入直接丢弃，FetchModels 拿到的
// 能力表就永远读不回来（reasoning_effort 降级静默失效）。能力表是"模型 → 支持档位"的
// 只读映射（同一模型无论哪条线都一样），因此跨 client 共享无副作用。
// 生产路径（New()）始终有实例缓存，这里只是给注入/测试用的兜底。
var fallbackEfforts = newEffortCache()

// effortCacheRef 返回本 client 的能力表，必要时回落包级兜底表。
func (c *Client) effortCacheRef() *effortCache {
	if c == nil || c.efforts == nil {
		return fallbackEfforts
	}
	return c.efforts
}
