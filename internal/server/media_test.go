package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

type mediaCall struct{ method, host, path, body string }

type mediaStub struct {
	mu     sync.Mutex
	calls  []mediaCall
	status int
	body   string
}

func (s *mediaStub) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body string
			if r.Body != nil {
				raw, _ := io.ReadAll(r.Body)
				body = string(raw)
			}
			s.mu.Lock()
			s.calls = append(s.calls, mediaCall{r.Method, r.URL.Host, r.URL.Path, body})
			st, respBody := s.status, s.body
			s.mu.Unlock()
			if st == 0 {
				st = http.StatusOK
			}
			if respBody == "" {
				respBody = `{"code":0,"data":{"data":[{"b64_json":"AAA"}],"usage":{"credit":1}}}`
			}
			return &http.Response{
				StatusCode: st,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(respBody)),
			}, nil
		})},
		RealmDefault: realm.WB,
	}
}

func (s *mediaStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *mediaStub) last(t *testing.T) mediaCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("上游没收到请求")
	}
	return s.calls[len(s.calls)-1]
}

// mediaHandler 构造一个带三线池/三把 key 的 handler（上游被 stub 拦下）。
func mediaHandler(stub *mediaStub) (*Handler, *pool.Pool) {
	cn := pool.New("")
	wb := testPoolWith(&auth.Auth{UID: "u-wb", AccessToken: "at-wb", ExpiresAt: 9999999999, Realm: realm.WB})
	return NewHandler(Config{
		Pool:           cn,
		Pools:          map[string]*pool.Pool{realm.CN: cn, realm.WB: wb, realm.CB: wb},
		RealmKeys:      map[string]string{realm.WB: "sk-wb", realm.CB: "sk-cb"},
		DefaultRealm:   realm.CN,
		APIKey:         "sk-master",
		Upstream:       stub.client(),
		ConsoleEnabled: true,
	}), wb
}

