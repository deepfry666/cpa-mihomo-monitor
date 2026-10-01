package sidecar

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"cpa-mihomo-monitor/internal/mihomo"
	"cpa-mihomo-monitor/internal/model"
	"cpa-mihomo-monitor/internal/ui"
)

const (
	// A collection normally finishes in milliseconds; this budget exists so a
	// slow controller plus one retry still fits inside a single poll cycle.
	snapshotTimeout = 7 * time.Second
	// cacheDuration only coalesces viewers that arrive at nearly the same
	// moment. It has to stay well below the dashboard polling interval,
	// otherwise a fixed-interval poll can serve a snapshot two intervals old.
	cacheDuration   = 2 * time.Second
	sessionDuration = 12 * time.Hour
	sessionCookie   = "mihomo_monitor_session"
)

type Handler struct {
	adminKey   []byte
	source     model.SnapshotSource
	selectable map[string]bool
	order      []string
	sessionMu  sync.Mutex
	sessions   map[string]time.Time

	mu         sync.Mutex
	cached     model.Snapshot
	until      time.Time
	generation int
	inflight   *collection
}

// collection is one shared Mihomo read. It belongs to the handler rather than
// to the viewer that triggered it, so a canceled viewer cannot publish an
// "offline" snapshot to everybody else, and a slow viewer cannot hold the lock
// while other viewers wait.
type collection struct {
	done     chan struct{}
	snapshot model.Snapshot
	err      error
	stale    bool
}

