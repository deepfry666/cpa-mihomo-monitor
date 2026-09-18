package entryplugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	ID           = "mihomo-monitor"
	Version      = "0.2.3"
	dashboardURL = "/mihomo-monitor/dashboard"
	projectURL   = "https://github.com/deepfry666/cpa-mihomo-monitor"
)

type Application struct{}

type managementRequest struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func New() *Application { return &Application{} }

func (a *Application) Handle(method string, raw []byte) (json.RawMessage, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return json.Marshal(map[string]any{
			"schema_version": 1,
			"metadata": map[string]any{
				"Name":             "Mihomo Monitor Entry",
				"Version":          Version,
				"Author":           "imfourj",
				"GitHubRepository": projectURL,
				"Logo":             "",
				"ConfigFields":     []any{},
			},
			"capabilities": map[string]bool{"management_api": true},
		})
	case "management.register":
		return json.Marshal(map[string]any{
			"resources": []map[string]string{{
				"Path":        "/dashboard",
				"Menu":        "Mihomo 监控",
				"Description": "打开独立运行的 Mihomo 监控与代理组管理页面。",
			}},
		})
	case "management.handle":
		return a.handleManagement(raw)
	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
}

func (a *Application) handleManagement(raw []byte) (json.RawMessage, error) {
	var request managementRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
	path := strings.TrimSuffix(request.Path, "/")
	if request.Method != http.MethodGet || path != "/v0/resource/plugins/"+ID+"/dashboard" {
		return json.Marshal(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"content-type": []string{"text/plain; charset=utf-8"}},
			Body:       []byte("not found\n"),
		})
	}
	return json.Marshal(managementResponse{
		StatusCode: http.StatusTemporaryRedirect,
		Headers: http.Header{
			"location":      []string{dashboardURL},
			"cache-control": []string{"no-store"},
		},
	})
}
