package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func decodeCompat(test *testing.T, raw []byte) map[string]any {
	test.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		test.Fatal(err)
	}
	return object
}

func TestPanelRequestCompatibility(test *testing.T) {
	for _, sample := range []struct {
		name, body string
		tokens     any
		alias      bool
	}{
		{"alias", `{"max_completion_tokens":1234}`, float64(1234), false},
		{"explicit", `{"max_completion_tokens":1234,"max_tokens":500}`, float64(500), false},
		{"fraction", `{"max_completion_tokens":1.5}`, nil, true},
		{"negative", `{"max_completion_tokens":-1}`, nil, true},
		{"string", `{"max_completion_tokens":"1024"}`, nil, true},
		{"overflow", `{"max_completion_tokens":1e30}`, nil, true},
	} {
		test.Run(sample.name, func(test *testing.T) {
			object := decodeCompat(test, PrepareBodyOpt([]byte(sample.body), false))
			if object["max_tokens"] != sample.tokens {
				test.Errorf("max_tokens = %v", object["max_tokens"])
			}
			if _, present := object["max_completion_tokens"]; present != sample.alias {
				test.Error("unexpected alias presence")
			}
			if object["stream"] != true || object["stream_options"].(map[string]any)["include_usage"] != true {
				test.Error("missing stream usage")
			}
		})
	}
	object := decodeCompat(test, PrepareBodyOpt([]byte(`{"stream_options":{"include_usage":false,"extra":1},"messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:image/png;base64,AA=="},{"type":"image_url","image_url":{"url":"https://example.invalid/image","detail":"high"}}]}]}`), false))
	if object["stream_options"].(map[string]any)["include_usage"] != false {
		test.Error("explicit stream options changed")
	}
	parts := object["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,AA==" {
		test.Error("image not normalized")
	}
	if parts[1].(map[string]any)["image_url"].(map[string]any)["detail"] != "high" {
		test.Error("detail changed")
	}
	for _, raw := range []string{"null", "[]", "broken"} {
		if string(PrepareBodyOpt([]byte(raw), false)) != raw {
			test.Errorf("invalid input changed: %s", raw)
		}
	}
}

func TestPanelCacheKeyIsolation(test *testing.T) {
	body := []byte(`{"conversation_id":"same","model":"model"}`)
	first := decodeCompat(test, injectPromptCacheKey(body, "cn", "uid", "header"))["prompt_cache_key"]
	if first == nil {
		test.Fatal("missing key")
	}
	if first != decodeCompat(test, injectPromptCacheKey(body, "cn", "uid", "other"))["prompt_cache_key"] {
		test.Error("body did not take precedence")
	}
	for _, scope := range [][2]string{{"workbuddy", "uid"}, {"codebuddy", "uid"}, {"cn", "other"}} {
		if first == decodeCompat(test, injectPromptCacheKey(body, scope[0], scope[1], "header"))["prompt_cache_key"] {
			test.Errorf("scope collision: %v", scope)
		}
	}
	for _, body := range []string{`{}`, `{"metadata":{"user_id":"user-only"}}`, `{"prompt_cache_key":"explicit","conversation_id":"other"}`} {
		if string(injectPromptCacheKey([]byte(body), "cn", "uid", "")) != body {
			test.Errorf("unexpected cache injection: %s", body)
		}
	}
	if decodeCompat(test, injectPromptCacheKey([]byte(`{}`), "cn", "uid", "header"))["prompt_cache_key"] == nil {
		test.Error("header not supported")
	}
	if decodeCompat(test, injectPromptCacheKey([]byte(`{"metadata":{"conversationId":"camel"}}`), "cn", "uid", ""))["prompt_cache_key"] == nil {
		test.Error("metadata not supported")
	}
}

func TestPanelToolRepairOptIn(test *testing.T) {
	body := []byte(`{"messages":[{"role":"tool","tool_call_id":"orphan","content":"drop"},{"role":"assistant","content":null,"tool_calls":[{"id":"one"},{"id":"missing"},{"id":"two"}]},{"role":"tool","tool_call_id":"one","content":"1"},{"role":"system","content":"notice"},{"role":"tool","tool_call_id":"two","content":"2"},{"role":"user","content":"next"}]}`)
	client := &Client{}
	account := &auth.Auth{Realm: "cn"}
	unchanged := decodeCompat(test, client.prepareBodyFor(account, body))
	if len(unchanged["messages"].([]any)) != 6 {
		test.Fatal("repair should default off")
	}
	client.RepairToolHistory = true
	repaired := client.prepareBodyFor(account, body)
	messages := decodeCompat(test, repaired)["messages"].([]any)
	if len(messages) != 5 {
		test.Fatalf("messages = %s", repaired)
	}
	for index, role := range []string{"assistant", "tool", "tool", "system", "user"} {
		if messages[index].(map[string]any)["role"] != role {
			test.Errorf("role at %d", index)
		}
	}
	if len(messages[0].(map[string]any)["tool_calls"].([]any)) != 2 {
		test.Error("partial pair was not preserved")
	}
	if string(repairToolHistory(repaired)) != string(repaired) {
		test.Error("repair not idempotent")
	}
}

func TestPanelToolRepairCannotPairAcrossTurns(test *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"reused"}]},{"role":"user","content":"boundary"},{"role":"tool","tool_call_id":"reused","content":"late"},{"role":"assistant","tool_calls":[{"id":"reused"}]},{"role":"tool","tool_call_id":"reused","content":"valid"}]}`)
	messages := decodeCompat(test, repairToolHistory(body))["messages"].([]any)
	if len(messages) != 4 {
		test.Fatal("wrong repaired message count")
	}
	if _, present := messages[0].(map[string]any)["tool_calls"]; present {
		test.Error("paired across user boundary")
	}
	if messages[3].(map[string]any)["content"] != "valid" {
		test.Error("wrong reused ID result")
	}
}

func TestPanelToolRepairReordersResults(test *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"one"},{"id":"two"}]},{"role":"tool","tool_call_id":"two","content":"2"},{"role":"tool","tool_call_id":"one","content":"1"}]}`)
	repaired := repairToolHistory(body)
	messages := decodeCompat(test, repaired)["messages"].([]any)
	if messages[1].(map[string]any)["tool_call_id"] != "one" || messages[2].(map[string]any)["tool_call_id"] != "two" {
		test.Fatal("tool results not reordered")
	}
	if string(repairToolHistory(repaired)) != string(repaired) {
		test.Fatal("reordering not idempotent")
	}
}

