package sidecar_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-mihomo-monitor/internal/mihomo"
	"cpa-mihomo-monitor/internal/model"
	"cpa-mihomo-monitor/internal/sidecar"
)

type sourceFunc func(context.Context) (model.Snapshot, error)

func (f sourceFunc) Snapshot(ctx context.Context) (model.Snapshot, error) { return f(ctx) }

type selectorSource struct {
	snapshot    func() model.Snapshot
	selectGroup func(string, string) error
}

func (s selectorSource) Snapshot(context.Context) (model.Snapshot, error) { return s.snapshot(), nil }
func (s selectorSource) SelectGroup(_ context.Context, group, name string) error {
	return s.selectGroup(group, name)
}

func TestHandlerServesPageButRequiresMonitorKeyForSnapshot(t *testing.T) {
	handler := sidecar.NewHandler([]byte("monitor-admin-key"), sourceFunc(func(context.Context) (model.Snapshot, error) {
		return model.Snapshot{Status: "ok", Controller: model.ControllerSnapshot{Reachable: true}}, nil
	}), nil)

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/mihomo-monitor/dashboard", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-page="mihomo-monitor"`) {
		t.Fatalf("dashboard = %d %q", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), `type !== 'mihomo-monitor-auth'`) ||
		strings.Contains(page.Body.String(), "localStorage.getItem") ||
		strings.Contains(page.Body.String(), "/v0/management/plugins/") {
		t.Fatal("dashboard must accept same-origin auth messages without reading CPA storage or calling CPA")
	}

	for _, key := range []string{"", "wrong-key"} {
		request := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil)
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("key=%q status = %d, want 401", key, response.Code)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil)
	request.Header.Set("Authorization", "Bearer monitor-admin-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot status/headers = %d %#v", response.Code, response.Header())
	}
	var snapshot model.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.Status != "ok" {
		t.Fatalf("snapshot = %#v, err=%v", snapshot, err)
	}
}

func TestCanceledViewerCannotPoisonTheSharedCache(t *testing.T) {
	var calls atomic.Int32
	var sawCanceled atomic.Bool
	handler := sidecar.NewHandler([]byte("monitor-admin-key"), sourceFunc(func(ctx context.Context) (model.Snapshot, error) {
		calls.Add(1)
		select {
		case <-ctx.Done():
			// The real collector turns a canceled read into an offline
			// snapshot with no error, which is exactly what used to be cached
			// and served to everyone else.
			sawCanceled.Store(true)
			return model.Snapshot{Status: "offline", Issues: []string{"Controller 无法连接"}}, nil
		case <-time.After(120 * time.Millisecond):
			return model.Snapshot{Status: "ok", Controller: model.ControllerSnapshot{Reachable: true}}, nil
		}
	}), nil)

	leaving, cancel := context.WithCancel(context.Background())
	abandoned := httptest.NewRecorder()
	first := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil).WithContext(leaving)
	first.Header.Set("Authorization", "Bearer monitor-admin-key")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.ServeHTTP(abandoned, first)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-finished
	if body := abandoned.Body.String(); body != "" {
		t.Fatalf("a canceled viewer must not be handed a snapshot, got %q", body)
	}

	second := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil)
	second.Header.Set("Authorization", "Bearer monitor-admin-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, second)
	var snapshot model.Snapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil {
		t.Fatalf("snapshot = %d %s", response.Code, response.Body.String())
	}
	if snapshot.Status != "ok" {
		t.Fatalf("second viewer saw %q, want ok", snapshot.Status)
	}
	if sawCanceled.Load() {
		t.Fatal("the viewer's cancellation reached the shared collection")
	}
	if calls.Load() != 1 {
		t.Fatalf("collections = %d, want one shared read", calls.Load())
	}
}

