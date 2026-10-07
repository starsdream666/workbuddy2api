package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

type protocolBlock struct {
	kind, id, callID, name, text string
	buffer                       strings.Builder
	index                        int
	started                      bool
}

type protocolOutput struct {
	w                           http.ResponseWriter
	req                         map[string]any
	protocol, id, model, finish string
	created                     int64
	stream                      bool
	seq                         int
	blocks                      []*protocolBlock
	byKey                       map[string]*protocolBlock
	usage                       map[string]any
	writeErr                    error
	stopSequence                string
}

func protocolID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}

func serveProtocol(w http.ResponseWriter, r io.Reader, req map[string]any, protocol string, stream bool) error {
	p := &protocolOutput{w: w, req: req, protocol: protocol, model: stringOf(req["model"]), created: time.Now().Unix(), stream: stream, byKey: map[string]*protocolBlock{}}
	p.id = protocolID("resp_")
	if protocol == "messages" {
		p.id = protocolID("msg_")
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if protocol == "responses" {
			p.emit("response.created", map[string]any{"response": p.response("in_progress")})
			p.emit("response.in_progress", map[string]any{"response": p.response("in_progress")})
		} else {
			p.emit("message_start", map[string]any{"message": p.message(false)})
		}
	}
	err := p.read(r)
	if err == nil {
		err = p.complete()
	}
	if p.writeErr != nil {
		return p.writeErr
	}
	if err != nil {
		if stream {
			if protocol == "responses" {
				resp := p.response("failed")
				resp["error"] = map[string]any{"code": "upstream_error", "message": err.Error()}
				p.emit("response.failed", map[string]any{"response": resp})
			} else {
				p.emit("error", map[string]any{"error": map[string]any{"type": "api_error", "message": err.Error()}})
			}
		} else {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		}
		return err
	}
	return p.writeErr
}

