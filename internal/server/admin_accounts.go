// admin_accounts.go 控制台人工启停凭证（PATCH /admin/api/accounts）。
//
// 与 DELETE 的分工：DELETE 物理删除本地凭证文件并把账号踢出池；本接口只切"人工停用层"，
// 凭证文件不动、系统层状态（12153 判死 / 冷却 / 额度冻结）不动——停用只是让它不参与选号
// 与后台调度任务（签到/旅行/活跃/保活），随时可以再点回来。两层语义见 internal/pool/manual.go。
package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
)

// accountEnabledRequest 启停请求体。enabled 用指针：字段缺失与显式 false 必须区分开，
// 否则一个 {"realm":..,"uid":..} 的空壳请求会被当成"停用"执行——这是不可逆误操作里
// 最容易踩的一类。
type accountEnabledRequest struct {
	Realm   string `json:"realm"`
	UID     string `json:"uid"`
	Enabled *bool  `json:"enabled"`
	// Selection 可选的账号级选号配置（排除 / 落位 / 自定义优先级）。
	//
	// 用指针而非值：**字段缺失**必须与"显式清空配置"区分开——
	// 一个只改启停的请求不该顺手把选号配置重置成默认值。
	// 传了才动（见 applyAccountSelection），不传则保持原值。
	Selection *accountSelectionRequest `json:"selection"`
}

// accountSelectionRequest 账号级选号配置的请求体。
// 字段全部用指针，理由同上：只有请求里出现的字段才会被写入。
type accountSelectionRequest struct {
	// Excluded 是否排除在选号策略之外。
	Excluded *bool `json:"excluded"`
	// Placement 排除后的落位：first（最优先使用）/ last（最后使用）/ ""（按 last）。
	Placement *string `json:"placement"`
	// Priority 自定义优先级（数值大者优先；仅 custom_priority 策略使用）。
	Priority *int `json:"priority"`
}

// adminSetAccountEnabled 账号设置端点（PATCH /admin/api/accounts）：人工启停 +
// 可选的账号级选号配置（排除 / 落位 / 自定义优先级）。
//
// 校验与删除同口径：realm 必须是已知产品线、uid 必须是该 realm 池里已加载的账号、
// 不回退默认池（避免跨产品线命中别人的账号）。持久化由池的后台 flusher 落 state.json，
// 重启后仍生效。
//
// 启用是"只摘手动层"：若该号同时被系统判死（disabled）或处于冷却/冻结，启用后依旧不可选——
// 本接口不替上游状态做决定，需要复活走重新登录或 ReviveDisabled。
//
// 请求契约：enabled 与 selection 至少要有一个（都是可选的，但不能都不给）；
// selection 内部的字段同样是"传了才动"——只改启停的请求不会顺手重置选号配置，
// 反之只改选号配置的请求也不会碰启停状态。
func (h *Handler) adminSetAccountEnabled(w http.ResponseWriter, r *http.Request) {
	var req accountEnabledRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体无效，请提供 realm、uid 与 enabled 或 selection"})
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体必须是单个 JSON 对象"})
		return
	}
	if strings.TrimSpace(req.UID) == "" || strings.TrimSpace(req.Realm) == "" || !realm.Known(req.Realm) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请提供有效的 realm 和 uid"})
		return
	}
	if req.Enabled == nil && req.Selection == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少可修改的字段（enabled 或 selection）"})
		return
	}
	// 落位取值在动手之前挡下来：静默回落会让"我配了但没生效"无从排查。
	// 校验的是**归一前**的原始输入——空串合法（表示未指定，按 last 解释）。
	if req.Selection != nil && req.Selection.Placement != nil && !pool.ValidPlacement(*req.Selection.Placement) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "selection.placement 只能是 first（最优先使用）或 last（最后使用）"})
		return
	}
	rn := realm.Normalize(req.Realm)

	pl := h.pools()[rn]
	if len(h.pools()) == 0 && rn == realm.Normalize(h.cfg.DefaultRealm) {
		pl = h.cfg.Pool
	}
	if pl == nil || pl.AuthByUID(req.UID) == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "凭证不存在，请刷新列表"})
		return
	}

	// 先改选号配置再改启停：两者互不影响，但选号配置的"字段缺失即不改"语义
	// 依赖当前值做合并，放在前面读改一致。
	if req.Selection != nil {
		cur, ok := pl.AccountSelection(req.UID)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "凭证不存在，请刷新列表"})
			return
		}
		next := cur
		if req.Selection.Excluded != nil {
			next.Excluded = *req.Selection.Excluded
		}
		if req.Selection.Placement != nil {
			next.Placement = *req.Selection.Placement
		}
		if req.Selection.Priority != nil {
			next.Priority = *req.Selection.Priority
		}
		if !pl.SetAccountSelection(req.UID, next) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "凭证不存在，请刷新列表"})
			return
		}
		log.Printf("INFO: [admin] realm=%s uid=%s selection excluded=%v placement=%q priority=%d",
			rn, logfmt.UID8(req.UID), next.Excluded, next.Placement, next.Priority)
	}

	disabled := false
	if req.Enabled != nil {
		disabled = !*req.Enabled
		if !pl.SetManualDisabled(req.UID, disabled) {
			// AuthByUID 与 SetManualDisabled 之间账号被删除/重扫剔除：按不存在处理，
			// 不能让控制台拿到一个"成功"却什么都没发生。
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "凭证不存在，请刷新列表"})
			return
		}
		log.Printf("INFO: [admin] realm=%s uid=%s manual_disabled=%v", rn, logfmt.UID8(req.UID), disabled)
	} else {
		// 未传 enabled：回显该号当前的人工停用状态，让前端不必再猜。
		disabled = pl.ManualDisabled(req.UID)
	}

	sel, _ := pl.AccountSelection(req.UID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"uid":             req.UID,
		"realm":           rn,
		"manual_disabled": disabled,
		"selection":       sel,
	})
}
