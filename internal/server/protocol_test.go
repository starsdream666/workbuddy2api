package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

func decodeObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, raw)
	}
	return obj
}

func TestResponsesInputConversion(t *testing.T) {
	raw := `{"model":"codebuddy/test","instructions":"be precise","store":false,"max_output_tokens":100,"reasoning":{"effort":"high"},"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true}],"tool_choice":{"type":"function","name":"lookup"},"input":[{"role":"user","content":[{"type":"input_text","text":"read this"},{"type":"input_image","image_url":"data:image/png;base64,abc","detail":"low"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"checking"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"one"},{"type":"function_call_output","call_id":"call_2","output":"two"}]}`
	out, _, err := protocolToChat([]byte(raw), "responses")
	if err != nil {
		t.Fatal(err)
	}
	chat := decodeObject(t, string(out))
	msgs := chat["messages"].([]any)
	if len(msgs) != 5 || objectOf(msgs[0])["content"] != "be precise" {
		t.Fatalf("messages: %#v", msgs)
	}
	assistant := objectOf(msgs[2])
	if assistant["reasoning_content"] != "checking" || len(assistant["tool_calls"].([]any)) != 2 {
		t.Fatalf("tool history: %#v", assistant)
	}
	if objectOf(msgs[3])["tool_call_id"] != "call_1" || objectOf(msgs[4])["content"] != "two" {
		t.Fatalf("tool results: %#v", msgs)
	}
	parts := objectOf(msgs[1])["content"].([]any)
	if objectOf(objectOf(parts[1])["image_url"])["url"] != "data:image/png;base64,abc" {
		t.Fatalf("image: %#v", parts)
	}
	if chat["max_tokens"] != float64(100) || chat["reasoning_effort"] != "high" {
		t.Fatalf("options: %#v", chat)
	}
	if objectOf(objectOf(chat["response_format"])["json_schema"])["name"] != "answer" {
		t.Fatalf("format: %#v", chat)
	}
	if objectOf(objectOf(chat["tool_choice"])["function"])["name"] != "lookup" {
		t.Fatalf("tool choice: %#v", chat)
	}
}

func TestResponsesAdditionalTools(t *testing.T) {
	// PI-Desktop 的 gpt-6-luna 档案（compat.supportsAdditionalTools）会在 input 中途发
	// {type:"additional_tools", role:"developer", tools:[...]}。这些工具须与顶层 tools 合并。
	raw := `{"model":"test","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"extra","description":"d","parameters":{"type":"object"},"strict":true}]}],` +
		`"tools":[{"type":"function","name":"top","parameters":{"type":"object"}},{"type":"function","name":"extra","parameters":{"type":"object"}}]}`
	out, _, err := protocolToChat([]byte(raw), "responses")
	if err != nil {
		t.Fatal(err)
	}
	chat := decodeObject(t, string(out))
	tools := chat["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools: %#v", tools)
	}
	// input 先解析，additional_tools 的 extra 先入列；顶层同名 extra 被去重丢弃。
	first, second := objectOf(objectOf(tools[0])["function"]), objectOf(objectOf(tools[1])["function"])
	if first["name"] != "extra" || first["description"] != "d" || first["strict"] != true {
		t.Fatalf("additional tool fields: %#v", first)
	}
	if second["name"] != "top" {
		t.Fatalf("top-level tool: %#v", second)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 1 || objectOf(msgs[0])["role"] != "user" {
		t.Fatalf("additional_tools must not become a message: %#v", msgs)
	}
}

func TestResponsesAdditionalToolsOnly(t *testing.T) {
	// 只有 additional_tools、无顶层 tools 时也要出站 tools。
	raw := `{"model":"test","input":[{"role":"user","content":"hi"},` +
		`{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"extra","parameters":{"type":"object"}}]}]}`
	out, _, err := protocolToChat([]byte(raw), "responses")
	if err != nil {
		t.Fatal(err)
	}
	chat := decodeObject(t, string(out))
	tools := chat["tools"].([]any)
	if len(tools) != 1 || objectOf(objectOf(tools[0])["function"])["name"] != "extra" {
		t.Fatalf("tools: %#v", tools)
	}
}

