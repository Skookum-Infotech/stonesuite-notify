// Package controllers wires HTTP handlers to the notifications store.
package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"stonesuite-notify/audit"
	"stonesuite-notify/config"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/middleware"
	"stonesuite-notify/models"
	"stonesuite-notify/notifications"
	"stonesuite-notify/preferences"
)

// Handler exposes the notification HTTP endpoints against a Store.
type Handler struct {
	Store           notifications.Store
	PreferenceStore preferences.Store
	DeliveryStore   deliveries.Store
	Audit           audit.Recorder
	Config          config.Config
}

// NewHandler builds a Handler backed by the given stores. Config carries the
// email/push provider settings the workers package uses to actually send
// once a delivery is enqueued here.
func NewHandler(store notifications.Store, prefStore preferences.Store, deliveryStore deliveries.Store, recorder audit.Recorder, cfg config.Config) *Handler {
	return &Handler{
		Store:           store,
		PreferenceStore: prefStore,
		DeliveryStore:   deliveryStore,
		Audit:           recorder,
		Config:          cfg,
	}
}

// Summary handles GET /api/notifications/summary — the endpoint the
// frontend bell polls for its unread badge count.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	count, err := h.Store.UnreadCount(r.Context(), user.TenantID, user.UserID)
	if err != nil {
		log.Printf("notifications: summary for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load notification summary.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"unreadCount": count},
	})
}

// List handles GET /api/notifications — the bell dropdown's "latest"
// feed: newest first, optionally unread-only, capped by limit
// (default/max enforced in Store). Use History for the paged "view all"
// screen instead.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	unreadOnly := r.URL.Query().Get("unreadOnly") == "true"
	limit := notifications.DefaultPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			limit = parsed
		}
	}

	list, err := h.Store.ListForUser(r.Context(), user.TenantID, user.UserID, unreadOnly, limit, 0)
	if err != nil {
		log.Printf("notifications: list for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load notifications.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"notifications": list},
	})
}

// History handles GET /api/notifications/history — the bell's "view all"
// screen. Same rows as List, but paged and accompanied by a total so the
// client can render page controls.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	unreadOnly := r.URL.Query().Get("unreadOnly") == "true"
	page, pageSize := parsePageParams(r.URL.Query(), notifications.DefaultPageSize, notifications.MaxPageSize)

	total, err := h.Store.CountForUser(r.Context(), user.TenantID, user.UserID, unreadOnly)
	if err != nil {
		log.Printf("notifications: count history for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load notifications.")
		return
	}

	list, err := h.Store.ListForUser(r.Context(), user.TenantID, user.UserID, unreadOnly, pageSize, (page-1)*pageSize)
	if err != nil {
		log.Printf("notifications: list history for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load notifications.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data: map[string]any{
			"notifications": list,
			"pagination":    newPagination(page, pageSize, total),
		},
	})
}

// MarkRead handles POST /api/notifications/{id}/read.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	id := r.PathValue("id")
	if id == "" {
		fail(w, http.StatusBadRequest, "Notification id is required.")
		return
	}

	if err := h.Store.MarkRead(r.Context(), user.TenantID, user.UserID, id); err != nil {
		if errors.Is(err, notifications.ErrNotFound) {
			fail(w, http.StatusNotFound, "Notification not found.")
			return
		}
		log.Printf("notifications: mark read %s for user %s: %v", id, user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to mark notification read.")
		return
	}

	h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
		Action:      audit.ActionNotificationRead,
		Resource:    audit.ResourceNotification,
		ResourceID:  id,
	}))

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

// MarkAllRead handles POST /api/notifications/read-all.
func (h *Handler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	if err := h.Store.MarkAllRead(r.Context(), user.TenantID, user.UserID); err != nil {
		log.Printf("notifications: mark all read for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to mark notifications read.")
		return
	}

	h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
		Action:      audit.ActionNotificationReadAll,
		Resource:    audit.ResourceNotification,
	}))

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

// recipientTarget is one addressee of a create request. Email is only used
// if "email" is requested in Channels; the caller (e.g. StoneSuite-Backend
// resolving "everyone with role Manager") supplies it since this service
// has no user directory of its own to look addresses up in.
type recipientTarget struct {
	UserID string `json:"userId"`
	Email  string `json:"email,omitempty"`
}

// attachmentInput is the wire shape of createNotificationRequest.Attachment
// — content travels as base64 over JSON, decoded to bytes before building
// notifications.AttachmentInput.
type attachmentInput struct {
	FileName      string `json:"fileName"`
	ContentType   string `json:"contentType"`
	ContentBase64 string `json:"contentBase64"`
}

