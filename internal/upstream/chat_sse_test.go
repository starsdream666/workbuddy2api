package upstream

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatSSEFramingAcrossAdapters(t *testing.T) {
	raw := "\uFEFF: heartbeat\r\n\r\nevent: message\r\ndata:{\"choices\":[{\"delta\":\r\ndata: {\"content\":\"你好\"},\"finish_reason\":\"stop\"}],\r\ndata:\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\r\n\r\ndata:[DONE]"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "你好" {
		t.Fatalf("response=%v", resp)
	}
	w := httptest.NewRecorder()
	if err := Stream(w, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), "你好") || strings.Count(w.Body.String(), "data: [DONE]") != 1 {
		t.Fatalf("output=%s", w.Body)
	}
}

func TestChatSSERejectsBadFrames(t *testing.T) {
	for _, raw := range []string{
		"data: null\n\n", "data: {}\n\n", "data: broken\n\n",
		"data: {\"error\":{\"message\":\"provider failed\"}}\n\n",
		"data: {\"choices\":[null]}\n\n",
		"data: {\"choices\":[{\"index\":-1,\"delta\":{}}]}\n\n",
		"data: {\"choices\":[{\"index\":0.5,\"delta\":{}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":[]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[null]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"function_call\":{\"arguments\":\"{}\"}},\"finish_reason\":\"function_call\"}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":2147483648}]}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":{}}}]}}]}\n\n",
		"data: {\"usage\":{\"completion_tokens\":3}}\n\n",
	} {
		t.Run(raw, func(t *testing.T) {
			raw += "data: [DONE]\n\n"
			if resp, err := Aggregate(strings.NewReader(raw)); err == nil {
				t.Fatalf("false success: %v", resp)
			}
			w := httptest.NewRecorder()
			if err := Stream(w, strings.NewReader(raw)); err == nil {
				t.Fatal("false stream success")
			}
			if !strings.Contains(w.Body.String(), `"error"`) || strings.Count(w.Body.String(), "data: [DONE]") != 1 {
				t.Fatalf("error lost: %s", w.Body)
			}
			if strings.Contains(raw, "provider failed") && !strings.Contains(w.Body.String(), "provider failed") {
				t.Fatalf("message lost: %s", w.Body)
			}
		})
	}
}

func TestChatSSETruncation(t *testing.T) {
	partial := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	if _, err := Aggregate(strings.NewReader(partial)); err == nil {
		t.Fatal("accepted truncated aggregate")
	}
	w := httptest.NewRecorder()
	if err := Stream(w, strings.NewReader(partial)); err == nil {
		t.Fatal("accepted truncated stream")
	}
	if !strings.Contains(w.Body.String(), "ended before completion") {
		t.Fatalf("missing error: %s", w.Body)
	}
	// A completed choice does not make other, still-open choices complete.
	multiple := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"},{\"index\":1,\"delta\":{\"content\":\"partial\"}}]}\n\n"
	if _, err := Aggregate(strings.NewReader(multiple)); err == nil {
		t.Fatal("accepted incomplete second choice")
	}
}

func TestAggregateKeepsChoicesAndRefusalsSeparate(t *testing.T) {
	raw := `data: {"choices":[{"index":1,"delta":{"refusal":"cannot"}},{"index":0,"delta":{"content":"A"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"B"},"finish_reason":"stop"},{"index":1,"delta":{"refusal":" answer"},"finish_reason":"content_filter"}]}` + "\n\ndata: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choices := resp["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices=%v", choices)
	}
	first, second := choices[0].(map[string]any), choices[1].(map[string]any)
	if first["index"] != 0 || first["message"].(map[string]any)["content"] != "AB" {
		t.Fatalf("first=%v", first)
	}
	if second["index"] != 1 || second["message"].(map[string]any)["refusal"] != "cannot answer" || second["finish_reason"] != "content_filter" {
		t.Fatalf("second=%v", second)
	}
}

func TestAggregateFullMessageToolsAndLegacyFunctions(t *testing.T) {
	for _, field := range []string{`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]`, `"function_call":{"name":"lookup","arguments":"{}"}`} {
		raw := `data: {"choices":[{"message":{"role":"assistant","reasoning_content":"check",` + field + `},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
		resp, err := Aggregate(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["reasoning_content"] != "check" {
			t.Fatalf("message=%v", msg)
		}
		if msg["tool_calls"] == nil && msg["function_call"] == nil {
			t.Fatalf("lost call: %v", msg)
		}
		if calls, ok := msg["tool_calls"].([]map[string]any); ok {
			if _, present := calls[0]["index"]; present {
				t.Fatalf("stream index in final tool call: %v", calls)
			}
		}
	}
}

func TestStreamFullMessageAndMetadata(t *testing.T) {
	raw := `data: {"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"full"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"usage":{"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"
	frames, _ := streamFrames(t, raw)
	if len(frames) != 2 {
		t.Fatalf("frames=%v", frames)
	}
	if frames[0]["id"] != frames[1]["id"] || frames[0]["created"] == nil || frames[1]["object"] != "chat.completion.chunk" {
		t.Fatalf("metadata=%v", frames)
	}
	choice := frames[0]["choices"].([]any)[0].(map[string]any)
	if choice["delta"].(map[string]any)["content"] != "full" {
		t.Fatalf("lost message=%v", choice)
	}
	if _, ok := frames[1]["choices"].([]any); !ok {
		t.Fatalf("usage frame choices=%v", frames[1])
	}
	second, _ := streamFrames(t, raw)
	if second[0]["id"] == frames[0]["id"] {
		t.Fatal("generated ids reused across requests")
	}
}

func TestChatSSESizeLimit(t *testing.T) {
	err := ReadChatSSE(io.MultiReader(strings.NewReader("data: "), io.LimitReader(zeroChatReader{}, maxChatEventBytes)), func(map[string]any) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("err=%v", err)
	}
}

type zeroChatReader struct{}

func (zeroChatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type brokenChatReader struct{}

func (brokenChatReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestChatSSEReadFailureEmitsError(t *testing.T) {
	w := httptest.NewRecorder()
	err := Stream(w, io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), brokenChatReader{}))
	if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("err=%v body=%s", err, w.Body)
	}
}

func FuzzChatSSE(f *testing.F) {
	for _, seed := range []string{sseFixture, "data: null\n\n", "data:[DONE]", "data: {\"choices\":[{}]}\n\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 65536 {
			t.Skip()
		}
		_, _ = Aggregate(strings.NewReader(raw))
		_ = ReadChatSSE(strings.NewReader(raw), func(map[string]any) error { return nil })
	})
}