func TestResponsesAdditionalToolsReachesUpstream(t *testing.T) {
	// 端到端：带 additional_tools 的 /v1/responses 请求应被接受（不再 400），
	// 且出站 chat 请求体里带上合并后的 tools。
	var outbound map[string]any
	up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		outbound = decodeObject(t, string(body))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})}}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret"})
	body := `{"model":"glm-5.2","input":[{"role":"user","content":"hi"},` +
		`{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"pi_workspace_file_guard_project_root","parameters":{"type":"object"}}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	tools, _ := outbound["tools"].([]any)
	if len(tools) != 1 || objectOf(objectOf(tools[0])["function"])["name"] != "pi_workspace_file_guard_project_root" {
		t.Fatalf("outbound tools: %#v", outbound)
	}
}

func TestMessagesInputConversion(t *testing.T) {
	raw := `{"model":"test","max_tokens":512,"system":[{"type":"text","text":"be brief"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"any","disable_parallel_tool_use":true},"stop_sequences":["END"],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"check"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"result"}]},{"type":"text","text":"continue"}]}]}`
	out, _, err := protocolToChat([]byte(raw), "messages")
	if err != nil {
		t.Fatal(err)
	}
	chat := decodeObject(t, string(out))
	msgs := chat["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages: %#v", msgs)
	}
	if objectOf(msgs[0])["role"] != "system" || objectOf(msgs[3])["role"] != "tool" || objectOf(msgs[4])["role"] != "user" {
		t.Fatalf("message order: %#v", msgs)
	}
	call := objectOf(objectOf(msgs[2])["tool_calls"].([]any)[0])
	if call["id"] != "call_1" || objectOf(call["function"])["arguments"] != `{"q":"x"}` {
		t.Fatalf("call: %#v", call)
	}
	if chat["tool_choice"] != "required" || chat["parallel_tool_calls"] != false {
		t.Fatalf("tool choice: %#v", chat)
	}
}

func TestProtocolRejectsUnsupportedRequests(t *testing.T) {
	for _, tc := range []struct{ protocol, body string }{
		{"responses", `null`}, {"responses", `[]`}, {"responses", `{"model":"x","input":"a"} {}`},
		{"responses", `{"input":"a"}`}, {"responses", `{"model":"x","input":null}`},
		{"responses", `{"model":"x","input":"a","stream":"true"}`},
		{"responses", `{"model":"x","input":"a","previous_response_id":"resp_old"}`},
		{"responses", `{"model":"x","input":"a","store":true}`},
		{"responses", `{"model":"x","input":"a","background":true}`},
		{"responses", `{"model":"x","input":"a","tools":[{"type":"web_search"}]}`},
		{"responses", `{"model":"x","input":[{"type":"item_reference","id":"old"}]}`},
		{"responses", `{"model":"x","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_x"}]}]}`},
		{"messages", `{"model":"x","messages":[{"role":"user","content":"a"}]}`},
		{"messages", `{"model":"x","max_tokens":-1,"messages":[]}`},
		{"messages", `{"model":"x","max_tokens":1,"messages":[{"role":"system","content":"a"}]}`},
		{"messages", `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document"}]}]}`},
		{"messages", `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"a"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`},
	} {
		t.Run(tc.protocol+tc.body, func(t *testing.T) {
			if _, _, err := protocolToChat([]byte(tc.body), tc.protocol); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func protocolHandler(t *testing.T, sse string) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: p, APIKey: "secret", Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })})
}

func protocolRequest(protocol string, stream bool) *http.Request {
	body := fmt.Sprintf(`{"model":"glm-5.2","stream":%t,"input":"hello"}`, stream)
	if protocol == "messages" {
		body = fmt.Sprintf(`{"model":"glm-5.2","stream":%t,"max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`, stream)
	}
	r := httptest.NewRequest("POST", "/v1/"+protocol, strings.NewReader(body))
	if protocol == "messages" {
		r.Header.Set("X-Api-Key", "secret")
	} else {
		r.Header.Set("Authorization", "Bearer secret")
	}
	return r
}

func streamEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	events := []map[string]any{}
	for _, frame := range strings.Split(strings.TrimSpace(raw), "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("invalid event: %s", frame)
		}
		event := decodeObject(t, strings.TrimPrefix(lines[1], "data: "))
		if event["type"] != strings.TrimPrefix(lines[0], "event: ") {
			t.Fatalf("event type mismatch: %s", frame)
		}
		events = append(events, event)
	}
	return events
}

