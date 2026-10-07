// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	return AggregateWithToolValidation(r, false)
}

func AggregateWithToolValidation(r io.Reader, validateTools bool) (map[string]any, error) {
	var id, model string
	var created float64
	var usage map[string]any
	choices := map[int]*chatChoice{}
	var order []int
	err := ReadChatSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["model"].(string); ok && model == "" {
			model = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		items, _ := chunk["choices"].([]any)
		for _, value := range items {
			choice := value.(map[string]any)
			idx := int(choice["index"].(float64))
			acc := choices[idx]
			if acc == nil {
				acc = &chatChoice{role: "assistant", tools: map[int]map[string]any{}}
				choices[idx] = acc
				order = append(order, idx)
			}
			if finish, _ := choice["finish_reason"].(string); finish != "" {
				acc.finish = finish
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				delta, _ = choice["message"].(map[string]any)
			}
			if role, _ := delta["role"].(string); role != "" {
				acc.role = role
			}
			if text, ok := delta["content"].(string); ok {
				acc.content.WriteString(text)
				acc.hasContent = true
			}
			if text, ok := delta["reasoning_content"].(string); ok {
				acc.reasoning.WriteString(text)
			}
			if text, ok := delta["refusal"].(string); ok {
				acc.refusal.WriteString(text)
			}
			if fn, ok := delta["function_call"].(map[string]any); ok {
				if acc.function == nil {
					acc.function = map[string]any{}
				}
				mergeFunctionDelta(acc.function, fn)
			}
			calls, _ := delta["tool_calls"].([]any)
			for _, value := range calls {
				call := value.(map[string]any)
				toolIdx := int(call["index"].(float64))
				if acc.tools[toolIdx] == nil {
					acc.tools[toolIdx] = map[string]any{"type": "function"}
					acc.toolOrder = append(acc.toolOrder, toolIdx)
				}
				mergeToolCallDelta(acc.tools[toolIdx], call)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = chatCompletionID()
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	sortInts(order)
	out := make([]any, 0, len(order))
	for _, index := range order {
		acc := choices[index]
		message := map[string]any{"role": acc.role, "content": nil}
		if acc.hasContent {
			message["content"] = acc.content.String()
		}
		if acc.reasoning.Len() > 0 {
			message["reasoning_content"] = acc.reasoning.String()
		}
		if acc.refusal.Len() > 0 {
			message["refusal"] = acc.refusal.String()
		}
		if acc.function != nil {
			message["function_call"] = acc.function
		}
		if len(acc.tools) > 0 {
			sortInts(acc.toolOrder)
			calls := make([]map[string]any, 0, len(acc.toolOrder))
			for _, idx := range acc.toolOrder {
				call := acc.tools[idx]
				fn, _ := call["function"].(map[string]any)
				id, _ := call["id"].(string)
				name, _ := fn["name"].(string)
				if id == "" || name == "" {
					return nil, fmt.Errorf("upstream tool call is missing id or name")
				}
				args, _ := fn["arguments"].(string)
				if args == "" {
					fn["arguments"] = "{}"
					args = "{}"
				}
				if validateTools && !json.Valid([]byte(args)) {
					return nil, fmt.Errorf("upstream returned invalid or truncated tool arguments")
				}
				calls = append(calls, call)
			}
			message["tool_calls"] = calls
		}
		finish := acc.finish
		if finish == "" {
			finish = "stop"
			if len(acc.tools) > 0 {
				finish = "tool_calls"
			} else if acc.function != nil {
				finish = "function_call"
			}
		}
		out = append(out, map[string]any{"index": index, "message": message, "finish_reason": finish})
	}
	resp := map[string]any{"id": id, "object": "chat.completion", "created": int64(created), "model": model, "choices": out}
	if usage != nil {
		resp["usage"] = normalizeUsage(usage)
	}
	return resp, nil
}

type chatChoice struct {
	role, finish                string
	content, reasoning, refusal strings.Builder
	hasContent                  bool
	function                    map[string]any
	tools                       map[int]map[string]any
	toolOrder                   []int
}

func mergeFunctionDelta(merged, delta map[string]any) {
	if name, _ := delta["name"].(string); name != "" {
		merged["name"] = name
	}
	if args, ok := delta["arguments"].(string); ok {
		prev, _ := merged["arguments"].(string)
		merged["arguments"] = prev + args
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	out["object"] = "chat.completion.chunk"
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			d, ok := c["delta"].(map[string]any)
			if !ok {
				d, _ = c["message"].(map[string]any)
			}
			if d != nil {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if v, exists := c["logprobs"]; exists {
				nc["logprobs"] = v
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	} else {
		out["choices"] = []any{}
	}
	if u, ok := obj["usage"]; ok {
		if usage, valid := u.(map[string]any); valid {
			out["usage"] = normalizeUsage(usage)
		} else {
			out["usage"] = u
		}
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream validates each upstream event before forwarding it. A failed stream
// ends with an error event and one DONE marker, so SDKs cannot mistake it for
// a successful (but silently truncated) completion.
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamWithModel(w, r, "")
}

// StreamWithModel fills omitted response metadata without changing ids between
// chunks. The handler supplies the requested model when upstream omits it.
func StreamWithModel(w http.ResponseWriter, r io.Reader, requestedModel string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	var writeErr error
	var id, model string
	var created float64
	writeRaw := func(payload string) error {
		_, writeErr = io.WriteString(w, "data: "+payload+"\n\n")
		if writeErr == nil && fl != nil {
			fl.Flush()
		}
		return writeErr
	}
	err := ReadChatSSE(r, func(chunk map[string]any) error {
		if id == "" {
			id, _ = chunk["id"].(string)
			if id == "" {
				id = chatCompletionID()
			}
			model, _ = chunk["model"].(string)
			if model == "" {
				model = requestedModel
			}
			created, _ = chunk["created"].(float64)
			if created == 0 {
				created = float64(time.Now().Unix())
			}
		}
		chunk["id"], chunk["model"], chunk["created"] = id, model, created
		raw, err := json.Marshal(normalizeFrame(chunk))
		if err != nil {
			return err
		}
		return writeRaw(string(raw))
	})
	if writeErr != nil {
		return writeErr
	}
	if err != nil {
		message := err.Error()
		if errors.Is(err, errEmptyChatStream) {
			message = "empty upstream stream"
		}
		raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error", "code": "upstream_parse"}})
		if e := writeRaw(string(raw)); e != nil {
			return e
		}
	}
	if e := writeRaw("[DONE]"); e != nil {
		return e
	}
	return err
}

func chatCompletionID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("chatcmpl-%x", id)
}