func (p *protocolOutput) emit(event string, fields map[string]any) {
	if !p.stream || p.writeErr != nil {
		return
	}
	fields["type"] = event
	if p.protocol == "responses" {
		fields["sequence_number"] = p.seq
		p.seq++
	}
	raw, err := json.Marshal(fields)
	if err == nil {
		_, err = fmt.Fprintf(p.w, "event: %s\ndata: %s\n\n", event, raw)
	}
	p.writeErr = err
	if err == nil {
		if f, ok := p.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (p *protocolOutput) read(r io.Reader) error {
	if p.writeErr != nil {
		return p.writeErr
	}
	return upstream.ReadChatSSE(r, func(chunk map[string]any) error {
		if usage := objectOf(chunk["usage"]); usage != nil {
			p.usage = usage
		}
		choices, _ := chunk["choices"].([]any)
		for _, value := range choices {
			choice := objectOf(value)
			if choice["index"] != float64(0) {
				return fmt.Errorf("upstream returned multiple choices for a single-response API")
			}
			if finish := stringOf(choice["finish_reason"]); finish != "" {
				p.finish = finish
			}
			if stop := stringOf(choice["stop_sequence"]); stop != "" {
				p.stopSequence = stop
			}
			delta := objectOf(choice["delta"])
			if delta == nil {
				delta = objectOf(choice["message"])
			}
			for _, field := range []struct{ name, kind string }{{"reasoning_content", "reasoning"}, {"content", "text"}, {"refusal", "refusal"}} {
				if text := stringOf(delta[field.name]); text != "" {
					p.addText(field.kind, text)
				}
			}
			calls, _ := delta["tool_calls"].([]any)
			for _, value := range calls {
				call := objectOf(value)
				idx := int(call["index"].(float64))
				b := p.block("tool:"+strconv.Itoa(idx), "tool")
				if id := stringOf(call["id"]); id != "" {
					if b.started && b.callID != id {
						return fmt.Errorf("upstream changed tool call id after its start")
					}
					b.callID = id
				}
				fn := objectOf(call["function"])
				if name := stringOf(fn["name"]); name != "" {
					if b.started && b.name != name {
						return fmt.Errorf("upstream changed tool name after its start")
					}
					b.name = name
				}
				p.appendText(b, stringOf(fn["arguments"]))
			}
			// Legacy chat function_call has no call id; generate one stable id so
			// either target API can replay the resulting tool call.
			if fn := objectOf(delta["function_call"]); fn != nil && (stringOf(fn["name"]) != "" || stringOf(fn["arguments"]) != "") {
				b := p.block("legacy-tool", "tool")
				if b.callID == "" {
					b.callID = protocolID("call_")
				}
				if name := stringOf(fn["name"]); name != "" {
					b.name = name
				}
				p.appendText(b, stringOf(fn["arguments"]))
			}
		}
		return p.writeErr
	})
}

func (p *protocolOutput) block(key, kind string) *protocolBlock {
	if b := p.byKey[key]; b != nil {
		return b
	}
	prefix := "msg_"
	if kind == "tool" {
		prefix = "fc_"
	} else if kind == "reasoning" {
		prefix = "rs_"
	}
	b := &protocolBlock{kind: kind, id: protocolID(prefix), index: len(p.blocks)}
	p.blocks = append(p.blocks, b)
	p.byKey[key] = b
	return b
}

func (p *protocolOutput) addText(kind, text string) {
	b := p.block(kind, kind)
	p.appendText(b, text)
}

func (p *protocolOutput) appendText(b *protocolBlock, text string) {
	b.buffer.WriteString(text)
	b.text = b.buffer.String()
	if b.started {
		p.delta(b, text)
		return
	}
	// SDK accumulators append items on start, so publish starts in index order
	// even when an upstream delays a tool's id/name until a later chunk.
	for _, pending := range p.blocks {
		if pending.started {
			continue
		}
		if pending.kind == "tool" && (pending.callID == "" || pending.name == "") {
			break
		}
		p.start(pending)
		p.delta(pending, pending.text)
	}
}

func (p *protocolOutput) start(b *protocolBlock) {
	b.started = true
	if p.protocol == "messages" {
		p.emit("content_block_start", map[string]any{"index": b.index, "content_block": p.anthropicBlock(b, true)})
		return
	}
	p.emit("response.output_item.added", map[string]any{"output_index": b.index, "item": p.responseItem(b, true, "in_progress")})
	if b.kind == "tool" {
		return
	}
	if b.kind == "reasoning" {
		p.emit("response.reasoning_summary_part.added", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
	} else {
		p.emit("response.content_part.added", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "part": responsePart(b, "")})
	}
}

func (p *protocolOutput) delta(b *protocolBlock, text string) {
	if text == "" {
		return
	}
	if p.protocol == "messages" {
		delta := map[string]any{"type": "text_delta", "text": text}
		if b.kind == "tool" {
			delta = map[string]any{"type": "input_json_delta", "partial_json": text}
		}
		if b.kind == "reasoning" {
			delta = map[string]any{"type": "thinking_delta", "thinking": text}
		}
		p.emit("content_block_delta", map[string]any{"index": b.index, "delta": delta})
		return
	}
	fields := map[string]any{"item_id": b.id, "output_index": b.index, "delta": text}
	event := "response.output_text.delta"
	switch b.kind {
	case "tool":
		event = "response.function_call_arguments.delta"
	case "reasoning":
		event = "response.reasoning_summary_text.delta"
		fields["summary_index"] = 0
	case "refusal":
		event = "response.refusal.delta"
		fields["content_index"] = 0
	default:
		fields["content_index"] = 0
		fields["logprobs"] = []any{}
	}
	p.emit(event, fields)
}

func (p *protocolOutput) complete() error {
	status := "completed"
	if p.finish == "length" || p.finish == "content_filter" {
		status = "incomplete"
	}
	for _, b := range p.blocks {
		if b.kind == "tool" {
			if b.callID == "" || b.name == "" {
				return fmt.Errorf("upstream tool call is missing id or name")
			}
			if b.text == "" {
				b.text = "{}"
				p.delta(b, "{}")
			}
			var args map[string]any
			if json.Unmarshal([]byte(b.text), &args) != nil || args == nil {
				// Responses can represent truncated arguments as an incomplete item.
				// Anthropic requires an object for tool_use.input, so never fabricate one.
				if p.protocol == "messages" || status != "incomplete" {
					return fmt.Errorf("upstream returned invalid tool arguments")
				}
			}
		}
	}
	for _, b := range p.blocks {
		if p.protocol == "messages" {
			p.emit("content_block_stop", map[string]any{"index": b.index})
			continue
		}
		fields := map[string]any{"item_id": b.id, "output_index": b.index}
		switch b.kind {
		case "tool":
			fields["arguments"] = b.text
			p.emit("response.function_call_arguments.done", fields)
		case "reasoning":
			fields["summary_index"] = 0
			fields["text"] = b.text
			p.emit("response.reasoning_summary_text.done", fields)
			p.emit("response.reasoning_summary_part.done", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": b.text}})
		default:
			fields["content_index"] = 0
			if b.kind == "refusal" {
				fields["refusal"] = b.text
				p.emit("response.refusal.done", fields)
			} else {
				fields["text"] = b.text
				fields["logprobs"] = []any{}
				p.emit("response.output_text.done", fields)
			}
			p.emit("response.content_part.done", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "part": responsePart(b, b.text)})
		}
		p.emit("response.output_item.done", map[string]any{"output_index": b.index, "item": p.responseItem(b, false, status)})
	}
	if p.protocol == "responses" {
		resp := p.response(status)
		if p.stream {
			p.emit("response."+status, map[string]any{"response": resp})
		} else {
			p.writeJSON(resp)
		}
	} else {
		if p.stream {
			p.emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": p.stopReason(), "stop_sequence": p.stopSequenceValue()}, "usage": p.anthropicUsage()})
			p.emit("message_stop", map[string]any{})
		} else {
			p.writeJSON(p.message(true))
		}
	}
	return p.writeErr
}

func (p *protocolOutput) writeJSON(value any) {
	var raw []byte
	raw, p.writeErr = json.Marshal(value)
	if p.writeErr != nil {
		return
	}
	p.w.Header().Set("Content-Type", "application/json")
	p.w.WriteHeader(http.StatusOK)
	_, p.writeErr = p.w.Write(raw)
}

func responsePart(b *protocolBlock, text string) map[string]any {
	if b.kind == "refusal" {
		return map[string]any{"type": "refusal", "refusal": text}
	}
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
}

func (p *protocolOutput) responseItem(b *protocolBlock, initial bool, status string) map[string]any {
	text := b.text
	if initial {
		text = ""
	}
	switch b.kind {
	case "tool":
		return map[string]any{"id": b.id, "type": "function_call", "call_id": b.callID, "name": b.name, "arguments": text, "status": status}
	case "reasoning":
		summary := []any{}
		if !initial {
			summary = append(summary, map[string]any{"type": "summary_text", "text": text})
		}
		return map[string]any{"id": b.id, "type": "reasoning", "summary": summary}
	default:
		content := []any{}
		if !initial {
			content = append(content, responsePart(b, text))
		}
		return map[string]any{"id": b.id, "type": "message", "role": "assistant", "status": status, "content": content}
	}
}

func (p *protocolOutput) response(status string) map[string]any {
	output := []any{}
	if status != "in_progress" {
		for _, b := range p.blocks {
			itemStatus := status
			if status == "failed" {
				itemStatus = "incomplete"
			}
			output = append(output, p.responseItem(b, false, itemStatus))
		}
	}
	r := map[string]any{"id": p.id, "object": "response", "created_at": p.created, "status": status, "model": p.model,
		"output": output, "error": nil, "incomplete_details": nil, "usage": nil, "store": false, "background": false,
		"instructions": nil, "max_output_tokens": nil, "metadata": map[string]any{}, "parallel_tool_calls": true,
		"previous_response_id": nil, "reasoning": map[string]any{"effort": nil, "summary": nil},
		"temperature": 1, "top_p": 1, "text": map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice": "auto", "tools": []any{}, "truncation": "disabled"}
	for _, key := range []string{"instructions", "max_output_tokens", "metadata", "parallel_tool_calls", "reasoning", "temperature", "top_p", "text", "tool_choice", "tools", "user"} {
		if v, ok := p.req[key]; ok {
			r[key] = v
		}
	}
	if status == "incomplete" {
		reason := "max_output_tokens"
		if p.finish == "content_filter" {
			reason = "content_filter"
		}
		r["incomplete_details"] = map[string]any{"reason": reason}
	}
	if status != "in_progress" && p.usage != nil {
		in, out, cached, reasoning := p.tokenCounts()
		r["usage"] = map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out,
			"input_tokens_details": map[string]any{"cached_tokens": cached}, "output_tokens_details": map[string]any{"reasoning_tokens": reasoning}}
	}
	return r
}

func (p *protocolOutput) anthropicBlock(b *protocolBlock, initial bool) map[string]any {
	text := b.text
	if initial {
		text = ""
	}
	switch b.kind {
	case "tool":
		args := map[string]any{}
		if !initial {
			_ = json.Unmarshal([]byte(text), &args)
		}
		return map[string]any{"type": "tool_use", "id": b.callID, "name": b.name, "input": args}
	case "reasoning":
		return map[string]any{"type": "thinking", "thinking": text, "signature": ""}
	default:
		return map[string]any{"type": "text", "text": text}
	}
}

func (p *protocolOutput) message(final bool) map[string]any {
	content := []any{}
	var stop any
	if final {
		for _, b := range p.blocks {
			content = append(content, p.anthropicBlock(b, false))
		}
		stop = p.stopReason()
	}
	return map[string]any{"id": p.id, "type": "message", "role": "assistant", "model": p.model, "content": content, "stop_reason": stop, "stop_sequence": p.stopSequenceValue(), "usage": p.anthropicUsage()}
}

func (p *protocolOutput) stopSequenceValue() any {
	if p.stopSequence != "" {
		return p.stopSequence
	}
	return nil
}

func (p *protocolOutput) stopReason() string {
	if p.stopSequence != "" {
		return "stop_sequence"
	}
	switch p.finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	for _, b := range p.blocks {
		if b.kind == "tool" {
			return "tool_use"
		}
	}
	return "end_turn"
}

func (p *protocolOutput) tokenCounts() (in, out, cached, reasoning int64) {
	get := func(obj map[string]any, key string) int64 { n, _ := obj[key].(float64); return int64(n) }
	in = get(p.usage, "prompt_tokens")
	if _, ok := p.usage["prompt_tokens"]; !ok {
		in = get(p.usage, "input_tokens")
	}
	out = get(p.usage, "completion_tokens")
	if _, ok := p.usage["completion_tokens"]; !ok {
		out = get(p.usage, "output_tokens")
	}
	cached = get(objectOf(p.usage["prompt_tokens_details"]), "cached_tokens")
	if cached == 0 {
		cached = get(objectOf(p.usage["input_tokens_details"]), "cached_tokens")
	}
	if cached == 0 {
		cached = int64(cachedTokensOf(p.usage))
	}
	reasoning = get(objectOf(p.usage["completion_tokens_details"]), "reasoning_tokens")
	if reasoning == 0 {
		reasoning = get(objectOf(p.usage["output_tokens_details"]), "reasoning_tokens")
	}
	return
}

func (p *protocolOutput) anthropicUsage() map[string]any {
	in, out, cached, _ := p.tokenCounts()
	uncached := in - cached
	if uncached < 0 {
		uncached = 0
	}
	return map[string]any{"input_tokens": uncached, "output_tokens": out, "cache_read_input_tokens": cached, "cache_creation_input_tokens": 0}
}
