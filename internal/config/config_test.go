package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"cpa-mihomo-monitor/internal/config"
)

func TestLoadUsesDockerNetworkDefaultAndReadsSecretFile(t *testing.T) {
	directory := t.TempDir()
	secretPath := filepath.Join(directory, "mihomo-secret")
	if err := os.WriteFile(secretPath, []byte("  controller-secret\n"), 0o600); err != nil {
		t.Fatalf("write secret fixture: %v", err)
	}
	t.Setenv("MIHOMO_MONITOR_CONTROLLER_URL", "")
	t.Setenv("MIHOMO_MONITOR_SECRET_FILE", secretPath)

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got, want := loaded.ControllerURL, "http://mihomo:9090"; got != want {
		t.Fatalf("controller URL = %q, want %q", got, want)
	}
	if got, want := loaded.Secret, "controller-secret"; got != want {
		t.Fatalf("secret = %q, want trimmed secret", got)
	}
}

func TestLoadFailsClosedWhenConfiguredSecretFileCannotBeRead(t *testing.T) {
	t.Setenv("MIHOMO_MONITOR_SECRET_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := config.Load(); err == nil {
		t.Fatal("missing configured secret file must fail closed")
	}
}
