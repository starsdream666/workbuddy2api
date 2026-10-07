// admin.go 控制台：内嵌单页（/admin）+ 管理 API（/admin/api/*）。
//
// 用途：不装 CLI 也能在浏览器里
//  1. 触发登录授权（选产品线 → 拿授权链接 → 自动轮询 → 成功后落盘 + 热加载账号池）
//  2. 查看凭证数量与逐个账号的积分 / 健康 / 冷却 / 熔断 / 成功率
//
// 生产鉴权使用管理员会话 Cookie，写操作校验 CSRF；分发 Key 没有管理权限。
// 静态登录页不含私有数据。仅未注入 Access 的历史嵌入调用保留旧鉴权逻辑。
package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/oauth"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

//go:embed console.html
var consoleHTML []byte

// Embedded assets keep the console self-contained: no CDN or frontend build step.
//
//go:embed console/console.css
var consoleCSS []byte

//go:embed console/metrics.js
var consoleMetricsJS []byte

//go:embed console/console.js
var consoleJS []byte

//go:embed console/tasks.js
var consoleTasksJS []byte

//go:embed console/models.js
var consoleModelsJS []byte

//go:embed console/packages.js
var consolePackagesJS []byte

//go:embed console/access.js
var consoleAccessJS []byte

//go:embed console/settings.js
var consoleSettingsJS []byte

// loginSessionTTL 控制台授权会话有效期（超过即视为过期，避免内存里堆积僵尸 state）。
const loginSessionTTL = 10 * time.Minute

// loginSession 控制台发起的一次授权。
type loginSession struct {
	state     string
	realmName string
	createdAt time.Time
}

// adminState 控制台运行时状态（凭证刷新时间戳 + 待完成授权会话）。
type adminState struct {
	authMu        sync.Mutex // 串行化控制台落盘、重扫与删除，避免相互覆盖。
	mu            sync.Mutex
	creditsAt     map[string]time.Time // uid → 上次余额刷新时间
	loginSessions map[string]loginSession
}

func newAdminState() *adminState {
	return &adminState{
		creditsAt:     map[string]time.Time{},
		loginSessions: map[string]loginSession{},
	}
}

// authorizedAny 控制台鉴权：全局 api_key 或任一 realm 的 key 均可访问。
// 两者都未配置时返回 true（与网关其它端点一致的"无 key = 不鉴权"语义）。
func (h *Handler) authorizedAny(token string) bool {
	if h.cfg.APIKey == "" && len(h.cfg.RealmKeys) == 0 {
		return true
	}
	if h.cfg.APIKey != "" && secretEqual(token, h.cfg.APIKey) {
		return true
	}
	for _, rn := range realm.All() {
		if k, ok := h.cfg.RealmKeys[rn]; ok && k != "" && secretEqual(token, k) {
			return true
		}
	}
	return false
}

