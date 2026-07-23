package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"stonesuite-notify/config"
	"stonesuite-notify/middleware"
	"stonesuite-notify/models"
	"stonesuite-notify/notifications"
	"stonesuite-notify/pushsubs"
)

const testJWTSecret = "test-secret"

// fakeStore is an in-memory notifications.Store used to test handlers
// without a real database.
type fakeStore struct {
	rows        []notifications.Notification
	nextID      int
	forceErr    error
	notFoundErr bool
}

func (f *fakeStore) Create(_ context.Context, in notifications.CreateInput) (*notifications.Notification, error) {
	if f.forceErr != nil {
		return nil, f.forceErr
	}
	f.nextID++
	n := notifications.Notification{
		ID:              itoa(f.nextID),
		TenantID:        in.TenantID,
		RecipientUserID: in.RecipientUserID,
		ActorUserID:     in.ActorUserID,
		EventType:       in.EventType,
		Resource:        in.Resource,
		ResourceID:      in.ResourceID,
		Title:           in.Title,
		Body:            in.Body,
		Link:            in.Link,
		CreatedAt:       time.Now(),
	}
	f.rows = append(f.rows, n)
	return &n, nil
}

func (f *fakeStore) ListForUser(_ context.Context, tenantID, recipientUserID string, unreadOnly bool, limit int) ([]notifications.Notification, error) {
	if f.forceErr != nil {
		return nil, f.forceErr
	}
	var out []notifications.Notification
	for _, n := range f.rows {
		if n.TenantID != tenantID || n.RecipientUserID != recipientUserID {
			continue
		}
		if unreadOnly && n.ReadAt != nil {
			continue
		}
		out = append(out, n)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) UnreadCount(_ context.Context, tenantID, recipientUserID string) (int, error) {
	if f.forceErr != nil {
		return 0, f.forceErr
	}
	count := 0
	for _, n := range f.rows {
		if n.TenantID == tenantID && n.RecipientUserID == recipientUserID && n.ReadAt == nil {
			count++
		}
	}
	return count, nil
}

func (f *fakeStore) MarkRead(_ context.Context, tenantID, recipientUserID, id string) error {
	if f.forceErr != nil {
		return f.forceErr
	}
	for i := range f.rows {
		n := &f.rows[i]
		if n.ID == id && n.TenantID == tenantID && n.RecipientUserID == recipientUserID && n.ReadAt == nil {
			now := time.Now()
			n.ReadAt = &now
			return nil
		}
	}
	return notifications.ErrNotFound
}

func (f *fakeStore) MarkAllRead(_ context.Context, tenantID, recipientUserID string) error {
	if f.forceErr != nil {
		return f.forceErr
	}
	now := time.Now()
	for i := range f.rows {
		n := &f.rows[i]
		if n.TenantID == tenantID && n.RecipientUserID == recipientUserID && n.ReadAt == nil {
			n.ReadAt = &now
		}
	}
	return nil
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

// fakePushStore is an in-memory pushsubs.Store used so Create/dispatch
// tests don't need a real database or network access.
type fakePushStore struct {
	subs []pushsubs.Subscription
}

func (f *fakePushStore) Save(_ context.Context, tenantID, userID string, in pushsubs.SaveInput) error {
	f.subs = append(f.subs, pushsubs.Subscription{
		TenantID: tenantID, UserID: userID,
		Endpoint: in.Endpoint, P256dh: in.P256dh, Auth: in.Auth,
	})
	return nil
}

func (f *fakePushStore) Delete(_ context.Context, tenantID, userID, endpoint string) error {
	out := f.subs[:0]
	for _, s := range f.subs {
		if s.TenantID == tenantID && s.UserID == userID && s.Endpoint == endpoint {
			continue
		}
		out = append(out, s)
	}
	f.subs = out
	return nil
}

func (f *fakePushStore) ListForUser(_ context.Context, tenantID, userID string) ([]pushsubs.Subscription, error) {
	var out []pushsubs.Subscription
	for _, s := range f.subs {
		if s.TenantID == tenantID && s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
}

// authedRequest builds a request carrying a valid JWT for the given tenant
// and user, run through the real RequireAuth middleware so tests exercise
// the same context-population path production traffic does.
func authedRequest(t *testing.T, method, target, tenantID, userID string, body []byte, next http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	// Matches StoneSuite-Backend's real generateTenantJWT shape: "id" is the
	// only per-user identifier — there is no separate "user_id" claim.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"id":        userID,
		"email":     "user@example.com",
		"tenant_id": tenantID,
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reqBody)
	req.Header.Set("Authorization", "Bearer "+signed)

	rec := httptest.NewRecorder()
	middleware.RequireAuth(testJWTSecret)(next).ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) models.APIResponse {
	t.Helper()
	var resp models.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	return resp
}

func TestSummary_ReturnsUnreadCountForCaller(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1"},
		{ID: "2", TenantID: "t1", RecipientUserID: "u1"},
		{ID: "3", TenantID: "t1", RecipientUserID: "u2"}, // different user — must not be counted
	}}
	h := NewHandler(store, &fakePushStore{}, config.Config{})

	rec := authedRequest(t, http.MethodGet, "/api/notifications/summary", "t1", "u1", nil, h.Summary)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeResponse(t, rec)
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("data is not a map: %#v", resp.Data)
	}
	if data["unreadCount"] != float64(2) {
		t.Fatalf("unreadCount = %v, want 2", data["unreadCount"])
	}
}