func TestSwitchDropsTheCacheEvenWhenTheResponseNeverArrives(t *testing.T) {
	current := "node-A"
	handler := sidecar.NewHandler([]byte("monitor-admin-key"), selectorSource{
		snapshot: func() model.Snapshot {
			return model.Snapshot{Status: "ok", Groups: []model.GroupSnapshot{
				{Name: "AI代理7891", Type: "Selector", Now: current, All: []string{"node-A", "node-B"}},
			}}
		},
		selectGroup: func(_, name string) error {
			// The controller changed state, then the reply was lost.
			current = name
			time.Sleep(80 * time.Millisecond)
			return context.DeadlineExceeded
		},
	}, []string{"AI代理7891"})

	get := func() model.Snapshot {
		request := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil)
		request.Header.Set("Authorization", "Bearer monitor-admin-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var snapshot model.Snapshot
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil {
			t.Fatalf("snapshot = %d %s", response.Code, response.Body.String())
		}
		return snapshot
	}
	if get().Groups[0].Now != "node-A" {
		t.Fatal("baseline exit was not cached")
	}

	request := httptest.NewRequest(http.MethodPut, "/mihomo-monitor/select", strings.NewReader(`{"group":"AI代理7891","name":"node-B"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Mihomo-Monitor-Request", "1")
	request.Header.Set("Authorization", "Bearer monitor-admin-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("switch status = %d, want 502", response.Code)
	}
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status != "uncertain" {
		t.Fatalf("switch body = %s", response.Body.String())
	}
	if got := get().Groups[0].Now; got != "node-B" {
		t.Fatalf("cached exit = %q, want node-B: the cache must drop before the write", got)
	}
}

func TestSlowCollectionDoesNotTrapAViewerThatLeaves(t *testing.T) {
	release := make(chan struct{})
	var wait sync.WaitGroup
	handler := sidecar.NewHandler([]byte("monitor-admin-key"), sourceFunc(func(ctx context.Context) (model.Snapshot, error) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		return model.Snapshot{Status: "ok"}, nil
	}), nil)
	defer func() {
		close(release)
		wait.Wait()
	}()

	busy := httptest.NewRecorder()
	first := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil)
	first.Header.Set("Authorization", "Bearer monitor-admin-key")
	wait.Add(1)
	go func() {
		defer wait.Done()
		handler.ServeHTTP(busy, first)
	}()
	time.Sleep(30 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	waiting := httptest.NewRecorder()
	second := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil).WithContext(ctx)
	second.Header.Set("Authorization", "Bearer monitor-admin-key")
	start := time.Now()
	handler.ServeHTTP(waiting, second)
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("waiting viewer was held for %v by a slow collection", elapsed)
	}
	if waiting.Body.Len() != 0 {
		t.Fatalf("canceled viewer body = %q", waiting.Body.String())
	}
}

func TestHandlerCoalescesConcurrentRefreshesAndCachesBriefly(t *testing.T) {
	var calls atomic.Int32
	handler := sidecar.NewHandler([]byte("monitor-admin-key"), sourceFunc(func(context.Context) (model.Snapshot, error) {
		calls.Add(1)
		time.Sleep(40 * time.Millisecond)
		return model.Snapshot{Status: "ok"}, nil
	}), nil)
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil)
			request.Header.Set("Authorization", "Bearer monitor-admin-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Errorf("status = %d", response.Code)
			}
		}()
	}
	wait.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("Mihomo collections = %d, want 1", got)
	}
}

func TestSelectionRequiresKeyAndExplicitAllowlistAndInvalidatesCache(t *testing.T) {
	current := "DIRECT"
	calls := 0
	handler := sidecar.NewHandler([]byte("monitor-admin-key"), selectorSource{
		snapshot: func() model.Snapshot {
			return model.Snapshot{Status: "ok", Groups: []model.GroupSnapshot{
				{Name: "AI代理7891", Type: "Selector", Now: "node", All: []string{"node", "other"}},
				{Name: "GLOBAL", Type: "Selector", Now: current, All: []string{"DIRECT", "node"}},
				{Name: "auto", Type: "URLTest", All: []string{"DIRECT", "node"}},
			}}
		},
		selectGroup: func(group, name string) error {
			calls++
			if group == "auto" {
				return mihomo.ErrNotSelectable
			}
			current = name
			return nil
		},
	}, []string{"GLOBAL", "auto"})
	get := func() model.Snapshot {
		request := httptest.NewRequest(http.MethodGet, "/mihomo-monitor/snapshot", nil)
		request.Header.Set("Authorization", "Bearer monitor-admin-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var snapshot model.Snapshot
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil {
			t.Fatalf("snapshot = %d %s", response.Code, response.Body.String())
		}
		return snapshot
	}
	selectRequest := func(key, group, name string) int {
		request := httptest.NewRequest(http.MethodPut, "/mihomo-monitor/select", strings.NewReader(fmt.Sprintf(`{"group":%q,"name":%q}`, group, name)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Mihomo-Monitor-Request", "1")
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	initial := get()
	if initial.Groups[0].Selectable || !initial.Groups[1].Selectable || initial.Groups[2].Selectable {
		t.Fatalf("selectable flags = %#v", initial.Groups)
	}
	if got := selectRequest("", "GLOBAL", "node"); got != 401 {
		t.Fatalf("missing key = %d", got)
	}
	if got := selectRequest("wrong", "GLOBAL", "node"); got != 401 {
		t.Fatalf("wrong key = %d", got)
	}
	if got := selectRequest("monitor-admin-key", "AI代理7891", "other"); got != 403 {
		t.Fatalf("protected group = %d", got)
	}
	if got := selectRequest("monitor-admin-key", "auto", "node"); got != 409 {
		t.Fatalf("auto group = %d", got)
	}
	if got := selectRequest("monitor-admin-key", "GLOBAL", "node"); got != 200 {
		t.Fatalf("selection = %d", got)
	}
	if calls != 2 || get().Groups[1].Now != "node" {
		t.Fatalf("cache not invalidated, calls = %d", calls)
	}
}
