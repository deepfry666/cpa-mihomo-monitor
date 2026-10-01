package mihomo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-mihomo-monitor/internal/model"
)

const (
	// maxJSONBytes bounds a single small controller document (/version,
	// /proxies, /providers/proxies). A subscription that grows past this is
	// reported per endpoint instead of silently discarding every document.
	maxJSONBytes = 8 << 20
	// maxConnectionsBytes bounds the streamed /connections document. Traffic
	// and connection totals stay usable up to this point.
	maxConnectionsBytes    = 16 << 20
	maxRetainedConnections = 20000
	maxVisibleConnections  = 200
	maxProviderNodes       = 500
	maxResolutionDepth     = 8
)

type Collector struct {
	baseURL *url.URL
	secret  string
	client  *http.Client
	now     func() time.Time
}

type controllerStatusError int

var ErrNotSelectable = errors.New("group is not a manual selector")
var ErrInvalidChoice = errors.New("choice is not in the group")

func (e controllerStatusError) Error() string {
	return fmt.Sprintf("controller returned HTTP %d", int(e))
}

// ControllerStatus reports the HTTP status the controller returned, if the
// failure came from the controller at all.
func ControllerStatus(err error) (int, bool) {
	var status controllerStatusError
	if errors.As(err, &status) {
		return int(status), true
	}
	return 0, false
}

type versionResponse struct {
	Version string `json:"version"`
}

type historyEntry struct {
	Time  string `json:"time"`
	Delay int    `json:"delay"`
}

type proxyEntry struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Now     string         `json:"now"`
	All     []string       `json:"all"`
	Alive   *bool          `json:"alive"`
	History []historyEntry `json:"history"`
}

type proxiesResponse struct {
	Proxies map[string]proxyEntry `json:"proxies"`
}

type providerEntry struct {
	Name        string       `json:"name"`
	Type        string       `json:"type"`
	VehicleType string       `json:"vehicleType"`
	UpdatedAt   string       `json:"updatedAt"`
	Proxies     []proxyEntry `json:"proxies"`
}

type providersResponse struct {
	Providers map[string]providerEntry `json:"providers"`
}

type connectionsResponse struct {
	DownloadTotal int64             `json:"downloadTotal"`
	UploadTotal   int64             `json:"uploadTotal"`
	Connections   []connectionEntry `json:"connections"`
}

// connectionsResult carries the streamed /connections document.
type connectionsResult struct {
	DownloadTotal int64
	UploadTotal   int64
	Total         int
	Entries       []connectionEntry
	// Truncated means the retention cap trimmed the detail list while the whole
	// document was still read, so totals and the connection count stay valid.
	Truncated bool
	// Err means the document could not be read to the end. A half-read document
	// has no trustworthy connection count, so nothing from it is published.
	Err error
}

