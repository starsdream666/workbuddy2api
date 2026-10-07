package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/realm"
)

// mediaAuth 一个 WorkBuddy 线账号（带 uid/domain，便于断言出站头部）。
func mediaAuth() *auth.Auth {
	return &auth.Auth{
		UID: "u-media", AccessToken: "at-media", RefreshToken: "rt-media",
		ExpiresAt: 9999999999, Realm: realm.WB, Domain: "www.workbuddy.ai",
	}
}

// mediaCapture 记录一次出站请求，并回固定 JSON。
type mediaCapture struct {
	method, host, path, body, authz, uid, domain, ua string
	resp                                             string
	seen                                             int
}

func (cap *mediaCapture) client() *Client {
	return &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			cap.seen++
			cap.method, cap.host, cap.path = r.Method, r.URL.Host, r.URL.Path
			cap.authz = r.Header.Get("Authorization")
			cap.uid = r.Header.Get("X-User-Id")
			cap.domain = r.Header.Get("X-Domain")
			cap.ua = r.Header.Get("User-Agent")
			if r.Body != nil {
				raw, _ := io.ReadAll(r.Body)
				cap.body = string(raw)
			}
			resp := cap.resp
			if resp == "" {
				resp = `{"code":0,"data":{"ok":true}}`
			}
			return jsonResp(http.StatusOK, resp), nil
		})},
		RealmDefault: realm.WB,
	}
}

// TestGenerateImagePathBodyHeaders 图片生成：路径/方法/头部/body 逐项对照官方 CLI 的形状。
func TestGenerateImagePathBodyHeaders(t *testing.T) {
	cap := &mediaCapture{resp: `{"code":0,"data":{"data":[{"b64_json":"AAA"}],"usage":{"credit":2}}}`}
	c := cap.client()
	data, err := c.GenerateImage(mediaAuth(), []byte(`{"model":"hunyuan-image-v3.0","prompt":"a cat","size":"1024x1024","response_format":"b64_json"}`))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != imageGenPath {
		t.Errorf("method=%s path=%s want POST %s", cap.method, cap.path, imageGenPath)
	}
	if cap.host != "www.workbuddy.ai" {
		t.Errorf("host=%q want www.workbuddy.ai（workbuddy 线档案）", cap.host)
	}
	if cap.authz != "Bearer at-media" || cap.uid != "u-media" || cap.domain != "www.workbuddy.ai" {
		t.Errorf("headers authz=%q uid=%q domain=%q", cap.authz, cap.uid, cap.domain)
	}
	if !strings.Contains(cap.body, `"prompt":"a cat"`) || !strings.Contains(cap.body, `"response_format":"b64_json"`) {
		t.Errorf("body=%s", cap.body)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("data 不是 JSON: %v", err)
	}
	if _, ok := got["data"]; !ok {
		t.Errorf("返回应是信封里的 data（含 data 数组）: %s", data)
	}
}

// TestEditImagePathAndBody 图片编辑走 /v2/images/edits，image 字段原样带上。
func TestEditImagePathAndBody(t *testing.T) {
	cap := &mediaCapture{}
	c := cap.client()
	if _, err := c.EditImage(mediaAuth(), []byte(`{"model":"m","prompt":"p","image":["data:image/png;base64,AAA"],"input_fidelity":"high"}`)); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if cap.path != imageEditPath {
		t.Errorf("path=%s want %s", cap.path, imageEditPath)
	}
	if !strings.Contains(cap.body, `"image":["data:image/png;base64,AAA"]`) || !strings.Contains(cap.body, `"input_fidelity":"high"`) {
		t.Errorf("body=%s", cap.body)
	}
}

// TestSubmitVideoAndVideoTask 视频：提交走 POST /v2/videos/generations；查询是 POST /v2/videos/tasks + {task_id}。
func TestSubmitVideoAndVideoTask(t *testing.T) {
	cap := &mediaCapture{resp: `{"code":0,"data":{"id":"task-1"}}`}
	c := cap.client()
	if _, err := c.SubmitVideo(mediaAuth(), []byte(`{"model":"video-1","prompt":"跑猫","resolution":"720p"}`)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != videoGenPath {
		t.Errorf("submit method=%s path=%s", cap.method, cap.path)
	}

	cap2 := &mediaCapture{resp: `{"code":0,"data":{"status":"completed"}}`}
	c2 := cap2.client()
	data, err := c2.VideoTask(mediaAuth(), "task-1")
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if cap2.method != http.MethodPost || cap2.path != videoTaskPath {
		t.Errorf("task method=%s path=%s want POST %s（上游是 POST，不是 GET）", cap2.method, cap2.path, videoTaskPath)
	}
	if !strings.Contains(cap2.body, `"task_id":"task-1"`) {
		t.Errorf("task body=%s", cap2.body)
	}
	if !strings.Contains(string(data), "completed") {
		t.Errorf("data=%s", data)
	}
}

// TestUpstreamAccountsPath 账号信息走 GET /v2/accounts。
func TestUpstreamAccountsPath(t *testing.T) {
	cap := &mediaCapture{resp: `{"code":0,"data":{"accounts":[{"uid":"u1"}]}}`}
	c := cap.client()
	if _, err := c.UpstreamAccounts(mediaAuth()); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != accountsPath {
		t.Errorf("method=%s path=%s want GET %s", cap.method, cap.path, accountsPath)
	}
}

// TestMediaResponseOverOneMegabyteIsNotTruncated 媒体响应（b64 图片）远超 1MB：
// 常规 doJSON 的 1MB 上限会把它截断成 parse failed，媒体端点必须用更大的上限。
func TestMediaResponseOverOneMegabyteIsNotTruncated(t *testing.T) {
	big := strings.Repeat("A", 1<<20+2048) // ~1.05MB base64
	cap := &mediaCapture{resp: `{"code":0,"data":{"data":[{"b64_json":"` + big + `"}]}}`}
	c := cap.client()
	data, err := c.GenerateImage(mediaAuth(), []byte(`{"model":"m","prompt":"p"}`))
	if err != nil {
		t.Fatalf("大响应被截断/解析失败: %v", err)
	}
	var got struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Data) != 1 || len(got.Data[0].B64) != len(big) {
		t.Errorf("b64 长度=%d want %d（响应被截断）", len(got.Data[0].B64), len(big))
	}
}

// TestMediaUpstreamErrorClassification 上游 402 → *Error{ErrHardCredit, 402}（供上层冻结账号）。
func TestMediaUpstreamErrorClassification(t *testing.T) {
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResp(http.StatusPaymentRequired, `{"code":1,"msg":"余额不足"}`), nil
		})},
		RealmDefault: realm.WB,
	}
	_, err := c.GenerateImage(mediaAuth(), []byte(`{"model":"m","prompt":"p"}`))
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("err=%T want *Error", err)
	}
	if ue.Kind != ErrHardCredit || ue.Status != http.StatusPaymentRequired {
		t.Errorf("kind=%v status=%d want ErrHardCredit/402", ue.Kind, ue.Status)
	}
}
