// Package preferences implements the notify service's Preference Manager:
// tenant-wide defaults plus per-user overrides for which channels a
// recipient receives (email, in-app, push). controllers.Handler.Create
// resolves these before enqueueing any delivery, so every notification —
// including in-app — flows through preferences.
package preferences

// Preferences is the resolved, effective set of channel toggles for one
// user: their own overrides merged on top of their tenant's defaults
// (missing rows at either level mean "inherit" / "enabled").
type Preferences struct {
	EmailEnabled bool `json:"emailEnabled"`
	InAppEnabled bool `json:"inAppEnabled"`
	PushEnabled  bool `json:"pushEnabled"`
}

// OverrideInput is a partial update to a user's own preference overrides —
// a nil field is left untouched (still inheriting the tenant default),
// distinguishing "explicitly set to false" from "not set".
type OverrideInput struct {
	EmailEnabled *bool `json:"emailEnabled,omitempty"`
	InAppEnabled *bool `json:"inAppEnabled,omitempty"`
	PushEnabled  *bool `json:"pushEnabled,omitempty"`
}

// TenantDefaultsInput sets a tenant's org-wide defaults. Unlike
// OverrideInput this is a full replace (all three required) — tenant
// defaults are an administrative setting, not a per-field patch.
type TenantDefaultsInput struct {
	EmailEnabled bool `json:"emailEnabled"`
	InAppEnabled bool `json:"inAppEnabled"`
	PushEnabled  bool `json:"pushEnabled"`
}