type connectionEntry struct {
	ID          string    `json:"id"`
	Start       time.Time `json:"start"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
	Chains      []string  `json:"chains"`
	Rule        string    `json:"rule"`
	RulePayload string    `json:"rulePayload"`
	Metadata    struct {
		Host            string `json:"host"`
		SourceIP        string `json:"sourceIP"`
		SourcePort      string `json:"sourcePort"`
		DestinationIP   string `json:"destinationIP"`
		DestinationPort string `json:"destinationPort"`
		Process         string `json:"process"`
		Network         string `json:"network"`
	} `json:"metadata"`
}

func NewCollector(controllerURL, secret string, client *http.Client) (*Collector, error) {
	controllerURL = strings.TrimRight(strings.TrimSpace(controllerURL), "/")
	parsed, err := url.Parse(controllerURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("Mihomo controller URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Mihomo controller URL must not contain credentials, a query, or a fragment")
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	return &Collector{
		baseURL: parsed,
		secret:  strings.TrimSpace(secret),
		client:  client,
		now:     time.Now,
	}, nil
}

func (c *Collector) Snapshot(ctx context.Context) (model.Snapshot, error) {
	started := c.now()
	var version versionResponse
	if err := c.getJSON(ctx, "/version", &version); err != nil {
		issue := "Controller 无法连接"
		var status controllerStatusError
		if errors.As(err, &status) && status == http.StatusUnauthorized {
			issue = "Controller 鉴权失败"
		} else if errors.Is(err, context.DeadlineExceeded) {
			issue = "Controller 连接超时"
		} else {
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				issue = "Controller 连接超时"
			}
		}
		return model.Snapshot{
			Status:     "offline",
			ObservedAt: c.now().UTC(),
			CollectMS:  time.Since(started).Milliseconds(),
			Controller: model.ControllerSnapshot{},
			Issues:     []string{issue},
		}, nil
	}

	var (
		proxies                 proxiesResponse
		providers               providersResponse
		connections             connectionsResult
		proxiesErr, providerErr error
		wait                    sync.WaitGroup
	)
	wait.Add(3)
	go func() {
		defer wait.Done()
		proxiesErr = c.getJSON(ctx, "/proxies", &proxies)
	}()
	go func() {
		defer wait.Done()
		providerErr = c.getJSON(ctx, "/providers/proxies", &providers)
	}()
	go func() {
		defer wait.Done()
		connections = c.fetchConnections(ctx)
	}()
	wait.Wait()
	if connections.Err != nil && ctx.Err() == nil {
		// A cut read is usually a transient reset or an aborted body, so one
		// fresh attempt usually recovers without bothering the operator.
		first := connections.Err
		retry := c.fetchConnections(ctx)
		if retry.Err == nil {
			connections = retry
			log.Printf("mihomo monitor: connections read recovered on retry: %v", first)
		} else {
			log.Printf("mihomo monitor: connections read failed twice: %v", first)
		}
	}

	snapshot := model.Snapshot{
		Status:     "ok",
		ObservedAt: c.now().UTC(),
		Controller: model.ControllerSnapshot{
			Reachable: true,
			Version:   version.Version,
		},
	}
	if proxiesErr != nil {
		snapshot.Issues = append(snapshot.Issues, "代理组读取失败")
	} else {
		snapshot.Groups = buildGroups(proxies.Proxies, newProxyIndex(proxies.Proxies, providers.Providers))
	}
	if providerErr != nil {
		snapshot.Issues = append(snapshot.Issues, "Provider 读取失败")
	} else {
		snapshot.Providers = buildProviders(providers.Providers)
	}
	if connections.Err != nil {
		// Surfaced to the operator's notice as a short message; the underlying
		// reason stays in the sidecar log so a rare cut read can be diagnosed.
		log.Printf("mihomo monitor: connections read failed: %v", connections.Err)
		snapshot.Issues = append(snapshot.Issues, "连接统计读取失败")
	} else {
		snapshot.Traffic = model.TrafficSnapshot{
			UploadTotal:   connections.UploadTotal,
			DownloadTotal: connections.DownloadTotal,
			Connections:   connections.Total,
		}
		snapshot.Connections = buildConnections(connections.Entries)
		snapshot.ConnectionsTruncated = connections.Truncated || connections.Total > len(snapshot.Connections)
	}
	snapshot.CollectMS = time.Since(started).Milliseconds()
	if len(snapshot.Issues) > 0 {
		snapshot.Status = "degraded"
	}
	return snapshot, nil
}

func buildConnections(entries []connectionEntry) []model.ConnectionSnapshot {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Start.After(entries[j].Start) })
	if len(entries) > maxVisibleConnections {
		entries = entries[:maxVisibleConnections]
	}
	result := make([]model.ConnectionSnapshot, 0, len(entries))
	for _, entry := range entries {
		metadata := entry.Metadata
		chains := append([]string(nil), entry.Chains...)
		for i, j := 0, len(chains)-1; i < j; i, j = i+1, j-1 {
			chains[i], chains[j] = chains[j], chains[i]
		}
		rule := entry.Rule
		if entry.RulePayload != "" {
			rule += " / " + entry.RulePayload
		}
		source, destination := "", ""
		if metadata.SourceIP != "" {
			source = net.JoinHostPort(metadata.SourceIP, metadata.SourcePort)
		}
		if metadata.DestinationIP != "" {
			destination = net.JoinHostPort(metadata.DestinationIP, metadata.DestinationPort)
		}
		result = append(result, model.ConnectionSnapshot{
			ID: entry.ID, Start: entry.Start, Host: metadata.Host,
			Process: metadata.Process,
			Source:  source, Destination: destination,
			Network: metadata.Network, Rule: rule, Chains: chains,
			Upload: entry.Upload, Download: entry.Download,
		})
	}
	return result
}

func (c *Collector) getJSON(ctx context.Context, path string, output any) error {
	endpoint, err := c.endpoint(path)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if c.secret != "" {
		request.Header.Set("Authorization", "Bearer "+c.secret)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("request controller: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxJSONBytes))
		return controllerStatusError(response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxJSONBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if int64(len(raw)) > maxJSONBytes {
		return fmt.Errorf("controller response exceeds %d bytes", maxJSONBytes)
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// meteredReader tracks how many bytes the decoder has consumed so a runaway
// document can be cut off without losing the data already parsed.
type meteredReader struct {
	source io.Reader
	read   int64
}

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.source.Read(p)
	m.read += int64(n)
	return n, err
}

// fetchConnections streams /connections so that a large document degrades into
// "totals plus the newest details" instead of discarding every statistic.
func (c *Collector) fetchConnections(ctx context.Context) connectionsResult {
	var result connectionsResult
	endpoint, err := c.endpoint("/connections")
	if err != nil {
		result.Err = err
		return result
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		result.Err = fmt.Errorf("create request: %w", err)
		return result
	}
	request.Header.Set("Accept", "application/json")
	if c.secret != "" {
		request.Header.Set("Authorization", "Bearer "+c.secret)
	}
	response, err := c.client.Do(request)
	if err != nil {
		result.Err = fmt.Errorf("request controller: %w", err)
		return result
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxJSONBytes))
		result.Err = controllerStatusError(response.StatusCode)
		return result
	}
	return decodeConnections(&meteredReader{source: response.Body})
}

func decodeConnections(reader *meteredReader) connectionsResult {
	result := connectionsResult{}
	decoder := json.NewDecoder(reader)
	token, err := decoder.Token()
	if err != nil {
		result.Err = fmt.Errorf("decode response: %w", err)
		return result
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		result.Err = errors.New("decode response: unexpected JSON document")
		return result
	}
	for decoder.More() {
		if reader.read > maxConnectionsBytes {
			result.Err = errors.New("connections response exceeds the read budget")
			return result
		}
		key, err := decoder.Token()
		if err != nil {
			result.Err = fmt.Errorf("read connections: %w", err)
			return result
		}
		switch key {
		case "downloadTotal":
			if err := decoder.Decode(&result.DownloadTotal); err != nil {
				result.Err = fmt.Errorf("read downloadTotal: %w", err)
				return result
			}
		case "uploadTotal":
			if err := decoder.Decode(&result.UploadTotal); err != nil {
				result.Err = fmt.Errorf("read uploadTotal: %w", err)
				return result
			}
		case "connections":
			opening, err := decoder.Token()
			if err != nil {
				result.Err = fmt.Errorf("read connections: %w", err)
				return result
			}
			if opening == nil {
				// Mihomo sends "connections": null whenever nothing is active.
				break
			}
			if delimiter, ok := opening.(json.Delim); !ok || delimiter != '[' {
				result.Err = errors.New("connections is not an array")
				return result
			}
			for decoder.More() {
				var entry connectionEntry
				if err := decoder.Decode(&entry); err != nil {
					result.Err = fmt.Errorf("read connection entry: %w", err)
					return result
				}
				result.Total++
				if len(result.Entries) < maxRetainedConnections {
					result.Entries = append(result.Entries, entry)
				} else {
					result.Truncated = true
				}
				if reader.read > maxConnectionsBytes {
					result.Err = errors.New("connections response exceeds the read budget")
					return result
				}
			}
			if _, err := decoder.Token(); err != nil {
				result.Err = fmt.Errorf("read connections: %w", err)
				return result
			}
		default:
			var discard json.RawMessage
			if err := decoder.Decode(&discard); err != nil {
				result.Err = fmt.Errorf("read %v: %w", key, err)
				return result
			}
		}
	}
	return result
}

func (c *Collector) SelectGroup(ctx context.Context, name, choice string) error {
	var proxies proxiesResponse
	if err := c.getJSON(ctx, "/proxies", &proxies); err != nil {
		return err
	}
	group, ok := proxies.Proxies[name]
	if !ok || !strings.EqualFold(group.Type, "selector") {
		return ErrNotSelectable
	}
	allowed := false
	for _, option := range group.All {
		if option == choice {
			allowed = true
			break
		}
	}
	if !allowed {
		return ErrInvalidChoice
	}
	if group.Now == choice {
		return nil
	}

	endpoint, err := c.endpoint("/proxies/" + url.PathEscape(name))
	if err != nil {
		return err
	}
	body, _ := json.Marshal(struct {
		Name string `json:"name"`
	}{choice})
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create selection request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		request.Header.Set("Authorization", "Bearer "+c.secret)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("request controller: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxJSONBytes))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return controllerStatusError(response.StatusCode)
	}
	return nil
}

func (c *Collector) endpoint(path string) (string, error) {
	endpoint := *c.baseURL
	basePath := strings.TrimRight(c.baseURL.EscapedPath(), "/")
	escapedPath := basePath + path
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return "", fmt.Errorf("build request path: %w", err)
	}
	endpoint.Path = decodedPath
	endpoint.RawPath = escapedPath
	return endpoint.String(), nil
}

// proxyIndex resolves a group's selection against every node the controller
// knows about: the top-level /proxies map and the nodes nested in
// /providers/proxies. A provider-backed exit is otherwise invisible, which used
// to leave the group's own cached alive flag as the only health signal.
type proxyIndex struct {
	top      map[string]proxyEntry
	provider map[string]proxyEntry
}

func newProxyIndex(proxies map[string]proxyEntry, providers map[string]providerEntry) proxyIndex {
	index := proxyIndex{top: proxies, provider: make(map[string]proxyEntry)}
	for _, provider := range providers {
		for _, node := range provider.Proxies {
			name := strings.TrimSpace(node.Name)
			if name == "" {
				continue
			}
			if _, exists := index.provider[name]; exists {
				continue
			}
			index.provider[name] = node
		}
	}
	return index
}

func (index proxyIndex) lookup(name string) (proxyEntry, bool) {
	if entry, ok := index.top[name]; ok {
		return entry, true
	}
	entry, ok := index.provider[name]
	return entry, ok
}

// resolve walks a group's "now" chain down to the leaf that actually carries
// traffic. Cycles and runaway nesting degrade to "unknown" instead of looping
// or trusting the group flag.
func (index proxyIndex) resolve(name string) ([]string, *model.ProxySnapshot) {
	chain := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	current := strings.TrimSpace(name)
	for range maxResolutionDepth {
		if current == "" || seen[current] {
			break
		}
		seen[current] = true
		chain = append(chain, current)
		entry, ok := index.lookup(current)
		if !ok {
			return chain, staticSnapshot(current)
		}
		if !isGroupType(entry.Type) {
			leaf := snapshotFor(entry, current)
			return chain, &leaf
		}
		current = strings.TrimSpace(entry.Now)
	}
	if len(chain) == 0 {
		return nil, nil
	}
	return chain, &model.ProxySnapshot{Name: chain[len(chain)-1], Health: model.HealthUnknown}
}

// staticSnapshot covers the built-in names that never appear as regular nodes.
func staticSnapshot(name string) *model.ProxySnapshot {
	leaf := model.ProxySnapshot{Name: name, Health: model.HealthUnknown}
	switch strings.ToUpper(name) {
	case "DIRECT", "PASS":
		leaf.Type = "Direct"
		leaf.Health = model.HealthOK
	case "REJECT":
		leaf.Type = "Reject"
		leaf.Health = model.HealthDown
	}
	return &leaf
}

func snapshotFor(entry proxyEntry, name string) model.ProxySnapshot {
	if trimmed := strings.TrimSpace(entry.Name); trimmed != "" {
		name = trimmed
	}
	result := model.ProxySnapshot{Name: name, Type: entry.Type, Health: model.HealthUnknown}
	if entry.Alive != nil {
		if *entry.Alive {
			result.Health = model.HealthOK
		} else {
			result.Health = model.HealthDown
		}
	}
	result.DelayMS, result.CheckedAt = latestCheck(entry.History)
	if result.DelayMS > 0 && result.Health == model.HealthUnknown {
		result.Health = model.HealthOK
	}
	switch strings.ToLower(strings.TrimSpace(entry.Type)) {
	case "direct":
		result.Health = model.HealthOK
	case "reject":
		result.Health = model.HealthDown
	}
	return result
}

func buildGroups(proxies map[string]proxyEntry, index proxyIndex) []model.GroupSnapshot {
	groups := make([]model.GroupSnapshot, 0, len(proxies))
	for key, proxy := range proxies {
		if !isGroupType(proxy.Type) {
			continue
		}
		name := strings.TrimSpace(proxy.Name)
		if name == "" {
			name = key
		}
		group := model.GroupSnapshot{
			Name:   name,
			Type:   proxy.Type,
			Now:    proxy.Now,
			Health: model.HealthUnknown,
			All:    append([]string(nil), proxy.All...),
		}
		if chain, leaf := index.resolve(proxy.Now); leaf != nil {
			group.Chain = chain
			group.Exit = leaf
			group.Health = leaf.Health
		}
		if isSelectorType(proxy.Type) && len(proxy.All) > 1 {
			group.Choices = make([]model.ProxySnapshot, 0, len(proxy.All))
			for _, option := range proxy.All {
				if _, leaf := index.resolve(option); leaf != nil {
					group.Choices = append(group.Choices, *leaf)
				} else {
					group.Choices = append(group.Choices, model.ProxySnapshot{Name: option, Health: model.HealthUnknown})
				}
			}
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})
	return groups
}

func buildProviders(providers map[string]providerEntry) []model.ProviderSnapshot {
	result := make([]model.ProviderSnapshot, 0, len(providers))
	for key, provider := range providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			name = key
		}
		snapshot := model.ProviderSnapshot{
			Name:        name,
			Type:        provider.Type,
			VehicleType: provider.VehicleType,
			UpdatedAt:   provider.UpdatedAt,
			ProxyCount:  len(provider.Proxies),
		}
		nodes := make([]model.ProxySnapshot, 0, min(len(provider.Proxies), maxProviderNodes))
		for _, node := range provider.Proxies {
			entry := snapshotFor(node, strings.TrimSpace(node.Name))
			switch entry.Health {
			case model.HealthOK:
				snapshot.HealthyCount++
			case model.HealthDown:
				snapshot.DownCount++
			default:
				snapshot.UnknownCount++
			}
			if len(nodes) < maxProviderNodes {
				nodes = append(nodes, entry)
			}
		}
		sort.SliceStable(nodes, func(i, j int) bool {
			return strings.ToLower(nodes[i].Name) < strings.ToLower(nodes[j].Name)
		})
		snapshot.Nodes = nodes
		result = append(result, snapshot)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

func isGroupType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "selector", "urltest", "fallback", "loadbalance":
		return true
	default:
		return false
	}
}

func isSelectorType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "selector")
}

func latestCheck(history []historyEntry) (int, *time.Time) {
	if len(history) == 0 {
		return 0, nil
	}
	last := history[len(history)-1]
	delay := last.Delay
	if delay < 0 {
		delay = 0
	}
	if last.Time == "" {
		return delay, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, last.Time)
	if err != nil {
		return delay, nil
	}
	utc := parsed.UTC()
	return delay, &utc
}