func TestList_ScopesToCallerOnly(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1", Title: "mine"},
		{ID: "2", TenantID: "t1", RecipientUserID: "other-user", Title: "not mine"},
	}}
	h := NewHandler(store, &fakePushStore{}, config.Config{})

	rec := authedRequest(t, http.MethodGet, "/api/notifications", "t1", "u1", nil, h.List)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	list := data["notifications"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d notifications, want 1 (must not leak other users' rows)", len(list))
	}
}

func TestMarkRead_UnknownIDReturns404(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler(store, &fakePushStore{}, config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/notifications/does-not-exist/read", nil)
	req.SetPathValue("id", "does-not-exist")

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"id": "u1", "email": "user@example.com",
		"tenant_id": "t1",
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
	signed, _ := token.SignedString([]byte(testJWTSecret))
	req.Header.Set("Authorization", "Bearer "+signed)

	rec := httptest.NewRecorder()
	middleware.RequireAuth(testJWTSecret)(http.HandlerFunc(h.MarkRead)).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCreate_RejectsMissingRequiredFields(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler(store, &fakePushStore{}, config.Config{})

	body, _ := json.Marshal(map[string]string{"tenantId": "t1"}) // missing everything else
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCreate_PersistsValidNotification(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler(store, &fakePushStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 1 {
		t.Fatalf("store has %d rows, want 1", len(store.rows))
	}
}

func TestCreate_MultipleRecipients_CreatesOneRowEach(t *testing.T) {
	// Simulates a caller fanning a single business event out to every user
	// holding a role (e.g. all "Managers") in one request.
	store := &fakeStore{}
	h := NewHandler(store, &fakePushStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID: "t1",
		Recipients: []recipientTarget{
			{UserID: "u1", Email: "u1@example.com"},
			{UserID: "u2", Email: "u2@example.com"},
		},
		EventType:  "quote.sent",
		Resource:   "quote",
		ResourceID: "q-1",
		Title:      "Quote Q-1 sent",
		Channels:   []string{"email"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 2 {
		t.Fatalf("store has %d rows, want 2 (one per recipient)", len(store.rows))
	}
}

func TestCreate_RejectsRecipientMissingUserID(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler(store, &fakePushStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{Email: "no-id@example.com"}},
		EventType:  "quote.sent",
		Resource:   "quote",
		ResourceID: "q-1",
		Title:      "Quote Q-1 sent",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 0 {
		t.Fatalf("store has %d rows, want 0 (invalid recipient must reject whole request)", len(store.rows))
	}
}
