package upstream

import (
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/realm"
)

// productConfigBody 模拟 /v3/config 的响应（含 models 明细 + cli agent 名单）。
const productConfigBody = `{"code":0,"msg":"OK","data":{
  "models":[
    {"id":"default-model","name":"Auto","maxInputTokens":176000,"maxOutputTokens":24000},
    {"id":"hy4-preview-f","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000},
    {"id":"gpt-5.6-luna","name":"GPT-5.6-Luna","maxInputTokens":1000000,"maxOutputTokens":128000,
     "reasoning":{"supportedEfforts":["low","medium","high"]}},
    {"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000},
    {"id":"retired-model","name":"Retired","disabled":true,"maxInputTokens":1000,"maxOutputTokens":100},
    {"id":"gpt-image-2.5-sunburst","name":"GPT-Image-2.5-Sunburst","tags":["text-to-image","image-to-image"]},
    {"id":"seedance-2.5","name":"Seedance-2.5","tags":["text-to-video","image-to-video"]},
    {"id":"retired-media","name":"Retired media","disabled":true,"tags":["text-to-image"]}
  ],
  "agents":[{"name":"cli","models":["default-model","hy4-preview-f","gpt-5.6-luna","retired-model"]},
            {"name":"Explore","models":["default-model"]}]
}}`

// TestFetchModelsAIUsesProductConfig workbuddy 线走 /v3/config，取 cli agent 名单：
//   - 名单顺序保持（决定选择器里的顺序）
//   - disabled 模型被剔除
//   - 上下文/输出长度与配置一致，supportedEfforts 进缓存
func TestFetchModelsAIUsesProductConfig(t *testing.T) {
	var gotPath, gotProduct, gotIDE string
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotProduct = r.Header.Get("X-Product")
			gotIDE = r.Header.Get("X-IDE-Type")
			if got := r.Header.Get("Authorization"); got != "Bearer at" {
				t.Errorf("Authorization=%q", got)
			}
			return jsonResp(200, productConfigBody), nil
		})},
		RealmDefault:      realm.CN,
		RealmFingerprints: map[string]realm.Fingerprint{realm.WB: realm.FingerprintDesktop},
		efforts:           newEffortCache(),
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Realm: realm.WB, Domain: "www.workbuddy.ai"}
	models, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/v3/config" {
		t.Errorf("path=%q want /v3/config", gotPath)
	}
	// 取配置的身份必须与出站身份一致：桌面指纹才拿到桌面那份名单。
	if gotProduct != "WorkBuddy" || gotIDE != "WorkBuddy" {
		t.Errorf("身份头 X-Product=%q X-IDE-Type=%q want WorkBuddy", gotProduct, gotIDE)
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	want := []string{"default-model", "hy4-preview-f", "gpt-5.6-luna", "gpt-image-2.5-sunburst", "seedance-2.5"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids=%v want %v（cli 名单保序、剔除 disabled；媒体模型追加在尾部）", ids, want)
	}
	if models[1].ContextWindow != 1000000 || models[1].MaxTokens != 64000 {
		t.Errorf("hy4-preview-f 上下文=%d/%d", models[1].ContextWindow, models[1].MaxTokens)
	}
	if len(models[2].Efforts) != 3 {
		t.Errorf("supportedEfforts 未进 ModelInfo: %v", models[2].Efforts)
	}
	// 媒体模型（图片 / 视频）必须一并列出并带 tags（handler 据此打 kind 分类）；
	// disabled 的媒体模型（retired-media）必须被剔除。
	if !HasMediaTag(models[3].Tags) || MediaKind(models[3].Tags) != "image" {
		t.Errorf("gpt-image-2.5-sunburst tags=%v kind=%q want image", models[3].Tags, MediaKind(models[3].Tags))
	}
	if MediaKind(models[4].Tags) != "video" {
		t.Errorf("seedance-2.5 kind=%q want video", MediaKind(models[4].Tags))
	}
	if MediaKind(models[0].Tags) != "" {
		t.Errorf("对话模型不该被归为媒体: %v", models[0].Tags)
	}
	// effort 缓存刷新（供请求体档位降级）；缓存为指针字段，手工构造 Client 时需显式建一次。
	got := c.effortsSnapshot()
	if _, ok := got["gpt-5.6-luna"]; !ok {
		t.Error("efforts 缓存未刷新")
	}
}

// TestFetchModelsAICLIFingerprintIdentity workbuddy 线 CLI 指纹下同样走 /v3/config，
// 只是身份头不同（服务端会返回另一份名单）——这里断言身份头随指纹变化。
func TestFetchModelsAICLIFingerprintIdentity(t *testing.T) {
	var gotProduct, gotIDE string
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			gotProduct = r.Header.Get("X-Product")
			gotIDE = r.Header.Get("X-IDE-Type")
			return jsonResp(200, productConfigBody), nil
		})},
		RealmDefault: realm.CN, // workbuddy 指纹未显式配置 → 用档案默认 cli
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Realm: realm.WB}
	if _, err := c.FetchModels(a); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotProduct != "SaaS" || gotIDE != "" {
		t.Errorf("cli 指纹身份头 X-Product=%q X-IDE-Type=%q", gotProduct, gotIDE)
	}
}

// TestFetchModelsAIEmptyConfigErrors 未鉴权时上游返回空配置（models=null）→ 明确报错，
// 由上层回落静态表（不能静默返回空列表）。workbuddy 线（旧名 ai）同样适用。
func TestFetchModelsAIEmptyConfigErrors(t *testing.T) {
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResp(200, `{"code":0,"msg":"OK","data":{"models":null,"agents":null}}`), nil
		})},
		RealmDefault: realm.CN,
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Realm: realm.WB}
	if _, err := c.FetchModels(a); err == nil {
		t.Fatal("空配置应报错，交由调用方回落静态表")
	}
}

// TestFetchModelsCNStillConsole CN 线仍走控制台接口（回归保护）。
func TestFetchModelsCNStillConsole(t *testing.T) {
	var gotPath string
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		})},
		RealmDefault: realm.CN,
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1"} // 无 realm → cn
	models, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/console/enterprises/personal/models" {
		t.Errorf("cn path=%q", gotPath)
	}
	if len(models) != 1 || models[0].ID != "glm-5.2" {
		t.Errorf("models=%v", models)
	}
}
