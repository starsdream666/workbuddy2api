package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Both alternate APIs enter the same routing, retry, accounting and prompt pipeline.
func protocolToChat(raw []byte, protocol string) ([]byte, map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil || req == nil {
		return nil, nil, fmt.Errorf("request body must be a JSON object")
	}
	if strings.TrimSpace(stringOf(req["model"])) == "" {
		return nil, nil, fmt.Errorf("model must be a non-empty string")
	}
	if v := req["stream"]; v != nil {
		if _, ok := v.(bool); !ok {
			return nil, nil, fmt.Errorf("stream must be a boolean")
		}
	}
	chat := map[string]any{"model": req["model"], "stream": req["stream"] == true}
	for _, key := range []string{"temperature", "top_p", "user", "conversation_id", "prompt_cache_key"} {
		if v, ok := req[key]; ok {
			chat[key] = v
		}
	}
	var err error
	if protocol == "responses" {
		err = convertResponseRequest(req, chat)
	} else {
		err = convertMessagesRequest(req, chat)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := validateChatObject(chat); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(chat)
	return out, req, err
}

func stringOf(v any) string         { s, _ := v.(string); return s }
func objectOf(v any) map[string]any { m, _ := v.(map[string]any); return m }

func convertResponseRequest(req, chat map[string]any) error {
	for _, key := range []string{"reasoning", "text", "metadata"} {
		if v := req[key]; v != nil && objectOf(v) == nil {
			return fmt.Errorf("%s must be an object", key)
		}
	}
	for _, k := range []string{"previous_response_id", "conversation"} {
		if v := req[k]; v != nil && v != "" {
			return fmt.Errorf("%s is not supported; send complete history in input", k)
		}
	}
	for _, k := range []string{"background", "store"} {
		if v, exists := req[k]; exists && v != nil {
			b, ok := v.(bool)
			if !ok || b {
				return fmt.Errorf("%s must be false; this gateway is stateless", k)
			}
		}
	}
	msgs := []any{}
	// additional_tools 项声明的工具（Responses 规范允许在对话中途补充工具定义）。
	// 网关是无状态 chat 网关，工具集合同样是请求级：这里先收集，最后与顶层 tools 合并。
	extraTools := []any{}
	if instructions, ok := req["instructions"]; ok && instructions != nil {
		s, ok := instructions.(string)
		if !ok {
			return fmt.Errorf("instructions must be a string")
		}
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}
	switch input := req["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": input})
	case []any:
		for i, value := range input {
			item := objectOf(value)
			if item == nil {
				return fmt.Errorf("input[%d] must be an object", i)
			}
			if typ, exists := item["type"]; exists {
				if _, ok := typ.(string); !ok {
					return fmt.Errorf("input[%d].type must be a string", i)
				}
			}
			switch typ := stringOf(item["type"]); typ {
			case "", "message":
				role := stringOf(item["role"])
				if role != "user" && role != "assistant" && role != "system" && role != "developer" {
					return fmt.Errorf("input[%d]: unsupported role %q", i, role)
				}
				content, err := responseContent(item["content"])
				if err != nil {
					return fmt.Errorf("input[%d]: %w", i, err)
				}
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			case "function_call":
				if stringOf(item["call_id"]) == "" || stringOf(item["name"]) == "" {
					return fmt.Errorf("function_call requires call_id and name")
				}
				args, ok := item["arguments"].(string)
				if !ok {
					return fmt.Errorf("function_call arguments must be a string")
				}
				call := map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": args}}
				appendToolCall(&msgs, call)
			case "function_call_output":
				if stringOf(item["call_id"]) == "" {
					return fmt.Errorf("function_call_output requires call_id")
				}
				content, err := responseContent(item["output"])
				if err != nil {
					return fmt.Errorf("function_call_output: %w", err)
				}
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": item["call_id"], "content": content})
			case "reasoning":
				// Responses clients replay output items. Preserve visible summaries for
				// upstream models that require reasoning in assistant tool history.
				var summary strings.Builder
				parts, ok := item["summary"].([]any)
				if item["summary"] != nil && !ok {
					return fmt.Errorf("reasoning.summary must be an array")
				}
				for _, p := range parts {
					text, ok := objectOf(p)["text"].(string)
					if !ok {
						return fmt.Errorf("reasoning summary requires text")
					}
					summary.WriteString(text)
				}
				if summary.Len() > 0 {
					msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "reasoning_content": summary.String()})
				}
			case "additional_tools":
				// Responses 客户端（如 PI-Desktop 的 gpt-6-luna 档案，compat.supportsAdditionalTools）
				// 会在对话中途以 developer 角色的 additional_tools 项补充工具定义。
				// chat 协议没有对应的中途工具概念：这里收集，末尾与顶层 tools 合并（同名去重）。
				tools, ok := item["tools"].([]any)
				if !ok {
					return fmt.Errorf("input[%d]: additional_tools requires a tools array", i)
				}
				for _, t := range tools {
					converted, err := convertResponseTool(t)
					if err != nil {
						return fmt.Errorf("input[%d]: %w", i, err)
					}
					extraTools = append(extraTools, converted)
				}
			default:
				return fmt.Errorf("input[%d]: unsupported item type %q", i, typ)
			}
		}
	default:
		return fmt.Errorf("input must be a string or an array of input items")
	}
	if len(msgs) == 0 {
		return fmt.Errorf("input must contain at least one message")
	}
	chat["messages"] = msgs
	if v := req["max_output_tokens"]; v != nil {
		if !positiveInteger(v) {
			return fmt.Errorf("max_output_tokens must be a positive integer")
		}
		chat["max_tokens"] = v
	}
	if v, ok := req["parallel_tool_calls"]; ok {
		chat["parallel_tool_calls"] = v
	}
	if reasoning := objectOf(req["reasoning"]); reasoning != nil {
		if v, ok := reasoning["effort"]; ok {
			chat["reasoning_effort"] = v
		}
	}
	if text := objectOf(req["text"]); text != nil {
		if v := text["format"]; v != nil && objectOf(v) == nil {
			return fmt.Errorf("text.format must be an object")
		}
		if f := objectOf(text["format"]); f != nil {
			switch f["type"] {
			case "text", "json_object":
				chat["response_format"] = map[string]any{"type": f["type"]}
			case "json_schema":
				schema := map[string]any{}
				for _, k := range []string{"name", "description", "schema", "strict"} {
					if v, ok := f[k]; ok {
						schema[k] = v
					}
				}
				chat["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
			default:
				return fmt.Errorf("unsupported text.format.type")
			}
		}
	}
	if rawTools := req["tools"]; rawTools != nil {
		tools, ok := rawTools.([]any)
		if !ok {
			return fmt.Errorf("tools must be an array")
		}
		for _, t := range tools {
			converted, err := convertResponseTool(t)
			if err != nil {
				return err
			}
			extraTools = append(extraTools, converted)
		}
	}
	if len(extraTools) > 0 {
		chat["tools"] = dedupeResponseTools(extraTools)
	}
	if v := req["tool_choice"]; v != nil {
		if tc := objectOf(v); tc != nil {
			if tc["type"] != "function" || stringOf(tc["name"]) == "" {
				return fmt.Errorf("unsupported tool_choice")
			}
			chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc["name"]}}
		} else if v == "auto" || v == "none" || v == "required" {
			chat["tool_choice"] = v
		} else {
			return fmt.Errorf("unsupported tool_choice")
		}
	}
	return nil
}