func (h *Handler) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	if h.cfg.Access != nil {
		return h.withAdminSession(next)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authorizedAny(bearerToken(r)) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// registerAdmin 挂载控制台路由（NewHandler 内调用）。
func (h *Handler) registerAdmin() {
	h.mux.HandleFunc("GET /admin/api/tasks", h.withAdminAuth(h.adminTasks))
	h.mux.HandleFunc("POST /admin/api/tasks", h.withAdminAuth(h.adminTaskCreate))
	h.mux.HandleFunc("DELETE /admin/api/tasks/{id}", h.withAdminAuth(h.adminTaskCancel))
	h.mux.HandleFunc("GET /admin", h.consolePage)
	h.mux.HandleFunc("GET /admin/", h.consolePage)
	for _, asset := range []struct {
		path, contentType string
		body              []byte
	}{
		{"console.css", "text/css; charset=utf-8", consoleCSS},
		{"metrics.js", "text/javascript; charset=utf-8", consoleMetricsJS},
		{"console.js", "text/javascript; charset=utf-8", consoleJS},
		{"tasks.js", "text/javascript; charset=utf-8", consoleTasksJS},
		{"models.js", "text/javascript; charset=utf-8", consoleModelsJS},
		{"packages.js", "text/javascript; charset=utf-8", consolePackagesJS},
		{"access.js", "text/javascript; charset=utf-8", consoleAccessJS},
		{"settings.js", "text/javascript; charset=utf-8", consoleSettingsJS},
	} {
		h.mux.HandleFunc("GET /admin/assets/"+asset.path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", asset.contentType)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(asset.body)
		})
	}
	h.mux.HandleFunc("GET /admin/api/overview", h.withAdminAuth(h.adminOverview))
	// Settings are available only with administrator sessions, including embeds.
	if h.cfg.Access != nil {
		h.mux.HandleFunc("GET /admin/api/settings", h.withAdminAuth(h.adminSettings))
		h.mux.HandleFunc("PATCH /admin/api/settings", h.withAdminAuth(h.adminSaveSettings))
	}
	// 模型列表：聚合全部启用线的动态模型（含上游倍率 credits），供控制台展示。
	h.mux.HandleFunc("GET /admin/api/models", h.withAdminAuth(h.adminModels))
	// 上游账号身份信息；套餐积分与有效期使用 account-packages。
	h.mux.HandleFunc("GET /admin/api/upstream-accounts", h.withAdminAuth(h.adminUpstreamAccounts))
	h.mux.HandleFunc("GET /admin/api/account-packages", h.withAdminAuth(h.adminAccountPackages))
	h.mux.HandleFunc("POST /admin/api/balance", h.withAdminAuth(h.adminBalance))
	h.mux.HandleFunc("POST /admin/api/login/start", h.withAdminAuth(h.adminLoginStart))
	h.mux.HandleFunc("GET /admin/api/login/poll", h.withAdminAuth(h.adminLoginPoll))
	h.mux.HandleFunc("POST /admin/api/reload", h.withAdminAuth(h.adminReload))
	h.mux.HandleFunc("DELETE /admin/api/accounts", h.withAdminAuth(h.adminDeleteAccount))
	// 人工启停：与 DELETE 同资源路径（改同一批账号的 enabled 属性），PATCH 语义。
	h.mux.HandleFunc("PATCH /admin/api/accounts", h.withAdminAuth(h.adminSetAccountEnabled))
	h.mux.HandleFunc("GET /admin/api/usage", h.withAdminAuth(h.adminUsage))
	// 清空使用日志（DELETE 语义：抹掉历史观测数据）。与 GET 同资源路径。
	h.mux.HandleFunc("DELETE /admin/api/usage", h.withAdminAuth(h.adminUsageClear))
	// 状态变化实时推送（SSE 长连接）。前端收到信号后去拉 overview/usage 取全量。
	h.mux.HandleFunc("GET /admin/api/events", h.withAdminAuth(h.adminEvents))
}

func (h *Handler) consolePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(consoleHTML)
}

// oauthHTTP 控制台 OAuth 请求复用的 HTTP 客户端（与短 RPC 同款：统一的超时/代理/连接池）。
func (h *Handler) oauthHTTP() *http.Client {
	if up := h.CurrentRuntime().Upstream; up != nil {
		return up.HTTP
	}
	return nil
}

// accountView 控制台展示用的账号视图（池状态 + realm 归属）。
type accountView struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Realm    string `json:"realm"`
	Credits  int64  `json:"credits"`
	// CreditsExact 是池内整数基准减去本地小数消耗后的估算余额。
	// 上游的精确小数字段另见 account-packages，不应与该估算值混用。
	CreditsExact float64 `json:"credits_exact"`
	// CreditsKnown 余额是否被上游真实观测过。
	// 前端必须据此区分"未知"与"为 0"：前者显示 —，后者显示 0（语义完全不同：
	// 未知要优先探测，0 是已耗尽）。缺这个字段前端只能把两者混为一谈。
	CreditsKnown   bool       `json:"credits_known"`
	CreditsChecked *time.Time `json:"credits_checked_at,omitempty"`
	Healthy        bool       `json:"healthy"`
	Cooling        bool       `json:"cooling"`
	CoolRemaining  int64      `json:"cool_remaining_seconds"`
	CoolKind       string     `json:"cool_kind,omitempty"`
	Disabled       bool       `json:"disabled"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	// ManualDisabled 人工停用层（控制台开关）：与 Disabled（系统判定）正交，
	// 两者可同时为真。停用口径 = disabled || manual_disabled，控制台据此显示与筛选。
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// Frozen 额度冻结：余额为 0，等额度巡检确认恢复才解冻（不是时间冷却）。
	Frozen       bool   `json:"frozen"`
	FrozenReason string `json:"frozen_reason,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// Selection* 账号级选号配置（排除 / 落位 / 自定义优先级）：控制台据此显示与编辑。
	// 与停用正交——被排除的号照常健康、照常参与调度，只是不再按策略排序。
	SelectionExcluded  bool       `json:"selection_excluded"`
	SelectionPlacement string     `json:"selection_placement,omitempty"`
	SelectionPriority  int        `json:"selection_priority"`
	SuccessCount       int64      `json:"success_count"`
	ErrTotal           int64      `json:"err_total"`
	InFlight           int        `json:"in_flight"`
	BreakerFails       int        `json:"breaker_fails"`
	Until              *time.Time `json:"until,omitempty"`
	HasRefresh         bool       `json:"has_refresh_token"`
}

