package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"stonesuite-notify/audit"
	"stonesuite-notify/preferences"
)

func TestPreferencesGet_ReturnsEffectiveAndOverrides(t *testing.T) {
	store := &fakePreferencesStore{resolved: preferences.Preferences{EmailEnabled: false, InAppEnabled: true, PushEnabled: true}}
	h := NewPreferencesHandler(store, &fakeRecorder{})

	rec := authedRequest(t, http.MethodGet, "/api/preferences", "t1", "u1", nil, h.Get)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	effective := data["effective"].(map[string]any)
	if effective["emailEnabled"] != false {
		t.Fatalf("effective.emailEnabled = %v, want false", effective["emailEnabled"])
	}
}

func TestPreferencesUpdate_SetsCallerOwnOverride(t *testing.T) {
	store := &fakePreferencesStore{}
	h := NewPreferencesHandler(store, &fakeRecorder{})

	body, _ := json.Marshal(map[string]any{"emailEnabled": false})
	var setTenantID, setUserID string
	store.setOverrideHook = func(tenantID, userID string) { setTenantID, setUserID = tenantID, userID }

	rec := authedRequest(t, http.MethodPut, "/api/preferences", "t1", "u1", body, h.Update)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if setTenantID != "t1" || setUserID != "u1" {
		t.Fatalf("override set for (%q, %q), want (t1, u1) — must scope to caller", setTenantID, setUserID)
	}
}

func TestPreferencesUpdate_RequiresAuth(t *testing.T) {
	h := NewPreferencesHandler(&fakePreferencesStore{}, &fakeRecorder{})

	body, _ := json.Marshal(map[string]any{"emailEnabled": false})
	req := httptest.NewRequest(http.MethodPut, "/api/preferences", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Update(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no auth middleware ran, so no caller identity is present)", rec.Code)
	}
}

func TestSetTenantDefaults_RequiresTenantID(t *testing.T) {
	h := NewPreferencesHandler(&fakePreferencesStore{}, &fakeRecorder{})

	body, _ := json.Marshal(map[string]any{"emailEnabled": true, "inAppEnabled": true, "pushEnabled": true})
	req := httptest.NewRequest(http.MethodPut, "/api/tenant-defaults", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.SetTenantDefaults(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSetTenantDefaults_ValidRequest_Succeeds(t *testing.T) {
	store := &fakePreferencesStore{}
	h := NewPreferencesHandler(store, &fakeRecorder{})

	body, _ := json.Marshal(map[string]any{"tenantId": "t1", "emailEnabled": true, "inAppEnabled": false, "pushEnabled": true})
	req := httptest.NewRequest(http.MethodPut, "/api/tenant-defaults", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.SetTenantDefaults(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestPreferencesUpdate_AuditsOnlyTheFieldsTheCallerSet(t *testing.T) {
	// A partial patch must be distinguishable in the trail from a full
	// replace: "turned email off" is not the same as "left it inherited".
	recorder := &fakeRecorder{}
	h := NewPreferencesHandler(&fakePreferencesStore{}, recorder)

	body, _ := json.Marshal(map[string]any{"emailEnabled": false})
	rec := authedRequest(t, http.MethodPut, "/api/preferences", "t1", "u1", body, h.Update)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	entry, ok := recorder.find(audit.ActionPreferenceUpdated)
	if !ok {
		t.Fatalf("no %s audit entry recorded", audit.ActionPreferenceUpdated)
	}
	if entry.TenantID != "t1" || entry.ActorUserID != "u1" {
		t.Fatalf("audit entry = %+v, want tenant t1, actor u1", entry)
	}
	if got := string(entry.Metadata); got != `{"emailEnabled":false}` {
		t.Fatalf("metadata = %s, want only the field the caller set", got)
	}
}

func TestAdminSetTenantDefaults_UsesCallersOwnTenant(t *testing.T) {
	// The admin route must ignore any tenant in the body — the caller's
	// token is the only source of tenant scope.
	store := &fakePreferencesStore{}
	recorder := &fakeRecorder{}
	h := NewPreferencesHandler(store, recorder)

	body, _ := json.Marshal(map[string]any{
		"tenantId": "someone-elses-tenant", "emailEnabled": false, "inAppEnabled": true, "pushEnabled": true,
	})
	rec := authedRequest(t, http.MethodPut, "/api/admin/tenant-defaults", "t1", "admin-user", body, h.AdminSetTenantDefaults)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if store.lastTenantDefaultsID != "t1" {
		t.Fatalf("defaults written for tenant %q, want t1 (body tenantId must be ignored)", store.lastTenantDefaultsID)
	}
	if store.lastTenantDefaults.EmailEnabled {
		t.Fatal("emailEnabled = true, want false (the request body's values must still apply)")
	}

	entry, ok := recorder.find(audit.ActionTenantDefaultsUpdated)
	if !ok {
		t.Fatalf("no %s audit entry recorded", audit.ActionTenantDefaultsUpdated)
	}
	if entry.ActorType != audit.ActorUser || entry.ActorUserID != "admin-user" {
		t.Fatalf("audit entry = %+v, want a user-actor entry for admin-user", entry)
	}
}

func TestAdminGetTenantDefaults_ReturnsCallersOwnTenantDefaults(t *testing.T) {
	store := &fakePreferencesStore{tenantDefaults: preferences.Preferences{EmailEnabled: false, InAppEnabled: true, PushEnabled: true}}
	h := NewPreferencesHandler(store, &fakeRecorder{})

	rec := authedRequest(t, http.MethodGet, "/api/admin/tenant-defaults", "t1", "admin-user", nil, h.AdminGetTenantDefaults)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	defaults := resp.Data.(map[string]any)["defaults"].(map[string]any)
	if defaults["emailEnabled"] != false {
		t.Fatalf("defaults.emailEnabled = %v, want false", defaults["emailEnabled"])
	}
}

func TestSetTenantDefaults_InternalRouteAuditsAsService(t *testing.T) {
	recorder := &fakeRecorder{}
	h := NewPreferencesHandler(&fakePreferencesStore{}, recorder)

	body, _ := json.Marshal(map[string]any{"tenantId": "t1", "emailEnabled": true, "inAppEnabled": true, "pushEnabled": false})
	req := httptest.NewRequest(http.MethodPut, "/api/tenant-defaults", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.SetTenantDefaults(rec, req)

	entry, ok := recorder.find(audit.ActionTenantDefaultsUpdated)
	if !ok {
		t.Fatalf("no %s audit entry recorded", audit.ActionTenantDefaultsUpdated)
	}
	if entry.ActorType != audit.ActorService || entry.ActorUserID != "" {
		t.Fatalf("audit entry = %+v, want a service-actor entry with no end user", entry)
	}
}
