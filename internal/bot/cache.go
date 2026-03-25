package bot

import (
	"iter"
	"sync"
	"time"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

var _ cache.GroupedCache[discord.Member] = (*groupedCache[discord.Member])(nil)

func newGroupedCache[T any](entityExpiration time.Duration) cache.GroupedCache[T] {
	return &groupedCache[T]{
		cache:            make(map[snowflake.ID]map[snowflake.ID]cacheEntity[T]),
		entityExpiration: entityExpiration,
	}
}

type cacheEntity[T any] struct {
	value   T
	lastPut time.Time
}

type groupedCache[T any] struct {
	cache            map[snowflake.ID]map[snowflake.ID]cacheEntity[T]
	mu               sync.RWMutex
	entityExpiration time.Duration
}

func (g *groupedCache[T]) Get(groupID snowflake.ID, id snowflake.ID) (T, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		var entity T
		return entity, false
	}

	entity, ok := groupEntities[id]
	return entity.value, ok
}

func (g *groupedCache[T]) Put(groupID snowflake.ID, id snowflake.ID, entity T) {
	g.mu.Lock()
	defer g.mu.Unlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		groupEntities = make(map[snowflake.ID]cacheEntity[T])
		g.cache[groupID] = groupEntities
	}
	if g.entityExpiration > 0 {
		for _, gV := range g.cache {
			for cK, cV := range gV {
				if time.Since(cV.lastPut) > g.entityExpiration {
					delete(gV, cK)
				}
			}
		}
	}

	groupEntities[id] = cacheEntity[T]{value: entity, lastPut: time.Now()}
}

func (g *groupedCache[T]) Remove(groupID snowflake.ID, id snowflake.ID) (T, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		var entity T
		return entity, false
	}

	entity, ok := groupEntities[id]
	if !ok {
		return entity.value, false
	}

	delete(groupEntities, id)
	return entity.value, true
}

func (g *groupedCache[T]) GroupRemove(groupID snowflake.ID) {
	g.mu.Lock()
	defer g.mu.Unlock()

	delete(g.cache, groupID)
}

func (g *groupedCache[T]) RemoveIf(filterFunc cache.GroupedFilterFunc[T]) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for groupID, groupEntities := range g.cache {
		for id, entity := range groupEntities {
			if filterFunc(groupID, entity.value) {
				delete(groupEntities, id)
			}
		}
	}
}

func (g *groupedCache[T]) GroupRemoveIf(groupID snowflake.ID, filterFunc cache.GroupedFilterFunc[T]) {
	g.mu.Lock()
	defer g.mu.Unlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		return
	}

	for id, entity := range groupEntities {
		if filterFunc(groupID, entity.value) {
			delete(groupEntities, id)
		}
	}
}

func (g *groupedCache[T]) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var length int
	for _, groupEntities := range g.cache {
		length += len(groupEntities)
	}
	return length
}

func (g *groupedCache[T]) GroupLen(groupID snowflake.ID) int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		return 0
	}

	return len(groupEntities)
}

func (g *groupedCache[T]) ForEach(f func(groupID snowflake.ID, entity T)) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	for groupID, groupEntities := range g.cache {
		for _, entity := range groupEntities {
			f(groupID, entity.value)
		}
	}
}

func (g *groupedCache[T]) GroupForEach(groupID snowflake.ID, forEachFunc func(entity T)) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	groupEntities, ok := g.cache[groupID]
	if !ok {
		return
	}

	for _, entity := range groupEntities {
		forEachFunc(entity.value)
	}
}

func (g *groupedCache[T]) All() iter.Seq2[snowflake.ID, T] {
	return func(yield func(snowflake.ID, T) bool) {
		g.mu.Lock()
		defer g.mu.Unlock()

		for groupID, groupEntities := range g.cache {
			for _, entity := range groupEntities {
				if !yield(groupID, entity.value) {
					return
				}
			}
		}
	}
}

func (g *groupedCache[T]) GroupAll(groupID snowflake.ID) iter.Seq[T] {
	return func(yield func(T) bool) {
		g.mu.Lock()
		defer g.mu.Unlock()

		if groupEntities, ok := g.cache[groupID]; ok {
			for _, entity := range groupEntities {
				if !yield(entity.value) {
					return
				}
			}
		}
	}
}