// adminOverview 总览：网关身份 + 各 realm 池的账号明细 + 汇总。
func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	realms := h.activeRealms()
	accounts := make([]accountView, 0, 8)
	total, healthy, cooling, disabled, manualN, frozenN := 0, 0, 0, 0, 0, 0
	var creditsSum float64 // 精确值合计（含小数扣减），与各行 credits_exact 之和一致

	h.admin.mu.Lock()
	creditsAt := make(map[string]time.Time, len(h.admin.creditsAt))
	for k, v := range h.admin.creditsAt {
		creditsAt[k] = v
	}
	h.admin.mu.Unlock()

	routes := map[string]string{}
	for _, rn := range realms {
		// 路线别名（codebuddy）与来源线（workbuddy）**共用同一个池对象**：同一批账号、同一份钱包。
		// 别名必须跳过，否则同一账号会在凭证表里出现两次、totals 也跟着翻倍。
		// 去重口径与 /status、adminBalance 一致：别名只登记进 routes（别名 → 来源线），不参与计数。
		if src := realm.AuthRealmOf(rn); src != rn {
			routes[rn] = src
			continue
		}
		pl := h.poolFor(rn)
		if pl == nil {
			continue
		}
		for _, st := range pl.List() {
			total++
			switch {
			case st.Disabled:
				// 系统层优先归类：两层同时为真时只计入 disabled，
				// 使 disabled + manual_disabled 恰好等于停用总数（互斥口径，前端可直接求和）。
				disabled++
			case st.ManualDisabled:
				manualN++
			case st.Frozen:
				// 冻结是"额度耗尽、等恢复"，既不是冷却也不是健康——单列，
				// 否则控制台会把一个余额为 0 的号显示成健康。
				frozenN++
			case st.Cooling:
				cooling++
			default:
				healthy++
			}
			// 合计也走精确值：否则"各行之和不等于合计"，一眼就看出对不上。
			creditsSum += st.CreditsExact
			v := accountView{
				UID:            st.UID,
				Nickname:       st.Nickname,
				Realm:          rn,
				Credits:        st.Credits,
				CreditsExact:   st.CreditsExact,
				CreditsKnown:   st.CreditsKnown,
				Healthy:        !st.Cooling && !st.Disabled && !st.ManualDisabled && !st.Frozen,
				Cooling:        st.Cooling,
				CoolRemaining:  st.CoolRemaining,
				CoolKind:       st.CoolKind,
				Disabled:       st.Disabled,
				DisabledReason: st.DisabledReason,
				ManualDisabled: st.ManualDisabled,
				ManualReason:   st.ManualReason,
				Frozen:         st.Frozen,
				FrozenReason:   st.FrozenReason,
				Reason:         st.Reason,
				SuccessCount:   st.SuccessCount,
				ErrTotal:       st.ErrTotal,
				InFlight:       st.InFlight,
				BreakerFails:   st.BreakerFails,

				SelectionExcluded:  st.SelectionExcluded,
				SelectionPlacement: st.SelectionPlacement,
				SelectionPriority:  st.SelectionPriority,
			}
			if !st.Until.IsZero() {
				u := st.Until
				v.Until = &u
			}
			if t, ok := creditsAt[st.UID]; ok {
				tt := t
				v.CreditsChecked = &tt
			}
			if a := pl.AuthByUID(st.UID); a != nil {
				v.HasRefresh = strings.TrimSpace(a.RefreshTokenValue()) != ""
			}
			accounts = append(accounts, v)
		}
	}

	body := map[string]any{
		"service":       ServiceName,
		"default_realm": realm.Normalize(h.cfg.DefaultRealm),
		"realms":        realms,
		"auth_dir":      h.cfg.AuthDir,
		"console":       h.cfg.ConsoleEnabled,
	}
	if len(routes) > 0 {
		body["routes"] = routes
	}
	body["totals"] = map[string]any{
		"accounts": total,
		"healthy":  healthy,
		"cooling":  cooling,
		"disabled": disabled,
		// manual_disabled 与 disabled **互斥**（两层同时为真只计 disabled）：
		// 两者相加 = 当前不参与调度的账号总数，前端据此显示明细而不重复计数。
		"manual_disabled": manualN,
		"frozen":          frozenN,
		"credits":         creditsSum,
	}
	body["accounts"] = accounts
	body["now"] = time.Now()
	writeJSON(w, http.StatusOK, body)
}

