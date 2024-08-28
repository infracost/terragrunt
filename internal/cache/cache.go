package cache

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gruntwork-io/terragrunt/telemetry"
)

var MaxCacheSize = 8 * 1024 * 1024 * 1024 // 8 GB

// Cache - generic cache implementation
// type Cache[V any] struct {
// 	Name  string
// 	Cache map[string]V
// 	Mutex *sync.RWMutex
// }

// // NewCache - create new cache with generic type V
// func NewCache[V any](name string) *Cache[V] {
// 	return &Cache[V]{
// 		Name:  name,
// 		Cache: make(map[string]V),
// 		Mutex: &sync.RWMutex{},
// 	}
// }

// // Get - fetch value from cache by key
// func (c *Cache[V]) Get(ctx context.Context, key string) (V, bool) {
// 	c.Mutex.RLock()
// 	defer c.Mutex.RUnlock()

// 	keyHash := sha256.Sum256([]byte(key))
// 	cacheKey := hex.EncodeToString(keyHash[:])
// 	value, found := c.Cache[cacheKey]

// 	telemetry.Count(ctx, c.Name+"_cache_get", 1)

// 	if found {
// 		telemetry.Count(ctx, c.Name+"_cache_hit", 1)
// 	} else {
// 		telemetry.Count(ctx, c.Name+"_cache_miss", 1)
// 	}

// 	return value, found
// }

// // Put - put value into cache by key
// func (c *Cache[V]) Put(ctx context.Context, key string, value V) {
// 	c.Mutex.Lock()
// 	defer c.Mutex.Unlock()
// 	telemetry.Count(ctx, c.Name+"_cache_put", 1)

// 	keyHash := sha256.Sum256([]byte(key))
// 	cacheKey := hex.EncodeToString(keyHash[:])
// 	c.Cache[cacheKey] = value
// }

// LRUCacheItem - item in the LRU cache with its size
type LRUCacheItem[V any] struct {
	Key   string
	Value V
	Size  int
}

// Cache - LRU cache with memory size limit
type Cache[V any] struct {
	Name        string
	Cache       map[string]*list.Element
	Mutex       *sync.RWMutex
	List        *list.List
	MaxSize     int
	CurrentSize int
}

// NewCache - create a new LRU cache with memory size limit
func NewCache[V any](name string) *Cache[V] {
	cacheSize, err := strconv.ParseInt(os.Getenv("INFRACOST_TERRAGRUNT_CACHE_SIZE"), 10, 64)
	if cacheSize == 0 || err != nil {
		cacheSize = int64(MaxCacheSize)
	}
	return &Cache[V]{
		Name:        name,
		Cache:       make(map[string]*list.Element),
		Mutex:       &sync.RWMutex{},
		List:        list.New(),
		MaxSize:     int(cacheSize),
		CurrentSize: 0,
	}
}

// Get - fetch value from LRU cache by key
func (c *Cache[V]) Get(ctx context.Context, key string) (V, bool) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	keyHash := sha256.Sum256([]byte(key))
	cacheKey := hex.EncodeToString(keyHash[:])

	element, found := c.Cache[cacheKey]
	telemetry.Count(ctx, c.Name+"_cache_get", 1)

	var zeroValue V
	if !found {
		telemetry.Count(ctx, c.Name+"_cache_miss", 1)
		return zeroValue, false
	}

	// Move the accessed item to the front of the list
	c.List.MoveToFront(element)

	telemetry.Count(ctx, c.Name+"_cache_hit", 1)

	return element.Value.(*LRUCacheItem[V]).Value, true
}

// Put - put value into LRU cache by key, evicting items if necessary
func (c *Cache[V]) Put(ctx context.Context, key string, value V) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	telemetry.Count(ctx, c.Name+"_cache_put", 1)

	// Calculate the size of the new item
	itemSize := c.calculateSize(value)

	keyHash := sha256.Sum256([]byte(key))
	cacheKey := hex.EncodeToString(keyHash[:])

	if element, found := c.Cache[cacheKey]; found {
		// Update existing item and move to front
		c.List.MoveToFront(element)
		c.CurrentSize += itemSize - element.Value.(*LRUCacheItem[V]).Size
		element.Value = &LRUCacheItem[V]{Key: cacheKey, Value: value, Size: itemSize}
	} else {
		// Add new item
		newItem := &LRUCacheItem[V]{Key: cacheKey, Value: value, Size: itemSize}
		element := c.List.PushFront(newItem)
		c.Cache[cacheKey] = element
		c.CurrentSize += itemSize
	}

	// Evict items if necessary
	for c.CurrentSize > c.MaxSize {
		c.evict()
	}
}

// calculateSize - calculate the size of an item in bytes using gob encoding
func (c *Cache[V]) calculateSize(value V) int {
	var buffer bytes.Buffer
	encoder := gob.NewEncoder(&buffer)
	err := encoder.Encode(value)
	if err != nil {
		return 0
	}
	return buffer.Len()
}

// evict - remove the least recently used item from the cache
func (c *Cache[V]) evict() {
	// Get the least recently used item (back of the list)
	element := c.List.Back()
	if element == nil {
		return
	}

	item := element.Value.(*LRUCacheItem[V])

	// Remove it from the cache
	delete(c.Cache, item.Key)
	c.List.Remove(element)
	c.CurrentSize -= item.Size

	telemetry.Count(context.Background(), c.Name+"_cache_eviction", 1)
}

// ExpiringItem - item with expiration time
type ExpiringItem[V any] struct {
	Value      V
	Expiration time.Time
}

// ExpiringCache - cache with items with expiration time
type ExpiringCache[V any] struct {
	Name  string
	Cache map[string]ExpiringItem[V]
	Mutex *sync.RWMutex
}

// NewExpiringCache - create new cache with generic type V
func NewExpiringCache[V any](name string) *ExpiringCache[V] {
	return &ExpiringCache[V]{
		Name:  name,
		Cache: make(map[string]ExpiringItem[V]),
		Mutex: &sync.RWMutex{},
	}
}

// Get - fetch value from cache by key
func (c *ExpiringCache[V]) Get(ctx context.Context, key string) (V, bool) {
	c.Mutex.RLock()
	defer c.Mutex.RUnlock()
	item, found := c.Cache[key]
	telemetry.Count(ctx, c.Name+"_cache_get", 1)

	if !found {
		telemetry.Count(ctx, c.Name+"_cache_miss", 1)
		return item.Value, false
	}

	if time.Now().After(item.Expiration) {
		telemetry.Count(ctx, c.Name+"_cache_expiry", 1)
		delete(c.Cache, key)

		return item.Value, false
	}

	telemetry.Count(ctx, c.Name+"_cache_hit", 1)

	return item.Value, true
}

// Put - put value into cache by key
func (c *ExpiringCache[V]) Put(ctx context.Context, key string, value V, expiration time.Time) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	telemetry.Count(ctx, c.Name+"_cache_put", 1)
	c.Cache[key] = ExpiringItem[V]{Value: value, Expiration: expiration}
}

// ContextCache returns cache from the context. If the cache is nil, it creates a new instance.
func ContextCache[T any](ctx context.Context, key any) *Cache[T] {
	cacheInstance, ok := ctx.Value(key).(*Cache[T])
	if !ok || cacheInstance == nil {
		cacheInstance = NewCache[T](fmt.Sprintf("%v", key))
	}

	return cacheInstance
}
