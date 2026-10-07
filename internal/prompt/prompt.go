// Package prompt 提供出站请求的 system 提示词改写能力。
//
// 立场（2026-09-30 起）：**网关不内置任何提示词**。
//   - 缺省 prompt.mode=passthrough：原样转发客户端请求体，网关不注入、不替换任何文本；
//   - 只有运维显式配置 prompt.mode=custom + prompt.file=<路径> 时，才用那份文件文本替换
//     客户端的 system/developer 消息（历史用途：消除客户端模板句引起的上游误报，见 issue #36/PR39）；
//   - 内容拦截降级重试不再注入任何文案，改为**剥离** system/developer 消息（StripSystem），
//     把上游看到的内容减到最小，而不是替换成另一段网关注入文案。
//
// 唯一仍由网关补的 system 是**国际线的空 role 占位**（见 internal/upstream.EnsureLeadingSystem，
// content 为空串、不带任何文案），因为上游对首条消息做硬校验（400 code=11128）。
package prompt

import (
	"encoding/json"
	"fmt"
	"os"
)

// Load 按 file 加载系统提示词文本：
//   - file 非空 → 读文件（不存在/读失败返回 error，调用方 fail fast）；
//   - file 空 → 返回空串：**不注入任何提示词**（不再回落到内置文案）。
//
// mode 在此仅做透传记录（实际 custom/passthrough 路由由调用方决定），
// Load 只负责"拿到一段提示词文本，或空"。
func Load(mode, file string) (string, error) {
	if file == "" {
		return "", nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	return string(raw), nil
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//   - 删除 messages 中所有 role 为 system/developer 的消息；
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}；
//   - 其余字段与 user/assistant/tool 消息逐字不动。
//
// systemPrompt 为空 → 原样返回（自定义提示词未配置时不要注入空消息）。
// 解析失败 → 原样返回（绝不失败）：Rewrite 是出站改写的关键路径，
// 任何解析错误都不应阻塞请求转发，让上游按其原始语义处理。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// 无 messages 字段或类型不符 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// 过滤掉所有 system/developer 消息，保留 user/assistant/tool 及其他角色。
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	// 头部插入单条 system 消息（prepend 避免整体重排语义）。
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// StripSystem 删除 messages 中所有 system/developer 消息，**不插入任何内容**。
//
// 用途：内容策略误报（HTTP 400 + 审核文案）的降级重试。误报几乎都来自 system 里的
// 客户端模板句，直接剥掉是最小干预；网关不借此注入任何自有文案。
//
// 没有任何 system/developer 时逐字原样返回；body 非 JSON / 无 messages / messages
// 非数组一律原样返回（不猜客户端意图）。
func StripSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return body
	}
	kept := make([]any, 0, len(msgs))
	stripped := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			stripped = true
			continue
		}
		kept = append(kept, m)
	}
	if !stripped {
		return body
	}
	obj["messages"] = kept
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
