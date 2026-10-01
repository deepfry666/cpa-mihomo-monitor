package mihomo_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-mihomo-monitor/internal/mihomo"
	"cpa-mihomo-monitor/internal/model"
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
	if snapshot.Groups[0].Name != "AI代理7891" || snapshot.Groups[0].Now != "SG" || snapshot.Groups[0].Health != "ok" ||
		snapshot.Groups[0].Exit == nil || snapshot.Groups[0].Exit.Name != "node-sg" || snapshot.Groups[0].Exit.DelayMS != 83 {
		t.Fatalf("first group = %#v", snapshot.Groups[0])
	}
	if len(snapshot.Groups[0].Chain) != 2 || snapshot.Groups[0].Chain[0] != "SG" || snapshot.Groups[0].Chain[1] != "node-sg" {
		t.Fatalf("nested chain = %#v", snapshot.Groups[0].Chain)
	}
	if snapshot.Groups[1].Name != "SG" || snapshot.Groups[1].Now != "node-sg" || snapshot.Groups[1].Health != "ok" ||
		snapshot.Groups[1].Exit == nil || snapshot.Groups[1].Exit.DelayMS != 83 {
		t.Fatalf("selected-node health = %#v", snapshot.Groups[1])
	}
	if checked := snapshot.Groups[1].Exit.CheckedAt; checked == nil || checked.Format(time.RFC3339) != "2026-09-17T12:00:00Z" {
		t.Fatalf("checked_at = %v", snapshot.Groups[1].Exit.CheckedAt)
	}
	if len(snapshot.Providers) != 1 || snapshot.Providers[0].Name != "us" || snapshot.Providers[0].ProxyCount != 1 ||
		snapshot.Providers[0].UnknownCount != 1 || len(snapshot.Providers[0].Nodes) != 1 {
		t.Fatalf("providers = %#v", snapshot.Providers)
	}
}

func TestCollectorResolvesProviderBackedExitHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{
				"AI代理7891":{"name":"AI代理7891","type":"Selector","now":"SG-01","alive":true,"all":["SG-01","SG-02","SG-03"]},
				"GLOBAL":{"name":"GLOBAL","type":"Selector","now":"DIRECT","alive":true,"all":["DIRECT"]}
			}}`)
		case "/providers/proxies":
			fmt.Fprint(writer, `{"providers":{"out":{"name":"out","type":"Proxy","vehicleType":"HTTP","proxies":[
				{"name":"SG-01","type":"Shadowsocks","alive":true,"history":[{"time":"2026-09-17T12:00:00Z","delay":35}]},
				{"name":"SG-02","type":"Shadowsocks","alive":false},
				{"name":"SG-03","type":"Shadowsocks"}
			]}}}`)
		case "/connections":
			fmt.Fprint(writer, `{"downloadTotal":0,"uploadTotal":0,"connections":[]}`)
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
	var ai *model.GroupSnapshot
	for i := range snapshot.Groups {
		if snapshot.Groups[i].Name == "AI代理7891" {
			ai = &snapshot.Groups[i]
		}
	}
	if ai == nil {
		t.Fatalf("group missing: %#v", snapshot.Groups)
	}
	if ai.Health != model.HealthOK || ai.Exit == nil || ai.Exit.Name != "SG-01" || ai.Exit.DelayMS != 35 {
		t.Fatalf("provider-backed exit = %#v", ai)
	}
	if len(ai.Choices) != 3 {
		t.Fatalf("choices = %#v", ai.Choices)
	}
	want := map[string]model.HealthState{"SG-01": model.HealthOK, "SG-02": model.HealthDown, "SG-03": model.HealthUnknown}
	for _, choice := range ai.Choices {
		if got := want[choice.Name]; choice.Health != got {
			t.Fatalf("choice %s health = %q, want %q", choice.Name, choice.Health, got)
		}
	}
}

func TestCollectorDoesNotTrustTheGroupFlagForUntestedNodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{"AI代理7891":{"name":"AI代理7891","type":"Selector","now":"SG-01","alive":true,"all":["SG-01"]}}}`)
		case "/providers/proxies":
			fmt.Fprint(writer, `{"providers":{"out":{"name":"out","proxies":[{"name":"SG-01"}]}}}`)
		case "/connections":
			fmt.Fprint(writer, `{"downloadTotal":0,"uploadTotal":0,"connections":[]}`)
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
	if snapshot.Status != "ok" || len(snapshot.Groups) != 1 || snapshot.Groups[0].Health != model.HealthUnknown {
		t.Fatalf("untested node must stay unknown: %#v", snapshot)
	}
}

