package upstream

import (
	"encoding/json"
	"log"
)

func repairToolHistory(body []byte) []byte {
	var object map[string]any
	if json.Unmarshal(body, &object) != nil || object == nil {
		return body
	}
	messages, valid := object["messages"].([]any)
	if !valid {
		return body
	}
	repaired, changes := repairToolMessages(messages)
	if changes == 0 {
		return body
	}
	object["messages"] = repaired
	encoded, err := json.Marshal(object)
	if err != nil {
		return body
	}
	log.Printf("WARN: [upstream] repaired tool history changes=%d", changes)
	return encoded
}

func repairToolMessages(messages []any) ([]any, int) {
	output := make([]any, 0, len(messages))
	changes := 0
	for position := 0; position < len(messages); position++ {
		message, valid := messages[position].(map[string]any)
		if !valid {
			output = append(output, messages[position])
			continue
		}
		if message["role"] == "tool" {
			changes++
			continue
		}
		calls, hasCalls := message["tool_calls"].([]any)
		if message["role"] != "assistant" || !hasCalls || len(calls) == 0 {
			output = append(output, message)
			continue
		}
		results := make(map[string]any)
		var resultOrder []string
		var displaced []any
		next := position + 1
		for ; next < len(messages); next++ {
			candidate, valid := messages[next].(map[string]any)
			if !valid {
				break
			}
			role, _ := candidate["role"].(string)
			if role == "system" || role == "developer" {
				displaced = append(displaced, candidate)
				continue
			}
			if role != "tool" {
				break
			}
			identifier, _ := candidate["tool_call_id"].(string)
			if _, duplicate := results[identifier]; identifier == "" || duplicate {
				changes++
				continue
			}
			results[identifier] = candidate
			resultOrder = append(resultOrder, identifier)
			if len(displaced) > 0 {
				changes++
			}
		}
		keptCalls := make([]any, 0, len(calls))
		keptResults := make([]any, 0, len(results))
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			identifier, _ := call["id"].(string)
			result, found := results[identifier]
			if identifier == "" || !found {
				changes++
				continue
			}
			keptCalls = append(keptCalls, rawCall)
			if resultOrder[len(keptResults)] != identifier {
				changes++
			}
			keptResults = append(keptResults, result)
			delete(results, identifier)
		}
		changes += len(results)
		copied := make(map[string]any, len(message))
		for key, value := range message {
			copied[key] = value
		}
		if len(keptCalls) == 0 {
			delete(copied, "tool_calls")
			if copied["content"] == nil {
				copied["content"] = ""
			}
		} else {
			copied["tool_calls"] = keptCalls
		}
		output = append(output, copied)
		output = append(output, keptResults...)
		output = append(output, displaced...)
		position = next - 1
	}
	return output, changes
}
