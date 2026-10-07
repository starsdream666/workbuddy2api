package server

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Validate before selecting an account: malformed requests cannot be repaired by
// rotation, and must not consume a lease or be reported as pool exhaustion.
func validateChatRequest(raw []byte) error {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil || req == nil {
		return fmt.Errorf("request body must be a JSON object")
	}
	return validateChatObject(req)
}

func validateChatObject(req map[string]any) error {
	if strings.TrimSpace(stringOf(req["model"])) == "" {
		return fmt.Errorf("model must be a non-empty string")
	}
	for _, key := range []string{"stream", "parallel_tool_calls", "logprobs", "store"} {
		if v := req[key]; v != nil {
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%s must be a boolean", key)
			}
		}
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "n"} {
		if v := req[key]; v != nil && !positiveInteger(v) {
			return fmt.Errorf("%s must be a positive integer", key)
		}
	}
	for _, spec := range []struct {
		key      string
		min, max float64
	}{{"temperature", 0, 2}, {"top_p", 0, 1}, {"frequency_penalty", -2, 2}, {"presence_penalty", -2, 2}} {
		if v := req[spec.key]; v != nil {
			n, ok := v.(float64)
			if !ok || n < spec.min || n > spec.max {
				return fmt.Errorf("%s must be a number between %g and %g", spec.key, spec.min, spec.max)
			}
		}
	}
	if v := req["stop"]; v != nil {
		if _, ok := v.(string); !ok {
			if err := validateStringArray(v, "stop"); err != nil {
				return err
			}
		}
	}
	if v := req["stream_options"]; v != nil {
		options := objectOf(v)
		if options == nil {
			return fmt.Errorf("stream_options must be an object")
		}
		for _, key := range []string{"include_usage", "include_obfuscation"} {
			if v := options[key]; v != nil {
				if _, ok := v.(bool); !ok {
					return fmt.Errorf("stream_options.%s must be a boolean", key)
				}
			}
		}
	}
	for _, key := range []string{"user", "conversation_id", "prompt_cache_key", "reasoning_effort"} {
		if v := req[key]; v != nil {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s must be a string", key)
			}
		}
	}
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return fmt.Errorf("messages must be a non-empty array")
	}
	for i, value := range msgs {
		m := objectOf(value)
		role := stringOf(m["role"])
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return fmt.Errorf("messages[%d]: unsupported role %q", i, role)
		}
		if v := m["tool_calls"]; v != nil {
			calls, ok := v.([]any)
			if !ok || role != "assistant" {
				return fmt.Errorf("messages[%d].tool_calls requires an assistant and an array", i)
			}
			for _, value := range calls {
				call := objectOf(value)
				if stringOf(call["id"]) == "" || call["type"] != "function" {
					return fmt.Errorf("messages[%d]: tool call requires id and function type", i)
				}
				if err := validateFunctionCall(call["function"]); err != nil {
					return fmt.Errorf("messages[%d]: %w", i, err)
				}
			}
		}
		if v := m["function_call"]; v != nil {
			if role != "assistant" {
				return fmt.Errorf("function_call requires assistant role")
			}
			if err := validateFunctionCall(v); err != nil {
				return err
			}
		}
		if role == "tool" && strings.TrimSpace(stringOf(m["tool_call_id"])) == "" {
			return fmt.Errorf("messages[%d]: tool message requires tool_call_id", i)
		}
		if role == "function" && strings.TrimSpace(stringOf(m["name"])) == "" {
			return fmt.Errorf("messages[%d]: function message requires name", i)
		}
		if m["content"] == nil && role == "assistant" && (m["tool_calls"] != nil || m["function_call"] != nil || m["refusal"] != nil || m["reasoning_content"] != nil) {
			continue
		}
		if err := validateChatContent(m["content"]); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	if v := req["tools"]; v != nil {
		tools, ok := v.([]any)
		if !ok {
			return fmt.Errorf("tools must be an array")
		}
		for _, value := range tools {
			tool := objectOf(value)
			if tool["type"] != "function" {
				return fmt.Errorf("only function tools are supported")
			}
			if err := validateFunctionDefinition(tool["function"]); err != nil {
				return err
			}
		}
	}
	if v := req["functions"]; v != nil {
		functions, ok := v.([]any)
		if !ok {
			return fmt.Errorf("functions must be an array")
		}
		for _, fn := range functions {
			if err := validateFunctionDefinition(fn); err != nil {
				return err
			}
		}
	}
	if v := req["tool_choice"]; v != nil {
		if name, ok := v.(string); ok {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("tool_choice must be non-empty")
			}
		} else {
			tc := objectOf(v)
			switch tc["type"] {
			case "auto", "none", "required": // Existing gateway compatibility forms.
			case "function":
				name := stringOf(objectOf(tc["function"])["name"])
				if name == "" {
					name = stringOf(tc["name"])
				}
				if strings.TrimSpace(name) == "" {
					return fmt.Errorf("tool_choice requires a function name")
				}
			default:
				return fmt.Errorf("tool_choice must be a string or a named function choice")
			}
		}
	}
	if v := req["response_format"]; v != nil {
		format := objectOf(v)
		switch format["type"] {
		case "text", "json_object":
		case "json_schema":
			schema := objectOf(format["json_schema"])
			if strings.TrimSpace(stringOf(schema["name"])) == "" || objectOf(schema["schema"]) == nil {
				return fmt.Errorf("response_format.json_schema requires name and object schema")
			}
			if v := schema["strict"]; v != nil {
				if _, ok := v.(bool); !ok {
					return fmt.Errorf("json_schema.strict must be a boolean")
				}
			}
		default:
			return fmt.Errorf("unsupported response_format.type")
		}
	}
	return nil
}