func TestCollectorCutsGroupCyclesAndMissingLeaves(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{
				"A":{"name":"A","type":"Selector","now":"B","all":["B"]},
				"B":{"name":"B","type":"Selector","now":"A","all":["A"]},
				"C":{"name":"C","type":"Selector","now":"GONE","all":["GONE"]}
			}}`)
		case "/providers/proxies":
			fmt.Fprint(writer, `{"providers":{}}`)
		case "/connections":
			fmt.Fprint(writer, `{"downloadTotal":0,"uploadTotal":0,"connections":[]}`)
		}
	}))
	defer server.Close()
	collector, err := mihomo.NewCollector(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan model.Snapshot, 1)
	go func() {
		snapshot, err := collector.Snapshot(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- snapshot
	}()
	var snapshot model.Snapshot
	select {
	case snapshot = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cycle in group selection did not terminate")
	}
	for _, group := range snapshot.Groups {
		if group.Health != model.HealthUnknown {
			t.Fatalf("group %s health = %q, want unknown", group.Name, group.Health)
		}
	}
}

func TestCollectorKeepsTotalsWhenConnectionDetailsAreHuge(t *testing.T) {
	payload := strings.Repeat("x", 512)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{}}`)
		case "/providers/proxies":
			fmt.Fprint(writer, `{"providers":{}}`)
		case "/connections":
			fmt.Fprint(writer, `{"downloadTotal":900000,"uploadTotal":800000,"connections":[`)
			for i := 0; i < 9000; i++ {
				if i > 0 {
					fmt.Fprint(writer, ",")
				}
				fmt.Fprintf(writer, `{"id":"c-%d","start":"2026-09-17T12:00:00Z","metadata":{"host":"%s"}}`, i, payload)
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
	if snapshot.Traffic.DownloadTotal != 900000 || snapshot.Traffic.UploadTotal != 800000 || snapshot.Traffic.Connections != 9000 {
		t.Fatalf("totals were dropped: %#v", snapshot.Traffic)
	}
	if len(snapshot.Connections) != 200 {
		t.Fatalf("visible details = %d, want 200", len(snapshot.Connections))
	}
	if !snapshot.ConnectionsTruncated {
		t.Fatal("truncation must be reported, not implied")
	}
}

