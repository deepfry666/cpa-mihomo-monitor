package mihomo_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cpa-mihomo-monitor/internal/mihomo"
)

func TestCollectorBuildsSnapshotFromReadOnlyMihomoEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.Header.Get("Authorization"), "Bearer topsecret"; got != want {
			t.Errorf("authorization = %q, want %q", got, want)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"meta":true,"premium":false,"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{
				"AI代理7891":{"name":"AI代理7891","type":"Selector","now":"SG","all":["SG","JP"]},
				"SG":{"name":"SG","type":"Selector","now":"node-sg","all":["node-sg"]},
				"node-sg":{"name":"node-sg","type":"Shadowsocks","alive":true,"history":[{"time":"2026-09-17T12:00:00Z","delay":83}]}
			}}`)
		case "/providers/proxies":
			fmt.Fprint(writer, `{"providers":{"us":{"name":"us","type":"Proxy","vehicleType":"HTTP","updatedAt":"2026-09-17T11:59:00Z","proxies":[{"name":"node-sg"}]}}}`)
		case "/connections":
			fmt.Fprint(writer, `{"downloadTotal":2048,"uploadTotal":1024,"connections":[{"id":"one","start":"2026-09-17T12:00:00Z","upload":512,"download":1024,"chains":["DIRECT","直连7892"],"metadata":{"host":"api.example.net","sourceIP":"172.18.0.3","sourcePort":"56630","destinationIP":"203.0.113.10","destinationPort":"443","network":"tcp","processPath":"/sensitive/path"}},{"id":"two","start":"2026-09-17T11:00:00Z"}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	collector, err := mihomo.NewCollector(server.URL, "topsecret", server.Client())
	if err != nil {
		t.Fatalf("create collector: %v", err)
	}
	snapshot, err := collector.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("collect snapshot: %v", err)
	}

	if snapshot.Status != "ok" || !snapshot.Controller.Reachable {
		t.Fatalf("status/controller = %q/%#v", snapshot.Status, snapshot.Controller)
	}
	if snapshot.Controller.Version != "1.19.30" {
		t.Fatalf("version = %q", snapshot.Controller.Version)
	}
	if snapshot.Traffic.UploadTotal != 1024 || snapshot.Traffic.DownloadTotal != 2048 || snapshot.Traffic.Connections != 2 {
		t.Fatalf("traffic = %#v", snapshot.Traffic)
	}
	if got := snapshot.Connections[0]; got.ID != "one" || got.Host != "api.example.net" || got.Source != "172.18.0.3:56630" || got.Destination != "203.0.113.10:443" || len(got.Chains) != 2 || got.Chains[0] != "直连7892" || got.Chains[1] != "DIRECT" {
		t.Fatalf("connection detail = %#v", got)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(encoded), "/sensitive/path") {
		t.Fatalf("unexpected raw connection metadata in snapshot: %v", err)
	}
	if len(snapshot.Groups) != 2 {
		t.Fatalf("groups = %#v", snapshot.Groups)
	}
	if snapshot.Groups[0].Name != "AI代理7891" || snapshot.Groups[0].Now != "SG" {
		t.Fatalf("first group = %#v", snapshot.Groups[0])
	}
	if snapshot.Groups[1].Name != "SG" || snapshot.Groups[1].Now != "node-sg" || !snapshot.Groups[1].Alive || snapshot.Groups[1].DelayMS != 83 {
		t.Fatalf("selected-node health = %#v", snapshot.Groups[1])
	}
	if len(snapshot.Providers) != 1 || snapshot.Providers[0].Name != "us" || snapshot.Providers[0].ProxyCount != 1 {
		t.Fatalf("providers = %#v", snapshot.Providers)
	}
}

func TestCollectorReportsAuthorizationFailureWithoutExposingSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(writer, `{"error":"topsecret"}`)
	}))
	defer server.Close()
	collector, err := mihomo.NewCollector(server.URL, "topsecret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := collector.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("collector must return a degraded snapshot: %v", err)
	}
	if snapshot.Status != "offline" || snapshot.Controller.Reachable || len(snapshot.Issues) != 1 || snapshot.Issues[0] != "Controller 鉴权失败" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if strings.Contains(fmt.Sprint(snapshot), "topsecret") {
		t.Fatal("controller secret leaked")
	}
}

