package middleware

import (
	"net/http"
	"strings"
)

// Permissions recognised by this service. They are named with the same
// "<resource>:<action>" shape StoneSuite-Backend uses so a single role
// definition over there can grant them without translation.
//
// The self-service permissions cover a user acting on their own rows only —
// every store query behind them is already scoped to
// (tenant_id, recipient_user_id), so holding one never exposes another
// user's data. The admin permissions cover tenant-wide reads and writes.
const (
	// Self-service: acting on your own notifications and settings.
	PermNotificationRead   = "notification:read"
	PermNotificationUpdate = "notification:update"
	PermPreferenceRead     = "preference:read"
	PermPreferenceUpdate   = "preference:update"
	PermPushManage         = "push:manage"

	// Elevated: acting across a whole tenant. Never implicitly granted.
	PermNotificationAdmin = "notification:admin"
	PermPreferenceAdmin   = "preference:admin"
	PermAuditRead         = "audit:read"
)

// selfServicePermissions are granted implicitly to any authenticated caller
// whose token carries no explicit "permissions" claim.
//
// StoneSuite-Backend's generateTenantJWT currently mints only
// id/email/tenant_id/active_role_id (see auth_test.go), so requiring an
// explicit claim for these would lock every user out of their own
// notification bell. Elevated permissions are deliberately absent from this
// set: they stay deny-by-default until the backend actually grants them, so
// the tenant-wide endpoints are never reachable by accident.
var selfServicePermissions = map[string]bool{
	PermNotificationRead:   true,
	PermNotificationUpdate: true,
	PermPreferenceRead:     true,
	PermPreferenceUpdate:   true,
	PermPushManage:         true,
}

// Can reports whether the caller holds a permission.
//
// A token carrying an explicit "permissions" claim is authoritative: only
// what it lists (plus wildcards) is granted. A token without the claim
// falls back to the self-service set — see selfServicePermissions.
func (u UserContext) Can(permission string) bool {
	if len(u.Permissions) == 0 {
		return selfServicePermissions[permission]
	}
	for _, granted := range u.Permissions {
		if granted == "*" || granted == permission {
			return true
		}
		// "notification:*" grants every notification:<action>.
		if prefix, ok := strings.CutSuffix(granted, "*"); ok && strings.HasPrefix(permission, prefix) {
			return true
		}
	}
	return false
}

// RequirePermission gates a route on a single permission. It must be
// composed inside RequireAuth, which is what populates the UserContext this
// reads — on its own it denies every request.
func RequirePermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			user, err := GetUserFromContext(r.Context())
			if err != nil {
				deny(w, http.StatusUnauthorized, "Access denied. No authenticated session.")
				return
			}
			if !user.Can(permission) {
				deny(w, http.StatusForbidden, "Access denied. Missing required permission: "+permission+".")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Protected composes RequireAuth and RequirePermission into the single
// wrapper every user-facing route uses, so no route can accidentally be
// registered with authentication but no permission check.
func Protected(jwtSecret, permission string) func(http.Handler) http.Handler {
	requireAuth := RequireAuth(jwtSecret)
	requirePermission := RequirePermission(permission)
	return func(next http.Handler) http.Handler {
		return requireAuth(requirePermission(next))
	}
}

// parsePermissionsClaim normalises the shapes a "permissions" JWT claim can
// arrive in. JSON decoding yields []any for arrays; a claim minted as a
// single space- or comma-delimited string (the OAuth "scope" convention) is
// accepted too so this service does not constrain how the backend mints it.
func parsePermissionsClaim(raw any) []string {
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		fields := strings.FieldsFunc(v, func(r rune) bool {
			return r == ' ' || r == ','
		})
		out := make([]string, 0, len(fields))
		for _, f := range fields {
			if f != "" {
				out = append(out, f)
			}
		}
		return out
	default:
		return nil
	}
}
