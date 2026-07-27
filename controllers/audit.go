package controllers

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"stonesuite-notify/audit"
	"stonesuite-notify/middleware"
	"stonesuite-notify/models"
)

// AuditHandler exposes the audit trail. Both of its routes are tenant-
// scoped: the admin route takes the tenant from the caller's JWT, the
// internal route from an explicit query parameter, and neither can be
// widened to "all tenants".
type AuditHandler struct {
	Store audit.Store
}

// NewAuditHandler builds an AuditHandler backed by the given store.
func NewAuditHandler(store audit.Store) *AuditHandler {
	return &AuditHandler{Store: store}
}

// List handles GET /api/admin/audit-logs — the calling user's own tenant
// only, gated by the audit:read permission.
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}
	h.list(w, r, user.TenantID)
}

// ListInternal handles GET /api/audit-logs — the same query for a
// service-to-service caller, which names the tenant explicitly since there
// is no JWT on that call path.
func (h *AuditHandler) ListInternal(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenantId")
	if tenantID == "" {
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	}
	h.list(w, r, tenantID)
}

func (h *AuditHandler) list(w http.ResponseWriter, r *http.Request, tenantID string) {
	query := r.URL.Query()

	filter := audit.Filter{
		Action:      query.Get("action"),
		ActorUserID: query.Get("actorUserId"),
		Resource:    query.Get("resource"),
		ResourceID:  query.Get("resourceId"),
	}

	from, err := parseTimeParam(query.Get("from"))
	if err != nil {
		fail(w, http.StatusBadRequest, "from must be an RFC 3339 timestamp.")
		return
	}
	to, err := parseTimeParam(query.Get("to"))
	if err != nil {
		fail(w, http.StatusBadRequest, "to must be an RFC 3339 timestamp.")
		return
	}
	filter.From, filter.To = from, to

	page, pageSize := parsePageParams(query, audit.DefaultLimit, audit.MaxLimit)
	filter.Limit = pageSize
	filter.Offset = (page - 1) * pageSize

	entries, total, err := h.Store.List(r.Context(), tenantID, filter)
	if err != nil {
		log.Printf("audit: list for tenant %s: %v", tenantID, err)
		fail(w, http.StatusInternalServerError, "Failed to load audit log.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data: map[string]any{
			"entries":    entries,
			"pagination": newPagination(page, pageSize, total),
		},
	})
}

func parseTimeParam(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// pagination is the paging envelope every list endpoint returns alongside
// its rows, so a client never has to infer whether more pages exist.
type pagination struct {
	Page       int  `json:"page"`
	PageSize   int  `json:"pageSize"`
	Total      int  `json:"total"`
	TotalPages int  `json:"totalPages"`
	HasMore    bool `json:"hasMore"`
}

func newPagination(page, pageSize, total int) pagination {
	totalPages := 0
	if pageSize > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	return pagination{
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
		HasMore:    page < totalPages,
	}
}

// parsePageParams reads 1-based page/pageSize query parameters, falling
// back to defaultSize for anything missing, unparseable, or out of range.
func parsePageParams(query url.Values, defaultSize, maxSize int) (page, pageSize int) {
	page, pageSize = 1, defaultSize

	if parsed, err := strconv.Atoi(query.Get("page")); err == nil && parsed > 0 {
		page = parsed
	}
	if parsed, err := strconv.Atoi(query.Get("pageSize")); err == nil && parsed > 0 && parsed <= maxSize {
		pageSize = parsed
	}
	return page, pageSize
}

// auditEntry fills in the request-derived fields (caller address, user
// agent) shared by every entry this package records.
func auditEntry(r *http.Request, e audit.Entry) audit.Entry {
	e.IPAddress = clientIP(r)
	e.UserAgent = truncate(r.UserAgent(), 300)
	return e
}

// clientIP prefers the first X-Forwarded-For hop (the original client when
// the service runs behind a proxy) and falls back to the socket address.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		if trimmed := strings.TrimSpace(first); trimmed != "" {
			return truncate(trimmed, 64)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return truncate(host, 64)
	}
	return truncate(r.RemoteAddr, 64)
}

// truncate keeps a value inside its column width — an oversized header must
// never fail the insert of an audit row.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
