package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"stonesuite-notify/audit"
)

// fakeAuditStore is an in-memory audit.Store that records the tenant and
// filter it was queried with, so tests can assert the handler scoped the
// query correctly without a real database.
type fakeAuditStore struct {
	entries      []audit.Entry
	lastTenantID string
	lastFilter   audit.Filter
}

func (f *fakeAuditStore) Record(_ context.Context, e audit.Entry) error {
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAuditStore) List(_ context.Context, tenantID string, filter audit.Filter) ([]audit.Entry, int, error) {
	f.lastTenantID = tenantID
	f.lastFilter = filter

	var out []audit.Entry
	for _, e := range f.entries {
		if e.TenantID != tenantID {
			continue
		}
		if filter.Action != "" && e.Action != filter.Action {
			continue
		}
		out = append(out, e)
	}
	return out, len(out), nil
}

func auditLogFixture() *fakeAuditStore {
	return &fakeAuditStore{entries: []audit.Entry{
		{ID: "a1", TenantID: "t1", ActorUserID: "u1", ActorType: audit.ActorUser, Action: audit.ActionNotificationRead},
		{ID: "a2", TenantID: "t1", ActorType: audit.ActorService, Action: audit.ActionNotificationCreated},
		{ID: "a3", TenantID: "other-tenant", ActorUserID: "u9", ActorType: audit.ActorUser, Action: audit.ActionNotificationRead},
	}}
}

func TestAuditList_ScopesToCallersTenant(t *testing.T) {
	store := auditLogFixture()
	h := NewAuditHandler(store)

	rec := authedRequest(t, http.MethodGet, "/api/admin/audit-logs", "t1", "admin-user", nil, h.List)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if store.lastTenantID != "t1" {
		t.Fatalf("queried tenant %q, want t1 (must come from the caller's token)", store.lastTenantID)
	}
	resp := decodeResponse(t, rec)
	entries := resp.Data.(map[string]any)["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (must not leak another tenant's trail)", len(entries))
	}
}

func TestAuditList_RequiresAuth(t *testing.T) {
	h := NewAuditHandler(auditLogFixture())

	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs", nil)
	rec := httptest.NewRecorder()

	h.List(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuditListInternal_RequiresTenantID(t *testing.T) {
	h := NewAuditHandler(auditLogFixture())

	req := httptest.NewRequest(http.MethodGet, "/api/audit-logs", nil)
	rec := httptest.NewRecorder()

	h.ListInternal(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (an unscoped audit query must never be possible)", rec.Code)
	}
}

func TestAuditListInternal_UsesQueryTenant(t *testing.T) {
	store := auditLogFixture()
	h := NewAuditHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/audit-logs?tenantId=other-tenant", nil)
	rec := httptest.NewRecorder()

	h.ListInternal(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if store.lastTenantID != "other-tenant" {
		t.Fatalf("queried tenant %q, want other-tenant", store.lastTenantID)
	}
}

func TestAuditList_PassesFiltersThrough(t *testing.T) {
	store := auditLogFixture()
	h := NewAuditHandler(store)

	target := "/api/admin/audit-logs?action=" + audit.ActionNotificationRead +
		"&actorUserId=u1&resource=notification&resourceId=n1" +
		"&from=2026-07-01T00:00:00Z&to=2026-07-31T00:00:00Z&page=3&pageSize=10"
	rec := authedRequest(t, http.MethodGet, target, "t1", "admin-user", nil, h.List)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	got := store.lastFilter
	if got.Action != audit.ActionNotificationRead || got.ActorUserID != "u1" ||
		got.Resource != "notification" || got.ResourceID != "n1" {
		t.Fatalf("filter = %+v, want the query's action/actor/resource values", got)
	}
	if got.Limit != 10 || got.Offset != 20 {
		t.Fatalf("limit/offset = %d/%d, want 10/20 (page 3 at pageSize 10)", got.Limit, got.Offset)
	}
	if got.From == nil || !got.From.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("from = %v, want 2026-07-01T00:00:00Z", got.From)
	}
	if got.To == nil || !got.To.Equal(time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("to = %v, want 2026-07-31T00:00:00Z", got.To)
	}
}

func TestAuditList_RejectsMalformedTimestamp(t *testing.T) {
	h := NewAuditHandler(auditLogFixture())

	rec := authedRequest(t, http.MethodGet, "/api/admin/audit-logs?from=yesterday", "t1", "admin-user", nil, h.List)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestClientIP_PrefersForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	req.RemoteAddr = "10.0.0.5:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")

	if got := clientIP(req); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q, want 203.0.113.9 (the original client, not the proxy)", got)
	}
}

func TestClientIP_FallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	req.RemoteAddr = "10.0.0.5:54321"

	if got := clientIP(req); got != "10.0.0.5" {
		t.Fatalf("clientIP = %q, want 10.0.0.5 (host without port)", got)
	}
}

func TestNewPagination(t *testing.T) {
	tests := []struct {
		name           string
		page           int
		pageSize       int
		total          int
		wantTotalPages int
		wantHasMore    bool
	}{
		{"exact multiple", 1, 10, 30, 3, true},
		{"partial last page", 3, 10, 25, 3, false},
		{"single partial page", 1, 10, 4, 1, false},
		{"empty result", 1, 10, 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newPagination(tt.page, tt.pageSize, tt.total)
			if got.TotalPages != tt.wantTotalPages || got.HasMore != tt.wantHasMore {
				t.Fatalf("newPagination(%d, %d, %d) = totalPages %d hasMore %v, want %d / %v",
					tt.page, tt.pageSize, tt.total, got.TotalPages, got.HasMore, tt.wantTotalPages, tt.wantHasMore)
			}
		})
	}
}
