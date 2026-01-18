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
	Name        string
	CrashCount  int
	IsDown      bool
	IsUnhealthy bool

	DebounceTimer  *time.Timer
	StabilityTimer *time.Timer

	// Generation counters to detect superseded timers
	debounceGen  int64
	stabilityGen int64
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
		httpClient: &http.Client{},
		states:     make(map[string]*ContainerState),
		msgQueue:   make(chan string, 100), // Buffer up to 100 alerts
	}

	log.Printf("🔥 Monitor started on %s", cfg.ServerName)
	log.Printf("⏱  Debounce: %s | Stability: %s", cfg.DebounceWindow, cfg.StabilityPeriod)

	// Start the Telegram Worker (Rate Limiter)
	go monitor.telegramWorker(ctx)

	// Send startup notification
	monitor.queueAlert(fmt.Sprintf(
		"<b>👻 Monitor Online | %s</b>\nMonitoring service has started/restarted.",
		cfg.ServerName,
	))

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
		return fmt.Errorf("create docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	if _, err := cli.Ping(ctx); err != nil {
		return fmt.Errorf("ping docker daemon: %w", err)
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
		case err, ok := <-errs:
			if !ok {
				return fmt.Errorf("docker error channel closed")
			}
			return fmt.Errorf("docker event stream: %w", err)
		case event, ok := <-msgs:
			if !ok {
				return fmt.Errorf("docker event channel closed")
			}
			m.processEvent(event)
		}
	}
}

func (m *Monitor) processEvent(event events.Message) {
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

	switch event.Action {
	case events.ActionDie:
		m.handleDie(state, event)
	case events.ActionStart:
		m.handleStart(state)
	case events.ActionHealthStatus:
		switch event.Actor.Attributes["health_status"] {
		case "unhealthy":
			m.handleUnhealthy(state)
		case "healthy":
			m.handleHealthy(state)
		}
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

		// Start debounce window with generation tracking
		s.debounceGen++
		gen := s.debounceGen
		s.DebounceTimer = time.AfterFunc(m.cfg.DebounceWindow, func() {
			m.mu.Lock()
			defer m.mu.Unlock()

			// Check if this timer was superseded
			if s.debounceGen != gen {
				return
			}

			if s.CrashCount > 1 {
				msg := fmt.Sprintf("Crash Loop: Died %d times in last %s", s.CrashCount, m.cfg.DebounceWindow)
				m.sendFormattedAlert(s.Name, "⚠️ Unstable", msg)
			}
			s.CrashCount = 0
		})
	}
}

func (m *Monitor) handleStart(s *ContainerState) {
	if !s.IsDown {
		return
	}

	if s.StabilityTimer != nil {
		s.StabilityTimer.Stop()
	}

	// Wait for stability period with generation tracking
	s.stabilityGen++
	gen := s.stabilityGen
	s.StabilityTimer = time.AfterFunc(m.cfg.StabilityPeriod, func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		// Check if this timer was superseded
		if s.stabilityGen != gen {
			return
		}

		s.IsDown = false
		s.CrashCount = 0
		if s.DebounceTimer != nil {
			s.DebounceTimer.Stop()
		}

		m.sendFormattedAlert(s.Name, "✅ Recovered", "Container is running and stable")
	})
}

func (m *Monitor) handleUnhealthy(s *ContainerState) {
	if s.IsUnhealthy {
		return // Already unhealthy, avoid spam
	}
	s.IsUnhealthy = true
	m.sendFormattedAlert(s.Name, "💊 Unhealthy", "Healthcheck failed")
}

func (m *Monitor) handleHealthy(s *ContainerState) {
	// Only care if we thought it was unhealthy
	if !s.IsUnhealthy {
		return
	}

	s.IsUnhealthy = false
	m.sendFormattedAlert(s.Name, "✅ Recovered", "Healthcheck passed (Service is healthy)")
}

// --- Helper Functions ---

func (m *Monitor) isContainerAllowed(name string) bool {
	// 1. Include List (Allowlist)
	if len(m.cfg.IncludeContainers) > 0 && !slices.Contains(m.cfg.IncludeContainers, name) {
		return false
	}
	// 2. Exclude List (Blocklist)
	if len(m.cfg.ExcludeContainers) > 0 && slices.Contains(m.cfg.ExcludeContainers, name) {
		return false
	}
	return true
}

// --- Alerting System ---

func (m *Monitor) sendFormattedAlert(name, status, details string) {
	m.queueAlert(fmt.Sprintf(
		"<b>%s | %s</b>\n\n"+
			"📦 <b>Container:</b> <code>%s</code>\n"+
			"📝 <b>Status:</b> %s\n"+
			"ℹ️ <b>Details:</b> %s",
		status, m.cfg.ServerName, name, status, details,
	))
}

func (m *Monitor) queueAlert(msg string) {
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
			m.drainQueue()
			return
		case msg := <-m.msgQueue:
			m.postTelegram(msg)
			// Wait before processing next message
			select {
			case <-ctx.Done():
				m.drainQueue()
				return
			case <-ticker.C:
			}
		}
	}
}

// drainQueue sends remaining messages on shutdown
func (m *Monitor) drainQueue() {
	for {
		select {
		case msg := <-m.msgQueue:
			m.postTelegram(msg)
		default:
			return
		}
	}
}

const maxRetries = 3

func (m *Monitor) postTelegram(text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.cfg.TelegramToken)
	payload := map[string]any{
		"chat_id":    m.cfg.TelegramChatID,
		"text":       text,
		"parse_mode": "HTML",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal TG payload: %v", err)
		return
	}

	var lastErr error
	for attempt := range maxRetries {
		if err := m.doTelegramRequest(url, body); err != nil {
			lastErr = err
			backoff := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s
			log.Printf("TG request failed (attempt %d/%d): %v, retrying in %s", attempt+1, maxRetries, err, backoff)
			time.Sleep(backoff)
			continue
		}
		return
	}
	log.Printf("TG request failed after %d attempts: %v", maxRetries, lastErr)
}

func (m *Monitor) doTelegramRequest(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API error: %s", resp.Status)
	}
	return nil
}
