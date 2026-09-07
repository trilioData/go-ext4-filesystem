package ext4

import (
	"container/list"
	"sync"
)

// defaultDirEntryCacheEntries bounds the directory-entry cache by the number
// of entries held rather than the number of directories, so one very large
// directory cannot consume everything. A DirectoryEntry2 is a name plus about
// 8 bytes, so a million entries is roughly 50 MB.
const defaultDirEntryCacheEntries = 1_000_000

// dirEntryCache holds parsed directory listings, keyed by directory inode.
//
// Reading the bytes of a directory is already cheap -- a byte-level cache in
// front of the device handles that. Parsing them is not: the entries are
// decoded field by field through reflection, and resolving a path re-parses
// every directory along it. A directory of n files was parsed once per lookup
// in it, so n times over a restore, and that accounted for about 30% of all
// CPU in a profile of one.
//
// The parsed result never changes for a read-only filesystem, so it is cached
// once. The returned slice is shared and must be treated as read-only.
type dirEntryCache struct {
	mu sync.Mutex

	maxEntries int
	curEntries int

	order *list.List               // front is most recently used
	items map[int64]*list.Element  // directory inode -> element
}

type dirEntryCacheItem struct {
	ino     int64
	entries []DirectoryEntry2
}

func newDirEntryCache(maxEntries int) *dirEntryCache {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &dirEntryCache{
		maxEntries: maxEntries,
		order:      list.New(),
		items:      make(map[int64]*list.Element),
	}
}

func (c *dirEntryCache) get(ino int64) ([]DirectoryEntry2, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[ino]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*dirEntryCacheItem).entries, true
}

func (c *dirEntryCache) put(ino int64, entries []DirectoryEntry2) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[ino]; ok {
		item := el.Value.(*dirEntryCacheItem)
		c.curEntries -= len(item.entries)
		item.entries = entries
		c.curEntries += len(entries)
		c.order.MoveToFront(el)
		return
	}

	c.items[ino] = c.order.PushFront(&dirEntryCacheItem{ino: ino, entries: entries})
	c.curEntries += len(entries)

	// Evict oldest-first until back under budget. A single directory larger
	// than the whole budget is kept anyway -- evicting it would leave the
	// cache empty and gain nothing.
	for c.curEntries > c.maxEntries && c.order.Len() > 1 {
		back := c.order.Back()
		item := c.order.Remove(back).(*dirEntryCacheItem)
		delete(c.items, item.ino)
		c.curEntries -= len(item.entries)
	}
}