func TestProtocolEndpoints(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				h := protocolHandler(t, sseOK)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, protocolRequest(protocol, stream))
				if rec.Code != 200 {
					t.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
				var obj map[string]any
				if stream {
					if !rec.Flushed || rec.Header().Get("Content-Type") != "text/event-stream" {
						t.Fatal("not flushed SSE")
					}
					events := streamEvents(t, rec.Body.String())
					if protocol == "responses" {
						if events[0]["type"] != "response.created" || events[len(events)-1]["type"] != "response.completed" {
							t.Fatalf("lifecycle: %#v", events)
						}
						for i, e := range events {
							if e["sequence_number"] != float64(i) {
								t.Fatalf("sequence: %#v", e)
							}
						}
						obj = objectOf(events[len(events)-1]["response"])
					} else {
						if events[0]["type"] != "message_start" || events[len(events)-1]["type"] != "message_stop" {
							t.Fatalf("lifecycle: %#v", events)
						}
						if objectOf(events[2]["delta"])["text"] != "你好" {
							t.Fatalf("text: %#v", events)
						}
						if objectOf(events[len(events)-2]["delta"])["stop_reason"] != "end_turn" {
							t.Fatal("wrong stop reason")
						}
						return
					}
				} else {
					obj = decodeObject(t, rec.Body.String())
				}
				if protocol == "responses" {
					if obj["object"] != "response" || obj["status"] != "completed" {
						t.Fatalf("response: %#v", obj)
					}
					item := objectOf(obj["output"].([]any)[0])
					if objectOf(item["content"].([]any)[0])["text"] != "你好" {
						t.Fatalf("text: %#v", item)
					}
				} else {
					if obj["type"] != "message" || obj["stop_reason"] != "end_turn" || objectOf(obj["content"].([]any)[0])["text"] != "你好" {
						t.Fatalf("message: %#v", obj)
					}
				}
				if objectOf(obj["usage"])["input_tokens"] != float64(1) || objectOf(obj["usage"])["output_tokens"] != float64(1) {
					t.Fatalf("usage: %#v", obj)
				}
			})
		}
	}
}

func TestProtocolAuthenticationAndBodyLimits(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		h := protocolHandler(t, sseOK)
		for _, status := range []int{401, 413, 400} {
			r := protocolRequest(protocol, false)
			switch status {
			case 401:
				r.Header.Del("Authorization")
				r.Header.Del("X-Api-Key")
			case 413:
				h.cfg.MaxBodyBytes = 1
			case 400:
				h.cfg.MaxBodyBytes = 1024
				r.Body = io.NopCloser(strings.NewReader("null"))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != status {
				t.Fatalf("%s got %d want %d: %s", protocol, rec.Code, status, rec.Body)
			}
			obj := decodeObject(t, rec.Body.String())
			if protocol == "messages" && obj["type"] != "error" {
				t.Fatalf("Anthropic error: %#v", obj)
			}
		}
	}
}

const toolSSE = "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"check\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}},{\"index\":1,\"id\":\"call_b\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"\\\"b\\\"}\"}},{\"index\":0,\"function\":{\"arguments\":\"\\\"a\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":5},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\ndata: [DONE]\n\n"

