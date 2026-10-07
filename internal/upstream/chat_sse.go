package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

const maxChatEventBytes = 16 << 20

var errEmptyChatStream = errors.New("upstream stream contained no valid data events")

type chatStreamChoice struct {
	finished   bool
	tools      map[int]map[string]string
	legacy     bool
	legacyName string
}

// ReadChatSSE is shared by all three API adapters. It handles SSE framing before
// decoding JSON, and only accepts EOF after DONE or explicit choice completion.
// Callback errors stop reading immediately (in particular on client disconnect).
func ReadChatSSE(r io.Reader, consume func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data strings.Builder
	states := map[int]*chatStreamChoice{}
	done, first := false, true
	dispatch := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		if payload == "[DONE]" {
			done = true
			return nil
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil || chunk == nil {
			return fmt.Errorf("invalid upstream SSE JSON")
		}
		if e := chunk["error"]; e != nil {
			return fmt.Errorf("upstream error: %v", e)
		}
		choices, ok := chunk["choices"].([]any)
		if !ok {
			if chunk["choices"] != nil {
				return fmt.Errorf("invalid upstream choices")
			}
			if _, ok := chunk["usage"].(map[string]any); !ok {
				return fmt.Errorf("upstream event is missing choices")
			}
		}
		for i, value := range choices {
			choice, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid upstream choice")
			}
			idx, err := chatIndex(choice, "index", i)
			if err != nil {
				return err
			}
			if v := choice["finish_reason"]; v != nil {
				if _, ok := v.(string); !ok {
					return fmt.Errorf("invalid upstream finish_reason")
				}
			}
			finish, _ := choice["finish_reason"].(string)
			if choice["delta"] == nil && choice["message"] == nil && finish == "" {
				return fmt.Errorf("upstream choice is missing delta or message")
			}
			state := states[idx]
			if state == nil {
				state = &chatStreamChoice{tools: map[int]map[string]string{}}
				states[idx] = state
			}
			if finish != "" {
				state.finished = true
			}
			for _, key := range []string{"delta", "message"} {
				if v := choice[key]; v != nil {
					delta, ok := v.(map[string]any)
					if !ok {
						return fmt.Errorf("invalid upstream %s", key)
					}
					for _, field := range []string{"content", "reasoning_content", "refusal", "role"} {
						if v := delta[field]; v != nil {
							if _, ok := v.(string); !ok {
								return fmt.Errorf("invalid upstream %s", field)
							}
						}
					}
					if v := delta["function_call"]; v != nil {
						if err := validateFunctionDelta(v); err != nil {
							return err
						}
						fn := v.(map[string]any)
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						if name != "" || args != "" {
							state.legacy = true
						}
						if name != "" {
							state.legacyName = name
						}
					}
					if v := delta["tool_calls"]; v != nil {
						calls, ok := v.([]any)
						if !ok {
							return fmt.Errorf("invalid upstream tool_calls")
						}
						for j, value := range calls {
							call, ok := value.(map[string]any)
							if !ok {
								return fmt.Errorf("invalid upstream tool call")
							}
							toolIdx, err := chatIndex(call, "index", j)
							if err != nil {
								return err
							}
							for _, field := range []string{"id", "type"} {
								if v := call[field]; v != nil {
									if _, ok := v.(string); !ok {
										return fmt.Errorf("invalid upstream tool %s", field)
									}
								}
							}
							if v := call["function"]; v != nil {
								if err := validateFunctionDelta(v); err != nil {
									return err
								}
							}
							meta := state.tools[toolIdx]
							if meta == nil {
								meta = map[string]string{}
								state.tools[toolIdx] = meta
							}
							if id, _ := call["id"].(string); id != "" {
								meta["id"] = id
							}
							fn, _ := call["function"].(map[string]any)
							if name, _ := fn["name"].(string); name != "" {
								meta["name"] = name
							}
						}
					}
				}
			}
		}
		return consume(chunk)
	}
	for {
		// ReadSlice bounds even a single unterminated line, unlike ReadString.
		var line strings.Builder
		var readErr error
		for {
			fragment, err := br.ReadSlice('\n')
			if line.Len()+len(fragment) > maxChatEventBytes {
				return fmt.Errorf("upstream SSE event exceeds size limit")
			}
			line.Write(fragment)
			if err != bufio.ErrBufferFull {
				readErr = err
				break
			}
		}
		value := strings.TrimRight(line.String(), "\r\n")
		if first {
			value = strings.TrimPrefix(value, "\uFEFF")
			first = false
		}
		if value == "" {
			if err := dispatch(); err != nil {
				return err
			}
		} else if strings.HasPrefix(value, "data:") {
			value = strings.TrimPrefix(strings.TrimPrefix(value, "data:"), " ")
			if data.Len()+len(value)+1 > maxChatEventBytes {
				return fmt.Errorf("upstream SSE event exceeds size limit")
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
		if done {
			break
		}
		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			if err := dispatch(); err != nil {
				return err
			}
			break
		}
	}
	if len(states) == 0 {
		return errEmptyChatStream
	}
	for _, state := range states {
		if !done && !state.finished {
			return fmt.Errorf("upstream stream ended before completion")
		}
		for _, meta := range state.tools {
			if meta["id"] == "" || meta["name"] == "" {
				return fmt.Errorf("upstream tool call is missing id or name")
			}
		}
		if state.legacy && state.legacyName == "" {
			return fmt.Errorf("upstream function call is missing name")
		}
	}
	return nil
}

func chatIndex(object map[string]any, key string, fallback int) (int, error) {
	if value, exists := object[key]; exists {
		n, ok := value.(float64)
		if !ok || n < 0 || n > math.MaxInt32 || math.Trunc(n) != n {
			return 0, fmt.Errorf("invalid upstream %s", key)
		}
		return int(n), nil
	}
	object[key] = float64(fallback)
	return fallback, nil
}

func validateFunctionDelta(value any) error {
	fn, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid upstream function call")
	}
	for _, key := range []string{"name", "arguments"} {
		if v := fn[key]; v != nil {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("invalid upstream function %s", key)
			}
		}
	}
	return nil
}
