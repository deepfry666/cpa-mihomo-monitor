package mihomo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-mihomo-monitor/internal/model"
)

const maxResponseBytes = 2 << 20
const maxVisibleConnections = 200

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

type versionResponse struct {
	Version string `json:"version"`
}

type historyEntry struct {
	Delay int `json:"delay"`
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
			Controller: model.ControllerSnapshot{
				LatencyMS: time.Since(started).Milliseconds(),
			},
			Issues: []string{issue},
		}, nil
	}

	var (
		proxies     proxiesResponse
		providers   providersResponse
		connections connectionsResponse
		proxiesErr  error
		providerErr error
		connectErr  error
		wait        sync.WaitGroup
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
		connectErr = c.getJSON(ctx, "/connections", &connections)
	}()
	wait.Wait()

	snapshot := model.Snapshot{
		Status:     "ok",
		ObservedAt: c.now().UTC(),
		Controller: model.ControllerSnapshot{
			Reachable: true,
			Version:   version.Version,
			LatencyMS: time.Since(started).Milliseconds(),
		},
	}
	if proxiesErr != nil {
		snapshot.Issues = append(snapshot.Issues, "代理组读取失败")
	} else {
		snapshot.Groups = buildGroups(proxies.Proxies)
	}
	if providerErr != nil {
		snapshot.Issues = append(snapshot.Issues, "Provider 读取失败")
	} else {
		snapshot.Providers = buildProviders(providers.Providers)
	}
	if connectErr != nil {
		snapshot.Issues = append(snapshot.Issues, "连接统计读取失败")
	} else {
		snapshot.Traffic = model.TrafficSnapshot{
			UploadTotal:   connections.UploadTotal,
			DownloadTotal: connections.DownloadTotal,
			Connections:   len(connections.Connections),
		}
		snapshot.Connections = buildConnections(connections.Connections)
	}
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
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return controllerStatusError(response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return errors.New("controller response exceeds 2 MiB")
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
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
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
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

func buildGroups(proxies map[string]proxyEntry) []model.GroupSnapshot {
	groups := make([]model.GroupSnapshot, 0)
	for key, proxy := range proxies {
		if !isGroupType(proxy.Type) {
			continue
		}
		name := strings.TrimSpace(proxy.Name)
		if name == "" {
			name = key
		}
		alive := proxy.Alive != nil && *proxy.Alive
		delay := latestDelay(proxy.History)
		if selected, ok := proxies[proxy.Now]; ok {
			if selected.Alive != nil {
				alive = *selected.Alive
			}
			if selectedDelay := latestDelay(selected.History); selectedDelay > 0 {
				delay = selectedDelay
			}
		}
		groups = append(groups, model.GroupSnapshot{
			Name:    name,
			Type:    proxy.Type,
			Now:     proxy.Now,
			Alive:   alive,
			DelayMS: delay,
			All:     append([]string(nil), proxy.All...),
		})
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
		result = append(result, model.ProviderSnapshot{
			Name:        name,
			Type:        provider.Type,
			VehicleType: provider.VehicleType,
			UpdatedAt:   provider.UpdatedAt,
			ProxyCount:  len(provider.Proxies),
		})
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

func latestDelay(history []historyEntry) int {
	if len(history) == 0 {
		return 0
	}
	return history[len(history)-1].Delay
}