// adminModelView 控制台模型列表的一行：裸模型名 + 归属线 + 上游倍率等元数据。
// 与 /v1/models 的差异：这里用**裸模型名**（不带渠道前缀），并额外给出解析后的倍率数值。
type adminModelView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Realm         string   `json:"realm"`
	Channel       string   `json:"channel,omitempty"`
	Credits       string   `json:"credits,omitempty"`    // 上游原始倍率文本（如 "x0.34 credits" / "x0.00"）
	Rate          *float64 `json:"rate,omitempty"`       // 解析后的倍率数值（免费模型为 0）
	Vendor        string   `json:"vendor,omitempty"`     // 可读厂商名（由模型 id 推导）
	RawVendor     string   `json:"raw_vendor,omitempty"` // 上游原始 vendor 字段（内部单字母枚举 e/i/f/j，无图例）
	DefaultEffort string   `json:"default_effort,omitempty"`
	Efforts       []string `json:"supported_efforts,omitempty"` // 实际可选档位（无 supportedEfforts 时为官方全档）
	ContextWindow int64    `json:"context_length"`
	MaxTokens     int64    `json:"max_output_tokens"`
	Kind          string   `json:"kind,omitempty"` // image / video（媒体模型）
	Tags          []string `json:"tags,omitempty"`
}

