package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditSymbolGoFunc(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "math.go")
	src := `package math

// Add returns the sum of two integers.
func Add(a, b int) int {
	return a + b
}

func Multiply(a, b int) int {
	return a * b
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &EditSymbolTool{}

	// Test 1: replace_body
	args := `{"path":"` + path + `","symbol":"Add","action":"replace_body","code":"return a + b + 0"}`
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("replace_body failed: %v", err)
	}
	if !strings.Contains(res, "Successfully edited symbol") {
		t.Errorf("unexpected result: %s", res)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "return a + b + 0") {
		t.Errorf("content not updated: %s", string(data))
	}

	// Test 2: replace_all
	newFunc := `// Add adds two numbers cleanly.
func Add(a, b int) int {
	// enhanced addition
	return a + b
}`
	args2 := `{"path":"` + path + `","symbol":"Add","action":"replace_all","code":` + escapeJSON(newFunc) + `}`
	_, err = tool.Execute(context.Background(), args2)
	if err != nil {
		t.Fatalf("replace_all failed: %v", err)
	}

	data2, _ := os.ReadFile(path)
	if !strings.Contains(string(data2), "// enhanced addition") {
		t.Errorf("content not updated with replace_all: %s", string(data2))
	}
}

func TestEditSymbolGoMethod(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "calc.go")
	src := `package calc

type Calculator struct {
	total int
}

func (c *Calculator) Add(v int) {
	c.total += v
}

func (c *Calculator) Total() int {
	return c.total
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &EditSymbolTool{}
	// Test matching without explicit pointer in symbol name
	args := `{"path":"` + path + `","symbol":"Calculator.Add","action":"replace_body","code":"c.total += v * 1"}`
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("method edit failed: %v", err)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "c.total += v * 1") {
		t.Errorf("method not updated: %s", string(data))
	}
}

func TestEditSymbolGoStruct(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "model.go")
	src := `package model

type Config struct {
	Port int
	Host string
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &EditSymbolTool{}
	newStruct := `type Config struct {
	Port int
	Host string
	Debug bool
}`
	args := `{"path":"` + path + `","symbol":"Config","action":"replace_all","code":` + escapeJSON(newStruct) + `}`
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("struct edit failed: %v", err)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "Debug bool") {
		t.Errorf("struct not updated: %s", string(data))
	}
}

func TestEditSymbolGoSyntaxErrorRejection(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "broken.go")
	src := `package main

func Run() {
	println("ok")
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &EditSymbolTool{}
	// Broken syntax with unclosed brace
	args := `{"path":"` + path + `","symbol":"Run","action":"replace_all","code":"func Run() {\n println(\"bad\""}`
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatalf("expected AST syntax error rejection, but got success")
	}
	if !strings.Contains(err.Error(), "AST syntax validation failed") {
		t.Errorf("expected AST error message, got: %v", err)
	}

	// Verify original file on disk was preserved untouched
	data, _ := os.ReadFile(path)
	if string(data) != src {
		t.Errorf("file on disk was corrupted despite syntax error!")
	}
}

func TestEditSymbolJavaScriptMethodWithComments(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "service.js")
	src := `const ProspectService = {
  // Returns { id: 1 } for testing
  async processExpiredBroadcasts(expired) {
    /* multiline comment with { braces } inside */
    for (const b of expired) {
      await cleanup(b);
    }
  },

  async otherMethod() {
    return true;
  }
};
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &EditSymbolTool{}
	replacement := `  async processExpiredBroadcasts(expired) {
    const ids = [...new Set(expired.map(b => b.id))];
    await batchCleanup(ids);
  },`

	args := `{"path":"` + path + `","symbol":"processExpiredBroadcasts","action":"replace_all","code":` + escapeJSON(replacement) + `}`
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("JavaScript method edit failed: %v", err)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "batchCleanup(ids)") {
		t.Errorf("JS content not updated: %s", string(data))
	}
	if !strings.Contains(string(data), "async otherMethod()") {
		t.Errorf("otherMethod was accidentally clobbered: %s", string(data))
	}
}

func TestEditSymbolStructuralBracketRejection(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "app.ts")
	src := `function handleRequest(req: any) {
  console.log(req);
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &EditSymbolTool{}
	// Broken replacement with unmatched closing brace
	replacement := `function handleRequest(req: any) {
  console.log(req);
}}`

	args := `{"path":"` + path + `","symbol":"handleRequest","action":"replace_all","code":` + escapeJSON(replacement) + `}`
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatalf("expected structural bracket rejection, got success")
	}

	// Verify original file on disk was preserved
	data, _ := os.ReadFile(path)
	if string(data) != src {
		t.Errorf("file on disk was corrupted despite structural error!")
	}
}

func TestEditSymbolPython(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "util.py")
	src := `def calculate_discount(price, rate):
    if price <= 0:
        return 0
    return price * rate

def format_currency(amount):
    return f"${amount:.2f}"
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &EditSymbolTool{}
	replacement := `def calculate_discount(price, rate):
    # Enhanced check
    if price <= 0 or rate <= 0:
        return 0
    return price * rate`

	args := `{"path":"` + path + `","symbol":"calculate_discount","action":"replace_all","code":` + escapeJSON(replacement) + `}`
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Python function edit failed: %v", err)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# Enhanced check") {
		t.Errorf("Python content not updated: %s", string(data))
	}
}

func escapeJSON(s string) string {
	b, _ := escapeJSONBytes(s)
	return string(b)
}

func escapeJSONBytes(s string) ([]byte, error) {
	var buf strings.Builder
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
	return []byte(buf.String()), nil
}
