package discache

import (
	"iter"
	"sync"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

var _ cache.Cache[discord.Guild] = (*FlatCache[discord.Guild])(nil)

// FlatCache is a plain thread-safe Cache[T] implementation (non-grouped).
type FlatCache[T any] struct {
	mu    sync.RWMutex
	items map[snowflake.ID]T
}

func NewFlatCache[T any]() *FlatCache[T] {
	return &FlatCache[T]{items: make(map[snowflake.ID]T)}
}

func (c *FlatCache[T]) Get(id snowflake.ID) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.items[id]
	return v, ok
}

func (c *FlatCache[T]) Put(id snowflake.ID, entity T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[id] = entity
}

func (c *FlatCache[T]) Remove(id snowflake.ID) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[id]
	if ok {
		delete(c.items, id)
	}
	return v, ok
}

func (c *FlatCache[T]) RemoveIf(filterFunc cache.FilterFunc[T]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, v := range c.items {
		if filterFunc(v) {
			delete(c.items, id)
		}
	}
}

func (c *FlatCache[T]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

func (c *FlatCache[T]) All() iter.Seq[T] {
	return func(yield func(T) bool) {
		c.mu.RLock()
		defer c.mu.RUnlock()
		for _, v := range c.items {
			if !yield(v) {
				return
			}
		}
	}
}
