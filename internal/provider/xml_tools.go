package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	toolCallTagRe   = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)
	functionTagRe   = regexp.MustCompile(`(?s)<function=([a-zA-Z0-9_\-\.]+)\s*>(.*?)</function>`)
	argKeyValueRe   = regexp.MustCompile(`(?s)<arg_key>(.*?)</arg_key>\s*<arg_value>(.*?)</arg_value>`)
)

// ExtractEmbeddedToolCalls inspects message content for pseudo-XML tool calls
// emitted by models like Poolside laguna-s-2.1, Qwen, or custom fine-tunes.
// It parses the tool calls into standard ToolCall structs and returns the
// cleaned remaining text content.
func ExtractEmbeddedToolCalls(content string) ([]ToolCall, string) {
	if !strings.Contains(content, "<tool_call>") && !strings.Contains(content, "<function=") {
		return nil, content
	}

	var calls []ToolCall
	cleaned := content

	// 1. Match <tool_call>...</tool_call>
	cleaned = toolCallTagRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		inner := toolCallTagRe.FindStringSubmatch(match)
		if len(inner) < 2 {
			return match
		}
		raw := strings.TrimSpace(inner[1])

		// Case A: Poolside format <tool_call>name<arg_key>k</arg_key><arg_value>v</arg_value>...</tool_call>
		if strings.Contains(raw, "<arg_key>") {
			firstKeyIdx := strings.Index(raw, "<arg_key>")
			toolName := strings.TrimSpace(raw[:firstKeyIdx])
			argsMap := make(map[string]any)

			matches := argKeyValueRe.FindAllStringSubmatch(raw, -1)
			for _, m := range matches {
				if len(m) >= 3 {
					k := strings.TrimSpace(m[1])
					v := m[2] // preserve whitespace inside arg_value
					argsMap[k] = v
				}
			}

			if toolName != "" && len(argsMap) > 0 {
				argsBytes, _ := json.Marshal(argsMap)
				calls = append(calls, ToolCall{
					ID:        fmt.Sprintf("call_xml_%d_%d", time.Now().UnixNano(), len(calls)+1),
					Name:      toolName,
					Arguments: string(argsBytes),
				})
				return "" // strip XML tag from visible text
			}
		}

		// Case B: JSON object inside <tool_call>...</tool_call>
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			name, _ := obj["name"].(string)
			if name != "" {
				var argsStr string
				if argsObj, ok := obj["arguments"]; ok {
					switch a := argsObj.(type) {
					case string:
						argsStr = a
					default:
						b, _ := json.Marshal(a)
						argsStr = string(b)
					}
				} else {
					delete(obj, "name")
					b, _ := json.Marshal(obj)
					argsStr = string(b)
				}
				calls = append(calls, ToolCall{
					ID:        fmt.Sprintf("call_xml_%d_%d", time.Now().UnixNano(), len(calls)+1),
					Name:      name,
					Arguments: argsStr,
				})
				return ""
			}
		}

		// Case C: tool_name\n{json_arguments}
		lines := strings.SplitN(raw, "\n", 2)
		if len(lines) == 2 {
			toolName := strings.TrimSpace(lines[0])
			jsonArgs := strings.TrimSpace(lines[1])
			var testObj map[string]any
			if toolName != "" && json.Unmarshal([]byte(jsonArgs), &testObj) == nil {
				calls = append(calls, ToolCall{
					ID:        fmt.Sprintf("call_xml_%d_%d", time.Now().UnixNano(), len(calls)+1),
					Name:      toolName,
					Arguments: jsonArgs,
				})
				return ""
			}
		}

		return match
	})

	// 2. Match <function=name>arguments</function>
	cleaned = functionTagRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		sub := functionTagRe.FindStringSubmatch(match)
		if len(sub) >= 3 {
			toolName := strings.TrimSpace(sub[1])
			argsStr := strings.TrimSpace(sub[2])
			if toolName != "" {
				calls = append(calls, ToolCall{
					ID:        fmt.Sprintf("call_fn_%d_%d", time.Now().UnixNano(), len(calls)+1),
					Name:      toolName,
					Arguments: argsStr,
				})
				return ""
			}
		}
		return match
	})

	cleaned = strings.TrimSpace(cleaned)
	return calls, cleaned
}