// adminModels 聚合全部启用线的动态模型列表（含上游倍率），供控制台「模型列表」页展示。
// 数据源与 /v1/models 完全一致（fetchDynamicModelsFor → FetchModels → enrichModelCatalog），
// 因此倍率、上下文、effort、媒体标签都与网关对外暴露的模型表同源。
func (h *Handler) adminModels(w http.ResponseWriter, r *http.Request) {
	models := make([]adminModelView, 0, 64)
	for _, rn := range h.activeRealms() {
		infos := h.fetchDynamicModelsFor(rn)
		prefix := realm.ChannelPrefixFor(rn, h.channelPrefixes())
		for _, mi := range infos {
			v := adminModelView{
				ID:            mi.ID,
				Name:          mi.Name,
				Realm:         rn,
				Channel:       prefix,
				Credits:       mi.Credits,
				Vendor:        upstream.ModelVendor(mi.ID),
				RawVendor:     mi.Vendor,
				DefaultEffort: mi.DefaultEffort,
				Efforts:       upstream.EffectiveEfforts(mi.Efforts, mi.DefaultEffort),
				ContextWindow: mi.ContextWindow,
				MaxTokens:     mi.MaxTokens,
				Tags:          mi.Tags,
			}
			if kind := upstream.MediaKind(mi.Tags); kind != "" {
				v.Kind = kind
			}
			if rate, known := upstream.ParseModelRate(mi.Credits); known {
				v.Rate = &rate
			}
			models = append(models, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"count":  len(models),
		"data":   models,
	})
}

// balanceRequest 刷新余额的请求体：uid 为空 = 刷新全部账号。
type balanceRequest struct {
	UID string `json:"uid"`
}

// adminBalance 查询上游余额并写回池（只读接口，不消耗额度）。
func (h *Handler) adminBalance(w http.ResponseWriter, r *http.Request) {
	var req balanceRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	updated := make(map[string]int64, 4)
	failed := make(map[string]string, 2)
	for _, rn := range h.activeRealms() {
		// 路线别名（codebuddy）与来源线（ai）是同一批账号、同一份钱包：
		// 跳过别名，避免同一账号在控制台上被刷两次、计数翻倍。
		if realm.IsRouteAlias(rn) {
			continue
		}
		pl := h.poolFor(rn)
		if pl == nil {
			continue
		}
		for _, st := range pl.List() {
			if req.UID != "" && st.UID != req.UID {
				continue
			}
			a := pl.AuthByUID(st.UID)
			if a == nil || strings.TrimSpace(a.AccessTokenValue()) == "" {
				continue
			}
			remain, err := h.upstreamFor(rn).UserResource(a)
			if err != nil {
				failed[st.UID] = err.Error()
				continue
			}
			// 与系统判定保持一致：余额为 0 → 冻结（移出轮转），恢复 → 解冻，
			// 而不是只改显示的数字。所有真实上游观测路径（请求出口的余额刷新、
			// 额度巡检）都走 ReconcileCredits，手动刷新没理由更弱——
			// 否则控制台显示 0、账号却仍在轮转里白撞 429/402，与实际调度状态脱节。
			pl.ReconcileCredits(st.UID, remain)
			updated[st.UID] = remain
			h.admin.mu.Lock()
			h.admin.creditsAt[st.UID] = time.Now()
			h.admin.mu.Unlock()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "failed": failed})
}

// adminUsage 返回最近的使用日志 + 磁盘占用快照 + 累计统计快照
// （供控制台「使用日志」面板与「累计统计」区块）。
//
// 参数 limit 可选：正整数只返回最近 N 条；省略或非正值返回全部保留记录。
// 返回 entries 按时间正序（最早→最新），前端自行决定展示顺序。
//
//   - entries 覆盖主文件与备份；未落盘时返回现有内存记录。
//   - summary 是**累计**（扫描日志文件覆盖的全部记录），用于总量与分维度统计。
//     它不受 limit 影响——否则同一个数字会随 limit 变化而变，账就没有意义了。
//
// 未启用使用日志时仍返回 200（enabled=false + 空 entries + 全零 summary），
// 让控制台可以直接渲染"未启用"状态，而不是把 404 当成错误处理。
func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	_, _, file, enabled := h.cfg.UsageLog.Stats()
	entries, incomplete := h.cfg.UsageLog.History()
	kept := len(entries)
	if limit > 0 && limit < kept {
		entries = entries[kept-limit:]
	}
	// 占用快照：把"会不会爆炸"变成可看的数字（占用 / 上限 / 上次清空 / 落盘错误）。
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            enabled,
		"file":               file,
		"total":              kept,
		"kept":               kept,
		"size":               h.cfg.UsageLog.SizeInfo(),
		"summary":            h.cfg.UsageLog.Summary(),
		"entries":            entries,
		"history_incomplete": incomplete,
	})
}

// adminUsageClear 清空使用日志（内存缓冲 + 落盘文件 + 全部备份 + 累计统计缓存）。
//
// 破坏性操作但可安全重试：日志是运行观测，没有业务状态依赖它。
// 返回删除的字节数与**清空后的统计快照**，让控制台能提示"释放了多少"并立即归零。
// 未启用/未注入日志器时返回 200 + removed=0（幂等，前端不必特判）。
func (h *Handler) adminUsageClear(w http.ResponseWriter, r *http.Request) {
	removed := h.cfg.UsageLog.Clear()
	_, _, file, enabled := h.cfg.UsageLog.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"enabled":    enabled,
		"file":       file,
		"removed":    removed,
		"cleared_at": time.Now(),
		"size":       h.cfg.UsageLog.SizeInfo(),
		// 清空后统计必然归零，但前端不该靠"自己猜零"来收口：
		// 一并回传权威快照，控制台就能立刻把累计卡片归零（不必等下一次轮询对齐）。
		"summary": h.cfg.UsageLog.Summary(),
	})
}