type selectResponse struct {
	Status string `json:"status"`
	Group  string `json:"group,omitempty"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type GroupSelector interface {
	SelectGroup(context.Context, string, string) error
}

func NewHandler(adminKey []byte, source model.SnapshotSource, selectableGroups []string) *Handler {
	selectable := make(map[string]bool)
	order := make([]string, 0, len(selectableGroups))
	for _, name := range selectableGroups {
		if name = strings.TrimSpace(name); name != "" {
			if !selectable[name] {
				order = append(order, name)
			}
			selectable[name] = true
		}
	}
	return &Handler{
		adminKey: append([]byte(nil), adminKey...), source: source, selectable: selectable,
		order: order, sessions: make(map[string]time.Time),
	}
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	switch request.URL.Path {
	case "/mihomo-monitor/dashboard":
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
		_, _ = writer.Write(ui.DashboardHTML)
	case "/mihomo-monitor/snapshot":
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.snapshot(writer, request)
	case "/mihomo-monitor/session":
		switch request.Method {
		case http.MethodPost:
			h.createSession(writer, request)
		case http.MethodDelete:
			h.deleteSession(writer, request)
		default:
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "/mihomo-monitor/select":
		if request.Method != http.MethodPut {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.selectGroup(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (h *Handler) snapshot(writer http.ResponseWriter, request *http.Request) {
	if !h.authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	snapshot, err := h.load(request.Context())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// The viewer went away. There is nothing to report and nothing
			// that may be cached on their behalf.
			return
		}
		http.Error(writer, "mihomo snapshot unavailable", http.StatusBadGateway)
		return
	}
	age := time.Since(snapshot.ObservedAt).Milliseconds()
	if age < 0 {
		age = 0
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(struct {
		model.Snapshot
		AgeMS int64 `json:"age_ms"`
	}{Snapshot: snapshot, AgeMS: age})
}

// load returns a snapshot that is at most cacheDuration old. Concurrent viewers
// share one collection; each viewer can still leave on its own context.
func (h *Handler) load(ctx context.Context) (model.Snapshot, error) {
	var (
		snapshot model.Snapshot
		err      error
	)
	for attempt := 0; attempt < 2; attempt++ {
		h.mu.Lock()
		if time.Now().Before(h.until) {
			cached := h.cached
			h.mu.Unlock()
			return cached, nil
		}
		pending := h.inflight
		if pending == nil {
			pending = &collection{done: make(chan struct{})}
			h.inflight = pending
			go h.collect(pending, h.generation)
		}
		h.mu.Unlock()

		select {
		case <-pending.done:
			snapshot, err = pending.snapshot, pending.err
			if !pending.stale {
				return snapshot, err
			}
		case <-ctx.Done():
			return model.Snapshot{}, ctx.Err()
		}
	}
	return snapshot, err
}

func (h *Handler) collect(pending *collection, generation int) {
	// Closing the channel is what wakes the waiters, so it happens even if the
	// source misbehaves. The handler never holds the lock while collecting.
	defer func() {
		h.mu.Lock()
		if h.inflight == pending {
			h.inflight = nil
		}
		h.mu.Unlock()
		close(pending.done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	defer cancel()
	snapshot, err := h.source.Snapshot(ctx)
	if err == nil {
		primary := ""
		for _, name := range h.order {
			if _, ok := h.selectable[name]; ok {
				primary = name
				break
			}
		}
		for i := range snapshot.Groups {
			group := &snapshot.Groups[i]
			group.Selectable = h.selectable[group.Name] && strings.EqualFold(group.Type, "selector") && len(group.All) > 1
			group.Primary = group.Selectable && group.Name == primary
		}
	}
	h.mu.Lock()
	pending.snapshot, pending.err = snapshot, err
	switch {
	case err != nil:
	case h.generation != generation:
		// A switch landed while this read was in flight, so the result no
		// longer describes the current exit. Report it, do not cache it.
		pending.stale = true
	default:
		h.cached = snapshot
		h.until = time.Now().Add(cacheDuration)
	}
	h.mu.Unlock()
}

// invalidate drops the shared cache and marks any read that is already running
// as stale. It runs before and after a switch, because a request that changes
// state can change the answer even when its own response never comes back.
func (h *Handler) invalidate() {
	h.mu.Lock()
	h.generation++
	h.until = time.Time{}
	h.mu.Unlock()
}

func (h *Handler) authorized(request *http.Request) bool {
	const prefix = "Bearer "
	authorization := request.Header.Get("Authorization")
	if strings.HasPrefix(authorization, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authorization, prefix)), h.adminKey) == 1 {
		return true
	}
	cookie, err := request.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := time.Now()
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	expires, ok := h.sessions[cookie.Value]
	if !ok || !now.Before(expires) {
		delete(h.sessions, cookie.Value)
		return false
	}
	return true
}

func (h *Handler) createSession(writer http.ResponseWriter, request *http.Request) {
	if !strings.HasPrefix(request.Header.Get("Content-Type"), "application/json") {
		http.Error(writer, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	var input struct {
		Key string `json:"key"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Key == "" {
		http.Error(writer, "invalid login", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(input.Key), h.adminKey) != 1 {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		http.Error(writer, "session unavailable", http.StatusInternalServerError)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	expires := time.Now().Add(sessionDuration)
	h.sessionMu.Lock()
	for value, deadline := range h.sessions {
		if !time.Now().Before(deadline) {
			delete(h.sessions, value)
		}
	}
	h.sessions[token] = expires
	h.sessionMu.Unlock()
	http.SetCookie(writer, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/mihomo-monitor",
		Expires: expires, MaxAge: int(sessionDuration.Seconds()), HttpOnly: true,
		Secure: true, SameSite: http.SameSiteStrictMode,
	})
	writer.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteSession(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Mihomo-Monitor-Request") != "1" {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	if cookie, err := request.Cookie(sessionCookie); err == nil {
		h.sessionMu.Lock()
		delete(h.sessions, cookie.Value)
		h.sessionMu.Unlock()
	}
	http.SetCookie(writer, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/mihomo-monitor", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	writer.WriteHeader(http.StatusNoContent)
}

func (h *Handler) selectGroup(writer http.ResponseWriter, request *http.Request) {
	if !h.authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	if request.Header.Get("X-Mihomo-Monitor-Request") != "1" {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	if request.Header.Get("Content-Type") != "application/json" {
		http.Error(writer, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	var input struct {
		Group string `json:"group"`
		Name  string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Group == "" || input.Name == "" {
		http.Error(writer, "invalid selection", http.StatusBadRequest)
		return
	}
	if !h.selectable[input.Group] {
		http.Error(writer, "group is read-only", http.StatusForbidden)
		return
	}
	selector, ok := h.source.(GroupSelector)
	if !ok {
		http.Error(writer, "selection unavailable", http.StatusServiceUnavailable)
		return
	}
	h.invalidate()
	ctx, cancel := context.WithTimeout(request.Context(), snapshotTimeout)
	defer cancel()
	err := selector.SelectGroup(ctx, input.Group, input.Name)
	h.invalidate()
	writer.Header().Set("Content-Type", "application/json")
	if err != nil {
		writer.WriteHeader(selectFailureStatus(err))
		_ = json.NewEncoder(writer).Encode(selectFailure(err, input.Group, input.Name))
		return
	}
	_ = json.NewEncoder(writer).Encode(selectResponse{Status: "applied", Group: input.Group, Name: input.Name})
}

// selectFailureStatus separates "the controller refused this" from "we do not
// know what happened". Only a refusal may be presented as a failure.
func selectFailureStatus(err error) int {
	if errors.Is(err, mihomo.ErrNotSelectable) || errors.Is(err, mihomo.ErrInvalidChoice) {
		return http.StatusConflict
	}
	if status, ok := mihomo.ControllerStatus(err); ok && status >= 400 && status < 500 && status != http.StatusRequestTimeout &&
		status != http.StatusTooManyRequests {
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

func selectFailure(err error, group, name string) selectResponse {
	response := selectResponse{Group: group, Name: name}
	switch {
	case errors.Is(err, mihomo.ErrNotSelectable):
		response.Status, response.Reason = "rejected", "group-not-selectable"
	case errors.Is(err, mihomo.ErrInvalidChoice):
		response.Status, response.Reason = "rejected", "choice-missing"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		response.Status, response.Reason = "uncertain", "timeout"
	default:
		if status, ok := mihomo.ControllerStatus(err); ok && status >= 400 && status < 500 &&
			status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			response.Status, response.Reason = "rejected", "controller-rejected"
		} else {
			response.Status, response.Reason = "uncertain", "no-response"
		}
	}
	return response
}
