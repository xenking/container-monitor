package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/cristalhq/aconfig"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

type Config struct {
	TelegramToken     string        `env:"TG_TOKEN" required:"true"`
	TelegramChatID    int64         `env:"TG_CHAT_ID" required:"true"`
	ServerName        string        `env:"SERVER_NAME" default:"MyServer"`
	IgnoreExitZero    bool          `env:"IGNORE_EXIT_ZERO" default:"true"`
	RequestTimeout    time.Duration `env:"REQUEST_TIMEOUT" default:"10s"`
	ReconnectDelay    time.Duration `env:"RECONNECT_DELAY" default:"5s"`
	IncludeContainers []string      `env:"INCLUDE_CONTAINERS"` // List of names to watch (allowlist)
	ExcludeContainers []string      `env:"EXCLUDE_CONTAINERS"` // List of names to ignore (blocklist)
}

type Notifier struct {
	cfg        Config
	httpClient *http.Client
}

func main() {
	var cfg Config
	loader := aconfig.LoaderFor(&cfg, aconfig.Config{
		SkipFlags: true,
		SkipFiles: true,
		EnvPrefix: "",
	})

	if err := loader.Load(); err != nil {
		log.Fatalf("config load error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	notifier := &Notifier{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
	}

	log.Printf("Starting monitor on %s...", cfg.ServerName)

	for {
		if err := runMonitor(ctx, notifier); err != nil {
			log.Printf("Monitor error: %v", err)
		}

		select {
		case <-ctx.Done():
			log.Println("Shutting down...")
			return
		case <-time.After(cfg.ReconnectDelay):
			log.Println("Reconnecting to Docker daemon...")
		}
	}
}

func runMonitor(ctx context.Context, n *Notifier) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client create: %w", err)
	}
	defer cli.Close()

	if _, err := cli.Ping(ctx); err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}

	filter := filters.NewArgs()
	filter.Add("type", "container")
	filter.Add("event", "die")

	opts := events.ListOptions{
		Filters: filter,
	}

	msgs, errs := cli.Events(ctx, opts)

	log.Println("Listening for events...")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			return fmt.Errorf("event stream error: %w", err)
		case event := <-msgs:
			if err := handleEvent(ctx, event, n); err != nil {
				log.Printf("Failed to handle event: %v", err)
			}
		}
	}
}

func handleEvent(ctx context.Context, event events.Message, n *Notifier) error {
	containerName := event.Actor.Attributes["name"]

	// Filter by Name
	// 1. If Include list is not empty, ONLY allow names in that list.
	if len(n.cfg.IncludeContainers) > 0 {
		if !slices.Contains(n.cfg.IncludeContainers, containerName) {
			return nil // Skip, not in allowlist
		}
	}

	// 2. If Exclude list is used, skip names in that list.
	if slices.Contains(n.cfg.ExcludeContainers, containerName) {
		return nil // Skip, explicitly excluded
	}
	exitCode := event.Actor.Attributes["exitCode"]

	if n.cfg.IgnoreExitZero && exitCode == "0" {
		return nil
	}

	imageName := event.Actor.Attributes["image"]
	ts := time.Unix(event.Time, 0).Format(time.RFC3339)

	msg := fmt.Sprintf(
		"🚨 <b>Docker Container Alert</b>\n\n"+
			"🖥 <b>Server:</b> %s\n"+
			"📦 <b>Container:</b> %s\n"+
			"💿 <b>Image:</b> %s\n"+
			"❌ <b>Exit Code:</b> %s\n"+
			"🕒 <b>Time:</b> %s",
		n.cfg.ServerName, containerName, imageName, exitCode, ts,
	)

	return n.sendTelegram(ctx, msg)
}

func (n *Notifier) sendTelegram(ctx context.Context, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.TelegramToken)

	payload := map[string]interface{}{
		"chat_id":    n.cfg.TelegramChatID,
		"text":       text,
		"parse_mode": "HTML",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram api error: status %s", resp.Status)
	}

	return nil
}
