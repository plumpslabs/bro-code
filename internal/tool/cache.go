package tool

import (
	"os"
	"sync"
	"time"
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

// readCache provides content-addressable caching for read_file specifically.
// Instead of keying on the full args string (which differs for start_line),
// it keys on the resolved file path + file mtime. If the file hasn't changed
// on disk since the last read, the cached content is returned instantly without
// touching the filesystem again — critical for multi-round reads of the same
// large file where the model issues read_file(path, start_line=50) after
// read_file(path, start_line=1).
var readCache = &ReadFileCache{
	entries: make(map[string]*readCacheEntry),
}

// readCacheEntry stores the full file content + mtime for path-based dedup.
type readCacheEntry struct {
	content   string
	mtime     time.Time
	lineCount int
}

// ReadFileCache is a path-keyed content-addressable cache for read_file.
type ReadFileCache struct {
	mu      sync.Mutex
	entries map[string]*readCacheEntry
}

// Get returns cached content for the given path if the file's mtime hasn't
// changed. Returns ("", false) on miss or stale entry.
func (rc *ReadFileCache) Get(path string) (string, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, ok := rc.entries[path]
	if !ok {
		return "", false
	}
	// Check mtime: if the file was modified since we cached it, invalidate.
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(e.mtime) {
		delete(rc.entries, path)
		return "", false
	}
	return e.content, true
}

// Put stores the file content indexed by path + current mtime.
func (rc *ReadFileCache) Put(path, content string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	lineCount := 0
	for range content {
		lineCount++
	}
	// More efficient line count (avoid scanning full content for large files)
	if len(content) > 0 {
		lineCount = 0
		for i := 0; i < len(content); i++ {
			if content[i] == '\n' {
				lineCount++
			}
		}
		if content[len(content)-1] != '\n' {
			lineCount++
		}
	}
	rc.entries[path] = &readCacheEntry{
		content:   content,
		mtime:     info.ModTime(),
		lineCount: lineCount,
	}
}

// Invalidate removes the cached entry for a specific path.
func (rc *ReadFileCache) Invalidate(path string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.entries, path)
}

// InvalidateAll clears the entire read cache (e.g., on turn start).
func (rc *ReadFileCache) InvalidateAll() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.entries = make(map[string]*readCacheEntry)
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
	// Also invalidate the content-addressable read cache for this path
	readCache.Invalidate(path)
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
