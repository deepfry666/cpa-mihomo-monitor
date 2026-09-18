package model

import (
	"context"
	"time"
)

type ControllerSnapshot struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

type TrafficSnapshot struct {
	UploadTotal   int64 `json:"upload_total"`
	DownloadTotal int64 `json:"download_total"`
	Connections   int   `json:"connections"`
}

type GroupSnapshot struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Now        string   `json:"now,omitempty"`
	Selectable bool     `json:"selectable,omitempty"`
	Alive      bool     `json:"alive"`
	DelayMS    int      `json:"delay_ms,omitempty"`
	All        []string `json:"all,omitempty"`
}

type ProviderSnapshot struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	VehicleType string `json:"vehicle_type,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	ProxyCount  int    `json:"proxy_count"`
}

type ConnectionSnapshot struct {
	ID          string    `json:"id"`
	Start       time.Time `json:"start"`
	Host        string    `json:"host,omitempty"`
	Process     string    `json:"process,omitempty"`
	Source      string    `json:"source,omitempty"`
	Destination string    `json:"destination,omitempty"`
	Network     string    `json:"network,omitempty"`
	Rule        string    `json:"rule,omitempty"`
	Chains      []string  `json:"chains,omitempty"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
}

type Snapshot struct {
	Status      string               `json:"status"`
	ObservedAt  time.Time            `json:"observed_at"`
	Controller  ControllerSnapshot   `json:"controller"`
	Traffic     TrafficSnapshot      `json:"traffic"`
	Groups      []GroupSnapshot      `json:"groups"`
	Providers   []ProviderSnapshot   `json:"providers"`
	Connections []ConnectionSnapshot `json:"connections"`
	Issues      []string             `json:"issues,omitempty"`
}

type SnapshotSource interface {
	Snapshot(context.Context) (Snapshot, error)
}
