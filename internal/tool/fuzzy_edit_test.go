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

func TestValidateSyntaxIntegrity(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		orig     string
		modified string
		wantErr  bool
	}{
		{
			name:     "Valid JS/TSX balance",
			path:     "src/components/Card.tsx",
			orig:     "function Card() { return (<div><span>Hello</span></div>); }",
			modified: "function Card() { return (<div><span>World</span></div>); }",
			wantErr:  false,
		},
		{
			name:     "Unbalanced closing parenthesis in JSX",
			path:     "src/components/MessageBubble.tsx",
			orig:     "const f = () => { return 1; };",
			modified: "const f = () => { return 1; }));",
			wantErr:  true,
		},
		{
			name:     "Unbalanced curly brace in TSX",
			path:     "src/components/MessageBubble.tsx",
			orig:     "const a = { x: 1 };",
			modified: "const a = { x: 1 }};",
			wantErr:  true,
		},
		{
			name:     "Valid JSON",
			path:     "config.json",
			orig:     `{"name": "test"}`,
			modified: `{"name": "test2", "age": 20}`,
			wantErr:  false,
		},
		{
			name:     "Invalid JSON",
			path:     "config.json",
			orig:     `{"name": "test"}`,
			modified: `{"name": "test2", "age": }`,
			wantErr:  true,
		},
		{
			name:     "Valid Go file",
			path:     "main.go",
			orig:     "package main\nfunc main() { fmt.Println(1) }",
			modified: "package main\nfunc main() { fmt.Println(2) }",
			wantErr:  false,
		},
		{
			name:     "Unbalanced Go curly brace",
			path:     "main.go",
			orig:     "package main\nfunc main() { fmt.Println(1) }",
			modified: "package main\nfunc main() { fmt.Println(1) ",
			wantErr:  true,
		},
		{
			name:     "Valid Python with comments and strings",
			path:     "app.py",
			orig:     "def calc(a, b):\n    # comment (with parens)\n    return [a + b]  \"\"\"triple (str)\"\"\"",
			modified: "def calc(a, b):\n    # comment (with parens)\n    return [a * b]  \"\"\"triple (str)\"\"\"",
			wantErr:  false,
		},
		{
			name:     "Unbalanced Python bracket",
			path:     "app.py",
			orig:     "def calc(a, b):\n    return [a + b]",
			modified: "def calc(a, b):\n    return [a + b]]",
			wantErr:  true,
		},
		{
			name:     "Valid Rust",
			path:     "src/main.rs",
			orig:     "fn main() { println!(\"hello {:?}\", (1, 2)); }",
			modified: "fn main() { println!(\"world {:?}\", (3, 4)); }",
			wantErr:  false,
		},
		{
			name:     "Unbalanced Rust delimiter",
			path:     "src/main.rs",
			orig:     "fn main() { println!(\"hello\"); }",
			modified: "fn main() { println!(\"hello\"); }}",
			wantErr:  true,
		},
		{
			name:     "Valid SQL with dash comments",
			path:     "schema.sql",
			orig:     "SELECT * FROM users WHERE id IN (1, 2, 3); -- ignore ( )",
			modified: "SELECT id, name FROM users WHERE id IN (1, 2, 3); -- ignore ( )",
			wantErr:  false,
		},
		{
			name:     "Unbalanced SQL parentheses",
			path:     "schema.sql",
			orig:     "SELECT * FROM users WHERE id IN (1, 2, 3);",
			modified: "SELECT * FROM users WHERE id IN (1, 2, 3));",
			wantErr:  true,
		},
		{
			name:     "Valid Java/C#/PHP/C++ class",
			path:     "App.java",
			orig:     "public class App { public static void main(String[] args) { System.out.println(1); } }",
			modified: "public class App { public static void main(String[] args) { System.out.println(2); } }",
			wantErr:  false,
		},
		{
			name:     "Markdown prose ignores asymmetric punctuation",
			path:     "README.md",
			orig:     "# Notes\nHere is a smiley :) and a list [1, 2",
			modified: "# Notes\nHere is another smiley ;) and [1, 2, 3",
			wantErr:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSyntaxIntegrity(tc.path, tc.orig, tc.modified)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.name, err)
			}
		})
	}
}

func TestFindClosestBlock(t *testing.T) {
	content := `package main

func calculateTotal(items []Item) int {
    var sum int = 0
    for _, it := range items {
        sum += it.Price * it.Qty
    }
    return sum
}`

	target := `    var sum = 0
    for _, it := range items {
        sum += it.Price
    }`

	closest := FindClosestBlock(content, target)
	if closest == "" {
		t.Fatalf("expected closest block match, got empty")
	}
	if !strings.Contains(closest, "sum += it.Price * it.Qty") {
		t.Errorf("expected closest block to contain actual file lines, got: %s", closest)
	}
}