func TestPanelUsageAliasesAndTotals(test *testing.T) {
	usage := decodeCompat(test, []byte(`{"prompt_tokens":100,"completion_tokens":10,"cached_tokens":0,"cache_read_input_tokens":0,"prompt_tokens_details":{"cached_tokens":80,"extra":3},"credit":0.2}`))
	before, _ := json.Marshal(usage)
	result := normalizeUsage(usage)
	for _, key := range []string{"cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens"} {
		if result[key] != float64(80) {
			test.Errorf("%s = %v", key, result[key])
		}
	}
	if result["total_tokens"] != float64(110) || result["credit"] != 0.2 {
		test.Error("totals or credit changed")
	}
	after, _ := json.Marshal(usage)
	if string(before) != string(after) {
		test.Error("input usage mutated")
	}
	zero := map[string]any{"cached_tokens": float64(0), "credit": 0.0}
	if !reflect.DeepEqual(zero, normalizeUsage(zero)) {
		test.Error("invented zero-use values")
	}
}

func TestPanelAggregateTruncatedTools(test *testing.T) {
	for _, done := range []bool{false, true} {
		chunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call", "type": "function", "function": map[string]any{"name": "tool", "arguments": `{"broken":`}}}}, "finish_reason": "length"}}}
		raw, _ := json.Marshal(chunk)
		stream := "data: " + string(raw) + "\n\n"
		if done {
			stream += "data: [DONE]\n\n"
		}
		if _, err := AggregateWithToolValidation(strings.NewReader(stream), true); err == nil {
			test.Error("truncated arguments accepted")
		}
		if _, err := Aggregate(strings.NewReader(stream)); err != nil {
			test.Errorf("opt-out changed: %v", err)
		}
	}
}
