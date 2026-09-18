package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const defaultControllerURL = "http://mihomo:9090"

type Config struct {
	ControllerURL string
	Secret        string
}

func Load() (Config, error) {
	controllerURL := strings.TrimSpace(os.Getenv("MIHOMO_MONITOR_CONTROLLER_URL"))
	if controllerURL == "" {
		controllerURL = defaultControllerURL
	}
	secretPath := strings.TrimSpace(os.Getenv("MIHOMO_MONITOR_SECRET_FILE"))
	if secretPath == "" {
		return Config{ControllerURL: controllerURL}, nil
	}
	raw, err := os.ReadFile(secretPath)
	if err != nil {
		return Config{}, fmt.Errorf("read Mihomo secret file: %w", err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return Config{}, errors.New("Mihomo secret file is empty")
	}
	return Config{ControllerURL: controllerURL, Secret: secret}, nil
}
