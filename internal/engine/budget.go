package engine

import (
	"context"
	"sync"
)

// byteBudget is a weighted semaphore bounding temp-file bytes in flight.
// total <= 0 means unlimited. A request larger than the whole budget takes
// the entire budget, i.e. runs alone; so does a request of unknown size (n <= 0).
type byteBudget struct {
	mu    sync.Mutex
	cond  *sync.Cond
	total int64
	used  int64
}

func newByteBudget(total int64) *byteBudget {
	b := &byteBudget{total: total}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire blocks until n bytes are available (or ctx is done) and returns a
// release func.
func (b *byteBudget) acquire(ctx context.Context, n int64) (func(), error) {
	if b.total <= 0 {
		return func() {}, ctx.Err()
	}
	if n <= 0 || n > b.total { // unknown size: reserve everything (runs alone)
		n = b.total
	}
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	})
	defer stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used+n > b.total {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.used += n
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.used -= n
			b.cond.Broadcast()
			b.mu.Unlock()
		})
	}, nil
}

// inUse reports the bytes currently reserved (for tests).
func (b *byteBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