// createNotificationRequest is the payload accepted by the internal create
// endpoint. Multiple recipients let a caller fan a single business event
// out to every user holding a role (role membership is resolved by the
// caller, which already owns that RBAC data) in one request. The in-app
// row is always created; email is opt-in per request via Channels; push is
// attempted automatically for any device the recipient has subscribed.
type createNotificationRequest struct {
	TenantID    string            `json:"tenantId"`
	Recipients  []recipientTarget `json:"recipients"`
	ActorUserID string            `json:"actorUserId,omitempty"`
	EventType   string            `json:"eventType"`
	Resource    string            `json:"resource"`
	ResourceID  string            `json:"resourceId"`
	Title       string            `json:"title"`
	Body        string            `json:"body,omitempty"`
	Link        string            `json:"link,omitempty"`
	Channels    []string          `json:"channels,omitempty"`
	Attachment  *attachmentInput  `json:"attachment,omitempty"`
}

const channelEmail = "email"

// Create handles POST /api/notifications/internal — called by another
// StoneSuite service (not an end user) when a business event fires. Gated
// by middleware.RequireInternalSecret rather than end-user auth.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}

	if req.TenantID == "" {
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	}
	if len(req.Recipients) == 0 {
		fail(w, http.StatusBadRequest, "recipients is required.")
		return
	}

	var attachment *notifications.AttachmentInput
	if req.Attachment != nil {
		content, decErr := base64.StdEncoding.DecodeString(req.Attachment.ContentBase64)
		if decErr != nil {
			fail(w, http.StatusBadRequest, "attachment.contentBase64 is not valid base64.")
			return
		}
		attachment = &notifications.AttachmentInput{
			FileName:    req.Attachment.FileName,
			ContentType: req.Attachment.ContentType,
			Content:     content,
		}
	}

	// Validate every recipient up front so a bad entry rejects the whole
	// request instead of partially creating notifications for some
	// recipients but not others. Preferences are resolved per recipient
	// only after validation passes, so an invalid entry never triggers a
	// wasted preference lookup.
	type plannedCreate struct {
		input notifications.CreateInput
		prefs preferences.Preferences
	}
	planned := make([]plannedCreate, 0, len(req.Recipients))
	for _, recipient := range req.Recipients {
		in := notifications.CreateInput{
			TenantID:        req.TenantID,
			RecipientUserID: recipient.UserID,
			RecipientEmail:  recipient.Email,
			ActorUserID:     req.ActorUserID,
			EventType:       req.EventType,
			Resource:        req.Resource,
			ResourceID:      req.ResourceID,
			Title:           req.Title,
			Body:            req.Body,
			Link:            req.Link,
		}
		if err := in.Validate(); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}

		prefs, err := h.PreferenceStore.Resolve(r.Context(), req.TenantID, recipient.UserID)
		if err != nil {
			// Fail open: an outage in the preferences table must never
			// block the in-app row, which remains the source of truth.
			log.Printf("notifications: resolve preferences for %s: %v", recipient.UserID, err)
			prefs = preferences.Preferences{EmailEnabled: true, InAppEnabled: true, PushEnabled: true}
		}
		in.VisibleInApp = prefs.InAppEnabled

		planned = append(planned, plannedCreate{input: in, prefs: prefs})
	}

	wantsEmail := containsChannel(req.Channels, channelEmail)

	created := make([]*notifications.Notification, 0, len(planned))
	for _, p := range planned {
		n, err := h.Store.Create(r.Context(), p.input)
		if err != nil {
			log.Printf("notifications: create for recipient %s: %v", p.input.RecipientUserID, err)
			continue
		}
		created = append(created, n)
		if attachment != nil && wantsEmail && p.prefs.EmailEnabled {
			if err := h.Store.SaveAttachment(r.Context(), n.ID, *attachment); err != nil {
				log.Printf("notifications: save attachment for %s: %v", n.ID, err)
			}
		}
		h.enqueueDeliveries(r.Context(), *n, p.prefs, wantsEmail)

		// One entry per notification, not per request, so the trail can
		// answer "was this user ever notified about resource X" directly.
		// The actor is the user whose action raised the event, if the
		// calling service named one; the actor type stays "service"
		// because the call itself came over the internal secret.
		h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
			TenantID:    n.TenantID,
			ActorUserID: req.ActorUserID,
			ActorType:   audit.ActorService,
			Action:      audit.ActionNotificationCreated,
			Resource:    audit.ResourceNotification,
			ResourceID:  n.ID,
			Metadata: audit.Metadata(map[string]any{
				"recipientUserId": n.RecipientUserID,
				"eventType":       n.EventType,
				"resource":        n.Resource,
				"resourceId":      n.ResourceID,
				"visibleInApp":    n.VisibleInApp,
				"emailEnabled":    p.prefs.EmailEnabled,
				"pushEnabled":     p.prefs.PushEnabled,
				"emailRequested":  wantsEmail,
			}),
		}))
	}

	if len(created) == 0 {
		fail(w, http.StatusInternalServerError, "Failed to create notification.")
		return
	}

	writeJSON(w, http.StatusCreated, models.APIResponse{
		Success: true,
		Data:    map[string]any{"notifications": created},
	})
}