// loginStartRequest 控制台发起授权。
type loginStartRequest struct {
	Realm string `json:"realm"`
}

// adminLoginStart 调上游 auth/state 拿授权链接（不落盘，等 poll 成功才落）。
func (h *Handler) adminLoginStart(w http.ResponseWriter, r *http.Request) {
	var req loginStartRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	rn := realm.Normalize(req.Realm)
	prof := h.cfg.Upstream.ProfileFor(rn)

	sess, err := oauth.StartWith(prof, h.oauthHTTP())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	h.admin.mu.Lock()
	// 顺手清理过期会话，避免内存里堆僵尸 state。
	for st, s := range h.admin.loginSessions {
		if time.Since(s.createdAt) > loginSessionTTL {
			delete(h.admin.loginSessions, st)
		}
	}
	h.admin.loginSessions[sess.State] = loginSession{state: sess.State, realmName: rn, createdAt: sess.CreatedAt}
	h.admin.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"state":              sess.State,
		"realm":              rn,
		"auth_url":           sess.AuthURL,
		"expires_in_seconds": int(loginSessionTTL.Seconds()),
	})
}

// adminLoginPoll 轮询一次授权结果；完成后落盘凭证并热加载账号池。
func (h *Handler) adminLoginPoll(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing state"})
		return
	}
	h.admin.mu.Lock()
	sess, ok := h.admin.loginSessions[state]
	h.admin.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"status": "unknown", "error": "授权会话不存在或已过期，请重新点「开始授权」"})
		return
	}
	if time.Since(sess.createdAt) > loginSessionTTL {
		h.admin.mu.Lock()
		delete(h.admin.loginSessions, state)
		h.admin.mu.Unlock()
		writeJSON(w, http.StatusGone, map[string]any{"status": "expired", "error": "授权会话已过期，请重新点「开始授权」"})
		return
	}

	bundle, err := oauth.PollWith(h.cfg.Upstream.ProfileFor(sess.realmName), state, h.oauthHTTP())
	if err != nil {
		if errors.Is(err, oauth.ErrPending) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	if strings.TrimSpace(bundle.UID) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "上游未返回 uid（token 已获取但账号信息缺失），请重试"})
		return
	}
	if strings.TrimSpace(bundle.AccessToken) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "上游未返回 accessToken"})
		return
	}
	if !auth.ValidUID(bundle.UID) {
		writeJSON(w, http.StatusBadGateway, map[string]any{"status": "error", "error": "上游返回的 uid 格式无效"})
		return
	}

	h.admin.authMu.Lock()
	defer h.admin.authMu.Unlock()

	// 落盘：文件名与 login.sh 保持一致（cn: workbuddy-<uid>.json；ai: workbuddy-ai-<uid>.json）。
	dir := h.cfg.AuthDir
	if dir == "" {
		dir = "./auths"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "error", "error": "创建 auth 目录失败: " + err.Error()})
		return
	}
	// 凭证按**来源线**归档（路线别名与来源线共用同一份账号/额度）。
	//
	// 例：在控制台选 codebuddy 路线登录 → 落盘仍是 workbuddy-ai-<uid>.json、realm=workbuddy，
	// 因为该 token 本就属于国际线（两个域通用）。若按别名线归档，重扫后会另起一个池，
	// 同一账号被两套状态重复记账（甚至二次签到），与"复用同一批账号"的设计相悖。
	// 文件名规则统一走 auth.FilePrefixFor（与 login.sh / importauth 同一套）。
	authRealm := realm.AuthRealmOf(sess.realmName)
	file := auth.AuthFileFor(dir, authRealm, bundle.UID)
	expiresAt := int64(0)
	if bundle.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(bundle.ExpiresIn) * time.Second).Unix()
	}
	a := &auth.Auth{
		AccessToken:  bundle.AccessToken,
		RefreshToken: bundle.RefreshToken,
		ExpiresAt:    expiresAt,
		Domain:       bundle.Domain,
		Realm:        authRealm,
		UID:          bundle.UID,
		EnterpriseID: bundle.EnterpriseID,
		Nickname:     bundle.Nickname,
		FilePath:     file,
	}
	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "error", "error": "写入凭证失败: " + err.Error()})
		return
	}

	// 热加载：重扫 auth 目录并对齐各 realm 池，新号立即可用（不必重启进程/容器）。
	perRealm := map[string]int{}
	reloadErr := ""
	if h.cfg.ReloadAuths != nil {
		counts, err := h.cfg.ReloadAuths()
		if err != nil {
			reloadErr = err.Error()
		} else {
			perRealm = counts
		}
	}

	h.admin.mu.Lock()
	delete(h.admin.loginSessions, state)
	h.admin.creditsAt[bundle.UID] = time.Now()
	h.admin.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "done",
		"uid":          bundle.UID,
		"nickname":     bundle.Nickname,
		"realm":        sess.realmName,
		"file":         file,
		"expires_at":   expiresAt,
		"reloaded":     perRealm,
		"reload_error": reloadErr,
	})
}

