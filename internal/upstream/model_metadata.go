package upstream

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

func enrichModelCatalog(raw []byte, models []ModelInfo) {
	var envelope struct {
		Data struct {
			Models []struct {
				ID        string          `json:"id"`
				Credits   json.RawMessage `json:"credits"`
				Vendor    string          `json:"vendor"`
				Reasoning struct {
					Default string `json:"defaultEffort"`
					Effort  string `json:"effort"`
				} `json:"reasoning"`
			} `json:"models"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	indexes := make(map[string]int, len(models))
	for index, model := range models {
		indexes[model.ID] = index
	}
	for _, entry := range envelope.Data.Models {
		index, found := indexes[entry.ID]
		if !found {
			continue
		}
		var credits string
		if json.Unmarshal(entry.Credits, &credits) != nil {
			if _, known := ParseModelRate(string(entry.Credits)); known {
				credits = string(entry.Credits)
			}
		}
		models[index].Credits, models[index].Vendor = credits, entry.Vendor
		models[index].DefaultEffort = entry.Reasoning.Default
		if models[index].DefaultEffort == "" {
			models[index].DefaultEffort = entry.Reasoning.Effort
		}
	}
}

// ParseModelRate 解析上游下发的倍率文本为数值。
//
// 上游实测格式（2026-10，acc-product-config-v3.json）：
//
//	"x0.34 credits"  → 0.34   （前缀 x + 数字 + 可选后缀 " credits"）
//	"x0.00"          → 0       （免费）
//	"x6.67"          → 6.67
//	""               → 未知（不返回）
//
// 同时兼容历史/其它形态：数字 0、"2x" / "2 X"（后缀 x）、" 2 "。非法值（NaN/Inf/负数/纯文本）返回 known=false。
func ParseModelRate(raw string) (float64, bool) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 0, false
	}
	// 去掉可能的后缀单位（"credits" / "credit"）。
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "credits"), "credit"))
	// 前缀倍率符号 "x0.34"：剥离前导 x（仅当后面还有内容时）。
	if strings.HasPrefix(s, "x") && len(s) > 1 {
		s = strings.TrimSpace(s[1:])
	}
	// 后缀倍率符号 "2x"：剥离尾部 x（仅当前面还有内容时）。
	if strings.HasSuffix(s, "x") && len(s) > 1 {
		s = strings.TrimSpace(strings.TrimSuffix(s, "x"))
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return rate, err == nil && rate >= 0 && !math.IsNaN(rate) && !math.IsInf(rate, 0)
}

// reasoningEffortEnum 上游 CLI 的推理档位完整枚举（低 → 高）。
//
// 依据官方 CLI 源码（WorkBuddy 桌面端 resources/.../codebuddy.js）：
//
//	efforts = [minimal, low, medium, high, xhigh, max]
//	shown   = supportedEfforts?.length ? efforts.filter(in supportedEfforts) : efforts
//
// 即：模型没有下发 supportedEfforts 时，UI 展示**全部六档**，reasoning.effort 只是默认档，
// 而不是"只支持这一个档"。这正是控制台此前只显示一个档位的根因。
var reasoningEffortEnum = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// EffectiveEfforts 返回模型实际可选的推理档位：
//   - 上游下发了 supportedEfforts → 原样返回（保序去重前的原始值）；
//   - 未下发但模型带推理（DefaultEffort 非空）→ 回退官方全档枚举；
//   - 完全没有推理能力（两者皆空）→ 返回 nil。
func EffectiveEfforts(supported []string, defaultEffort string) []string {
	if len(supported) > 0 {
		return supported
	}
	if strings.TrimSpace(defaultEffort) == "" {
		return nil
	}
	return append([]string(nil), reasoningEffortEnum...)
}

// vendorPrefixes 模型名族 → 可读厂商名。
//
// 上游 `vendor` 字段是内部单字母枚举（实测 e/i/f/j），无公开图例，直接展示没有意义
// （且同一字母横跨多家：e 同时是 OpenAI/Gemini/GLM）。这里改用模型 id 前缀推导，
// 是当前唯一可读且可验证的归属方式；未命中时返回空串（展示为 —）。
var vendorPrefixes = []struct {
	prefixes []string
	vendor   string
}{
	{[]string{"gpt-image-", "gpt-"}, "OpenAI"},
	{[]string{"gemini-"}, "Google"},
	{[]string{"glm-"}, "智谱 GLM"},
	{[]string{"kimi-"}, "Moonshot"},
	{[]string{"deepseek-"}, "DeepSeek"},
	{[]string{"hunyuan", "hy4-", "hy3"}, "腾讯混元"},
	{[]string{"grok-"}, "xAI"},
	{[]string{"seedance-"}, "Seedance"},
	{[]string{"default-model", "fast-model", "balanced-model", "primary-model", "deep-model"}, "WorkBuddy 内置"},
}

// ModelVendor 从模型 id 推导可读厂商名；未命中返回空串。
func ModelVendor(id string) string {
	lower := strings.ToLower(strings.TrimSpace(id))
	if lower == "" {
		return ""
	}
	for _, entry := range vendorPrefixes {
		for _, p := range entry.prefixes {
			if strings.HasPrefix(lower, p) {
				return entry.vendor
			}
		}
	}
	return ""
}
