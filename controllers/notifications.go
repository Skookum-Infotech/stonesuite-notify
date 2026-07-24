// Package controllers wires HTTP handlers to the notifications store.
package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"stonesuite-notify/channels"
	"stonesuite-notify/config"
	"stonesuite-notify/middleware"
	"stonesuite-notify/models"
	"stonesuite-notify/notifications"
	"stonesuite-notify/pushsubs"
)

// Handler exposes the notification HTTP endpoints against a Store.
type Handler struct {
	Store     notifications.Store
	PushStore pushsubs.Store
	Config    config.Config
}

// NewHandler builds a Handler backed by the given stores. Config carries the
// email/push provider settings used to dispatch side channels after an
// in-app notification is created.
func NewHandler(store notifications.Store, pushStore pushsubs.Store, cfg config.Config) *Handler {
	return &Handler{Store: store, PushStore: pushStore, Config: cfg}
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

// List handles GET /api/notifications — the dropdown feed, optionally
// filtered to unread-only, capped by limit (default/max enforced in Store).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	unreadOnly := r.URL.Query().Get("unreadOnly") == "true"
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			limit = parsed
		}
	}

	list, err := h.Store.ListForUser(r.Context(), user.TenantID, user.UserID, unreadOnly, limit)
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

	// Validate every recipient up front so a bad entry rejects the whole
	// request instead of partially creating notifications for some
	// recipients but not others.
	inputs := make([]notifications.CreateInput, 0, len(req.Recipients))
	for _, recipient := range req.Recipients {
		in := notifications.CreateInput{
			TenantID:        req.TenantID,
			RecipientUserID: recipient.UserID,
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
		inputs = append(inputs, in)
	}

	wantsEmail := containsChannel(req.Channels, channelEmail)

	created := make([]*notifications.Notification, 0, len(inputs))
	for i, in := range inputs {
		n, err := h.Store.Create(r.Context(), in)
		if err != nil {
			log.Printf("notifications: create for recipient %s: %v", in.RecipientUserID, err)
			continue
		}
		created = append(created, n)

		recipientEmail := req.Recipients[i].Email
		go h.dispatchChannels(*n, recipientEmail, wantsEmail)
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

func containsChannel(channelList []string, target string) bool {
	for _, c := range channelList {
		if c == target {
			return true
		}
	}
	return false
}

// dispatchChannels fires the email and push side channels for one
// already-persisted notification. It runs in its own goroutine, detached
// from the request context, so a slow or failing provider never delays or
// fails the create response — the in-app row is already the source of
// truth by the time this runs.
func (h *Handler) dispatchChannels(n notifications.Notification, recipientEmail string, wantsEmail bool) {
	if wantsEmail && recipientEmail != "" {
		if err := channels.SendNotificationEmail(h.Config, recipientEmail, n.Title, n.Body, n.Link); err != nil {
			log.Printf("notifications: email dispatch for %s: %v", n.ID, err)
		}
	}

	if !h.Config.PushConfigured() || h.PushStore == nil {
		return
	}

	ctx := context.Background()
	subs, err := h.PushStore.ListForUser(ctx, n.TenantID, n.RecipientUserID)
	if err != nil {
		log.Printf("notifications: list push subscriptions for %s: %v", n.RecipientUserID, err)
		return
	}

	for _, sub := range subs {
		stale, err := channels.SendWebPush(h.Config, channels.PushSubscription{
			Endpoint: sub.Endpoint,
			P256dh:   sub.P256dh,
			Auth:     sub.Auth,
		}, n.Title, n.Body, n.Link)
		if err != nil {
			log.Printf("notifications: push dispatch for %s: %v", n.ID, err)
			continue
		}
		if stale {
			if err := h.PushStore.Delete(ctx, n.TenantID, n.RecipientUserID, sub.Endpoint); err != nil {
				log.Printf("notifications: prune stale push subscription %s: %v", sub.ID, err)
			}
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