func TestCollectorReportsTimeoutWithoutBlockingPastDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	collector, err := mihomo.NewCollector(server.URL, "topsecret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	snapshot, err := collector.Snapshot(ctx)
	if err != nil {
		t.Fatalf("collector must return a degraded snapshot: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("collector ignored caller deadline")
	}
	if snapshot.Status != "offline" || len(snapshot.Issues) != 1 || snapshot.Issues[0] != "Controller 连接超时" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestCollectorKeepsAvailableDataWhenOneEndpointFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{"GLOBAL":{"name":"GLOBAL","type":"Selector","now":"DIRECT","alive":true,"all":["DIRECT"]}}}`)
		case "/providers/proxies":
			writer.WriteHeader(http.StatusInternalServerError)
		case "/connections":
			fmt.Fprint(writer, `{"downloadTotal":200,"uploadTotal":100,"connections":[]}`)
		}
	}))
	defer server.Close()
	collector, err := mihomo.NewCollector(server.URL, "topsecret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := collector.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "degraded" || len(snapshot.Groups) != 1 || snapshot.Traffic.DownloadTotal != 200 || len(snapshot.Issues) != 1 || snapshot.Issues[0] != "Provider 读取失败" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestCollectorCapsConnectionDetailsWithoutChangingTotalCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{}}`)
		case "/providers/proxies":
			fmt.Fprint(writer, `{"providers":{}}`)
		case "/connections":
			fmt.Fprint(writer, `{"connections":[`)
			for i := 0; i < 205; i++ {
				if i > 0 {
					fmt.Fprint(writer, ",")
				}
				fmt.Fprintf(writer, `{"id":"connection-%d"}`, i)
			}
			fmt.Fprint(writer, `]}`)
		}
	}))
	defer server.Close()
	collector, err := mihomo.NewCollector(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := collector.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Traffic.Connections != 205 || len(snapshot.Connections) != 200 {
		t.Fatalf("connection counts = %d / %d", snapshot.Traffic.Connections, len(snapshot.Connections))
	}
}

func TestSelectGroupValidatesLiveChoicesBeforeWriting(t *testing.T) {
	current := "DIRECT"
	putCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer topsecret" {
			t.Error("controller authorization missing")
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/proxies":
			fmt.Fprintf(writer, `{"proxies":{"GLOBAL":{"type":"Selector","now":%q,"all":["DIRECT","node"]},"AI代理7891":{"type":"Selector","now":"node","all":["node"]},"auto":{"type":"URLTest","all":["DIRECT","node"]}}}`, current)
		case request.Method == http.MethodPut && request.URL.Path == "/proxies/GLOBAL":
			putCount++
			var input struct {
				Name string `json:"name"`
			}
			if request.Header.Get("Content-Type") != "application/json" || json.NewDecoder(request.Body).Decode(&input) != nil {
				t.Error("invalid controller write")
			}
			current = input.Name
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected controller request: %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	collector, err := mihomo.NewCollector(server.URL, "topsecret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		group, name string
		want        error
	}{
		{"auto", "node", mihomo.ErrNotSelectable},
		{"missing", "node", mihomo.ErrNotSelectable},
		{"GLOBAL", "expired", mihomo.ErrInvalidChoice},
	} {
		if err := collector.SelectGroup(context.Background(), tc.group, tc.name); !errors.Is(err, tc.want) {
			t.Fatalf("%s/%s: %v, want %v", tc.group, tc.name, err, tc.want)
		}
	}
	if putCount != 0 {
		t.Fatalf("invalid selections wrote %d times", putCount)
	}
	if err := collector.SelectGroup(context.Background(), "GLOBAL", "node"); err != nil {
		t.Fatal(err)
	}
	if current != "node" || putCount != 1 {
		t.Fatalf("selection = %s, writes = %d", current, putCount)
	}
	if err := collector.SelectGroup(context.Background(), "GLOBAL", "node"); err != nil || putCount != 1 {
		t.Fatalf("no-op selection wrote again: %v / %d", err, putCount)
	}
}
