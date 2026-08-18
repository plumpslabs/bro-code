package tool

import (
	"strings"
	"testing"
)

func TestApplyResilientEdit(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		target      string
		replacement string
		wantTier    string
		wantContain string
		wantErr     bool
	}{
		{
			name:        "Exact Match",
			content:     "func hello() {\n\tprintln(\"hello world\")\n}",
			target:      "println(\"hello world\")",
			replacement: "println(\"hello bro\")",
			wantTier:    "exact",
			wantContain: "println(\"hello bro\")",
		},
		{
			name:        "CRLF Normalized Match",
			content:     "line 1\r\nline 2\r\nline 3\r\n",
			target:      "line 2\n",
			replacement: "line TWO\n",
			wantTier:    "crlf-normalized",
			wantContain: "line TWO",
		},
		{
			name: "Line Trimmed Match (whitespace differences)",
			content: `package main

func calculateTotal(items []Item) int {
    var sum int = 0
    for _, it := range items {
        sum += it.Price
    }
    return sum
}`,
			target: `    var sum int = 0
    for _, it := range items {
	sum += it.Price
    }`,
			replacement: `    var sum int = 0
    for _, it := range items {
        sum += it.Price * it.Qty
    }`,
			wantTier:    "line-trimmed",
			wantContain: "sum += it.Price * it.Qty",
		},
		{
			name: "Relative Indent Alignment (2 spaces in target vs 4 spaces in file)",
			content: `class UserService {
    public function getUser($id) {
        $user = $this->db->find($id);
        return $user;
    }
}`,
			target: `  public function getUser($id) {
    $user = $this->db->find($id);
    return $user;
  }`,
			replacement: `  public function getUser($id) {
    $user = $this->cache->get($id) ?? $this->db->find($id);
    return $user;
  }`,
			wantTier:    "line-trimmed",
			wantContain: "    $user = $this->cache->get($id) ?? $this->db->find($id);",
		},
		{
			name: "Fuzzy Similarity Window (minor comment change in target)",
			content: `def process_data(data):
    # step 1: filter invalid records
    filtered = [d for d in data if d.is_valid()]
    # step 2: sort by timestamp
    filtered.sort(key=lambda x: x.timestamp)
    return filtered`,
			target: `    # step 1: filter invalid items
    filtered = [d for d in data if d.is_valid()]
    # step 2: sort by time
    filtered.sort(key=lambda x: x.timestamp)`,
			replacement: `    # step 1: filter and clean
    filtered = [d.clean() for d in data if d.is_valid()]
    # step 2: sort ascending
    filtered.sort(key=lambda x: x.timestamp)`,
			wantTier:    "fuzzy-similarity",
			wantContain: "[d.clean() for d in data if d.is_valid()]",
		},
		{
			name: "Completely Unmatched Target",
			content: `const a = 1;
const b = 2;`,
			target:      `def entirely_different_function(): pass`,
			replacement: `def new_func(): pass`,
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, tier, err := ApplyResilientEdit(tc.content, tc.target, tc.replacement)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil result: %q", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tier != tc.wantTier {
				t.Errorf("tier mismatch: got %q, want %q", tier, tc.wantTier)
			}
			if !strings.Contains(res, tc.wantContain) {
				t.Errorf("result %q does not contain %q", res, tc.wantContain)
			}
		})
	}
}
