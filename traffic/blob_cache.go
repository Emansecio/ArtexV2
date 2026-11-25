package traffic

import (
	"container/list"
	"strings"
	"sync"
)

const trafficBlobCacheMaxEntries = 128

type trafficBlobEntry struct {
	key string
	val string
}

type trafficBlobLRU struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
}

var globalTrafficBlobCache = &trafficBlobLRU{
	entries: make(map[string]*list.Element, trafficBlobCacheMaxEntries),
	lru:     list.New(),
}

func (c *trafficBlobLRU) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return "", false
	}
	c.lru.MoveToFront(elem)
	return elem.Value.(*trafficBlobEntry).val, true
}

func (c *trafficBlobLRU) set(key, val string) {
	// Guard memory: skip caching unexpectedly huge rendered chunks (>64KB).
	if len(val) > 64*1024 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		elem.Value.(*trafficBlobEntry).val = val
		c.lru.MoveToFront(elem)
		return
	}
	for c.lru.Len() >= trafficBlobCacheMaxEntries {
		back := c.lru.Back()
		if back == nil {
			break
		}
		ev := back.Value.(*trafficBlobEntry)
		delete(c.entries, ev.key)
		c.lru.Remove(back)
	}
	c.entries[key] = c.lru.PushFront(&trafficBlobEntry{key: key, val: val})
}

func (c *trafficBlobLRU) clearPrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, elem := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
			c.lru.Remove(elem)
		}
	}
}
