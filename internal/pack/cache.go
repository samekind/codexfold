package pack

import (
	"container/list"
	"sync"
)

type cacheEntry struct {
	key  string
	data []byte
}

type blockCache struct {
	mu     sync.Mutex
	budget int64
	used   int64
	items  map[string]*list.Element
	lru    list.List
}

func newBlockCache(budget int64) *blockCache {
	if budget < 0 {
		budget = 0
	}
	return &blockCache{budget: budget, items: make(map[string]*list.Element)}
}

func (c *blockCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(element)
	return element.Value.(cacheEntry).data, true
}

func (c *blockCache) put(key string, data []byte) {
	if int64(len(data)) > c.budget || c.budget == 0 {
		return
	}
	if cap(data) != len(data) {
		owned := make([]byte, len(data))
		copy(owned, data)
		data = owned
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.items[key]; ok {
		c.lru.MoveToFront(existing)
		return
	}
	element := c.lru.PushFront(cacheEntry{key: key, data: data})
	c.items[key] = element
	c.used += int64(len(data))
	for c.used > c.budget {
		oldest := c.lru.Back()
		entry := oldest.Value.(cacheEntry)
		delete(c.items, entry.key)
		c.used -= int64(len(entry.data))
		c.lru.Remove(oldest)
	}
}
