package tool

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\xef\xbb\xbf")
}

// ValidateJSONNoDuplicateKeys checks if a JSON string contains duplicate keys
// within any object scope. Returns an error if duplicate keys are detected or
// if the JSON is malformed. BOM characters are silently stripped before parsing.
func ValidateJSONNoDuplicateKeys(content string) error {
	dec := json.NewDecoder(strings.NewReader(stripBOM(content)))
	
	type scope struct {
		isObject bool
		keys     map[string]bool
		expectKey bool
	}

	var stack []*scope

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid JSON syntax: %w", err)
		}

		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &scope{
					isObject:  true,
					keys:      make(map[string]bool),
					expectKey: true,
				})
			case '}':
				if len(stack) > 0 && stack[len(stack)-1].isObject {
					stack = stack[:len(stack)-1]
					if len(stack) > 0 && stack[len(stack)-1].isObject {
						stack[len(stack)-1].expectKey = true
					}
				}
			case '[':
				stack = append(stack, &scope{
					isObject:  false,
					expectKey: false,
				})
			case ']':
				if len(stack) > 0 && !stack[len(stack)-1].isObject {
					stack = stack[:len(stack)-1]
					if len(stack) > 0 && stack[len(stack)-1].isObject {
						stack[len(stack)-1].expectKey = true
					}
				}
			}
		case string:
			if len(stack) > 0 && stack[len(stack)-1].isObject {
				curr := stack[len(stack)-1]
				if curr.expectKey {
					if curr.keys[t] {
						return fmt.Errorf("duplicate key %q detected in object scope. Merge new entries into the existing %q block instead of creating a duplicate key", t, t)
					}
					curr.keys[t] = true
					curr.expectKey = false
				} else {
					curr.expectKey = true
				}
			}
		default:
			// Primitives (number, bool, null)
			if len(stack) > 0 && stack[len(stack)-1].isObject {
				stack[len(stack)-1].expectKey = true
			}
		}
	}

	return nil
}
