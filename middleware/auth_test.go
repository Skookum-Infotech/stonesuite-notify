package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const secret = "test-secret"

func signToken(t *testing.T, claims jwt.MapClaims, signingSecret string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(signingSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestRequireAuth_ValidBearerToken_InjectsUserContext(t *testing.T) {
	// Matches StoneSuite-Backend's real generateTenantJWT shape: no
	// "user_id" claim — "id" is the only per-user identifier.
	signed := signToken(t, jwt.MapClaims{
		"id": "identity-1", "email": "user@example.com",
		"tenant_id": "tenant-1",
		"exp":       time.Now().Add(time.Hour).Unix(),
	}, secret)

	var captured UserContext
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := GetUserFromContext(r.Context())
		if err != nil {
			t.Fatalf("GetUserFromContext: %v", err)
		}
		captured = user
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()

	RequireAuth(secret)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if captured.TenantID != "tenant-1" || captured.UserID != "identity-1" {
		t.Fatalf("captured context = %+v, want tenant-1/identity-1", captured)
	}
}

func TestRequireAuth_ValidCookieToken_InjectsUserContext(t *testing.T) {
	signed := signToken(t, jwt.MapClaims{
		"id": "identity-1", "email": "user@example.com",
		"tenant_id": "tenant-1",
		"exp":       time.Now().Add(time.Hour).Unix(),
	}, secret)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "auth_token", Value: signed})
	rec := httptest.NewRecorder()

	RequireAuth(secret)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireAuth_NoToken_Returns401(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run without a token")
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	RequireAuth(secret)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAuth_WrongSigningSecret_Returns401(t *testing.T) {
	signed := signToken(t, jwt.MapClaims{
		"id": "identity-1", "email": "user@example.com",
		"tenant_id": "tenant-1", "user_id": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	}, "a-different-secret")

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run with a mis-signed token")
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()

	RequireAuth(secret)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAuth_ExpiredToken_Returns401(t *testing.T) {
	signed := signToken(t, jwt.MapClaims{
		"id": "identity-1", "email": "user@example.com",
		"tenant_id": "tenant-1", "user_id": "user-1",
		"exp": time.Now().Add(-time.Hour).Unix(),
	}, secret)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run with an expired token")
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()

	RequireAuth(secret)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAuth_MissingTenantClaim_Returns401(t *testing.T) {
	// Token has id/email but no tenant_id. This service has no
	// non-tenant-scoped routes, so it must reject these rather than
	// silently proceeding with an empty scoping value.
	signed := signToken(t, jwt.MapClaims{
		"id": "identity-1", "email": "user@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	}, secret)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run without a tenant claim")
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()

	RequireAuth(secret)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireAuth_NoUserIDClaim_UsesIDForScoping(t *testing.T) {
	// StoneSuite-Backend's real tokens never carry a "user_id" claim —
	// generateTenantJWT only sets id/email/tenant_id(/active_role_id). This
	// must succeed and scope by "id", not reject for a missing "user_id".
	signed := signToken(t, jwt.MapClaims{
		"id": "identity-1", "email": "user@example.com", "tenant_id": "tenant-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	}, secret)

	var captured UserContext
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := GetUserFromContext(r.Context())
		if err != nil {
			t.Fatalf("GetUserFromContext: %v", err)
		}
		captured = user
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()

	RequireAuth(secret)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if captured.UserID != "identity-1" {
		t.Fatalf("UserID = %q, want it to fall back to the id claim (identity-1)", captured.UserID)
	}
}

func TestRequireInternalSecret_ValidSecret_Passes(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Internal-Secret", "the-secret")
	rec := httptest.NewRecorder()

	RequireInternalSecret("the-secret")(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireInternalSecret_WrongOrMissingSecret_Returns401(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not run without the correct internal secret")
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()

	RequireInternalSecret("the-secret")(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
