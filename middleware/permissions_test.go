package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestCan_NoPermissionsClaim_GrantsSelfServiceOnly(t *testing.T) {
	// The shape StoneSuite-Backend mints today: no "permissions" claim.
	user := UserContext{TenantID: "t1", UserID: "u1"}

	selfService := []string{
		PermNotificationRead, PermNotificationUpdate,
		PermPreferenceRead, PermPreferenceUpdate, PermPushManage,
	}
	for _, perm := range selfService {
		if !user.Can(perm) {
			t.Errorf("Can(%q) = false, want true (self-service must work with today's tokens)", perm)
		}
	}

	elevated := []string{PermNotificationAdmin, PermPreferenceAdmin, PermAuditRead}
	for _, perm := range elevated {
		if user.Can(perm) {
			t.Errorf("Can(%q) = true, want false (elevated permissions must be deny-by-default)", perm)
		}
	}
}

func TestCan_ExplicitClaimIsAuthoritative(t *testing.T) {
	// A token that lists permissions gets exactly those — the self-service
	// fallback must not widen an explicit grant.
	user := UserContext{Permissions: []string{PermNotificationRead}}

	if !user.Can(PermNotificationRead) {
		t.Errorf("Can(%q) = false, want true (explicitly granted)", PermNotificationRead)
	}
	if user.Can(PermNotificationUpdate) {
		t.Errorf("Can(%q) = true, want false (not in the explicit claim)", PermNotificationUpdate)
	}
}

func TestCan_Wildcards(t *testing.T) {
	tests := []struct {
		name        string
		permissions []string
		permission  string
		want        bool
	}{
		{"global wildcard grants everything", []string{"*"}, PermAuditRead, true},
		{"resource wildcard grants its actions", []string{"notification:*"}, PermNotificationAdmin, true},
		{"resource wildcard does not cross resources", []string{"notification:*"}, PermPreferenceAdmin, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := UserContext{Permissions: tt.permissions}
			if got := user.Can(tt.permission); got != tt.want {
				t.Fatalf("Can(%q) = %v, want %v", tt.permission, got, tt.want)
			}
		})
	}
}

func TestRequirePermission_DeniesWithoutPermission(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs", nil)
	req = req.WithContext(contextWithUser(req.Context(), UserContext{TenantID: "t1", UserID: "u1"}))
	rec := httptest.NewRecorder()

	RequirePermission(PermAuditRead)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("handler ran despite the caller lacking the permission")
	}
}

func TestRequirePermission_AllowsWithPermission(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs", nil)
	req = req.WithContext(contextWithUser(req.Context(), UserContext{
		TenantID: "t1", UserID: "u1", Permissions: []string{PermAuditRead},
	}))
	rec := httptest.NewRecorder()

	RequirePermission(PermAuditRead)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !called {
		t.Fatal("handler did not run despite the caller holding the permission")
	}
}

func TestRequirePermission_WithoutAuthContextReturns401(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler ran without an authenticated session")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	rec := httptest.NewRecorder()

	RequirePermission(PermNotificationRead)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestProtected_ReadsPermissionsFromToken(t *testing.T) {
	secret := "test-secret"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"id":             "u1",
		"email":          "user@example.com",
		"tenant_id":      "t1",
		"active_role_id": "role-admin",
		"permissions":    []string{PermAuditRead},
		"exp":            time.Now().Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	var seen UserContext
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = GetUserFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()

	Protected(secret, PermAuditRead)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if seen.ActiveRoleID != "role-admin" {
		t.Errorf("ActiveRoleID = %q, want %q", seen.ActiveRoleID, "role-admin")
	}
	if len(seen.Permissions) != 1 || seen.Permissions[0] != PermAuditRead {
		t.Errorf("Permissions = %v, want [%s]", seen.Permissions, PermAuditRead)
	}
}

func TestProtected_TokenWithoutElevatedPermissionIsForbidden(t *testing.T) {
	secret := "test-secret"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"id": "u1", "email": "user@example.com", "tenant_id": "t1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, _ := token.SignedString([]byte(secret))

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler ran for a token lacking the elevated permission")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()

	Protected(secret, PermAuditRead)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestParsePermissionsClaim(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want []string
	}{
		{"json array", []any{"a:read", "b:write"}, []string{"a:read", "b:write"}},
		{"space delimited string", "a:read b:write", []string{"a:read", "b:write"}},
		{"comma delimited string", "a:read,b:write", []string{"a:read", "b:write"}},
		{"absent claim", nil, nil},
		{"non-string entries are dropped", []any{"a:read", 42}, []string{"a:read"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStringListClaim(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}
