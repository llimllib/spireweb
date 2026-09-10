package web

import (
	"container/list"
	"os"
	"sync"

	"github.com/llimllib/spireweb/internal/session"
)

// sessionCache holds recently parsed sessions.
//
// Transcripts are parsed from the .jsonl on demand rather than stored, which
// keeps the database a pure search index and means the right pane cannot show
// stale content. The cost is re-parsing, and without a cache expanding a tool
// call would re-parse the whole file per click -- up to 7MB in this corpus.
//
// Entries are validated against the file's mtime and size, so a session pi is
// actively writing to is re-read rather than served from a stale entry.
type sessionCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List               // front = most recently used
	items map[string]*list.Element // path -> element
}

type cacheEntry struct {
	path    string
	mtime   int64
	size    int64
	session *session.Session
}

// defaultCacheSize is small on purpose. Sessions are read one at a time, the
// working set is whatever the user is looking at, and the tail of the size
// distribution is long: eight entries is at most a few hundred megabytes in
// the worst imaginable case and a few megabytes in practice.
const defaultCacheSize = 8

func newSessionCache(max int) *sessionCache {
	if max <= 0 {
		max = defaultCacheSize
	}
	return &sessionCache{max: max, ll: list.New(), items: map[string]*list.Element{}}
}

// Get returns the parsed session at path, parsing it if necessary.
func (c *sessionCache) Get(path string) (*session.Session, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mtime, size := fi.ModTime().UnixNano(), fi.Size()

	c.mu.Lock()
	if el, ok := c.items[path]; ok {
		e := el.Value.(*cacheEntry)
		if e.mtime == mtime && e.size == size {
			c.ll.MoveToFront(el)
			c.mu.Unlock()
			return e.session, nil
		}
		// Changed on disk: drop it and re-read below.
		c.ll.Remove(el)
		delete(c.items, path)
	}
	c.mu.Unlock()

	// Parsed outside the lock. Two requests for the same uncached session will
	// both parse it, which wastes work once; holding the lock across a 7MB
	// parse would instead block every other request in the server.
	s, err := session.Parse(path)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[path]; ok {
		c.ll.Remove(el)
	}
	el := c.ll.PushFront(&cacheEntry{path: path, mtime: mtime, size: size, session: s})
	c.items[path] = el
	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).path)
	}
	return s, nil
}
