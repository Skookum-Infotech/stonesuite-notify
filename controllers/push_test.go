package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"stonesuite-notify/pushsubs"
)

func TestVAPIDKey_ReturnsPublicKey(t *testing.T) {
	h := NewPushHandler(&fakePushStore{}, &fakeRecorder{}, "test-public-key")

	req := httptest.NewRequest(http.MethodGet, "/api/push/vapid-public-key", nil)
	rec := httptest.NewRecorder()

	h.VAPIDKey(rec, req)

	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	if data["publicKey"] != "test-public-key" {
		t.Fatalf("publicKey = %v, want test-public-key", data["publicKey"])
	}
}

func subscribeBody(endpoint, p256dh, auth string) []byte {
	body, _ := json.Marshal(subscribeRequest{
		Endpoint: endpoint,
		Keys: struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		}{P256dh: p256dh, Auth: auth},
	})
	return body
}

func TestSubscribe_SavesSubscriptionScopedToCaller(t *testing.T) {
	store := &fakePushStore{}
	h := NewPushHandler(store, &fakeRecorder{}, "test-public-key")

	rec := authedRequest(t, http.MethodPost, "/api/push/subscribe", "t1", "u1",
		subscribeBody("https://push.example.com/ep1", "p256dh-key", "auth-key"), h.Subscribe)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.subs) != 1 {
		t.Fatalf("store has %d subs, want 1", len(store.subs))
	}
	if store.subs[0].TenantID != "t1" || store.subs[0].UserID != "u1" {
		t.Fatalf("subscription scoped to %+v, want t1/u1", store.subs[0])
	}
}

func TestSubscribe_RejectsMissingFields(t *testing.T) {
	store := &fakePushStore{}
	h := NewPushHandler(store, &fakeRecorder{}, "test-public-key")

	body, _ := json.Marshal(map[string]string{"endpoint": "https://push.example.com/ep1"}) // missing keys
	rec := authedRequest(t, http.MethodPost, "/api/push/subscribe", "t1", "u1", body, h.Subscribe)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(store.subs) != 0 {
		t.Fatalf("store has %d subs, want 0", len(store.subs))
	}
}

func TestUnsubscribe_RemovesOnlyMatchingEndpointForCaller(t *testing.T) {
	store := &fakePushStore{subs: []pushsubs.Subscription{
		{TenantID: "t1", UserID: "u1", Endpoint: "https://push.example.com/keep"},
		{TenantID: "t1", UserID: "u1", Endpoint: "https://push.example.com/remove"},
		{TenantID: "t1", UserID: "other-user", Endpoint: "https://push.example.com/remove"}, // must survive
	}}
	h := NewPushHandler(store, &fakeRecorder{}, "test-public-key")

	body, _ := json.Marshal(unsubscribeRequest{Endpoint: "https://push.example.com/remove"})
	rec := authedRequest(t, http.MethodDelete, "/api/push/subscribe", "t1", "u1", body, h.Unsubscribe)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.subs) != 2 {
		t.Fatalf("store has %d subs, want 2 (only caller's matching endpoint removed)", len(store.subs))
	}
	for _, s := range store.subs {
		if s.UserID == "u1" && s.Endpoint == "https://push.example.com/remove" {
			t.Fatalf("caller's subscription was not removed: %+v", s)
		}
	}
}