// convertResponseTool 把 Responses 的函数工具定义转成 chat 的 {"type":"function","function":{...}}。
// 顶层 tools 与 input 中的 additional_tools 项共用同一份校验与字段搬运。
func convertResponseTool(t any) (map[string]any, error) {
	tool := objectOf(t)
	if tool["type"] != "function" || stringOf(tool["name"]) == "" {
		return nil, fmt.Errorf("only named function tools are supported")
	}
	fn := map[string]any{}
	for _, k := range []string{"name", "description", "parameters", "strict"} {
		if v, ok := tool[k]; ok {
			fn[k] = v
		}
	}
	return map[string]any{"type": "function", "function": fn}, nil
}

// dedupeResponseTools 按函数名去重（保留先出现者），避免顶层 tools 与 additional_tools 重叠时
// 向上游发送重复工具定义。
func dedupeResponseTools(tools []any) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		name := stringOf(objectOf(objectOf(t)["function"])["name"])
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, t)
	}
	return out
}

func appendToolCall(msgs *[]any, call map[string]any) {
	if n := len(*msgs); n > 0 {
		last := objectOf((*msgs)[n-1])
		if last["role"] == "assistant" {
			calls, _ := last["tool_calls"].([]any)
			last["tool_calls"] = append(calls, call)
			return
		}
	}
	*msgs = append(*msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}})
}

