package kivi

import (
	"container/list"
	"sync"
	"time"
)

const (
	Manual = iota + 1
	Auto
)

type DType int

// type Adapter func()

type Cache[K comparable, V any] interface {
	Get(K) (V, bool)
	GetAll() map[K]V
	Set(K, V)
	SetWithTimeout(K, V, time.Duration)
	NotFoundSet(K, V) bool
	NotFoundSetWithTimeout(K, V, time.Duration) bool
	Delete(K)
	DeleteAndGet(K) (V, bool)
	TransferTo(Cache[K, V])
	CopyTo(Cache[K, V])
	Keys() []K
	Purge()
	Count() uint64
	CountAll() uint64
	evict(int)
}

type baseCache struct {
	mu   sync.RWMutex
	size uint64
}

type Builder[K comparable, V any] struct {
	dt   DType
	size uint64
	ti   time.Duration
}

func New[K comparable, V any](size uint64) *Builder[K, V] {
	return &Builder[K, V]{
		dt:   Manual,
		size: size,
	}
}

func (b *Builder[K, V]) DType(t DType) *Builder[K, V] {
	b.dt = t
	return b
}

func (b *Builder[K, V]) WithTimeInterval(t time.Duration) *Builder[K, V] {
	b.ti = t
	return b
}

func (b *Builder[K, V]) Build() Cache[K, V] {
	switch b.dt {
	case Manual:
		return newMan(b)
	case Auto:
		return newAuto(b)
	default:
		panic("kivi: invalid type")
	}
}

type valueWithTimeout[V any] struct {
	value    V
	expireAt *time.Time
}

type ManCache[K comparable, V any] struct {
	baseCache
	storage      map[K]valueWithTimeout[V]
	stopChan     chan struct{}
	timeInterval time.Duration
}

func newMan[K comparable, V any](b *Builder[K, V]) *ManCache[K, V] {
	c := &ManCache[K, V]{
		storage:      make(map[K]valueWithTimeout[V]),
		stopChan:     make(chan struct{}),
		timeInterval: b.ti,
		baseCache: baseCache{
			size: b.size,
		},
	}

	if c.timeInterval > 0 {
		go c.expireKeys()
	}

	return c
}

func (mc *ManCache[K, V]) Get(k K) (V, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	v, ok := mc.storage[k]
	if !ok {
		return v.value, false
	}

	if v.expireAt != nil && !v.expireAt.Before(time.Now()) {
		delete(mc.storage, k)
		return v.value, false
	}

	return v.value, ok
}

func (mc *ManCache[K, V]) GetAll() map[K]V {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	m := make(map[K]V)
	for k, v := range mc.storage {
		if v.expireAt != nil && !v.expireAt.Before(time.Now()) {
			m[k] = v.value
		}
	}

	return m
}

func (mc *ManCache[K, V]) Set(k K, v V) {
	if mc.size == 0 {
		return
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	if len(mc.storage) == int(mc.size) {
		mc.evict(1)
	}

	mc.storage[k] = valueWithTimeout[V]{
		value:    v,
		expireAt: nil,
	}
}

func (mc *ManCache[K, V]) SetWithTimeout(k K, v V, t time.Duration) {
	if mc.size == 0 {
		return
	}

	if t > 0 {
		mc.mu.Lock()
		defer mc.mu.Unlock()

		if len(mc.storage) == int(mc.size) {
			mc.evict(1)
		}

		now := time.Now().Add(t)
		mc.storage[k] = valueWithTimeout[V]{
			value:    v,
			expireAt: &now,
		}
	} else {
		mc.Set(k, v)
	}
}

func (mc *ManCache[K, V]) NotFoundSet(k K, v V) bool {
	if mc.size == 0 {
		return false
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	_, ok := mc.storage[k]
	if !ok {
		if len(mc.storage) == int(mc.size) {
			mc.evict(1)
		}

		mc.storage[k] = valueWithTimeout[V]{
			value:    v,
			expireAt: nil,
		}
	}

	return !ok
}

func (mc *ManCache[K, V]) NotFoundSetWithTimeout(k K, v V, t time.Duration) bool {
	if mc.size > 0 {
		return false
	}

	var ok bool
	if t > 0 {
		mc.mu.Lock()
		defer mc.mu.Unlock()

		now := time.Now().Add(t)
		_, ok = mc.storage[k]

		if !ok {
			if len(mc.storage) == int(mc.size) {
				mc.evict(1)
			}

			mc.storage[k] = valueWithTimeout[V]{
				value:    v,
				expireAt: &now,
			}
		}
	} else {
		ok = !mc.NotFoundSet(k, v)
	}

	return !ok
}

func (mc *ManCache[K, V]) Delete(k K) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	delete(mc.storage, k)
}

func (mc *ManCache[K, V]) DeleteAndGet(k K) (V, bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	v, ok := mc.storage[k]
	if !ok {
		return v.value, false
	}

	if v.expireAt != nil && !v.expireAt.Before(time.Now()) {
		delete(mc.storage, k)
		return v.value, false
	}

	delete(mc.storage, k)
	return v.value, ok
}

func (mc *ManCache[K, V]) TransferTo(c Cache[K, V]) {
	all := mc.GetAll()

	mc.mu.Lock()
	mc.storage = make(map[K]valueWithTimeout[V])
	mc.mu.Unlock()

	for k, v := range all {
		c.Set(k, v)
	}
}

func (mc *ManCache[K, V]) CopyTo(c Cache[K, V]) {
	all := mc.GetAll()

	for k, v := range all {
		c.Set(k, v)
	}
}

func (mc *ManCache[K, V]) Keys() []K {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	ks := make([]K, len(mc.storage))
	var i uint64 = 0

	for k := range mc.storage {
		ks[i] = k
		i++
	}

	return ks
}

func (mc *ManCache[K, V]) Purge() {
	if mc.timeInterval > 0 {
		mc.stopChan <- struct{}{}
		close(mc.stopChan)
	}

	mc.storage = nil
}

func (mc *ManCache[K, V]) Count() uint64 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	var count uint64 = 0
	for _, v := range mc.storage {
		if v.expireAt != nil && !v.expireAt.Before(time.Now()) {
			count++
		}
	}

	return count
}

