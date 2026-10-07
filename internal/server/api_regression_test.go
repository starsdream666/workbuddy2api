package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestAPIInvalidRequestsNeverReachUpstream(t *testing.T) {
	cases := []struct{ api, body string }{
		{"chat/completions", `null`}, {"chat/completions", `[]`}, {"chat/completions", `{}`},
		{"chat/completions", `{"model":"x","messages":[]} {}`},
		{"chat/completions", `{"model":"x","messages":[]}`},
		{"chat/completions", `{"model":2,"messages":[{"role":"user","content":"hi"}]}`},
		{"chat/completions", `{"model":"x","messages":[null]}`},
		{"chat/completions", `{"model":"x","messages":[{"role":"user","content":12}]}`},
		{"chat/completions", `{"model":"x","messages":[{"role":"tool","content":"ok"}]}`},
		{"chat/completions", `{"model":"x","messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":{}}}]}]}`},
		{"responses", `{"model":"x","input":"hi","reasoning":[]}`},
		{"responses", `{"model":"x","input":"hi","text":{"format":3}}`},
		{"responses", `{"model":"x","input":"hi","text":{"format":{"type":"json_schema","name":"f"}}}`},
		{"responses", `{"model":"x","input":[{"type":{},"role":"user","content":"hi"}]}`},
		{"responses", `{"model":"x","input":[{"type":"reasoning","summary":[{}]}]}`},
		{"responses", `{"model":"x","input":"hi","tools":[{"type":"function","name":"f","parameters":[]}]}`},
		{"messages", `{"model":"x","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stop_sequences":"END"}`},
		{"messages", `{"model":"x","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`},
		{"messages", `{"model":"x","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":"false"}}`},
	}
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, invalid := range []string{`"stream":"true"`, `"parallel_tool_calls":"false"`, `"temperature":"0.2"`, `"top_p":2`} {
			// parallel_tool_calls is a Chat/Responses field, not an Anthropic field.
			if api == "messages" && strings.Contains(invalid, "parallel_tool_calls") {
				continue
			}
			base := `"messages":[{"role":"user","content":"hi"}],"max_tokens":32`
			if api == "responses" {
				base = `"input":"hi"`
			}
			cases = append(cases, struct{ api, body string }{api, `{"model":"x",` + base + `,` + invalid + `}`})
		}
	}
	for _, tc := range cases {
		t.Run(tc.api+"/"+tc.body, func(t *testing.T) {
			calls := 0
			p := testPoolWith(&auth.Auth{UID: "u", AccessToken: "token", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/"+tc.api, strings.NewReader(tc.body)))
			if w.Code != 400 || calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body)
			}
			body := decodeObject(t, w.Body.String())
			if objectOf(body["error"])["type"] != "invalid_request_error" {
				t.Fatalf("error=%v", body)
			}
			if tc.api == "messages" && body["type"] != "error" {
				t.Fatalf("Anthropic envelope=%v", body)
			}
			status, _ := p.Status("u")
			if status.InFlight != 0 || status.ErrTotal != 0 || status.Cooling {
				t.Fatalf("account changed: %+v", status)
			}
		})
	}
}

func TestAPIMalformedAndInterruptedStreams(t *testing.T) {
	for _, sse := range []string{
		"data: {\"error\":{\"message\":\"provider failed\"}}\n\n",
		"data: {\"usage\":{\"completion_tokens\":3}}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: broken\n\n",
	} {
		for _, api := range []string{"chat/completions", "responses", "messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%t/%s", api, stream, sse), func(t *testing.T) {
					h := protocolHandler(t, sse)
					r := protocolRequest(api, stream)
					if api == "chat/completions" {
						r.Body = io.NopCloser(strings.NewReader(fmt.Sprintf(`{"model":"x","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)))
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if !stream && w.Code != 502 {
						t.Fatalf("status=%d body=%s", w.Code, w.Body)
					}
					if !strings.Contains(w.Body.String(), `"error"`) {
						t.Fatalf("missing error: %s", w.Body)
					}
					if strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "message_stop") {
						t.Fatalf("false success: %s", w.Body)
					}
					if stream && api == "chat/completions" && strings.Count(w.Body.String(), "data: [DONE]") != 1 {
						t.Fatalf("termination: %s", w.Body)
					}
					status, _ := h.cfg.Pool.Status("u1")
					if status.InFlight != 0 {
						t.Fatalf("lease leak: %+v", status)
					}
				})
			}
		}
	}
}

func TestResponsesNullableOptions(t *testing.T) {
	_, _, err := protocolToChat([]byte(`{"model":"x","input":"hi","stream":null,"tools":null,"tool_choice":null,"max_output_tokens":null,"reasoning":null,"text":null}`), "responses")
	if err != nil {
		t.Fatal(err)
	}
}

func TestProtocolLegacyFunctionCall(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"function_call\":{\"name\":\"lookup\",\"arguments\":\"{}\"}},\"finish_reason\":\"function_call\"}]}\n\ndata: [DONE]\n\n"
	for _, api := range []string{"responses", "messages"} {
		w := httptest.NewRecorder()
		if err := serveProtocol(w, strings.NewReader(sse), map[string]any{"model": "x"}, api, false); err != nil {
			t.Fatal(err)
		}
		body := decodeObject(t, w.Body.String())
		key := "output"
		if api == "messages" {
			key = "content"
		}
		items := body[key].([]any)
		if len(items) != 1 || objectOf(items[0])["name"] != "lookup" {
			t.Fatalf("lost function: %v", body)
		}
		if api == "messages" && body["stop_reason"] != "tool_use" {
			t.Fatalf("stop: %v", body)
		}
	}
}

func TestMessagesStopSequence(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\",\"stop_sequence\":\"END\"}]}\n\ndata: [DONE]\n\n"
	w := httptest.NewRecorder()
	if err := serveProtocol(w, strings.NewReader(sse), map[string]any{"model": "x"}, "messages", false); err != nil {
		t.Fatal(err)
	}
	body := decodeObject(t, w.Body.String())
	if body["stop_reason"] != "stop_sequence" || body["stop_sequence"] != "END" {
		t.Fatalf("stop=%v", body)
	}
}

func TestProtocolNonStreamWriteFailure(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		w := &failingProtocolWriter{header: make(map[string][]string)}
		if err := serveProtocol(w, strings.NewReader(sseOK), map[string]any{"model": "x"}, protocol, false); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("error=%v", err)
		}
	}
}

func FuzzAPIRequestValidation(f *testing.F) {
	for _, seed := range []string{`null`, `{}`, `{"model":"x","input":"hi"}`, `{"model":"x","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 65536 {
			t.Skip()
		}
		_ = validateChatRequest([]byte(raw))
		for _, api := range []string{"responses", "messages"} {
			out, _, err := protocolToChat([]byte(raw), api)
			if err == nil && !json.Valid(out) {
				t.Fatalf("invalid converted JSON: %s", out)
			}
		}
	})
}
