package ui_test

import (
	"regexp"
	"strings"
	"testing"

	"cpa-mihomo-monitor/internal/ui"
)

// The dashboard is one embedded document served under a strict CSP
// (default-src 'none'), so it must not reach for anything outside itself, and
// the controls an operator uses daily must keep their accessible names.
func TestDashboardIsSelfContained(t *testing.T) {
	page := string(ui.DashboardHTML)
	for _, want := range []string{
		`data-page="mihomo-monitor"`,
		`<label for="adminKey"`,
		`id="adminKey"`,
		`id="exitPanel"`,
		`role="tablist"`,
		`@media (prefers-reduced-motion: reduce)`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}
	for _, banned := range []string{
		`src="http`,
		`href="http`,
		"localStorage",
		"window.confirm",
		"class=\"skeleton",
		"/v0/management/plugins/",
	} {
		if strings.Contains(page, banned) {
			t.Errorf("dashboard must not reference %q", banned)
		}
	}
}

func TestDashboardTabsDescribeTheirPanels(t *testing.T) {
	page := string(ui.DashboardHTML)
	tabs := regexp.MustCompile(`<button[^>]*role="tab"[^>]*>`).FindAllString(page, -1)
	if len(tabs) != 3 {
		t.Fatalf("tabs = %d, want 3", len(tabs))
	}
	for _, tab := range tabs {
		controls := regexp.MustCompile(`aria-controls="([^"]+)"`).FindStringSubmatch(tab)
		if controls == nil {
			t.Fatalf("tab without aria-controls: %s", tab)
		}
		if !strings.Contains(page, `id="`+controls[1]+`" role="tabpanel"`) {
			t.Errorf("panel %q is not labelled as a tabpanel", controls[1])
		}
	}
	if got := strings.Count(page, `role="tabpanel"`); got != 3 {
		t.Errorf("tabpanels = %d, want 3", got)
	}
}
