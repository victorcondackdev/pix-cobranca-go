package webhook

import (
	"context"
	"sync"
	"time"
)

// Deduper records which notifications were processed. Begin reserves a key
// and reports whether the caller should process it; the caller then calls
// Commit on success or Abort on failure. A key reserved by a concurrent
// delivery is reported as not to be processed: if that delivery fails, the
// PSP redelivers it.
//
// Implementations backed by a shared store (Redis SET NX with a TTL, a
// unique index in SQL) make deduplication work across instances.
type Deduper interface {
	Begin(ctx context.Context, key string) (bool, error)
	Commit(ctx context.Context, key string) error
	Abort(ctx context.Context, key string) error
}

// MemoryDeduper is an in-process Deduper with a TTL and a bound on the
// number of keys, evicting the oldest first.
type MemoryDeduper struct {
	ttl  time.Duration
	max  int
	now  func() time.Time
	mu   sync.Mutex
	keys map[string]memEntry
	fifo []fifoEntry
}

type memEntry struct {
	at        time.Time
	committed bool
}

type fifoEntry struct {
	key string
	at  time.Time
}

// NewMemoryDeduper returns a MemoryDeduper remembering keys for ttl, at
// most max of them.
func NewMemoryDeduper(ttl time.Duration, max int) *MemoryDeduper {
	if max <= 0 {
		max = 1
	}
	return &MemoryDeduper{ttl: ttl, max: max, now: time.Now, keys: make(map[string]memEntry)}
}

func (m *MemoryDeduper) pruneLocked(now time.Time) {
	for len(m.fifo) > 0 {
		f := m.fifo[0]
		e, ok := m.keys[f.key]
		stale := !ok || !e.at.Equal(f.at)
		if !stale && now.Sub(f.at) < m.ttl && len(m.keys) <= m.max {
			break
		}
		if !stale {
			delete(m.keys, f.key)
		}
		m.fifo[0] = fifoEntry{}
		m.fifo = m.fifo[1:]
	}
}

// Begin implements Deduper.
func (m *MemoryDeduper) Begin(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.pruneLocked(now)
	if _, ok := m.keys[key]; ok {
		return false, nil
	}
	m.keys[key] = memEntry{at: now}
	m.fifo = append(m.fifo, fifoEntry{key: key, at: now})
	m.pruneLocked(now)
	return true, nil
}

// Commit implements Deduper.
func (m *MemoryDeduper) Commit(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.keys[key]; ok {
		e.committed = true
		m.keys[key] = e
	}
	return nil
}

// Abort implements Deduper.
func (m *MemoryDeduper) Abort(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.keys[key]; ok && !e.committed {
		delete(m.keys, key)
	}
	return nil
}

// Len returns the number of keys currently remembered.
func (m *MemoryDeduper) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.keys)
}
