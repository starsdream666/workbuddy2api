// media.go 媒体与账号类端点：图片生成/编辑、视频生成与任务查询、上游账号信息。
//
// 与 chat 共用同一套分流与调度：X-Realm / realm 专属 key / 渠道前缀（`workbuddy/<模型>`）
// 决定走哪条线的账号池，出站档案按该线解析；失败同样按 applyErrorPolicyOn 处置账号。
// 差异只在两点：
//  1. 一次性 JSON（非 SSE）；
//  2. 响应把上游 `data` **原样**回给调用方 —— 媒体结果字段是上游的地盘，网关不解释、
//     不重排，避免与上游演进赛跑（字段来源见 internal/upstream/media.go 注释）。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

// mediaKind 媒体端点种类。
type mediaKind int

const (
	mediaImageGen mediaKind = iota
	mediaImageEdit
	mediaVideoGen
	mediaVideoTask
)

// defaultImageSize 上游默认画布（官方 CLI 未指定 size 时用 1024x1024）。
const defaultImageSize = "1024x1024"

// imageUpstreamFields 允许透传给上游的图片字段白名单（官方 CLI 实际会发这些）。
// 不在表里的字段直接丢弃：宁可少传，也不把客户端的杂项字段甩给上游。
var imageUpstreamFields = []string{
	"model", "prompt", "size", "response_format",
	"n", "quality", "style", "background",
	"footnote", "revise",
	"image", "input_fidelity",
}

func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	h.media(w, r, mediaImageGen)
}

func (h *Handler) imagesEdits(w http.ResponseWriter, r *http.Request) {
	h.media(w, r, mediaImageEdit)
}

func (h *Handler) videosGenerations(w http.ResponseWriter, r *http.Request) {
	h.media(w, r, mediaVideoGen)
}

func (h *Handler) videosTasks(w http.ResponseWriter, r *http.Request) {
	h.media(w, r, mediaVideoTask)
}

// media 媒体端点的统一处理：读体 → 渠道前缀/realm 解析 → 选号 → 调上游 → 原样回传。
func (h *Handler) media(w http.ResponseWriter, r *http.Request, kind mediaKind) {
	runtime := h.CurrentRuntime()
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

	// 路线解析：与 chat 同规则（前缀优先，其次 X-Realm / 专属 key / 默认线）。
	reqRealm := h.realmOfRequest(r)
	model := jsonStringField(body, "model")
	if channel, bareModel, prefixRealm := realm.SplitChannelPrefix(model, h.channelPrefixes()); channel != "" {
		if h.cfg.Access != nil && !h.issuedChannelAllowed(r, prefixRealm) {
			writeOpenAIError(w, 403, "channel_not_allowed", "该密钥不允许访问模型指定的渠道")
			return
		}
		if h.prefixAllowed(reqRealm, prefixRealm, bearerToken(r)) {
			reqRealm = prefixRealm
		} else {
			log.Printf("WARN: [server] media 模型前缀 %q 指向 %s，但本次凭据属于 %s → 仍按 %s 处理",
				channel, prefixRealm, reqRealm, reqRealm)
		}
		body = stripModelPrefix(body, bareModel)
		model = bareModel
	}

	if h.cfg.Access != nil && !h.issuedChannelAllowed(r, reqRealm) {
		writeOpenAIError(w, 403, "channel_not_allowed", "该密钥的渠道未授权或未启用")
		return
	}
	pl := h.poolFor(reqRealm)
	if pl == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_pool", "realm "+reqRealm+" 没有账号池")
		return
	}
	acct := pl.Pick()
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "all accounts unavailable (cooling/disabled)")
		return
	}
	up := runtime.upstreamFor(reqRealm)

	var raw json.RawMessage
	switch kind {
	case mediaImageGen:
		raw, err = up.GenerateImage(acct, mapImageRequest(body, model, false))
	case mediaImageEdit:
		raw, err = up.EditImage(acct, mapImageRequest(body, model, true))
	case mediaVideoGen:
		raw, err = up.SubmitVideo(acct, body)
	case mediaVideoTask:
		taskID := jsonStringField(body, "task_id")
		if taskID == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				`task_id 必填：POST /v1/videos/tasks {"task_id":"<submit 返回的 data.id>"}`)
			return
		}
		raw, err = up.VideoTask(acct, taskID)
	}

	if err != nil {
		kindCode := upstream.ErrClient
		var ue *upstream.Error
		if errors.As(err, &ue) {
			kindCode = ue.Kind
		}
		// 媒体请求的失败同样要反馈到账号健康度（402 冻结 / 429 冷却 / 12153 禁用…），
		// 否则 402 的号会被媒体流量反复选中白撞。
		h.applyErrorPolicyOn(pl, acct.UID, kindCode, err.Error(), model)
		writeOpenAIError(w, mediaErrorStatus(err), kindCode.String(), err.Error())
		return
	}
	pl.NoteSuccess(acct.UID)
	writeRawJSON(w, http.StatusOK, raw)
}

