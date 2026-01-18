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
	"sync"
	"syscall"
	"time"

	"github.com/cristalhq/aconfig"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Config holds all settings
type Config struct {
	TelegramToken     string        `env:"TG_TOKEN" required:"true"`
	TelegramChatID    int64         `env:"TG_CHAT_ID" required:"true"`
	ServerName        string        `env:"SERVER_NAME" default:"MyServer"`
	IncludeContainers []string      `env:"INCLUDE_CONTAINERS"`
	ExcludeContainers []string      `env:"EXCLUDE_CONTAINERS"`
	IgnoreExitZero    bool          `env:"IGNORE_EXIT_ZERO" default:"true"`
	DebounceWindow    time.Duration `env:"DEBOUNCE_WINDOW" default:"5m"`  // Time to accumulate errors
	StabilityPeriod   time.Duration `env:"STABILITY_PERIOD" default:"30s"` // Time to wait before marking "Recovered"
}

// ContainerState tracks the health of a single container
type ContainerState struct {
	Name           string
	CrashCount     int
	LastCrashTime  time.Time
	IsDown         bool
	DebounceTimer  *time.Timer // Timer for sending "Crash Loop" summary
	StabilityTimer *time.Timer // Timer for sending "Recovered" message
}

// Monitor holds the global state
type Monitor struct {
	cfg        Config
	httpClient *http.Client
	
	mu     sync.Mutex
	states map[string]*ContainerState
}

func main() {
	var cfg Config
	loader := aconfig.LoaderFor(&cfg, aconfig.Config{
		SkipFlags: true,
		SkipFiles: true,
		EnvPrefix: "",
	})
	if err := loader.Load(); err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	monitor := &Monitor{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		states:     make(map[string]*ContainerState),
	}

	log.Printf("🔥 Monitor started on %s", cfg.ServerName)
	log.Printf("⏱  Debounce: %s | Stability: %s", cfg.DebounceWindow, cfg.StabilityPeriod)

	// Retry loop for Docker connection
	for {
		if err := run(ctx, monitor); err != nil {
			log.Printf("❌ Error: %v. Retrying in 5s...", err)
		}
		select {
		case <-ctx.Done():
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

	// Filter: We need both 'die' (crash) and 'start' (recovery)
	args := filters.NewArgs()
	args.Add("type", "container")
	args.Add("event", "die")
	args.Add("event", "start")

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

	// 1. Filter Logic
	if !m.isContainerAllowed(containerName) {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Initialize state if not exists
	state, exists := m.states[containerName]
	if !exists {
		state = &ContainerState{Name: containerName}
		m.states[containerName] = state
	}

	switch event.Action {
	case "die":
		m.handleDie(ctx, state, event)
	case "start":
		m.handleStart(ctx, state)
	}
}

// handleDie processes a crash
func (m *Monitor) handleDie(ctx context.Context, s *ContainerState, event events.Message) {
	exitCode := event.Actor.Attributes["exitCode"]
	if m.cfg.IgnoreExitZero && exitCode == "0" {
		return
	}

	// Stop any pending recovery timer (it didn't survive the stability period)
	if s.StabilityTimer != nil {
		s.StabilityTimer.Stop()
	}

	s.IsDown = true
	s.LastCrashTime = time.Unix(event.Time, 0)
	s.CrashCount++

	// LOGIC:
	// If this is the FIRST crash in a while: Send Alert immediately + Start Debounce Timer.
	// If we are already in a debounce window: Just increment count (suppress alert).
	
	if s.CrashCount == 1 {
		// First crash: Send Notification
		go m.sendAlert(ctx, s.Name, exitCode, "🔴 Down", fmt.Sprintf("Process exited with code %s", exitCode))

		// Start Debounce/Accumulation Window
		s.DebounceTimer = time.AfterFunc(m.cfg.DebounceWindow, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			
			// Window finished. If we had multiple crashes, send summary.
			if s.CrashCount > 1 {
				msg := fmt.Sprintf("Crash Loop Detected! Died %d times in last %s", s.CrashCount, m.cfg.DebounceWindow)
				go m.sendAlert(context.Background(), s.Name, "VAR", "⚠️ Unstable", msg)
			}
			
			// Reset count so next crash triggers immediate alert again? 
			// Or keep counting? Let's reset to allow new "First" alerts later.
			// But keep IsDown=true.
			s.CrashCount = 0 
		})
	}
}

// handleStart processes a potential recovery
func (m *Monitor) handleStart(ctx context.Context, s *ContainerState) {
	// If it wasn't marked as down, we don't care about starts (maybe manual restart)
	if !s.IsDown {
		return
	}

	// Stop any existing stability timer (e.g. rapid restart)
	if s.StabilityTimer != nil {
		s.StabilityTimer.Stop()
	}

	// Start a Stability Timer.
	// Only send "Recovered" if it stays alive for StabilityPeriod.
	s.StabilityTimer = time.AfterFunc(m.cfg.StabilityPeriod, func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		// If we are here, no 'die' event happened for X seconds.
		s.IsDown = false
		s.CrashCount = 0 // Reset crash stats
		
		// Cancel Debounce timer if exists (we are healthy now)
		if s.DebounceTimer != nil {
			s.DebounceTimer.Stop()
		}

		go m.sendAlert(context.Background(), s.Name, "0", "✅ Recovered", "Container is running and stable")
	})
}

// Helper to check filters
func (m *Monitor) isContainerAllowed(name string) bool {
	// Check Include list
	if len(m.cfg.IncludeContainers) > 0 {
		found := false
		for _, allowed := range m.cfg.IncludeContainers {
			if allowed == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	// Check Exclude list
	for _, excluded := range m.cfg.ExcludeContainers {
		if excluded == name {
			return false
		}
	}
	return true
}

// Sending Logic
func (m *Monitor) sendAlert(ctx context.Context, name, exitCode, statusEmoji, details string) {
	msg := fmt.Sprintf(
		"<b>%s | %s</b>\n\n"+
			"📦 <b>Container:</b> <code>%s</code>\n"+
			"🖥 <b>Server:</b> %s\n"+
			"📝 <b>Status:</b> %s\n"+
			"ℹ️ <b>Details:</b> %s",
		statusEmoji, m.cfg.ServerName,
		name,
		m.cfg.ServerName,
		statusEmoji, // e.g. "🔴 Down" or "✅ Recovered"
		details,
	)

	m.postTelegram(ctx, msg)
}

func (m *Monitor) postTelegram(ctx context.Context, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.cfg.TelegramToken)
	payload := map[string]interface{}{
		"chat_id":    m.cfg.TelegramChatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.Printf("Failed to send TG: %v", err)
		return
	}
	defer resp.Body.Close()
}