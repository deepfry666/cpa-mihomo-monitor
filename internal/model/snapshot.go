package model

import (
	"context"
	"time"
)

// HealthState is the tri-state health of a proxy or of the leaf behind a group.
// It deliberately distinguishes "unknown" from "down": an unchecked node is not
// evidence of an outage, and a group flag is not evidence of a healthy exit.
type HealthState string

const (
	HealthOK      HealthState = "ok"
	HealthDown    HealthState = "down"
	HealthUnknown HealthState = "unknown"
)

type ControllerSnapshot struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
}

type TrafficSnapshot struct {
	UploadTotal   int64 `json:"upload_total"`
	DownloadTotal int64 `json:"download_total"`
	Connections   int   `json:"connections"`
}

// ProxySnapshot describes one node plus the result of its last health check.
type ProxySnapshot struct {
	Name      string      `json:"name"`
	Type      string      `json:"type,omitempty"`
	Health    HealthState `json:"health"`
	DelayMS   int         `json:"delay_ms,omitempty"`
	CheckedAt *time.Time  `json:"checked_at,omitempty"`
}

type GroupSnapshot struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Now  string `json:"now,omitempty"`
	// Health follows the resolved exit, not the group's own cached flag.
	Health     HealthState    `json:"health"`
	Exit       *ProxySnapshot `json:"exit,omitempty"`
	Chain      []string       `json:"chain,omitempty"`
	Selectable bool           `json:"selectable,omitempty"`
	// Primary marks the group the operator configured as the main exit.
	Primary bool            `json:"primary,omitempty"`
	All     []string        `json:"all,omitempty"`
	Choices []ProxySnapshot `json:"choices,omitempty"`
}

type ProviderSnapshot struct {
	Name         string          `json:"name"`
	Type         string          `json:"type,omitempty"`
	VehicleType  string          `json:"vehicle_type,omitempty"`
	UpdatedAt    string          `json:"updated_at,omitempty"`
	ProxyCount   int             `json:"proxy_count"`
	HealthyCount int             `json:"healthy_count"`
	DownCount    int             `json:"down_count"`
	UnknownCount int             `json:"unknown_count"`
	Nodes        []ProxySnapshot `json:"nodes,omitempty"`
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
	Status     string             `json:"status"`
	ObservedAt time.Time          `json:"observed_at"`
	CollectMS  int64              `json:"collect_ms"`
	Controller ControllerSnapshot `json:"controller"`
	Traffic    TrafficSnapshot    `json:"traffic"`
	Groups     []GroupSnapshot    `json:"groups"`
	Providers  []ProviderSnapshot `json:"providers"`
	// ConnectionsTruncated means the detail list is shorter than the totals.
	ConnectionsTruncated bool                 `json:"connections_truncated,omitempty"`
	Connections          []ConnectionSnapshot `json:"connections"`
	Issues               []string             `json:"issues,omitempty"`
}

type SnapshotSource interface {
	Snapshot(context.Context) (Snapshot, error)
}