// Deliveries handles GET /api/notifications/{id}/deliveries — the
// internal-secret-gated support/ops view of the delivery log (status
// history + provider response per channel) for one notification. The
// tenant is named explicitly because there is no JWT on this call path,
// and it is required: without it the lookup would be scoped by
// notification id alone and a leaked id would expose another tenant's log.
func (h *Handler) Deliveries(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenantId")
	if tenantID == "" {
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	}

	h.deliveries(w, r, tenantID, audit.Entry{
		TenantID:  tenantID,
		ActorType: audit.ActorService,
	})
}

// AdminDeliveries handles GET /api/admin/notifications/{id}/deliveries —
// the same view for a signed-in tenant administrator, gated by the
// notification:admin permission. The tenant comes from the caller's own
// token, never the request, so an admin can only ever read their own
// tenant's delivery log.
func (h *Handler) AdminDeliveries(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	h.deliveries(w, r, user.TenantID, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
	})
}

// deliveries serves both delivery-log routes. actor carries whichever
// identity the caller's route established; this function fills in the rest
// of the audit entry.
func (h *Handler) deliveries(w http.ResponseWriter, r *http.Request, tenantID string, actor audit.Entry) {
	id := r.PathValue("id")
	if id == "" {
		fail(w, http.StatusBadRequest, "Notification id is required.")
		return
	}

	list, err := h.DeliveryStore.ListForNotification(r.Context(), tenantID, id)
	if err != nil {
		log.Printf("notifications: list deliveries for %s: %v", id, err)
		fail(w, http.StatusInternalServerError, "Failed to load delivery log.")
		return
	}

	// Reading a delivery log exposes who was contacted on which channel, so
	// the read itself is auditable.
	actor.Action = audit.ActionDeliveryLogViewed
	actor.Resource = audit.ResourceNotification
	actor.ResourceID = id
	actor.Metadata = audit.Metadata(map[string]any{"deliveryCount": len(list)})
	h.Audit.Record(r.Context(), auditEntry(r, actor))

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"deliveries": list},
	})
}

func containsChannel(channelList []string, target string) bool {
	for _, c := range channelList {
		if c == target {
			return true
		}
	}
	return false
}

// enqueueDeliveries writes the queue/log rows for one already-persisted
// notification. in_app is recorded already-terminal (sent or skipped)
// since the notifications row's existence/visibility is itself the
// delivery; email and push are left pending for the workers package's
// QueueConsumer to pick up. Failures here are logged, never surfaced — the
// in-app row is already the source of truth.
func (h *Handler) enqueueDeliveries(ctx context.Context, n notifications.Notification, prefs preferences.Preferences, wantsEmail bool) {
	inAppStatus := deliveries.StatusSkipped
	if prefs.InAppEnabled {
		inAppStatus = deliveries.StatusSent
	}
	if _, err := h.DeliveryStore.Enqueue(ctx, n.ID, n.TenantID, n.RecipientUserID, deliveries.ChannelInApp, inAppStatus); err != nil {
		log.Printf("notifications: enqueue in_app delivery log for %s: %v", n.ID, err)
	}

	if wantsEmail && prefs.EmailEnabled {
		if _, err := h.DeliveryStore.Enqueue(ctx, n.ID, n.TenantID, n.RecipientUserID, deliveries.ChannelEmail, deliveries.StatusPending); err != nil {
			log.Printf("notifications: enqueue email delivery for %s: %v", n.ID, err)
		}
	}

	if prefs.PushEnabled && h.Config.PushConfigured() {
		if _, err := h.DeliveryStore.Enqueue(ctx, n.ID, n.TenantID, n.RecipientUserID, deliveries.ChannelPush, deliveries.StatusPending); err != nil {
			log.Printf("notifications: enqueue push delivery for %s: %v", n.ID, err)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body models.APIResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, models.APIResponse{Success: false, Message: message})
}
