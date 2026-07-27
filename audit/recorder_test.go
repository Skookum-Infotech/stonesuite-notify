package audit

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeStore is an in-memory Store used to assert on what a Recorder wrote
// without a real database.
type fakeStore struct {
	mu      sync.Mutex
	entries []Entry
	err     error
}

func (f *fakeStore) Record(_ context.Context, e Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeStore) List(_ context.Context, tenantID string, _ Filter) ([]Entry, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Entry
	for _, e := range f.entries {
		if e.TenantID == tenantID {
			out = append(out, e)
		}
	}
	return out, len(out), nil
}

func (f *fakeStore) recorded() []Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Entry(nil), f.entries...)
}

func TestAsyncRecorder_WritesEntry(t *testing.T) {
	store := &fakeStore{}
	recorder := NewAsyncRecorder(store)

	recorder.Record(context.Background(), Entry{
		TenantID: "t1", ActorUserID: "u1", ActorType: ActorUser,
		Action: ActionNotificationRead, Resource: ResourceNotification, ResourceID: "n1",
	})
	recorder.Wait()

	entries := store.recorded()
	if len(entries) != 1 {
		t.Fatalf("recorded %d entries, want 1", len(entries))
	}
	if entries[0].Action != ActionNotificationRead || entries[0].ResourceID != "n1" {
		t.Fatalf("recorded %+v, want the read action for n1", entries[0])
	}
}

func TestAsyncRecorder_SurvivesRequestContextCancellation(t *testing.T) {
	// A handler's context is cancelled the moment it returns; the audit
	// write is detached precisely so it still lands.
	store := &fakeStore{}
	recorder := NewAsyncRecorder(store)

	ctx, cancel := context.WithCancel(context.Background())
	recorder.Record(ctx, Entry{TenantID: "t1", Action: ActionNotificationReadAll})
	cancel()

	recorder.Wait()

	if len(store.recorded()) != 1 {
		t.Fatalf("recorded %d entries, want 1 (a cancelled request context must not drop the entry)", len(store.recorded()))
	}
}

func TestAsyncRecorder_StoreFailureIsSwallowed(t *testing.T) {
	// An audit write must never change the outcome of the audited
	// operation, so a failing store is logged and otherwise ignored.
	store := &fakeStore{err: errors.New("db down")}
	recorder := NewAsyncRecorder(store)

	recorder.Record(context.Background(), Entry{TenantID: "t1", Action: ActionPreferenceUpdated})
	recorder.Wait()

	if len(store.recorded()) != 0 {
		t.Fatalf("recorded %d entries, want 0", len(store.recorded()))
	}
}

func TestAsyncRecorder_WaitFlushesEveryInFlightWrite(t *testing.T) {
	store := &fakeStore{}
	recorder := NewAsyncRecorder(store)

	const count = 50
	for i := 0; i < count; i++ {
		recorder.Record(context.Background(), Entry{TenantID: "t1", Action: ActionNotificationRead})
	}
	recorder.Wait()

	if got := len(store.recorded()); got != count {
		t.Fatalf("recorded %d entries, want %d (Wait must flush everything in flight)", got, count)
	}
}

func TestFilterNormalize_ClampsPaging(t *testing.T) {
	tests := []struct {
		name       string
		in         Filter
		wantLimit  int
		wantOffset int
	}{
		{"zero limit falls back to default", Filter{}, DefaultLimit, 0},
		{"over-max limit falls back to default", Filter{Limit: MaxLimit + 1}, DefaultLimit, 0},
		{"negative offset is clamped", Filter{Limit: 10, Offset: -5}, 10, 0},
		{"valid paging is preserved", Filter{Limit: 25, Offset: 50}, 25, 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.normalize()
			if got.Limit != tt.wantLimit || got.Offset != tt.wantOffset {
				t.Fatalf("normalize() = limit %d offset %d, want limit %d offset %d",
					got.Limit, got.Offset, tt.wantLimit, tt.wantOffset)
			}
		})
	}
}

func TestMetadata_EmptyReturnsNil(t *testing.T) {
	if got := Metadata(nil); got != nil {
		t.Fatalf("Metadata(nil) = %s, want nil", got)
	}
	if got := Metadata(map[string]any{"channel": "email"}); string(got) != `{"channel":"email"}` {
		t.Fatalf("Metadata() = %s, want {\"channel\":\"email\"}", got)
	}
}
