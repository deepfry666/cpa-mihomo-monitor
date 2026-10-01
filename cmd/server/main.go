package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cpa-mihomo-monitor/internal/config"
	"cpa-mihomo-monitor/internal/mihomo"
	"cpa-mihomo-monitor/internal/sidecar"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get("http://127.0.0.1:18319/mihomo-monitor/dashboard")
		if err != nil {
			os.Exit(1)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	adminKeyPath := strings.TrimSpace(os.Getenv("MIHOMO_MONITOR_ADMIN_KEY_FILE"))
	if adminKeyPath == "" {
		adminKeyPath = strings.TrimSpace(os.Getenv("CPAMP_ADMIN_KEY_FILE"))
	}
	if adminKeyPath == "" {
		log.Fatal("MIHOMO_MONITOR_ADMIN_KEY_FILE is required")
	}
	rawKey, err := os.ReadFile(adminKeyPath)
	if err != nil {
		log.Fatalf("read Mihomo Monitor admin key: %v", err)
	}
	adminKey := strings.TrimSpace(string(rawKey))
	if adminKey == "" {
		log.Fatal("Mihomo Monitor admin key file is empty")
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load Mihomo monitor configuration: %v", err)
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 1500 * time.Millisecond}).DialContext,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	collector, err := mihomo.NewCollector(cfg.ControllerURL, cfg.Secret, &http.Client{
		Transport: transport,
		Timeout:   4 * time.Second,
	})
	if err != nil {
		log.Fatalf("create Mihomo collector: %v", err)
	}
	server := &http.Server{
		Addr:              ":18319",
		Handler:           sidecar.NewHandler([]byte(adminKey), collector, strings.Split(os.Getenv("MIHOMO_MONITOR_SELECTABLE_GROUPS"), ",")),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Print("Mihomo monitor listening on :18319")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
