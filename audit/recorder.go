package audit

import (
	"context"
	"log"
	"sync"
	"time"
)

// Recorder is what handlers and workers depend on to emit an audit entry.
// It has no error return by design: an audit write must never change the
// outcome of the operation being audited, so failures are logged, not
// propagated. Tests substitute a synchronous fake.
type Recorder interface {
	Record(ctx context.Context, e Entry)
}

// recordTimeout bounds a single detached audit write so a stalled database
// can't leak goroutines indefinitely.
const recordTimeout = 5 * time.Second

// AsyncRecorder writes entries on a detached goroutine so auditing never
// adds latency to (or fails) the request being audited.
type AsyncRecorder struct {
	store Store
	wg    sync.WaitGroup
}

// NewAsyncRecorder builds an AsyncRecorder backed by the given store.
func NewAsyncRecorder(store Store) *AsyncRecorder {
	return &AsyncRecorder{store: store}
}

// Record persists the entry in the background.
//
// The write uses context.WithoutCancel so it survives the HTTP handler
// returning (which cancels the request context) while still inheriting any
// request-scoped values. Call Wait during shutdown to flush entries still
// in flight.
func (r *AsyncRecorder) Record(ctx context.Context, e Entry) {
	detached := context.WithoutCancel(ctx)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		writeCtx, cancel := context.WithTimeout(detached, recordTimeout)
		defer cancel()

		if err := r.store.Record(writeCtx, e); err != nil {
			log.Printf("audit: record %s for tenant %s: %v", e.Action, e.TenantID, err)
		}
	}()
}

// Wait blocks until every in-flight audit write has finished. main calls
// this during graceful shutdown so entries recorded by the last few
// requests aren't lost when the process exits.
func (r *AsyncRecorder) Wait() {
	r.wg.Wait()
}