func validateStringArray(v any, field string) error {
	values, ok := v.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array of strings", field)
	}
	for _, value := range values {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be an array of strings", field)
		}
	}
	return nil
}

func validateFunctionDefinition(v any) error {
	fn := objectOf(v)
	if strings.TrimSpace(stringOf(fn["name"])) == "" {
		return fmt.Errorf("function requires a non-empty name")
	}
	if v := fn["parameters"]; v != nil && objectOf(v) == nil {
		return fmt.Errorf("function parameters must be an object")
	}
	if v := fn["description"]; v != nil {
		if _, ok := v.(string); !ok {
			return fmt.Errorf("function description must be a string")
		}
	}
	if v := fn["strict"]; v != nil {
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("function strict must be a boolean")
		}
	}
	return nil
}

func validateFunctionCall(v any) error {
	fn := objectOf(v)
	if strings.TrimSpace(stringOf(fn["name"])) == "" {
		return fmt.Errorf("function call requires a name")
	}
	// Arguments are opaque strings here: some clients replay incomplete calls.
	if _, ok := fn["arguments"].(string); !ok {
		return fmt.Errorf("function call arguments must be a string")
	}
	return nil
}

func validateChatContent(v any) error {
	if _, ok := v.(string); ok {
		return nil
	}
	parts, ok := v.([]any)
	if !ok {
		return fmt.Errorf("content must be a string or array")
	}
	for _, value := range parts {
		part := objectOf(value)
		switch part["type"] {
		case "text":
			if _, ok := part["text"].(string); !ok {
				return fmt.Errorf("text part requires text")
			}
		case "refusal":
			if _, ok := part["refusal"].(string); !ok {
				return fmt.Errorf("refusal part requires refusal")
			}
		case "image_url":
			url := stringOf(part["image_url"])
			if image := objectOf(part["image_url"]); image != nil {
				url = stringOf(image["url"])
			}
			if strings.TrimSpace(url) == "" {
				return fmt.Errorf("image_url requires a URL")
			}
		case "input_audio":
			audio := objectOf(part["input_audio"])
			if stringOf(audio["data"]) == "" || stringOf(audio["format"]) == "" {
				return fmt.Errorf("input_audio requires data and format")
			}
		case "file":
			file := objectOf(part["file"])
			if stringOf(file["file_id"]) == "" && stringOf(file["file_data"]) == "" {
				return fmt.Errorf("file requires file_id or file_data")
			}
		default:
			return fmt.Errorf("unsupported content part type %q", stringOf(part["type"]))
		}
	}
	return nil
}

func positiveInteger(v any) bool {
	n, ok := v.(float64)
	return ok && n > 0 && n < float64(math.MaxInt64) && math.Trunc(n) == n
}