// mapImageRequest 把入参映射成上游图片端点认得的字段：
//   - 白名单过滤（避免把客户端的杂项字段甩给上游）；
//   - size 缺省补 1024x1024；
//   - 按模型名前缀分支：hunyuan-* 不发 response_format、只补 n=1；gemini-* 只发前三项；
//     其余补 response_format=b64_json（三支照官方 CLI 的 buildBaseRequestBody 抄）；
//   - edits：image 单值时归一成数组（上游收数组）。
//
// 解析失败 → 原样返回（不阻塞转发，交给上游按它自己的语义报错）。
func mapImageRequest(body []byte, model string, isEdit bool) []byte {
	if len(body) == 0 {
		return body
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return body
	}
	out := make(map[string]any, len(in)+3)
	for _, k := range imageUpstreamFields {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if model != "" {
		out["model"] = model
	}
	if s, _ := out["size"].(string); s == "" {
		out["size"] = defaultImageSize
	}
	// 按模型名前缀分支 —— 与官方 CLI 的 buildBaseRequestBody 逐支对齐：
	// hunyuan-* 分支 CLI **不发** response_format，只把 n 补成 1；
	// gemini-* 分支只发 model/prompt/size；其余（OpenAI 风格）分支才补 response_format=b64_json。
	switch {
	case strings.HasPrefix(model, "hunyuan-"):
		if _, ok := out["n"]; !ok {
			out["n"] = 1
		}
	case strings.HasPrefix(model, "gemini-"):
		// 什么都不加。
	default:
		if s, _ := out["response_format"].(string); s == "" {
			out["response_format"] = "b64_json"
		}
	}
	if isEdit {
		switch v := out["image"].(type) {
		case string:
			out["image"] = []any{v}
		case []any:
			// 已是数组，原样。
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return body
	}
	return raw
}

// jsonStringField 取顶层字符串字段（解析失败返回空串）。
func jsonStringField(body []byte, key string) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	s, _ := obj[key].(string)
	return s
}

// mediaErrorStatus 上游错误 → 客户端可见状态：优先沿用上游状态码，没有则 502。
func mediaErrorStatus(err error) int {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Status >= 400 {
		return ue.Status
	}
	return http.StatusBadGateway
}

// writeRawJSON 原样写出上游 JSON（不重新序列化，避免丢字段/改精度）。
func writeRawJSON(w http.ResponseWriter, status int, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(raw) > 0 {
		_, _ = w.Write(raw)
	} else {
		_, _ = w.Write([]byte("{}"))
	}
}

// adminUpstreamAccounts 控制台用：取上游账号/订阅信息（GET /v2/accounts，原样透传 data）。
// 走管理面鉴权；realm 按控制台请求的 key 解析（与其它控制台接口一致）。
func (h *Handler) adminUpstreamAccounts(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Upstream == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "upstream 未配置"})
		return
	}
	// realm 解析：优先按 key 命中的线；该线没号时回落其它启用线 ——
	// 控制台看的是"全部账号"，用主人钥匙进来打到 cn 空池会很反直觉。
	type target struct {
		rn string
		pl *pool.Pool
	}
	var targets []target
	seen := map[*pool.Pool]bool{}
	add := func(rn string) {
		pl := h.poolFor(rn)
		if pl == nil || seen[pl] {
			return
		}
		seen[pl] = true
		targets = append(targets, target{rn, pl})
	}
	add(h.realmOfRequest(r))
	for _, rn := range h.activeRealms() {
		add(rn)
	}
	for _, tg := range targets {
		acct := tg.pl.Pick()
		if acct == nil {
			continue
		}
		raw, err := h.upstreamFor(tg.rn).UpstreamAccounts(acct)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "realm": tg.rn, "uid": acct.UID})
			return
		}
		writeRawJSON(w, http.StatusOK, raw)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "没有可用账号"})
}