func responseContent(value any) (any, error) {
	if s, ok := value.(string); ok {
		return s, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be a string or an array")
	}
	converted := []any{}
	for _, p := range parts {
		part := objectOf(p)
		switch part["type"] {
		case "input_text", "output_text":
			text, ok := part["text"].(string)
			if !ok {
				return nil, fmt.Errorf("text content requires text")
			}
			converted = append(converted, map[string]any{"type": "text", "text": text})
		case "input_image":
			url := stringOf(part["image_url"])
			if url == "" {
				return nil, fmt.Errorf("input_image requires image_url; file_id is not supported")
			}
			image := map[string]any{"url": url}
			if v, ok := part["detail"]; ok {
				image["detail"] = v
			}
			converted = append(converted, map[string]any{"type": "image_url", "image_url": image})
		case "refusal":
			text, ok := part["refusal"].(string)
			if !ok {
				return nil, fmt.Errorf("refusal content requires refusal text")
			}
			converted = append(converted, map[string]any{"type": "text", "text": text})
		default:
			return nil, fmt.Errorf("unsupported content type %q", stringOf(part["type"]))
		}
	}
	return converted, nil
}

func convertMessagesRequest(req, chat map[string]any) error {
	if !positiveInteger(req["max_tokens"]) {
		return fmt.Errorf("max_tokens must be a positive integer")
	}
	chat["max_tokens"] = req["max_tokens"]
	if v, ok := req["stop_sequences"]; ok {
		if err := validateStringArray(v, "stop_sequences"); err != nil {
			return err
		}
		chat["stop"] = v
	}
	if v := req["metadata"]; v != nil && objectOf(v) == nil {
		return fmt.Errorf("metadata must be an object")
	}
	if metadata := objectOf(req["metadata"]); metadata != nil {
		if v, ok := metadata["user_id"]; ok {
			chat["user"] = v
		}
	}
	if v := req["thinking"]; v != nil {
		thinking := objectOf(v)
		switch thinking["type"] {
		case "enabled":
			if !positiveInteger(thinking["budget_tokens"]) {
				return fmt.Errorf("thinking.budget_tokens must be a positive integer")
			}
			if budget := thinking["budget_tokens"].(float64); budget < 1024 || budget >= req["max_tokens"].(float64) {
				return fmt.Errorf("thinking.budget_tokens must be at least 1024 and less than max_tokens")
			}
			chat["thinking"] = thinking
		case "disabled":
			chat["thinking"] = thinking
		default:
			return fmt.Errorf("unsupported thinking.type")
		}
	}
	msgs := []any{}
	if v := req["system"]; v != nil {
		content, err := anthropicContent(v, false)
		if err != nil {
			return fmt.Errorf("system: %w", err)
		}
		msgs = append(msgs, map[string]any{"role": "system", "content": content})
	}
	input, ok := req["messages"].([]any)
	if !ok || len(input) == 0 {
		return fmt.Errorf("messages must be a non-empty array")
	}
	for i, m := range input {
		msg := objectOf(m)
		role := stringOf(msg["role"])
		if role != "user" && role != "assistant" {
			return fmt.Errorf("messages[%d]: role must be user or assistant", i)
		}
		if s, ok := msg["content"].(string); ok {
			msgs = append(msgs, map[string]any{"role": role, "content": s})
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			return fmt.Errorf("messages[%d].content must be a string or array", i)
		}
		content := []any{}
		calls := []any{}
		var reasoning strings.Builder
		for _, p := range parts {
			part := objectOf(p)
			switch part["type"] {
			case "tool_use":
				if role != "assistant" || stringOf(part["id"]) == "" || stringOf(part["name"]) == "" || objectOf(part["input"]) == nil {
					return fmt.Errorf("tool_use requires assistant role, id, name and object input")
				}
				args, _ := json.Marshal(part["input"])
				calls = append(calls, map[string]any{"id": part["id"], "type": "function", "function": map[string]any{"name": part["name"], "arguments": string(args)}})
			case "tool_result":
				if role != "user" || stringOf(part["tool_use_id"]) == "" {
					return fmt.Errorf("tool_result requires user role and tool_use_id")
				}
				v := part["content"]
				if v == nil {
					v = ""
				}
				c, err := anthropicContent(v, true)
				if err != nil {
					return err
				}
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": part["tool_use_id"], "content": c})
			case "thinking":
				if role != "assistant" {
					return fmt.Errorf("thinking requires assistant role")
				}
				text, ok := part["thinking"].(string)
				if !ok {
					return fmt.Errorf("thinking block requires thinking text")
				}
				reasoning.WriteString(text)
			default:
				c, err := anthropicContent([]any{p}, true)
				if err != nil {
					return err
				}
				content = append(content, c.([]any)...)
			}
		}
		if len(content) > 0 || len(calls) > 0 || reasoning.Len() > 0 || len(parts) == 0 {
			m := map[string]any{"role": role, "content": content}
			if len(calls) > 0 {
				m["tool_calls"] = calls
			}
			if reasoning.Len() > 0 {
				m["reasoning_content"] = reasoning.String()
			}
			msgs = append(msgs, m)
		}
	}
	chat["messages"] = msgs
	if v, exists := req["tools"]; exists {
		tools, ok := v.([]any)
		if !ok {
			return fmt.Errorf("tools must be an array")
		}
		converted := []any{}
		for _, t := range tools {
			tool := objectOf(t)
			if typ := stringOf(tool["type"]); typ != "" && typ != "custom" {
				return fmt.Errorf("only custom tools are supported")
			}
			if stringOf(tool["name"]) == "" || objectOf(tool["input_schema"]) == nil {
				return fmt.Errorf("tools require name and input_schema")
			}
			fn := map[string]any{"name": tool["name"], "parameters": tool["input_schema"]}
			if v, ok := tool["description"]; ok {
				fn["description"] = v
			}
			converted = append(converted, map[string]any{"type": "function", "function": fn})
		}
		chat["tools"] = converted
	}
	if v, exists := req["tool_choice"]; exists {
		tc := objectOf(v)
		switch tc["type"] {
		case "auto", "none":
			chat["tool_choice"] = tc["type"]
		case "any":
			chat["tool_choice"] = "required"
		case "tool":
			if stringOf(tc["name"]) == "" {
				return fmt.Errorf("tool_choice requires name")
			}
			chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc["name"]}}
		default:
			return fmt.Errorf("unsupported tool_choice.type")
		}
		if v, exists := tc["disable_parallel_tool_use"]; exists {
			disabled, ok := v.(bool)
			if !ok {
				return fmt.Errorf("disable_parallel_tool_use must be a boolean")
			}
			chat["parallel_tool_calls"] = !disabled
		}
	}
	return nil
}

