package tool

import "testing"

func TestToolCacheGetPut(t *testing.T) {
	c := &ToolCache{entries: make(map[string]cacheEntry), order: nil, maxEntries: 10, maxBytes: 1 << 20}
	c.Put("read_file", "k1", "hello", "file:/a.go")
	if v, ok := c.Get("read_file", "k1"); !ok || v != "hello" {
		t.Fatalf("expected hit hello, got %q ok=%v", v, ok)
	}
	if _, ok := c.Get("read_file", "missing"); ok {
		t.Fatalf("expected miss for unknown key")
	}
}

func TestToolCacheInvalidatePath(t *testing.T) {
	c := &ToolCache{entries: make(map[string]cacheEntry), order: nil, maxEntries: 10, maxBytes: 1 << 20}
	c.Put("read_file", "ra", "A", "file:/a.go")
	c.Put("grep", "g1", "G", "global") // tree-wide result
	c.Put("read_file", "rb", "B", "file:/b.go")

	c.InvalidatePath("/a.go")
	if _, ok := c.Get("read_file", "ra"); ok {
		t.Fatalf("file:/a.go read should be invalidated")
	}
	if _, ok := c.Get("grep", "g1"); ok {
		t.Fatalf("global (grep) entries must drop on any write")
	}
	if v, ok := c.Get("read_file", "rb"); !ok || v != "B" {
		t.Fatalf("unrelated file read should survive: %q ok=%v", v, ok)
	}
}

func TestToolCacheEvictsFIFO(t *testing.T) {
	c := &ToolCache{entries: make(map[string]cacheEntry), order: nil, maxEntries: 3, maxBytes: 1 << 30}
	for i := 0; i < 5; i++ {
		c.Put("t", string(rune('0'+i)), "x", "global")
	}
	// Oldest two (keys "0","1") should have been evicted.
	if _, ok := c.Get("t", "0"); ok {
		t.Fatalf("oldest entry should be evicted")
	}
	if _, ok := c.Get("t", "4"); !ok {
		t.Fatalf("newest entry should remain")
	}
}
