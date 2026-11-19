package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

const maxItemErrors = 100

func dirInc(d *PerDir, from Side) {
	if from == A {
		d.AtoB++
	} else {
		d.BtoA++
	}
}

func (r *run) now() time.Time { return r.o.Now() }

// canceled reports whether err is a context cancellation of this run.
func (r *run) canceled(err error) bool {
	if err == nil {
		return false
	}
	return r.ctx.Err() != nil || immich.KindOf(err) == immich.KindCanceled
}

func (r *run) addSkip(reason SkipReason, n int) {
	if n == 0 {
		return
	}
	r.mu.Lock()
	r.sum.Skipped[reason] += n
	r.mu.Unlock()
}

func (r *run) count(f func(s *RunSummary)) {
	r.mu.Lock()
	f(&r.sum)
	r.mu.Unlock()
}

// itemError records a per-item error in the summary (never for cancellation).
func (r *run) itemError(key, op string, side Side, err error) {
	if r.canceled(err) {
		return
	}
	kind := immich.KindOf(err)
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	r.mu.Lock()
	r.sum.ItemErrorCount++
	if len(r.sum.ItemErrors) < maxItemErrors {
		r.sum.ItemErrors = append(r.sum.ItemErrors, ItemError{Key: key, Op: op, Side: side, Kind: kind, Msg: msg})
	}
	r.mu.Unlock()
	r.log.Warn("item error", "phase", string(r.curPhase()), "op", op, "key", key, "side", side.String(), "kind", kind.String(), "err", msg)
}

// recordFailure persists failure backoff for key (never for cancellation).
// If fixedNext is non-zero it is used as the retry time.
func (r *run) recordFailure(key, op string, err error, fixedNext time.Time) {
	if r.canceled(err) {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	it := r.ps.Item(key)
	f := it.Failure
	if f == nil || f.Op != op {
		f = &state.Failure{Op: op}
	}
	f.Count++
	next := fixedNext
	if next.IsZero() {
		d := r.o.BackoffBase
		for i := 1; i < f.Count && d < r.o.BackoffMax; i++ {
			d *= 2
		}
		if d > r.o.BackoffMax || d <= 0 {
			d = r.o.BackoffMax
		}
		next = now.Add(d)
	}
	f.Next = next
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	f.LastError = msg
	it.Failure = f
}

// finalizeSummary computes the result class once the run is over.
func (r *run) finalizeSummary(runErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &r.sum
	s.Finished = r.now()
	s.Mutations = int(r.mutations.Load())
	s.BytesDown = r.bytesDown.Load()
	s.BytesUp = r.bytesUp.Load()
	s.Bootstrap = r.bootstrap
	s.AlbumIDs = r.albumIDs
	if r.ps != nil {
		n := 0
		for _, it := range r.ps.Items {
			if it != nil && it.Pending != nil {
				n++
			}
		}
		s.Pending = n
	}
	switch {
	case !r.mutating:
		s.Result = ResultDryRun
	case runErr != nil && !r.executed:
		s.Result = ResultFailed
	case runErr != nil || s.ItemErrorCount > 0 || s.GuardTripped[0] || s.GuardTripped[1] || s.Deferred > 0 ||
		s.Skipped[SkipQuota] > 0 || s.Skipped[SkipTooLarge] > 0:
		s.Result = ResultPartial
	default:
		s.Result = ResultOK
	}
}

func joinWarn(ws []string) string { return strings.Join(ws, "; ") }

var _ = errors.New

func zeroTime() (t time.Time) { return }

func contextForFlush() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}