func anthropicContent(value any, images bool) (any, error) {
	if s, ok := value.(string); ok {
		return s, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be a string or array")
	}
	out := []any{}
	for _, p := range parts {
		part := objectOf(p)
		switch part["type"] {
		case "text":
			s, ok := part["text"].(string)
			if !ok {
				return nil, fmt.Errorf("text block requires text")
			}
			out = append(out, map[string]any{"type": "text", "text": s})
		case "image":
			if !images {
				return nil, fmt.Errorf("system only supports text blocks")
			}
			source := objectOf(part["source"])
			url := ""
			switch source["type"] {
			case "url":
				url = stringOf(source["url"])
			case "base64":
				if stringOf(source["media_type"]) != "" && stringOf(source["data"]) != "" {
					url = "data:" + stringOf(source["media_type"]) + ";base64," + stringOf(source["data"])
				}
			}
			if url == "" {
				return nil, fmt.Errorf("image source requires url or base64 media_type/data")
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		default:
			return nil, fmt.Errorf("unsupported content block %q", stringOf(part["type"]))
		}
	}
	return out, nil
}

func (h *Handler) withMessagesAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.Header.Get("X-Api-Key") != "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+r.Header.Get("X-Api-Key"))
		}
		h.withAuth(next)(&messagesErrorWriter{ResponseWriter: w}, r)
	}
}

// Existing shared errors keep their HTTP status but use Anthropic's envelope.
type messagesErrorWriter struct {
	http.ResponseWriter
	status int
}

func (w *messagesErrorWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *messagesErrorWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *messagesErrorWriter) Write(p []byte) (int, error) {
	if w.status >= 400 {
		var body map[string]any
		if json.Unmarshal(p, &body) == nil {
			typ := "api_error"
			switch w.status {
			case 400, 413:
				typ = "invalid_request_error"
			case 401:
				typ = "authentication_error"
			case 403:
				typ = "permission_error"
			case 404:
				typ = "not_found_error"
			case 429:
				typ = "rate_limit_error"
			case 503:
				typ = "overloaded_error"
			}
			out, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": stringOf(objectOf(body["error"])["message"])}})
			_, err := w.ResponseWriter.Write(out)
			return len(p), err
		}
	}
	return w.ResponseWriter.Write(p)
}