func TestProtocolParallelToolsAndReplay(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%t", protocol, stream), func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := map[string]any{"model": "test"}
				if err := serveProtocol(rec, strings.NewReader(toolSSE), req, protocol, stream); err != nil {
					t.Fatal(err)
				}
				var output []any
				if stream {
					events := streamEvents(t, rec.Body.String())
					args := map[int]string{}
					for _, e := range events {
						if e["type"] == "response.function_call_arguments.delta" {
							args[int(e["output_index"].(float64))] += stringOf(e["delta"])
						}
						if e["type"] == "content_block_delta" && objectOf(e["delta"])["type"] == "input_json_delta" {
							args[int(e["index"].(float64))] += stringOf(objectOf(e["delta"])["partial_json"])
						}
					}
					if args[1] != `{"q":"a"}` || args[2] != `{"q":"b"}` {
						t.Fatalf("interleaved arguments: %#v", args)
					}
					return
				}
				obj := decodeObject(t, rec.Body.String())
				if protocol == "responses" {
					output = obj["output"].([]any)
					if objectOf(output[1])["call_id"] != "call_a" || objectOf(output[2])["arguments"] != `{"q":"b"}` {
						t.Fatalf("tools: %#v", output)
					}
					if objectOf(objectOf(obj["usage"])["output_tokens_details"])["reasoning_tokens"] != float64(3) {
						t.Fatal("reasoning usage missing")
					}
					req["input"] = append(output, map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "a"}, map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "b"})
				} else {
					output = obj["content"].([]any)
					if obj["stop_reason"] != "tool_use" || objectOf(objectOf(output[2])["input"])["q"] != "b" {
						t.Fatalf("tools: %#v", obj)
					}
					if objectOf(obj["usage"])["input_tokens"] != float64(15) {
						t.Fatal("cached tokens counted twice")
					}
					req["max_tokens"] = 100
					req["messages"] = []any{map[string]any{"role": "assistant", "content": output}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_a", "content": "a"}, map[string]any{"type": "tool_result", "tool_use_id": "call_b", "content": "b"}}}}
				}
				raw, _ := json.Marshal(req)
				converted, _, err := protocolToChat(raw, protocol)
				if err != nil {
					t.Fatal(err)
				}
				msgs := decodeObject(t, string(converted))["messages"].([]any)
				if len(msgs) != 3 || len(objectOf(msgs[0])["tool_calls"].([]any)) != 2 {
					t.Fatalf("replayed history: %#v", msgs)
				}
			})
		}
	}
}

func TestProtocolStreamFailures(t *testing.T) {
	for _, sse := range []string{"", "data: [DONE]\n\n", "data: broken\n\n", "data: {\"error\":{\"message\":\"bad\"}}\n\n", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", strings.Replace(toolSSE, `\"b\"}`, `\"b\"`, 1)} {
		for _, protocol := range []string{"responses", "messages"} {
			for _, stream := range []bool{false, true} {
				rec := httptest.NewRecorder()
				err := serveProtocol(rec, strings.NewReader(sse), map[string]any{"model": "test"}, protocol, stream)
				if err == nil {
					t.Fatalf("expected error for %q", sse)
				}
				if !stream && rec.Code != 502 {
					t.Fatalf("status: %d", rec.Code)
				}
				if stream {
					events := streamEvents(t, rec.Body.String())
					want := "response.failed"
					if protocol == "messages" {
						want = "error"
					}
					if events[len(events)-1]["type"] != want {
						t.Fatalf("last event: %#v", events[len(events)-1])
					}
				}
			}
		}
	}
}

func TestProtocolLengthAndNoSpaceSSE(t *testing.T) {
	sse := "data:{\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\r\n\r\n"
	for _, protocol := range []string{"responses", "messages"} {
		rec := httptest.NewRecorder()
		if err := serveProtocol(rec, strings.NewReader(sse), map[string]any{"model": "test"}, protocol, false); err != nil {
			t.Fatal(err)
		}
		obj := decodeObject(t, rec.Body.String())
		if protocol == "responses" && (obj["status"] != "incomplete" || objectOf(obj["incomplete_details"])["reason"] != "max_output_tokens") {
			t.Fatalf("length: %#v", obj)
		}
		if protocol == "messages" && obj["stop_reason"] != "max_tokens" {
			t.Fatalf("length: %#v", obj)
		}
	}
}

// The writer must see a text delta before the upstream is allowed to finish.
func TestProtocolStreamsBeforeUpstreamCompletes(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		r, writer := io.Pipe()
		flushed := make(chan struct{}, 1)
		w := &deltaObserver{ResponseRecorder: httptest.NewRecorder(), flushed: flushed}
		done := make(chan error, 1)
		go func() { done <- serveProtocol(w, r, map[string]any{"model": "test"}, protocol, true); r.Close() }()
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		select {
		case <-flushed:
		case <-time.After(2 * time.Second):
			writer.Close()
			t.Fatal("stream buffered until completion")
		}
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		writer.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

type deltaObserver struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (w *deltaObserver) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), `"hello"`) {
		select {
		case w.flushed <- struct{}{}:
		default:
		}
	}
}

