package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestParseRealmForms：realm 在顶层 / auth 段内都能读；缺失 = 空（由调用方按配置归一）。
func TestParseRealmForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "顶层 realm（importauth 落盘形态）",
			raw:  `{"realm":"ai","auth":{"accessToken":"at"},"account":{"uid":"u1"}}`,
			want: "ai",
		},
		{
			name: "auth.realm 优先于顶层",
			raw:  `{"realm":"cn","auth":{"accessToken":"at","realm":"ai"},"account":{"uid":"u1"}}`,
			want: "ai",
		},
		{
			name: "扁平形 realm",
			raw:  `{"accessToken":"at","realm":"ai","uid":"u1"}`,
			want: "ai",
		},
		{
			name: "无 realm（旧凭证）",
			raw:  `{"auth":{"accessToken":"at"},"account":{"uid":"u1"}}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Realm != tc.want {
				t.Errorf("realm=%q want %q", a.Realm, tc.want)
			}
		})
	}
}

// TestParseExpiresAtMilliseconds WorkBuddy 桌面端凭证的 expiresAt 是毫秒，
// 必须归一为 Unix 秒，否则会被当成"永不过期"（秒口径下 1.8e12 是公元 6 万年）。
func TestParseExpiresAtMilliseconds(t *testing.T) {
	const ms = int64(1820653886152) // 桌面端实测值
	a, err := Parse([]byte(`{"auth":{"accessToken":"at","expiresAt":1820653886152},"account":{"uid":"u1"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.ExpiresAt != ms/1000 {
		t.Errorf("ExpiresAt=%d want %d（秒）", a.ExpiresAt, ms/1000)
	}

	// 秒口径不受影响（回归保护）
	b, err := Parse([]byte(`{"auth":{"accessToken":"at","expiresAt":1790000000},"account":{"uid":"u1"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if b.ExpiresAt != 1790000000 {
		t.Errorf("秒口径被改动: %d", b.ExpiresAt)
	}
}

func TestNormalizeExpiresAt(t *testing.T) {
	if got := NormalizeExpiresAt(1820653886152); got != 1820653886 {
		t.Errorf("ms 归一=%d", got)
	}
	if got := NormalizeExpiresAt(0); got != 0 {
		t.Errorf("0 应保持 0, got %d", got)
	}
	if got := NormalizeExpiresAt(1790000000); got != 1790000000 {
		t.Errorf("秒不该被改, got %d", got)
	}
}

// TestSaveAtomicKeepsRealm：写回保留 realm，且无 realm 的旧账号不被网关单方面标注。
func TestSaveAtomicKeepsRealm(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-ai-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1790000000, UID: "u1", Realm: "ai", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["realm"] != "ai" {
		t.Errorf("realm 未落盘: %v", doc["realm"])
	}
	// 重新解析回读一致
	back, err := Parse(raw)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if back.Realm != "ai" || back.ExpiresAt != 1790000000 {
		t.Errorf("回读 realm=%q expiresAt=%d", back.Realm, back.ExpiresAt)
	}

	// 无 realm：不落 realm 键（保持旧文件原样语义）
	fp2 := filepath.Join(dir, "workbuddy-u2.json")
	b := &Auth{AccessToken: "at", UID: "u2", FilePath: fp2}
	if err := b.SaveAtomic(); err != nil {
		t.Fatalf("save2: %v", err)
	}
	raw2, _ := os.ReadFile(fp2)
	var doc2 map[string]any
	_ = json.Unmarshal(raw2, &doc2)
	if _, present := doc2["realm"]; present {
		t.Errorf("无 realm 的账号不该写 realm 键: %v", doc2["realm"])
	}
}
