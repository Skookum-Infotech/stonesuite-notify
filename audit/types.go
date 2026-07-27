// Package audit records an immutable, tenant-scoped trail of who did what
// in the notify service: end users marking notifications read or changing
// preferences, other StoneSuite services raising notifications, and the
// background workers finishing (or exhausting) a delivery.
//
// Entries are append-only — nothing in this service ever updates or deletes
// a row — and every read is scoped to a single tenant, so one tenant's trail
// is never visible to another.
package audit

import (
	"encoding/json"
	"time"
)

// Actor types distinguish the three kinds of caller that produce entries:
// an end user with a JWT, another StoneSuite service calling in over the
// internal secret, and this service's own background workers.
const (
	ActorUser    = "user"
	ActorService = "service"
	ActorWorker  = "worker"
)

// Actions recorded by this service. Named "<resource>.<past-tense-verb>" so
// the trail reads as a log of things that happened, not endpoints that were
// called.
const (
	ActionNotificationCreated   = "notification.created"
	ActionNotificationRead      = "notification.read"
	ActionNotificationReadAll   = "notification.read_all"
	ActionPreferenceUpdated     = "preference.updated"
	ActionTenantDefaultsUpdated = "tenant_defaults.updated"
	ActionPushSubscribed        = "push.subscribed"
	ActionPushUnsubscribed      = "push.unsubscribed"
	ActionDeliverySent          = "delivery.sent"
	ActionDeliveryFailed        = "delivery.failed"
	ActionDeliveryLogViewed     = "delivery_log.viewed"
)

// Resources an action can target.
const (
	ResourceNotification   = "notification"
	ResourcePreference     = "preference"
	ResourceTenantDefaults = "tenant_defaults"
	ResourcePushSub        = "push_subscription"
	ResourceDelivery       = "delivery"
)

// Entry is one audit row. TenantID and Action are the only required fields;
// everything else is contextual. ActorUserID is empty for service and worker
// entries, which have no end user behind them.
type Entry struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenantId"`
	ActorUserID string          `json:"actorUserId,omitempty"`
	ActorType   string          `json:"actorType"`
	Action      string          `json:"action"`
	Resource    string          `json:"resource,omitempty"`
	ResourceID  string          `json:"resourceId,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	IPAddress   string          `json:"ipAddress,omitempty"`
	UserAgent   string          `json:"userAgent,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
}

// Filter narrows an audit query. Every field is optional; the tenant scope
// is passed separately to List and is never optional.
type Filter struct {
	Action      string
	ActorUserID string
	Resource    string
	ResourceID  string
	From        *time.Time
	To          *time.Time
	Limit       int
	Offset      int
}

// DefaultLimit and MaxLimit bound how many entries one query returns, so a
// caller can't ask for a whole tenant's history in a single request.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// normalize clamps paging to the supported bounds.
func (f Filter) normalize() Filter {
	if f.Limit <= 0 || f.Limit > MaxLimit {
		f.Limit = DefaultLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
}

// Metadata marshals a map for Entry.Metadata, returning nil rather than an
// error — an audit entry is never worth failing an operation over, so a
// value that can't be marshalled is simply recorded without metadata.
func Metadata(fields map[string]any) json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return raw
}
