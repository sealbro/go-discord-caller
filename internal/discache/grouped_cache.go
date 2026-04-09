package discache

import (
	"iter"
	"sync"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

var _ cache.GroupedCache[discord.Member] = (*GroupedCache[discord.Member])(nil)
var _ cache.GroupedCache[discord.Role] = (*GroupedCache[discord.Role])(nil)

type GroupedCache[T any] struct {
	mu    sync.RWMutex
	cache map[snowflake.ID]map[snowflake.ID]T
}

func NewGroupedCache[T any]() *GroupedCache[T] {
	return &GroupedCache[T]{
		cache: make(map[snowflake.ID]map[snowflake.ID]T),
	}
}

func (g *GroupedCache[T]) Get(groupID snowflake.ID, id snowflake.ID) (T, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		var zero T
		return zero, false
	}
	v, ok := groupEntities[id]
	return v, ok
}

func (g *GroupedCache[T]) Put(groupID snowflake.ID, id snowflake.ID, entity T) {
	g.mu.Lock()
	defer g.mu.Unlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		groupEntities = make(map[snowflake.ID]T)
		g.cache[groupID] = groupEntities
	}
	groupEntities[id] = entity
}

func (g *GroupedCache[T]) Remove(groupID snowflake.ID, id snowflake.ID) (T, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		var zero T
		return zero, false
	}
	v, ok := groupEntities[id]
	if !ok {
		return v, false
	}
	delete(groupEntities, id)
	return v, true
}

func (g *GroupedCache[T]) GroupRemove(groupID snowflake.ID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.cache, groupID)
}

func (g *GroupedCache[T]) RemoveIf(filterFunc cache.GroupedFilterFunc[T]) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for groupID, groupEntities := range g.cache {
		for id, v := range groupEntities {
			if filterFunc(groupID, v) {
				delete(groupEntities, id)
			}
		}
	}
}

func (g *GroupedCache[T]) GroupRemoveIf(groupID snowflake.ID, filterFunc cache.GroupedFilterFunc[T]) {
	g.mu.Lock()
	defer g.mu.Unlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		return
	}
	for id, v := range groupEntities {
		if filterFunc(groupID, v) {
			delete(groupEntities, id)
		}
	}
}

func (g *GroupedCache[T]) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var n int
	for _, groupEntities := range g.cache {
		n += len(groupEntities)
	}
	return n
}

func (g *GroupedCache[T]) GroupLen(groupID snowflake.ID) int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	return len(g.cache[groupID])
}

func (g *GroupedCache[T]) ForEach(f func(groupID snowflake.ID, entity T)) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	for groupID, groupEntities := range g.cache {
		for _, v := range groupEntities {
			f(groupID, v)
		}
	}
}

func (g *GroupedCache[T]) GroupForEach(groupID snowflake.ID, forEachFunc func(entity T)) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	for _, v := range g.cache[groupID] {
		forEachFunc(v)
	}
}

func (g *GroupedCache[T]) All() iter.Seq2[snowflake.ID, T] {
	return func(yield func(snowflake.ID, T) bool) {
		g.mu.RLock()
		defer g.mu.RUnlock()

		for groupID, groupEntities := range g.cache {
			for _, v := range groupEntities {
				if !yield(groupID, v) {
					return
				}
			}
		}
	}
}

func (g *GroupedCache[T]) GroupAll(groupID snowflake.ID) iter.Seq[T] {
	return func(yield func(T) bool) {
		g.mu.RLock()
		defer g.mu.RUnlock()

		for _, v := range g.cache[groupID] {
			if !yield(v) {
				return
			}
		}
	}
}