// adminDeleteAccount 只接受 realm + uid，文件路径只能来自已加载的账号。
// 成功后立即剔除对应池条目；不重扫其他账号，保留其在途请求与统计状态。
func (h *Handler) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
		UID   string `json:"uid"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.UID) == "" ||
		strings.TrimSpace(req.Realm) == "" || !realm.Known(req.Realm) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请提供有效的 realm 和 uid"})
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体必须是单个 JSON 对象"})
		return
	}
	rn := realm.Normalize(req.Realm)
	h.admin.authMu.Lock()
	defer h.admin.authMu.Unlock()

	// 不使用 poolFor 的默认池回退：删除请求不能跨产品线命中其他账号。
	pl := h.pools()[rn]
	if len(h.pools()) == 0 && rn == realm.Normalize(h.cfg.DefaultRealm) {
		pl = h.cfg.Pool
	}
	var a *auth.Auth
	if pl != nil {
		a = pl.AuthByUID(req.UID)
	}
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "凭证不存在，请刷新列表"})
		return
	}
	dir := h.cfg.AuthDir
	if dir == "" {
		dir = "./auths"
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "无法解析凭证目录"})
		return
	}
	file, err := filepath.Abs(a.FilePath)
	if err != nil || a.FilePath == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "该凭证没有可删除的来源文件"})
		return
	}
	rel, err := filepath.Rel(root, file)
	matched, _ := filepath.Match("workbuddy*.json", filepath.Base(file))
	if err != nil || filepath.Dir(rel) != "." || !matched {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅允许删除凭证目录内的 workbuddy*.json 文件"})
		return
	}
	info, err := os.Lstat(file)
	if err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "无法检查凭证文件"})
		return
	}
	if err == nil && !info.Mode().IsRegular() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "不允许删除目录或符号链接"})
		return
	}
	// 同 UID 同产品线若存在多个来源文件，拒绝部分删除，避免下次重扫悄悄恢复。
	loaded, err := auth.LoadDir(root)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "无法检查重复凭证"})
		return
	}
	for _, other := range loaded {
		otherRealm := realm.Normalize(other.Realm)
		if strings.TrimSpace(other.Realm) == "" {
			otherRealm = realm.Normalize(h.cfg.DefaultRealm)
		}
		if other.FilePath == file {
			if other.UID != req.UID || otherRealm != rn {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "来源文件已变更，请先重扫凭证目录"})
				return
			}
		} else if other.UID == req.UID && otherRealm == rn {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "该账号存在重复凭证文件，请先在凭证目录中整理后重试"})
			return
		}
	}
	if err := a.DeleteFile(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "删除凭证失败，请检查目录写入权限"})
		return
	}
	pl.Remove(req.UID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": req.UID, "realm": rn})
}

// adminReload 手动重扫凭证目录（改/删 auth 文件后不必重启进程）。
func (h *Handler) adminReload(w http.ResponseWriter, r *http.Request) {
	h.admin.authMu.Lock()
	defer h.admin.authMu.Unlock()
	if h.cfg.ReloadAuths == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "该实例未提供凭证重载能力"})
		return
	}
	counts, err := h.cfg.ReloadAuths()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": counts})
}
