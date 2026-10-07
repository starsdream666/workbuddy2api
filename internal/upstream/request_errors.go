package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
)

func classifyRequestError(status int, body string) ErrKind {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge && status != http.StatusUnprocessableEntity {
		return ErrNone
	}
	var object map[string]any
	_ = json.Unmarshal([]byte(body), &object)
	lower := strings.ToLower(body)
	if hasErrorCode(object, 11115) || strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "context_length_exceeded") {
		return ErrPromptTooLong
	}
	if hasErrorCode(object, 11135) || strings.Contains(lower, "invalid_image_data") || strings.Contains(lower, "invalid image data") {
		return ErrImageInvalid
	}
	return ErrNone
}

func hasErrorCode(object map[string]any, expected int) bool {
	if code, valid := object["code"].(float64); valid && code == float64(expected) {
		return true
	}
	for _, key := range []string{"error", "data", "extError"} {
		if nested, valid := object[key].(map[string]any); valid && hasErrorCode(nested, expected) {
			return true
		}
	}
	return false
}

func RequestErrorHint(kind ErrKind) string {
	switch kind {
	case ErrPromptTooLong:
		return "request context exceeds the model limit; reduce message history"
	case ErrImageInvalid:
		return "image data rejected by upstream; check image_url format and image content"
	default:
		return ""
	}
}
