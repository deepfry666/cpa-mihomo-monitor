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
	snapshotTimeout = 4 * time.Second
	cacheDuration   = 10 * time.Second
	sessionDuration = 12 * time.Hour
	sessionCookie   = "mihomo_monitor_session"
)

type Handler struct {
	adminKey   []byte
	source     model.SnapshotSource
	selectable map[string]bool
	mu         sync.Mutex
	cached     model.Snapshot
	until      time.Time
	sessionMu  sync.Mutex
	sessions   map[string]time.Time
}

type GroupSelector interface {
	SelectGroup(context.Context, string, string) error
}

func NewHandler(adminKey []byte, source model.SnapshotSource, selectableGroups []string) *Handler {
	selectable := make(map[string]bool)
	for _, name := range selectableGroups {
		if name = strings.TrimSpace(name); name != "" {
			selectable[name] = true
		}
	}
	return &Handler{
		adminKey: append([]byte(nil), adminKey...), source: source, selectable: selectable,
		sessions: make(map[string]time.Time),
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if !time.Now().Before(h.until) {
		ctx, cancel := context.WithTimeout(request.Context(), snapshotTimeout)
		defer cancel()
		snapshot, err := h.source.Snapshot(ctx)
		if err != nil {
			http.Error(writer, "mihomo snapshot unavailable", http.StatusBadGateway)
			return
		}
		for i := range snapshot.Groups {
			group := &snapshot.Groups[i]
			group.Selectable = h.selectable[group.Name] && strings.EqualFold(group.Type, "selector") && len(group.All) > 1
		}
		h.cached = snapshot
		h.until = time.Now().Add(cacheDuration)
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(h.cached)
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
	h.mu.Lock()
	defer h.mu.Unlock()
	ctx, cancel := context.WithTimeout(request.Context(), snapshotTimeout)
	defer cancel()
	if err := selector.SelectGroup(ctx, input.Group, input.Name); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, mihomo.ErrNotSelectable) || errors.Is(err, mihomo.ErrInvalidChoice) {
			status = http.StatusConflict
		}
		http.Error(writer, "selection failed; refresh the group status", status)
		return
	}
	h.until = time.Time{}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{"group": input.Group, "name": input.Name})
}
