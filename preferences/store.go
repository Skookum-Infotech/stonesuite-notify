package preferences

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the persistence boundary for tenant/user notification
// preferences. controllers depend on this interface, not *PGStore, so
// they can be tested with a fake.
type Store interface {
	// Resolve merges the user's own overrides on top of their tenant's
	// defaults, defaulting to all-enabled when neither exists.
	Resolve(ctx context.Context, tenantID, userID string) (Preferences, error)
	GetUserOverride(ctx context.Context, tenantID, userID string) (OverrideInput, error)
	SetUserOverride(ctx context.Context, tenantID, userID string, in OverrideInput) error
	// GetTenantDefaults returns a tenant's org-wide defaults, or all-enabled
	// if the tenant has never set any — a missing row means "inherit the
	// service default", not an error.
	GetTenantDefaults(ctx context.Context, tenantID string) (Preferences, error)
	SetTenantDefaults(ctx context.Context, tenantID string, in TenantDefaultsInput) error
}

// PGStore implements Store against Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore builds a PGStore backed by the given pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// GetTenantDefaults reads a tenant's org-wide defaults, falling back to
// all-enabled when the tenant has no row yet.
func (s *PGStore) GetTenantDefaults(ctx context.Context, tenantID string) (Preferences, error) {
	prefs := Preferences{EmailEnabled: true, InAppEnabled: true, PushEnabled: true}

	err := s.pool.QueryRow(ctx, `
		SELECT email_enabled, in_app_enabled, push_enabled
		FROM tenant_notification_defaults WHERE tenant_id = $1`, tenantID,
	).Scan(&prefs.EmailEnabled, &prefs.InAppEnabled, &prefs.PushEnabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Preferences{}, fmt.Errorf("get tenant notification defaults: %w", err)
	}
	return prefs, nil
}

// Resolve reads the tenant defaults then applies any non-NULL column from
// user_notification_preferences on top.
func (s *PGStore) Resolve(ctx context.Context, tenantID, userID string) (Preferences, error) {
	prefs, err := s.GetTenantDefaults(ctx, tenantID)
	if err != nil {
		return Preferences{}, err
	}

	override, err := s.GetUserOverride(ctx, tenantID, userID)
	if err != nil {
		return Preferences{}, err
	}
	if override.EmailEnabled != nil {
		prefs.EmailEnabled = *override.EmailEnabled
	}
	if override.InAppEnabled != nil {
		prefs.InAppEnabled = *override.InAppEnabled
	}
	if override.PushEnabled != nil {
		prefs.PushEnabled = *override.PushEnabled
	}

	return prefs, nil
}

// GetUserOverride returns the caller's raw override row (nil fields mean
// "inherit"), for a settings page to show what's explicitly set versus
// inherited.
func (s *PGStore) GetUserOverride(ctx context.Context, tenantID, userID string) (OverrideInput, error) {
	var out OverrideInput
	err := s.pool.QueryRow(ctx, `
		SELECT email_enabled, in_app_enabled, push_enabled
		FROM user_notification_preferences WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID,
	).Scan(&out.EmailEnabled, &out.InAppEnabled, &out.PushEnabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return OverrideInput{}, fmt.Errorf("get user preference override: %w", err)
	}
	return out, nil
}

// SetUserOverride upserts the caller's own override row. Only non-nil
// fields in "in" change; omitted fields keep whatever was previously
// stored (or stay NULL/"inherit" on first write), via COALESCE against the
// existing row.
func (s *PGStore) SetUserOverride(ctx context.Context, tenantID, userID string, in OverrideInput) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_notification_preferences (tenant_id, user_id, email_enabled, in_app_enabled, push_enabled)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET
			email_enabled  = COALESCE(EXCLUDED.email_enabled, user_notification_preferences.email_enabled),
			in_app_enabled = COALESCE(EXCLUDED.in_app_enabled, user_notification_preferences.in_app_enabled),
			push_enabled   = COALESCE(EXCLUDED.push_enabled, user_notification_preferences.push_enabled),
			updated_at     = NOW()`,
		tenantID, userID, in.EmailEnabled, in.InAppEnabled, in.PushEnabled)
	if err != nil {
		return fmt.Errorf("set user notification preference override: %w", err)
	}
	return nil
}

// SetTenantDefaults upserts a tenant's org-wide defaults — a full replace,
// not a partial patch (all three fields are always written).
func (s *PGStore) SetTenantDefaults(ctx context.Context, tenantID string, in TenantDefaultsInput) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_notification_defaults (tenant_id, email_enabled, in_app_enabled, push_enabled)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET
			email_enabled  = EXCLUDED.email_enabled,
			in_app_enabled = EXCLUDED.in_app_enabled,
			push_enabled   = EXCLUDED.push_enabled,
			updated_at     = NOW()`,
		tenantID, in.EmailEnabled, in.InAppEnabled, in.PushEnabled)
	if err != nil {
		return fmt.Errorf("set tenant notification defaults: %w", err)
	}
	return nil
}
