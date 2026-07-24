// Package pushsubs persists browser Web Push subscriptions — one row per
// browser/device a user has granted push permission on.
package pushsubs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Subscription is one browser's Web Push registration.
type Subscription struct {
	ID        string
	TenantID  string
	UserID    string
	Endpoint  string
	P256dh    string
	Auth      string
	CreatedAt time.Time
}

// SaveInput is the payload accepted from the frontend's
// PushManager.subscribe() result.
type SaveInput struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Store is the persistence boundary for push subscriptions. Handlers
// depend on this interface, not *PGStore, so they can be tested with a
// fake store.
type Store interface {
	Save(ctx context.Context, tenantID, userID string, in SaveInput) error
	Delete(ctx context.Context, tenantID, userID, endpoint string) error
	ListForUser(ctx context.Context, tenantID, userID string) ([]Subscription, error)
}

// PGStore implements Store against Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore builds a PGStore backed by the given pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// Save upserts a subscription by endpoint — endpoint is globally unique
// per browser/device, so re-subscribing (e.g. after clearing site data)
// replaces the stale row rather than accumulating duplicates.
func (s *PGStore) Save(ctx context.Context, tenantID, userID string, in SaveInput) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO push_subscriptions (tenant_id, user_id, endpoint, p256dh_key, auth_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (endpoint) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			user_id = EXCLUDED.user_id,
			p256dh_key = EXCLUDED.p256dh_key,
			auth_key = EXCLUDED.auth_key`,
		tenantID, userID, in.Endpoint, in.P256dh, in.Auth)
	if err != nil {
		return fmt.Errorf("save push subscription: %w", err)
	}
	return nil
}

// Delete removes a subscription, scoped to the caller so a user can only
// remove their own device registration.
func (s *PGStore) Delete(ctx context.Context, tenantID, userID, endpoint string) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM push_subscriptions
		WHERE tenant_id = $1 AND user_id = $2 AND endpoint = $3`,
		tenantID, userID, endpoint); err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	return nil
}

// ListForUser returns every device a user has subscribed from — a
// notification fans out to all of them.
func (s *PGStore) ListForUser(ctx context.Context, tenantID, userID string) ([]Subscription, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, user_id, endpoint, p256dh_key, auth_key, created_at
		FROM push_subscriptions
		WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	defer rows.Close()

	subs := []Subscription{}
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("scan push subscription row: %w", err)
		}
		subs = append(subs, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate push subscriptions: %w", err)
	}
	return subs, nil
}

func scanSubscription(row pgx.Row) (*Subscription, error) {
	var s Subscription
	if err := row.Scan(&s.ID, &s.TenantID, &s.UserID, &s.Endpoint, &s.P256dh, &s.Auth, &s.CreatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}
