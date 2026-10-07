package upstream

import "testing"

func TestModelMetadata(test *testing.T) {
	models := []ModelInfo{{ID: "numeric"}, {ID: "string"}, {ID: "null"}}
	enrichModelCatalog([]byte(`{"data":{"models":[{"id":"numeric","credits":0,"vendor":"vendor","reasoning":{"defaultEffort":"high"}},{"id":"string","credits":"2x","reasoning":{"effort":"low"}},{"id":"null","credits":null}]}}`), models)
	if models[0].Credits != "0" || models[0].Vendor != "vendor" || models[0].DefaultEffort != "high" {
		test.Fatalf("numeric metadata=%+v", models[0])
	}
	if models[1].Credits != "2x" || models[1].DefaultEffort != "low" || models[2].Credits != "" {
		test.Fatalf("metadata=%+v", models)
	}
	for _, input := range []string{"", "null", "NaN", "+Inf", "-1", "free", "{}"} {
		if _, known := ParseModelRate(input); known {
			test.Fatalf("invalid rate %q accepted", input)
		}
	}
	if rate, known := ParseModelRate(" 2 X "); !known || rate != 2 {
		test.Fatalf("rate=%v known=%v", rate, known)
	}
}

// TestParseModelRateRealFormats 真实上游倍率文本（acc-product-config-v3.json 实测）：
// "x0.34 credits" / "x0.00" / "x6.67"，以及空串（未知）。
func TestParseModelRateRealFormats(t *testing.T) {
	cases := []struct {
		in    string
		rate  float64
		known bool
	}{
		{"x0.34 credits", 0.34, true},
		{"x0.00", 0, true},
		{"x6.67", 6.67, true},
		{"x1.20", 1.20, true},
		{"x0.21 credits", 0.21, true},
		{"", 0, false},
		{"x", 0, false},
		{"credits", 0, false},
	}
	for _, c := range cases {
		rate, known := ParseModelRate(c.in)
		if known != c.known || (known && rate != c.rate) {
			t.Errorf("ParseModelRate(%q) = (%v,%v) want (%v,%v)", c.in, rate, known, c.rate, c.known)
		}
	}
}

// TestEffectiveEfforts 档位回退：有 supportedEfforts 用原值；无但带推理 → 官方全档；无推理 → nil。
func TestEffectiveEfforts(t *testing.T) {
	got := EffectiveEfforts([]string{"low", "high"}, "high")
	if len(got) != 2 || got[0] != "low" || got[1] != "high" {
		t.Errorf("supportedEfforts 应原样返回: %v", got)
	}
	full := EffectiveEfforts(nil, "medium")
	if len(full) != 6 || full[0] != "minimal" || full[5] != "max" {
		t.Errorf("无 supportedEfforts 应回退官方全档: %v", full)
	}
	if EffectiveEfforts(nil, "") != nil {
		t.Error("无推理能力应返回 nil")
	}
}

// TestModelVendor 从模型 id 推导可读厂商名。
func TestModelVendor(t *testing.T) {
	cases := map[string]string{
		"gpt-6-astra":             "OpenAI",
		"gpt-image-2.5-sunburst":  "OpenAI",
		"gemini-3.8-flash":        "Google",
		"glm-5.2":                 "智谱 GLM",
		"kimi-k2.6":               "Moonshot",
		"deepseek-v4.1-flash":     "DeepSeek",
		"hy3":                     "腾讯混元",
		"hy4-preview-f":           "腾讯混元",
		"grok-4.7":                "xAI",
		"seedance-2.5":            "Seedance",
		"default-model":           "WorkBuddy 内置",
		"fast-model":              "WorkBuddy 内置",
		"unknown-xyz":             "",
		"":                        "",
	}
	for id, want := range cases {
		if got := ModelVendor(id); got != want {
			t.Errorf("ModelVendor(%q) = %q want %q", id, got, want)
		}
	}
}
