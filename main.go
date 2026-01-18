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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cristalhq/aconfig"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Config holds all application settings
type Config struct {
	TelegramToken     string        `env:"TG_TOKEN" required:"true"`
	TelegramChatID    int64         `env:"TG_CHAT_ID" required:"true"`
	ServerName        string        `env:"SERVER_NAME" default:"MyServer"`
	IncludeContainers []string      `env:"INCLUDE_CONTAINERS"`
	ExcludeContainers []string      `env:"EXCLUDE_CONTAINERS"`
	IgnoreExitZero    bool          `env:"IGNORE_EXIT_ZERO" default:"true"`
	DebounceWindow    time.Duration `env:"DEBOUNCE_WINDOW" default:"5m"`
	StabilityPeriod   time.Duration `env:"STABILITY_PERIOD" default:"30s"`
	RequestTimeout    time.Duration `env:"REQUEST_TIMEOUT" default:"10s"`
}

// ContainerState tracks the health and timers for a single container
type ContainerState struct {
	Name           string
	CrashCount     int
	IsDown         bool
	LastCrashTime  time.Time
	DebounceTimer  *time.Timer // Timer to send "Crash Loop" summary
	StabilityTimer *time.Timer // Timer to confirm "Recovery"
}

// Monitor is the main application struct
type Monitor struct {
	cfg        Config
	httpClient *http.Client
	
	mu     sync.Mutex
	states map[string]*ContainerState

	// Queue for Telegram messages to prevent 429 Rate Limits
	msgQueue chan string
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

	// Graceful shutdown context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	monitor := &Monitor{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cfg.RequestTimeout},
		states:     make(map[string]*ContainerState),
		msgQueue:   make(chan string, 100), // Buffer up to 100 alerts
	}

	log.Printf("🔥 Monitor started on %s", cfg.ServerName)
	log.Printf("⏱  Debounce: %s | Stability: %s", cfg.DebounceWindow, cfg.StabilityPeriod)

	// Start the Telegram Worker (Rate Limiter)
	go monitor.telegramWorker(ctx)

	// Send startup notification
	monitor.queueAlert("👻 <b>Monitor Online</b>", "Monitoring service has started/restarted.")

	// Main Retry Loop
	for {
		if err := run(ctx, monitor); err != nil {
			log.Printf("❌ Docker stream error: %v. Retrying in 5s...", err)
		}

		select {
		case <-ctx.Done():
			log.Println("Shutting down...")
			return
		case <-time.After(5 * time.Second):
			continue
		}
	}
}

func run(ctx context.Context, m *Monitor) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	if _, err := cli.Ping(ctx); err != nil {
		return err
	}

	// Subscribe to: Die (Crash), Start (Recovery), Health Status (Unhealthy)
	args := filters.NewArgs()
	args.Add("type", "container")
	args.Add("event", "die")
	args.Add("event", "start")
	args.Add("event", "health_status")

	msgs, errs := cli.Events(ctx, events.ListOptions{Filters: args})

	log.Println("👀 Listening for Docker events...")

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return err
		case event := <-msgs:
			m.processEvent(ctx, event)
		}
	}
}