func TestProtocolRoutingRetryAndOutbound(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		var tokens []string
		up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			tokens = append(tokens, r.Header.Get("Authorization"))
			body, _ := io.ReadAll(r.Body)
			chat := decodeObject(t, string(body))
			if chat["messages"] == nil || chat["input"] != nil || chat["stream"] != true {
				t.Fatalf("outbound: %s", body)
			}
			status, response := 200, sseOK
			if len(tokens) == 1 {
				status, response = 402, `{"msg":"余额不足"}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
		})}}
		p := testPoolWith(&auth.Auth{UID: "a", AccessToken: "bad", ExpiresAt: 9999999999}, &auth.Auth{UID: "b", AccessToken: "good", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, protocolRequest(protocol, false))
		if rec.Code != 200 || !reflect.DeepEqual(tokens, []string{"Bearer bad", "Bearer good"}) {
			t.Fatalf("retry status=%d tokens=%v body=%s", rec.Code, tokens, rec.Body)
		}
	}
}

func TestProtocolDelayedToolMetadata(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"call_b\",\"function\":{\"name\":\"b\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function\":{\"name\":\"a\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	for _, protocol := range []string{"responses", "messages"} {
		rec := httptest.NewRecorder()
		if err := serveProtocol(rec, strings.NewReader(sse), map[string]any{"model": "test"}, protocol, true); err != nil {
			t.Fatal(err)
		}
		starts := 0
		for _, e := range streamEvents(t, rec.Body.String()) {
			if e["type"] == "response.output_item.added" || e["type"] == "content_block_start" {
				idx := e["output_index"]
				if protocol == "messages" {
					idx = e["index"]
				}
				if idx != float64(starts) {
					t.Fatalf("start out of order: %#v", e)
				}
				starts++
			}
		}
		if starts != 2 {
			t.Fatalf("starts=%d", starts)
		}
	}
}

func TestProtocolRealmAndChannelRouting(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		for _, prefix := range []bool{false, true} {
			recorder := &authzRecorder{}
			cn := testPoolWith(&auth.Auth{UID: "cn", Realm: realm.CN, AccessToken: "cn-token", ExpiresAt: 9999999999})
			wb := testPoolWith(&auth.Auth{UID: "wb", Realm: realm.WB, AccessToken: "wb-token", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: cn, Pools: map[string]*pool.Pool{realm.CN: cn, realm.WB: wb}, Upstream: recorderUpstream(recorder), APIKey: "secret", RealmKeys: map[string]string{realm.WB: "wb-key"}})
			r := protocolRequest(protocol, false)
			if prefix {
				raw, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(raw), "glm-5.2", "workbuddy/glm-5.2", 1)))
			} else {
				if protocol == "messages" {
					r.Header.Set("X-Api-Key", "wb-key")
				} else {
					r.Header.Set("Authorization", "Bearer wb-key")
				}
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != 200 || !reflect.DeepEqual(recorder.all(), []string{"Bearer wb-token"}) || !reflect.DeepEqual(recorder.modelsAll(), []string{"glm-5.2"}) {
				t.Fatalf("routing: status=%d tokens=%v models=%v", rec.Code, recorder.all(), recorder.modelsAll())
			}
		}
	}
}

func TestProtocolUsageAndLeaseRelease(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		for _, stream := range []bool{false, true} {
			h, logger, _, p := usageFixture(t, []int64{347})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, protocolRequest(protocol, stream))
			entries := logger.Recent(0)
			if rec.Code != 200 || len(entries) != 1 {
				t.Fatalf("status=%d entries=%v", rec.Code, entries)
			}
			e := entries[0]
			if e.Status != 200 || e.CompletionTokens != 1 || e.PromptTokens != 1 {
				t.Fatalf("usage: %+v", e)
			}
			if s, _ := p.Status("u1"); s.InFlight != 0 {
				t.Fatalf("leaked lease: %+v", s)
			}
		}
	}
}

type failingProtocolWriter struct{ header http.Header }

func (w *failingProtocolWriter) Header() http.Header       { return w.header }
func (w *failingProtocolWriter) WriteHeader(int)           {}
func (w *failingProtocolWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type unexpectedProtocolReader struct{ t *testing.T }

func (r unexpectedProtocolReader) Read([]byte) (int, error) {
	r.t.Error("read upstream after client disconnected")
	return 0, io.EOF
}

func TestProtocolClientDisconnectStopsReading(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		w := &failingProtocolWriter{header: http.Header{}}
		err := serveProtocol(w, unexpectedProtocolReader{t}, map[string]any{"model": "test"}, protocol, true)
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("error=%v", err)
		}
	}
}
