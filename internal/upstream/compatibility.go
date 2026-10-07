package upstream

import (
	"encoding/json"
	"math"
)

func normalizeRequestCompatibility(object map[string]any) {
	if alias, present := object["max_completion_tokens"]; present {
		if _, explicit := object["max_tokens"]; explicit {
			delete(object, "max_completion_tokens")
		} else if tokens, valid := alias.(float64); valid && tokens > 0 && tokens < float64(math.MaxInt64) && math.Trunc(tokens) == tokens {
			object["max_tokens"] = tokens
			delete(object, "max_completion_tokens")
		}
	}
	if _, present := object["stream_options"]; !present {
		object["stream_options"] = map[string]any{"include_usage": true}
	}
	messages, _ := object["messages"].([]any)
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		parts, _ := message["content"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part["type"] != "image_url" {
				continue
			}
			if imageURL, valid := part["image_url"].(string); valid && imageURL != "" {
				part["image_url"] = map[string]any{"url": imageURL}
			}
		}
	}
}

func normalizeUsage(usage map[string]any) map[string]any {
	result := make(map[string]any, len(usage)+4)
	for key, value := range usage {
		result[key] = value
	}
	if _, present := result["total_tokens"]; !present {
		prompt, hasPrompt := usageNumber(result["prompt_tokens"])
		completion, hasCompletion := usageNumber(result["completion_tokens"])
		if hasPrompt && hasCompletion {
			result["total_tokens"] = prompt + completion
		}
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	inputDetails, _ := usage["input_tokens_details"].(map[string]any)
	var cached float64
	for _, value := range []any{details["cached_tokens"], usage["prompt_cache_hit_tokens"], usage["cache_read_input_tokens"], usage["cached_tokens"], inputDetails["cached_tokens"]} {
		if tokens, valid := usageNumber(value); valid && tokens > 0 {
			cached = tokens
			break
		}
	}
	if cached <= 0 {
		return result
	}
	for _, key := range []string{"cached_tokens", "prompt_cache_hit_tokens", "cache_read_input_tokens"} {
		result[key] = cached
	}
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		original, exists := usage[key]
		if key == "input_tokens_details" && !exists {
			continue
		}
		copied := make(map[string]any)
		if fields, valid := original.(map[string]any); valid {
			for field, value := range fields {
				copied[field] = value
			}
		}
		copied["cached_tokens"] = cached
		result[key] = copied
	}
	return result
}

func usageNumber(value any) (float64, bool) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, number >= 0 && !math.IsInf(number, 0) && !math.IsNaN(number)
}
