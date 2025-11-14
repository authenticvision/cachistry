package cache

import "sync"

type syncMap[K comparable, V any] struct{ sync.Map }

func (m *syncMap[K, V]) Load(key K) (value V, ok bool) {
	v, found := m.Map.Load(key)
	return v.(V), found
}

func (m *syncMap[K, V]) Store(key K, value V) {
	m.Map.Store(key, value)
}

func (m *syncMap[K, V]) Range(f func(key K, value V) bool) {
	m.Map.Range(func(k, v any) bool {
		return f(k.(K), v.(V))
	})
}