func (m *Monitor) processEvent(ctx context.Context, event events.Message) {
	containerName := event.Actor.Attributes["name"]

	if !m.isContainerAllowed(containerName) {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Get or Create State
	state, exists := m.states[containerName]
	if !exists {
		state = &ContainerState{Name: containerName}
		m.states[containerName] = state
	}

	// Convert Action to string for easier comparison
	action := string(event.Action)

	if action == "die" {
		m.handleDie(state, event)
	} else if action == "start" {
		m.handleStart(state)
	} else if strings.Contains(action, "health_status: unhealthy") {
		// Catch specific unhealthy event
		m.handleUnhealthy(state)
	}
}

// --- Event Handlers ---

func (m *Monitor) handleDie(s *ContainerState, event events.Message) {
	exitCode := event.Actor.Attributes["exitCode"]
	isOOM := event.Actor.Attributes["oomKilled"] == "true"

	// Ignore clean exit if configured
	if !isOOM && m.cfg.IgnoreExitZero && exitCode == "0" {
		return
	}

	// Cancel any pending recovery since it died again
	if s.StabilityTimer != nil {
		s.StabilityTimer.Stop()
	}

	s.IsDown = true
	s.LastCrashTime = time.Unix(event.Time, 0)
	s.CrashCount++

	// 1. Critical Alert: OOM Killed (Always send)
	if isOOM {
		m.sendFormattedAlert(s.Name, "💀 OOM Killed", "Container ran out of memory (RAM limit exceeded)")
		return
	}

	// 2. Debounce Logic for normal crashes
	if s.CrashCount == 1 {
		// First crash: Alert immediately
		details := fmt.Sprintf("Process exited with code %s", exitCode)
		m.sendFormattedAlert(s.Name, "🔴 Down", details)

		// Start Window
		s.DebounceTimer = time.AfterFunc(m.cfg.DebounceWindow, func() {
			m.mu.Lock()
			defer m.mu.Unlock()

			if s.CrashCount > 1 {
				// Send Summary
				msg := fmt.Sprintf("Crash Loop: Died %d times in last %s", s.CrashCount, m.cfg.DebounceWindow)
				m.sendFormattedAlert(s.Name, "⚠️ Unstable", msg)
			}
			// Reset count but keep IsDown true until recovery
			s.CrashCount = 0
		})
	}
	// If CrashCount > 1, we are silent (debouncing) until timer fires
}

func (m *Monitor) handleStart(s *ContainerState) {
	// Only care if we thought it was down
	if !s.IsDown {
		return
	}

	if s.StabilityTimer != nil {
		s.StabilityTimer.Stop()
	}

	// Wait for stability period before declaring recovery
	s.StabilityTimer = time.AfterFunc(m.cfg.StabilityPeriod, func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		s.IsDown = false
		s.CrashCount = 0
		if s.DebounceTimer != nil {
			s.DebounceTimer.Stop()
		}

		m.sendFormattedAlert(s.Name, "✅ Recovered", "Container is running and stable")
	})
}

func (m *Monitor) handleUnhealthy(s *ContainerState) {
	// Treat unhealthy as a warning, no complex debouncing needed usually
	// But we can limit it to avoid spam if it stays unhealthy
	// For simplicity: just fire alert.
	m.sendFormattedAlert(s.Name, "💊 Unhealthy", "Healthcheck failed")
}

// --- Helper Functions ---

func (m *Monitor) isContainerAllowed(name string) bool {
	// 1. Include List (Allowlist)
	if len(m.cfg.IncludeContainers) > 0 {
		for _, included := range m.cfg.IncludeContainers {
			if included == name {
				return true
			}
		}
		return false
	}
	// 2. Exclude List (Blocklist)
	for _, excluded := range m.cfg.ExcludeContainers {
		if excluded == name {
			return false
		}
	}
	return true
}

// --- Alerting System ---

func (m *Monitor) sendFormattedAlert(name, statusEmoji, details string) {
	msg := fmt.Sprintf(
		"<b>%s | %s</b>\n\n"+
			"📦 <b>Container:</b> <code>%s</code>\n"+
			"🖥 <b>Server:</b> %s\n"+
			"📝 <b>Status:</b> %s\n"+
			"ℹ️ <b>Details:</b> %s",
		statusEmoji, m.cfg.ServerName,
		name,
		m.cfg.ServerName,
		statusEmoji,
		details,
	)
	m.queueAlert(msg, "") // Second arg empty effectively
}

// queueAlert puts message into channel
func (m *Monitor) queueAlert(header, body string) {
	// If body is empty, header is the full message
	msg := header
	if body != "" {
		msg = fmt.Sprintf("<b>%s</b>\n%s", header, body)
	}
	
	select {
	case m.msgQueue <- msg:
	default:
		log.Println("⚠️ Alert queue full, dropping message")
	}
}

// telegramWorker processes the queue with rate limiting
func (m *Monitor) telegramWorker(ctx context.Context) {
	// Ticker limits us to ~1 message per second (Telegram limit is strict)
	ticker := time.NewTicker(time.Second) 
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-m.msgQueue:
			<-ticker.C // Wait for tick
			m.postTelegram(msg)
		}
	}
}

func (m *Monitor) postTelegram(text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.cfg.TelegramToken)
	payload := map[string]interface{}{
		"chat_id":    m.cfg.TelegramChatID,
		"text":       text,
		"parse_mode": "HTML",
	}

	body, _ := json.Marshal(payload)
	// Create context with timeout for the request
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.Printf("Failed to send TG: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Telegram API Error: %s", resp.Status)
	}
}