package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractEmbeddedToolCallsPoolside(t *testing.T) {
	rawContent := `<tool_call>edit_file<arg_key>path</arg_key><arg_value>/path/to/id.json</arg_value><arg_key>target</arg_key><arg_value>"roleModal": {
  "assignment": "Penugasan"
}</arg_value><arg_key>replacement</arg_key><arg_value>"roleModal": {
  "assignment": "Penugasan",
  "assignmentApproval": "Persetujuan Penugasan"
}</arg_value></tool_call><tool_call>edit_file<arg_key>path</arg_key><arg_value>/path/to/en.json</arg_value><arg_key>target</arg_key><arg_value>"roleModal": {
  "assignment": "Assignment"
}</arg_value><arg_key>replacement</arg_key><arg_value>"roleModal": {
  "assignment": "Assignment",
  "assignmentApproval": "Assignment Approval"
}</arg_value></tool_call>`

	calls, cleaned := ExtractEmbeddedToolCalls(rawContent)
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
	if cleaned != "" {
		t.Errorf("expected empty cleaned content, got %q", cleaned)
	}

	if calls[0].Name != "edit_file" {
		t.Errorf("call 0 name = %s, want edit_file", calls[0].Name)
	}
	var args0 map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args0); err != nil {
		t.Fatalf("failed to unmarshal args0: %v", err)
	}
	if args0["path"] != "/path/to/id.json" {
		t.Errorf("args0 path = %s, want /path/to/id.json", args0["path"])
	}

	if calls[1].Name != "edit_file" {
		t.Errorf("call 1 name = %s, want edit_file", calls[1].Name)
	}
	var args1 map[string]string
	if err := json.Unmarshal([]byte(calls[1].Arguments), &args1); err != nil {
		t.Fatalf("failed to unmarshal args1: %v", err)
	}
	if args1["path"] != "/path/to/en.json" {
		t.Errorf("args1 path = %s, want /path/to/en.json", args1["path"])
	}
}

func TestExtractEmbeddedToolCallsJSON(t *testing.T) {
	rawContent := `Here is the plan.
<tool_call>
{"name": "read_file", "arguments": {"path": "main.go"}}
</tool_call>
Done.`

	calls, cleaned := ExtractEmbeddedToolCalls(rawContent)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "read_file" {
		t.Errorf("call name = %s, want read_file", calls[0].Name)
	}
	if cleaned != "Here is the plan.\n\nDone." {
		t.Errorf("cleaned = %q", cleaned)
	}
}

func TestExtractEmbeddedReasoning(t *testing.T) {
	rawContent := `<think>
I need to check if roleModal already exists in id.json and en.json.
I will read lines 1 to 50 first.
</think>
Here is the verified translation.`

	reasoning, cleaned := ExtractEmbeddedReasoning(rawContent)
	if !strings.Contains(reasoning, "I need to check if roleModal") {
		t.Fatalf("expected reasoning extracted, got %q", reasoning)
	}
	if cleaned != "Here is the verified translation." {
		t.Fatalf("expected cleaned content, got %q", cleaned)
	}
}
