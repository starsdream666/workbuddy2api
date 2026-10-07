// media.go 媒体与账号类上游端点：图片生成/编辑、视频生成与任务查询、上游账号信息。
//
// 请求/响应字段形状来自**官方 CLI 的调用代码**（@tencent-ai/codebuddy-code 2.159.0 的
// ImageService / VideoService），不是猜的：
//
//	图片生成  POST {chatBase}/v2/images/generations
//	          body {model, prompt, size(默认 1024x1024), response_format:"b64_json",
//	                n?, quality?, style?, background?}     ← 非 hunyuan/gemini 分支还会带 response_format
//	图片编辑  POST {chatBase}/v2/images/edits
//	          body 同上 + {image:[...], input_fidelity?}
//	          响应  {code:0, msg?, request_id?, data:{data:[…], usage?:{credit:N}}}
//	视频生成  POST {chatBase}/v2/videos/generations   （提交；body 直传；响应 data.id = 任务号）
//	视频查询  POST {chatBase}/v2/videos/tasks         （**是 POST 不是 GET**；body {task_id:"…"}）
//	          响应  {code:0, msg?, requestId?, data:{status, …}}
//	账号信息  GET  {chatBase}/v2/accounts
//
// 本层只做**透传**：不解释媒体结果语义（避免与上游演进赛跑），响应原样交给调用方。
// 出站头部与 chat 完全一致（CommonHeaders + Authorization + X-User-Id + X-Domain + 客户端身份），
// 保证归因/风控口径统一；响应体量用 doJSONMaxMedia（b64 图片远超 1MB）。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"workbuddy2api/internal/auth"
)

const (
	imageGenPath  = "/v2/images/generations"
	imageEditPath = "/v2/images/edits"
	videoGenPath  = "/v2/videos/generations"
	videoTaskPath = "/v2/videos/tasks"
	accountsPath  = "/v2/accounts"
)

// apiJSON 向 chat 域（chatBase）发 JSON 请求并解信封；返回信封里的 data。
// 与 billingJSON（billing 域 + BillingHeaders）对称，但用 chat 域的标准头部。
func (c *Client) apiJSON(a *auth.Auth, method, path string, body []byte, max int64) (json.RawMessage, error) {
	a = a.Snapshot()
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a)
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
	c.applyClientIdentity(req, a)
	return c.doJSONLimit(req, max)
}

// GenerateImage 文生图：body 为上游字段（调用方在 server 层做 OpenAI→上游映射）。
func (c *Client) GenerateImage(a *auth.Auth, body []byte) (json.RawMessage, error) {
	return c.apiJSON(a, http.MethodPost, imageGenPath, body, doJSONMaxMedia)
}

// EditImage 图生图/改图：body 同上 + image / input_fidelity。
func (c *Client) EditImage(a *auth.Auth, body []byte) (json.RawMessage, error) {
	return c.apiJSON(a, http.MethodPost, imageEditPath, body, doJSONMaxMedia)
}

// SubmitVideo 提交视频生成任务（响应 data.id 为任务号）。
func (c *Client) SubmitVideo(a *auth.Auth, body []byte) (json.RawMessage, error) {
	return c.apiJSON(a, http.MethodPost, videoGenPath, body, doJSONMaxMedia)
}

// VideoTask 查询视频任务：上游是 POST + {"task_id": "…"}（不是 GET，别照 REST 直觉写）。
func (c *Client) VideoTask(a *auth.Auth, taskID string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{"task_id": taskID})
	if err != nil {
		return nil, err
	}
	return c.apiJSON(a, http.MethodPost, videoTaskPath, body, doJSONMaxMedia)
}

// UpstreamAccounts 取上游账号/订阅信息（响应原样返回，供控制台展示）。
func (c *Client) UpstreamAccounts(a *auth.Auth) (json.RawMessage, error) {
	return c.apiJSON(a, http.MethodGet, accountsPath, nil, doJSONMaxBody)
}
