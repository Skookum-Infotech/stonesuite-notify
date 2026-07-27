package notifications

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a notification lookup matches no row for the
// given (tenant, recipient) — including when it exists but belongs to
// someone else, so callers can't distinguish "missing" from "not yours".
var ErrNotFound = errors.New("notification not found")

// Store is the persistence boundary for notifications. Handlers depend on
// this interface (not *PGStore) so they can be tested with a fake store.
type Store interface {
	Create(ctx context.Context, in CreateInput) (*Notification, error)
	// Get loads a single notification by id, scoped to tenant only (not
	// recipient) — used by the delivery workers to read title/body/link/
	// recipient_email fresh at send time via notification_id.
	Get(ctx context.Context, tenantID, id string) (*Notification, error)
	// ListForUser returns one page of the recipient's feed, newest first.
	// The bell dropdown passes offset 0; the "view all" screen pages
	// through with a non-zero offset.
	ListForUser(ctx context.Context, tenantID, recipientUserID string, unreadOnly bool, limit, offset int) ([]Notification, error)
	// CountForUser returns how many rows ListForUser would return in total
	// under the same filter, ignoring paging — the denominator the "view
	// all" screen needs to render page controls.
	CountForUser(ctx context.Context, tenantID, recipientUserID string, unreadOnly bool) (int, error)
	UnreadCount(ctx context.Context, tenantID, recipientUserID string) (int, error)
	MarkRead(ctx context.Context, tenantID, recipientUserID, id string) error
	MarkAllRead(ctx context.Context, tenantID, recipientUserID string) error
}

// PGStore implements Store against Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore builds a PGStore backed by the given pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

const notificationColumns = `id, tenant_id, recipient_user_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, body, link, visible_in_app, read_at, created_at`

func scanNotification(row pgx.Row) (*Notification, error) {
	var n Notification
	var actorUserID *string
	if err := row.Scan(
		&n.ID, &n.TenantID, &n.RecipientUserID, &n.RecipientEmail, &actorUserID,
		&n.EventType, &n.Resource, &n.ResourceID, &n.Title, &n.Body, &n.Link, &n.VisibleInApp,
		&n.ReadAt, &n.CreatedAt,
	); err != nil {
		return nil, err
	}
	if actorUserID != nil {
		n.ActorUserID = *actorUserID
	}
	return &n, nil
}

// Create inserts one notification row. The row is always written
// regardless of in.VisibleInApp — that flag only gates feed/bell
// visibility, never row existence, since email/push delivery reads this
// row's content via Get regardless of the in-app toggle.
func (s *PGStore) Create(ctx context.Context, in CreateInput) (*Notification, error) {
	var actorUserIDArg any
	if in.ActorUserID != "" {
		actorUserIDArg = in.ActorUserID
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO notifications
			(tenant_id, recipient_user_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, body, link, visible_in_app)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+notificationColumns,
		in.TenantID, in.RecipientUserID, in.RecipientEmail, actorUserIDArg, in.EventType, in.Resource, in.ResourceID, in.Title, in.Body, in.Link, in.VisibleInApp)

	n, err := scanNotification(row)
	if err != nil {
		return nil, fmt.Errorf("create notification: %w", err)
	}
	return n, nil
}

// Get loads a single notification by id, scoped to tenant only — see the
// Store interface doc for why this is not additionally scoped to recipient.
func (s *PGStore) Get(ctx context.Context, tenantID, id string) (*Notification, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+notificationColumns+`
		FROM notifications WHERE id = $1 AND tenant_id = $2`,
		id, tenantID)

	n, err := scanNotification(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get notification %s: %w", id, err)
	}
	return n, nil
}

// ListForUser returns one page of the recipient's notifications, newest
// first.
func (s *PGStore) ListForUser(ctx context.Context, tenantID, recipientUserID string, unreadOnly bool, limit, offset int) ([]Notification, error) {
	limit, offset = NormalizePaging(limit, offset)

	query := `SELECT ` + notificationColumns + `
		FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND visible_in_app = true`
	if unreadOnly {
		query += ` AND read_at IS NULL`
	}
	// id breaks ties so paging is stable when several rows share a
	// created_at — without it a row can repeat on one page and be skipped
	// on the next.
	query += ` ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4`

	rows, err := s.pool.Query(ctx, query, tenantID, recipientUserID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	notifications := []Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("scan notification row: %w", err)
		}
		notifications = append(notifications, *n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notifications: %w", err)
	}
	return notifications, nil
}

// CountForUser returns the total number of feed rows matching the same
// filter ListForUser applies, ignoring paging.
func (s *PGStore) CountForUser(ctx context.Context, tenantID, recipientUserID string, unreadOnly bool) (int, error) {
	query := `SELECT COUNT(*) FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND visible_in_app = true`
	if unreadOnly {
		query += ` AND read_at IS NULL`
	}

	var count int
	if err := s.pool.QueryRow(ctx, query, tenantID, recipientUserID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count notifications: %w", err)
	}
	return count, nil
}

// UnreadCount returns the number of unread notifications for the recipient —
// the value the frontend bell polls.
func (s *PGStore) UnreadCount(ctx context.Context, tenantID, recipientUserID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND visible_in_app = true AND read_at IS NULL`,
		tenantID, recipientUserID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return count, nil
}

// MarkRead marks a single notification read, scoped to the caller so no one
// can mark (or even probe the existence of) another user's notification.
func (s *PGStore) MarkRead(ctx context.Context, tenantID, recipientUserID, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications SET read_at = NOW()
		WHERE id = $1 AND tenant_id = $2 AND recipient_user_id = $3 AND read_at IS NULL`,
		id, tenantID, recipientUserID)
	if err != nil {
		return fmt.Errorf("mark notification %s read: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead marks every unread notification for the recipient as read.
func (s *PGStore) MarkAllRead(ctx context.Context, tenantID, recipientUserID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE notifications SET read_at = NOW()
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND read_at IS NULL`,
		tenantID, recipientUserID); err != nil {
		return fmt.Errorf("mark all notifications read: %w", err)
	}
	return nil
}