func postMedia(t *testing.T, h *Handler, path, key, payload string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestMediaRequiresAuth 媒体端点与 chat 同鉴权：无 key / 坏 key → 401。
func TestMediaRequiresAuth(t *testing.T) {
	h, _ := mediaHandler(&mediaStub{})
	for _, tc := range []struct{ name, key, payload string }{
		{"无 key", "", `{"model":"m","prompt":"p"}`},
		{"坏 key", "nope", `{"model":"m","prompt":"p"}`},
	} {
		code, _ := postMedia(t, h, "/v1/images/generations", tc.key, tc.payload)
		if code != http.StatusUnauthorized {
			t.Errorf("%s: code=%d want 401", tc.name, code)
		}
	}
}

// TestImagesGenerationsPrefixRoutingAndMapping 前缀选线 + 出站剥离 + 字段映射 + 原样回传。
func TestImagesGenerationsPrefixRoutingAndMapping(t *testing.T) {
	stub := &mediaStub{}
	h, _ := mediaHandler(stub)
	code, body := postMedia(t, h, "/v1/images/generations", "sk-master",
		`{"model":"codebuddy/hunyuan-image-v3.0","prompt":"一只猫","extra_client_field":1}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	call := stub.last(t)
	if call.host != "www.codebuddy.ai" {
		t.Errorf("出站 host=%q want www.codebuddy.ai（codebuddy 前缀选了别名线）", call.host)
	}
	if call.path != "/v2/images/generations" {
		t.Errorf("出站 path=%q", call.path)
	}
	if !strings.Contains(call.body, `"model":"hunyuan-image-v3.0"`) {
		t.Errorf("模型前缀必须剥离: %s", call.body)
	}
	if !strings.Contains(call.body, `"size":"1024x1024"`) {
		t.Errorf("size 应补默认 1024x1024: %s", call.body)
	}
	// hunyuan-* 分支照官方 CLI：**不发** response_format，只补 n=1。
	if strings.Contains(call.body, "response_format") {
		t.Errorf("hunyuan 分支不该发 response_format: %s", call.body)
	}
	if !strings.Contains(call.body, `"n":1`) {
		t.Errorf("hunyuan 分支应补 n=1: %s", call.body)
	}
	if strings.Contains(call.body, "extra_client_field") {
		t.Errorf("白名单之外的字段不该透传: %s", call.body)
	}
	// 响应把上游 data 原样回给调用方。
	if !strings.Contains(body, `"b64_json":"AAA"`) {
		t.Errorf("响应应是上游 data 原文: %s", body)
	}
}

// TestImagesGenerationsNonHunyuanBranchAddsResponseFormat 非 hunyuan/gemini 分支才补 response_format（照 CLI）。
func TestImagesGenerationsNonHunyuanBranchAddsResponseFormat(t *testing.T) {
	stub := &mediaStub{}
	h, _ := mediaHandler(stub)
	code, body := postMedia(t, h, "/v1/images/generations", "sk-wb",
		`{"model":"gpt-image-1","prompt":"a cat"}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	call := stub.last(t)
	if !strings.Contains(call.body, `"response_format":"b64_json"`) {
		t.Errorf("OpenAI 风格分支应补 response_format: %s", call.body)
	}
	if strings.Contains(call.body, `"n":`) {
		t.Errorf("OpenAI 风格分支不该凭空补 n: %s", call.body)
	}
}

// TestImagesGenerationsGeminiBranchAddsNothing gemini-* 分支只发 model/prompt/size。
func TestImagesGenerationsGeminiBranchAddsNothing(t *testing.T) {
	stub := &mediaStub{}
	h, _ := mediaHandler(stub)
	code, body := postMedia(t, h, "/v1/images/generations", "sk-wb",
		`{"model":"gemini-3-pro-image","prompt":"a cat"}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	call := stub.last(t)
	if strings.Contains(call.body, "response_format") || strings.Contains(call.body, `"n":`) {
		t.Errorf("gemini 分支不该补 response_format / n: %s", call.body)
	}
	if !strings.Contains(call.body, `"size":"1024x1024"`) {
		t.Errorf("size 仍应补默认 1024x1024: %s", call.body)
	}
}

// TestImagesEditsNormalizesImageString edits：image 单值字符串归一成数组。
func TestImagesEditsNormalizesImageString(t *testing.T) {
	stub := &mediaStub{}
	h, _ := mediaHandler(stub)
	code, body := postMedia(t, h, "/v1/images/edits", "sk-wb",
		`{"model":"hunyuan-image-v2.0-general-edit","prompt":"改成油画","image":"data:image/png;base64,AAA","input_fidelity":"high"}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	call := stub.last(t)
	if call.path != "/v2/images/edits" {
		t.Errorf("出站 path=%q", call.path)
	}
	if !strings.Contains(call.body, `"image":["data:image/png;base64,AAA"]`) {
		t.Errorf("image 应归一成数组: %s", call.body)
	}
	if !strings.Contains(call.body, `"input_fidelity":"high"`) {
		t.Errorf("input_fidelity 应透传: %s", call.body)
	}
	if call.host != "www.workbuddy.ai" {
		t.Errorf("workbuddy key 应走 workbuddy 域, got %q", call.host)
	}
}

// TestVideosTasksRequiresTaskID 视频任务查询：缺 task_id → 400 且不打上游；有则 POST {task_id}。
func TestVideosTasksRequiresTaskID(t *testing.T) {
	stub := &mediaStub{body: `{"code":0,"data":{"status":"completed","url":"https://x/v.mp4"}}`}
	h, _ := mediaHandler(stub)
	if code, _ := postMedia(t, h, "/v1/videos/tasks", "sk-wb", `{}`); code != http.StatusBadRequest {
		t.Errorf("缺 task_id code=%d want 400", code)
	}
	if stub.count() != 0 {
		t.Errorf("缺 task_id 不该打上游，calls=%d", stub.count())
	}
	code, body := postMedia(t, h, "/v1/videos/tasks", "sk-wb", `{"task_id":"task-9"}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	call := stub.last(t)
	if call.method != http.MethodPost || call.path != "/v2/videos/tasks" {
		t.Errorf("method=%s path=%s want POST /v2/videos/tasks", call.method, call.path)
	}
	if !strings.Contains(call.body, `"task_id":"task-9"`) {
		t.Errorf("body=%s", call.body)
	}
	if !strings.Contains(body, "completed") {
		t.Errorf("响应应原样透传: %s", body)
	}
}

// TestVideosGenerationsPassthrough 视频提交：body 直传（网关不改字段），响应原样。
func TestVideosGenerationsPassthrough(t *testing.T) {
	stub := &mediaStub{body: `{"code":0,"data":{"id":"task-1"}}`}
	h, _ := mediaHandler(stub)
	code, body := postMedia(t, h, "/v1/videos/generations", "sk-wb",
		`{"model":"workbuddy/video-1","prompt":"猫在跑","resolution":"720p","negative_prompt":"糊"}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	call := stub.last(t)
	if call.path != "/v2/videos/generations" {
		t.Errorf("path=%q", call.path)
	}
	if !strings.Contains(call.body, `"prompt":"猫在跑"`) || !strings.Contains(call.body, `"negative_prompt":"糊"`) {
		t.Errorf("视频 body 应直传: %s", call.body)
	}
	if !strings.Contains(body, `"id":"task-1"`) {
		t.Errorf("响应应原样透传: %s", body)
	}
}

// TestMediaUpstream402FreezesAccount 媒体请求撞 402 → 账号按同一套策略冻结（不然会被反复选中白撞）。
func TestMediaUpstream402FreezesAccount(t *testing.T) {
	stub := &mediaStub{status: http.StatusPaymentRequired, body: `{"code":1,"msg":"余额不足"}`}
	h, wb := mediaHandler(stub)
	code, body := postMedia(t, h, "/v1/images/generations", "sk-wb", `{"model":"m","prompt":"p"}`)
	if code != http.StatusPaymentRequired {
		t.Errorf("code=%d want 402 body=%s", code, body)
	}
	if !wb.IsFrozen("u-wb") {
		t.Error("402 后账号应被冻结")
	}
}

// TestAdminUpstreamAccounts 控制台端点：GET /v2/accounts 原样透传。
func TestAdminUpstreamAccounts(t *testing.T) {
	stub := &mediaStub{body: `{"code":0,"data":{"accounts":[{"uid":"u-wb","plan":"pro"}]}}`}
	h, _ := mediaHandler(stub)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/upstream-accounts", nil)
	req.Header.Set("Authorization", "Bearer sk-master")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	call := stub.last(t)
	if call.method != http.MethodGet || call.path != "/v2/accounts" {
		t.Errorf("method=%s path=%s want GET /v2/accounts", call.method, call.path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if _, ok := got["accounts"]; !ok {
		t.Errorf("响应应是上游 data: %s", rec.Body.String())
	}
}

// TestAdminUpstreamAccountsRequiresAuth 控制台端点同样要鉴权。
func TestAdminUpstreamAccountsRequiresAuth(t *testing.T) {
	h, _ := mediaHandler(&mediaStub{})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/upstream-accounts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无 key code=%d want 401", rec.Code)
	}
}
