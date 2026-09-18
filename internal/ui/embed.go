package ui

import _ "embed"

// DashboardHTML is a self-contained plugin page served by CPA's resource route.
//
//go:embed index.html
var DashboardHTML []byte
