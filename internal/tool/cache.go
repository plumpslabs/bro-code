package tool

import (
	"sync"
)

// toolResultCache is a process-wide cache of tool results keyed by (tool, args).
// It is the "semantic tool-result cache" from the efficiency roadmap: repeated
// reads of the same file span, or the same grep over an unchanged tree, return
// the cached answer instead of re-reading the disk / re-spawning grep. Within a
// single agent turn (and across turns in a session) this removes the duplicated
// round-trips that bloat context and burn wall-clock. Entries are invalidated
// the moment a file is written or edited (see CacheInvalidatePath), so a cached
// read is always consistent with what the agent itself last wrote.
var toolResultCache = &ToolCache{
	entries:    make(map[string]cacheEntry),
	order:      make([]string, 0, 256),
	maxEntries: 256,
	maxBytes:   8 * 1024 * 1024, // 8 MB of cached payloads
}

// cacheEntry is one cached tool result. scope is either "file:<path>" (so a
// specific file's reads can be invalidated) or "global" (grep/glob results that
// can change when ANY file changes).
type cacheEntry struct {
	key   string
	val   string
	scope string
}

// ToolCache is a small bounded, FIFO-evicting cache for tool results.
type ToolCache struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	order      []string
	bytes      int
	maxEntries int
	maxBytes   int
}

func cacheKey(tool, args string) string { return tool + "\x00" + args }

// Get returns a cached result for (tool, args) and whether it was present.
func (c *ToolCache) Get(tool, args string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[cacheKey(tool, args)]
	if !ok {
		return "", false
	}
	return e.val, true
}

// Put stores a result. scope groups entries for invalidation: "file:<path>"
// invalidates with that path; "global" is dropped on any file write.
func (c *ToolCache) Put(tool, args, val, scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey(tool, args)
	if old, ok := c.entries[key]; ok {
		c.bytes -= len(old.val)
	} else {
		c.order = append(c.order, key)
	}
	c.entries[key] = cacheEntry{key: key, val: val, scope: scope}
	c.bytes += len(val)
	c.evict()
}

// InvalidatePath drops every cached entry that could be affected by a change to
// path: that file's own reads, plus every "global" (tree-wide) result.
func (c *ToolCache) InvalidatePath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	target := "file:" + path
	var keep []string
	for _, k := range c.order {
		e := c.entries[k]
		if e.scope == target || e.scope == "global" {
			c.bytes -= len(e.val)
			delete(c.entries, k)
			continue
		}
		keep = append(keep, k)
	}
	c.order = keep
}

func (c *ToolCache) evict() {
	for len(c.order) > c.maxEntries || c.bytes > c.maxBytes {
		if len(c.order) == 0 {
			return
		}
		oldest := c.order[0]
		c.order = c.order[1:]
		if e, ok := c.entries[oldest]; ok {
			c.bytes -= len(e.val)
			delete(c.entries, oldest)
		}
	}
}