func TestCollectorReportsACutConnectionStreamInsteadOfPartialCounts(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{}}`)
		case "/providers/proxies":
			fmt.Fprint(writer, `{"providers":{}}`)
		case "/connections":
			reads.Add(1)
			// A body that stops in the middle of the array: the totals arrive,
			// the array does not, so the connection count is unknowable.
			fmt.Fprint(writer, `{"downloadTotal":2048,"uploadTotal":1024,"connections":[{"id":"one","start":"2026-09-17T12:00:00Z"}`)
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
	if reads.Load() != 2 {
		t.Fatalf("connection reads = %d, want one retry", reads.Load())
	}
	if !contains(snapshot.Issues, "连接统计读取失败") {
		t.Fatalf("cut stream must be reported as a failure: %#v", snapshot)
	}
	if snapshot.Traffic.Connections != 0 || len(snapshot.Connections) != 0 || snapshot.Traffic.DownloadTotal != 0 {
		t.Fatalf("half-read stream must not publish counts: %#v", snapshot)
	}
	if snapshot.Status != "degraded" {
		t.Fatalf("status = %q", snapshot.Status)
	}
}

func TestCollectorRecoversWhenTheFirstConnectionReadIsCut(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			fmt.Fprint(writer, `{"version":"1.19.30"}`)
		case "/proxies":
			fmt.Fprint(writer, `{"proxies":{}}`)
		case "/providers/proxies":
			fmt.Fprint(writer, `{"providers":{}}`)
		case "/connections":
			if reads.Add(1) == 1 {
				fmt.Fprint(writer, `{"downloadTotal":2048,"uploadTotal":1024,"connections":[{"id":"one"}`)
				return
			}
			fmt.Fprint(writer, `{"downloadTotal":2048,"uploadTotal":1024,"connections":[{"id":"one","start":"2026-09-17T12:00:00Z"},{"id":"two","start":"2026-09-17T11:00:00Z"}]}`)
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
	if len(snapshot.Issues) != 0 || snapshot.Status != "ok" {
		t.Fatalf("a retried read must be clean: %#v", snapshot.Issues)
	}
	if snapshot.Traffic.Connections != 2 || len(snapshot.Connections) != 2 {
		t.Fatalf("recovered counts = %#v", snapshot.Traffic)
	}
}

// Mihomo reports "connections": null while nothing is active, which is a normal
// idle state and not a malformed document.
func TestCollectorTreatsAnIdleConnectionListAsEmpty(t *testing.T) {
	for name, body := range map[string]string{
		"null":  `{"downloadTotal":0,"uploadTotal":0,"connections":null,"memory":0}`,
		"empty": `{"downloadTotal":0,"uploadTotal":0,"connections":[],"memory":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/version":
					fmt.Fprint(writer, `{"version":"1.19.30"}`)
				case "/proxies":
					fmt.Fprint(writer, `{"proxies":{}}`)
				case "/providers/proxies":
					fmt.Fprint(writer, `{"providers":{}}`)
				case "/connections":
					fmt.Fprint(writer, body)
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
			if len(snapshot.Issues) != 0 || snapshot.Status != "ok" {
				t.Fatalf("idle controller must be clean: issues=%#v status=%q", snapshot.Issues, snapshot.Status)
			}
			if snapshot.Traffic.Connections != 0 || len(snapshot.Connections) != 0 || snapshot.ConnectionsTruncated {
				t.Fatalf("idle traffic = %#v details=%d truncated=%v", snapshot.Traffic, len(snapshot.Connections), snapshot.ConnectionsTruncated)
			}
		})
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// The controller omits or nulls fields whenever a feature is off, a provider is
// empty, or a connection has no metadata yet. None of that may crash the
// collector or invent a health verdict.
func TestCollectorSurvivesDegenerateControllerPayloads(t *testing.T) {
	longName := strings.Repeat("节点", 400)
	cases := []struct {
		name        string
		proxies     string
		providers   string
		connections string
	}{
		{
			name:        "null maps",
			proxies:     `{"proxies":null}`,
			providers:   `{"providers":null}`,
			connections: `{"downloadTotal":0,"uploadTotal":0,"connections":null,"memory":0}`,
		},
		{
			name:        "null node fields",
			proxies:     `{"proxies":{"A":{"name":"A","type":"Selector","now":"B","all":null,"history":null,"alive":null}}}`,
			providers:   `{"providers":{}}`,
			connections: `{"connections":[]}`,
		},
		{
			name:        "empty provider",
			proxies:     `{"proxies":{}}`,
			providers:   `{"providers":{"p":{"name":"p","proxies":null}}}`,
			connections: `{"connections":[]}`,
		},
		{
			name:        "bare connection entry",
			proxies:     `{"proxies":{}}`,
			providers:   `{"providers":{}}`,
			connections: `{"downloadTotal":null,"uploadTotal":null,"connections":[{},{}]}`,
		},
		{
			name:        "unparsable history time",
			proxies:     `{"proxies":{"G":{"name":"G","type":"Selector","now":"N","all":["N"]},"N":{"name":"N","type":"Vless","history":[{"time":"not-a-time","delay":42}]}}}`,
			providers:   `{"providers":{}}`,
			connections: `{"connections":[]}`,
		},
		{
			name:        "very long names",
			proxies:     `{"proxies":{"G":{"name":"` + longName + `","type":"Selector","now":"` + longName + `","all":["` + longName + `"]},"` + longName + `":{"name":"` + longName + `","type":"Vless","alive":true}}}`,
			providers:   `{"providers":{"p":{"name":"p","proxies":[{"name":"` + longName + `","alive":true}]}}}`,
			connections: `{"connections":[]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/version":
					fmt.Fprint(writer, `{"version":"1.19.30"}`)
				case "/proxies":
					fmt.Fprint(writer, testCase.proxies)
				case "/providers/proxies":
					fmt.Fprint(writer, testCase.providers)
				case "/connections":
					fmt.Fprint(writer, testCase.connections)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			collector, err := mihomo.NewCollector(server.URL, "", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := collector.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("collector must not fail on a well-formed but empty document: %v", err)
			}
			if len(snapshot.Issues) != 0 {
				t.Fatalf("issues = %#v, want none: every endpoint answered", snapshot.Issues)
			}
			if _, err := json.Marshal(snapshot); err != nil {
				t.Fatalf("snapshot must stay encodable: %v", err)
			}
			for _, group := range snapshot.Groups {
				if group.Health != model.HealthOK && group.Health != model.HealthDown && group.Health != model.HealthUnknown {
					t.Fatalf("group %q has health %q", group.Name, group.Health)
				}
			}
		})
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