func (mc *ManCache[K, V]) CountAll() uint64 {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	return uint64(len(mc.storage))
}

func (mc *ManCache[K, V]) evict(i int) {
	var counter int
	for k, v := range mc.storage {
		if counter == i {
			break
		}

		if v.expireAt != nil && !v.expireAt.Before(time.Now()) {
			delete(mc.storage, k)
			counter++
		}
	}

	if i > len(mc.storage) {
		i = len(mc.storage)
	}

	for ; counter < i; counter++ {
		for k := range mc.storage {
			delete(mc.storage, k)
			break
		}
	}
}

func (mc *ManCache[K, V]) expireKeys() {
	t := time.NewTicker(mc.timeInterval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			mc.mu.Lock()
			for k, v := range mc.storage {
				if v.expireAt != nil && v.expireAt.Before(time.Now()) {
					delete(mc.storage, k)
				}
			}
			mc.mu.Unlock()
		case <-mc.stopChan:
			return
		}
	}
}

type autoItem[K comparable, V any] struct {
	key      K
	value    V
	expireAt *time.Time
}

type AutoCache[K comparable, V any] struct {
	baseCache
	storage      map[K]*list.Element
	evictionList *list.List
}

func newAuto[K comparable, V any](b *Builder[K, V]) *AutoCache[K, V] {
	return &AutoCache[K, V]{
		baseCache: baseCache{
			size: b.size,
		},
		storage:      make(map[K]*list.Element),
		evictionList: list.New(),
	}
}

func (ac *AutoCache[K, V]) Get(k K) (v V, ok bool) {
	ac.mu.RLock()
	defer ac.mu.RUnlock()

	item, ok := ac.storage[k]
	if !ok {
		return
	}

	if item.Value.(*autoItem[K, V]).expireAt != nil && item.Value.(*autoItem[K, V]).expireAt.Before(time.Now()) {
		delete(ac.storage, k)
		ac.evictionList.Remove(item)
		return
	}

	ac.evictionList.MoveToFront(item)
	return item.Value.(*autoItem[K, V]).value, true
}

func (ac *AutoCache[K, V]) GetAll() map[K]V {}

func (ac *AutoCache[K, V]) Set(k K, v V) {}

func (ac *AutoCache[K, V]) SetWithTimeout(k K, v V, t time.Duration) {}

func (ac *AutoCache[K, V]) NotFoundSet(k K, v V) bool {}

func (ac *AutoCache[K, V]) NotFoundSetWithTimeout(k K, v V, t time.Duration) bool {}

func (ac *AutoCache[K, V]) Delete(k K) {}

func (ac *AutoCache[K, V]) DeleteAndGet(k K) (V, bool) {}

func (ac *AutoCache[K, V]) TransferTo(c Cache[K, V]) {}

func (ac *AutoCache[K, V]) CopyTo(c Cache[K, V]) {}

func (ac *AutoCache[K, V]) Keys() []K {}

func (ac *AutoCache[K, V]) Purge() {}

func (ac *AutoCache[K, V]) Count() uint64 {}

func (ac *AutoCache[K, V]) CountAll() uint64 {}

func (ac *AutoCache[K, V]) evict(i int) {}
