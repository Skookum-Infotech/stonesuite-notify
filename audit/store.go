package audit

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the persistence boundary for notification_audit_logs. It is
// deliberately append-and-read only: there is no Update or Delete, so a
// recorded entry cannot be altered through this service.
type Store interface {
	Record(ctx context.Context, e Entry) error
	// List returns one tenant's entries, newest first, plus the total
	// matching the filter (ignoring paging) so callers can page through.
	List(ctx context.Context, tenantID string, f Filter) ([]Entry, int, error)
}

// PGStore implements Store against Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore builds a PGStore backed by the given pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

const entryColumns = `id, tenant_id, actor_user_id, actor_type, action, resource, resource_id, metadata, ip_address, user_agent, created_at`

func scanEntry(row pgx.Row) (*Entry, error) {
	var e Entry
	var actorUserID *string
	if err := row.Scan(
		&e.ID, &e.TenantID, &actorUserID, &e.ActorType, &e.Action,
		&e.Resource, &e.ResourceID, &e.Metadata, &e.IPAddress, &e.UserAgent, &e.CreatedAt,
	); err != nil {
		return nil, err
	}
	if actorUserID != nil {
		e.ActorUserID = *actorUserID
	}
	return &e, nil
}

// Record appends one entry.
func (s *PGStore) Record(ctx context.Context, e Entry) error {
	// actor_user_id is a nullable UUID column, so an empty string (service
	// and worker entries) must be written as NULL, not "".
	var actorUserIDArg any
	if e.ActorUserID != "" {
		actorUserIDArg = e.ActorUserID
	}
	actorType := e.ActorType
	if actorType == "" {
		actorType = ActorUser
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_audit_logs
			(tenant_id, actor_user_id, actor_type, action, resource, resource_id, metadata, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.TenantID, actorUserIDArg, actorType, e.Action, e.Resource, e.ResourceID,
		nullableJSON(e.Metadata), e.IPAddress, e.UserAgent)
	if err != nil {
		return fmt.Errorf("record audit entry %s: %w", e.Action, err)
	}
	return nil
}

// List returns a tenant's entries newest first, along with the unpaged total.
func (s *PGStore) List(ctx context.Context, tenantID string, f Filter) ([]Entry, int, error) {
	f = f.normalize()

	// tenant_id is always the first condition and is never caller-optional,
	// so no filter combination can widen a query beyond one tenant.
	conditions := []string{"tenant_id = $1"}
	args := []any{tenantID}

	addCondition := func(clause string, value any) {
		args = append(args, value)
		conditions = append(conditions, clause+"$"+strconv.Itoa(len(args)))
	}
	if f.Action != "" {
		addCondition("action = ", f.Action)
	}
	if f.ActorUserID != "" {
		addCondition("actor_user_id = ", f.ActorUserID)
	}
	if f.Resource != "" {
		addCondition("resource = ", f.Resource)
	}
	if f.ResourceID != "" {
		addCondition("resource_id = ", f.ResourceID)
	}
	if f.From != nil {
		addCondition("created_at >= ", *f.From)
	}
	if f.To != nil {
		addCondition("created_at <= ", *f.To)
	}
	where := " WHERE " + strings.Join(conditions, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM notification_audit_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit entries: %w", err)
	}

	args = append(args, f.Limit, f.Offset)
	query := `SELECT ` + entryColumns + ` FROM notification_audit_logs` + where +
		` ORDER BY created_at DESC LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit entries: %w", err)
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan audit entry: %w", err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate audit entries: %w", err)
	}
	return out, total, nil
}

// nullableJSON keeps an absent metadata field as SQL NULL rather than the
// JSONB value "null".
func nullableJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
