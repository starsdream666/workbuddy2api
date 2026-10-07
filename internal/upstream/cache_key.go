package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

func injectPromptCacheKey(body []byte, route, uid, conversationID string) []byte {
	var object map[string]any
	if json.Unmarshal(body, &object) != nil || object == nil || uid == "" {
		return body
	}
	if _, present := object["prompt_cache_key"]; present {
		return body
	}
	metadata, _ := object["metadata"].(map[string]any)
	conversation := ""
	for _, candidate := range []any{metadata["conversation_id"], metadata["conversationId"], object["conversation_id"], object["conversationId"], conversationID} {
		if text, valid := candidate.(string); valid && strings.TrimSpace(text) != "" {
			conversation = strings.TrimSpace(text)
			break
		}
	}
	if conversation == "" {
		return body
	}
	identity, _ := json.Marshal([]string{route, uid, conversation})
	digest := sha256.Sum256(identity)
	object["prompt_cache_key"] = "wb2a-" + hex.EncodeToString(digest[:])
	encoded, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return encoded
}
