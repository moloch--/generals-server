package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type Server struct {
	cfg      Config
	log      *slog.Logger
	store    *ProfileStore
	relay    *Relay
	hub      *Hub
	control  *ControlServer
	health   *http.Server
	healthLn net.Listener
	cancel   context.CancelFunc
	mu       sync.Mutex
	started  bool
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := validatePublicHost(cfg.PublicHost); err != nil {
		return nil, fmt.Errorf("invalid public host: %w", err)
	}
	if cfg.MaxControlLineBytes < 1024 {
		return nil, errors.New("max control message must be at least 1024 bytes")
	}
	if cfg.MaxOnlinePlayers < 2 || cfg.MaxOnlinePlayers > 512 || cfg.MaxStagedGames < 1 || cfg.MaxStagedGames > 256 {
		return nil, errors.New("online player and staged game limits are outside supported bounds")
	}
	if cfg.MaxProfiles < 1 || cfg.MaxProfiles > maxSupportedProfiles {
		return nil, fmt.Errorf("persistent profile limit must be between 1 and %d", maxSupportedProfiles)
	}
	if cfg.MaxControlConnections < cfg.MaxOnlinePlayers || cfg.MaxControlConnections > 2048 ||
		cfg.MaxCommandsPerSecond < 1 || cfg.MaxChatMessagesPer10Secs < 1 {
		return nil, errors.New("control connection and command rate limits are outside supported bounds")
	}
	if cfg.MaxRelayPacketsPerSecond < 1 || cfg.MaxRelayBytesPerSecond < relayHeaderSize {
		return nil, errors.New("relay rate limits must be positive")
	}
	if cfg.GameIdleTimeout <= 0 || cfg.StartReadyTimeout <= 0 || cfg.StartReadyTimeout > 5*time.Minute || cfg.SessionTTL <= 0 {
		return nil, errors.New("game idle timeout, start-ready timeout, and session TTL are outside supported bounds")
	}
	if cfg.ControlReadTimeout <= 0 || cfg.ControlWriteTimeout <= 0 {
		return nil, errors.New("control read and write timeouts must be positive")
	}
	store, err := OpenProfileStoreWithLimit(cfg.DataFile, cfg.MaxProfiles)
	if err != nil {
		return nil, err
	}
	relay := NewRelay(cfg, logger)
	hub := NewHub(cfg, logger, store, relay)
	control := NewControlServer(cfg, logger, store, hub)
	return &Server{cfg: cfg, log: logger, store: store, relay: relay, hub: hub, control: control}, nil
}

func (s *Server) Start(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("server is already started")
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	if err := s.relay.Start(ctx); err != nil {
		cancel()
		return err
	}
	if err := s.control.Start(ctx); err != nil {
		_ = s.relay.Close()
		cancel()
		return err
	}
	listener, err := net.Listen("tcp", s.cfg.HealthAddr)
	if err != nil {
		_ = s.control.Close()
		_ = s.relay.Close()
		cancel()
		return fmt.Errorf("listen for health requests: %w", err)
	}
	s.healthLn = listener
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.health = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		if err := s.health.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("health server failed", "error", err)
			cancel()
		}
	}()
	s.started = true
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	if s.cancel != nil {
		s.cancel()
	}
	health := s.health
	s.mu.Unlock()

	s.hub.CloseAll()
	var errs []error
	if health != nil {
		if err := health.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.control.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, err)
	}
	if err := s.relay.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, err)
	}
	done := make(chan struct{})
	go func() { s.control.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}

func (s *Server) ControlAddress() string { return s.control.Address() }
func (s *Server) RelayAddress() string   { return s.relay.Address() }
func (s *Server) HealthAddress() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.healthLn != nil {
		return s.healthLn.Addr().String()
	}
	return s.cfg.HealthAddr
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Status   string     `json:"status"`
		Protocol int        `json:"protocol"`
		Control  string     `json:"control"`
		Relay    string     `json:"relay"`
		Hub      HubStats   `json:"hub"`
		UDP      RelayStats `json:"udp"`
	}{
		Status: "ok", Protocol: ProtocolVersion, Control: s.ControlAddress(),
		Relay: s.RelayAddress(), Hub: s.hub.Stats(), UDP: s.relay.Stats(),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	hub := s.hub.Stats()
	relay := s.relay.Stats()
	metrics := []struct {
		name  string
		help  string
		kind  string
		value uint64
	}{
		{"generals_online_players", "Currently authenticated control clients.", "gauge", uint64(hub.OnlinePlayers)},
		{"generals_online_open_games", "Currently joinable staged games.", "gauge", uint64(hub.OpenGames)},
		{"generals_online_active_games", "Currently started staged games.", "gauge", uint64(hub.ActiveGames)},
		{"generals_online_quickmatch_queued", "Players waiting for quickmatch.", "gauge", uint64(hub.QueuedPlayers)},
		{"generals_relay_datagrams_in_total", "Authenticated and unauthenticated relay datagrams received.", "counter", relay.DatagramsIn},
		{"generals_relay_datagrams_out_total", "Relay datagrams sent.", "counter", relay.DatagramsOut},
		{"generals_relay_bytes_in_total", "Relay bytes received.", "counter", relay.BytesIn},
		{"generals_relay_bytes_out_total", "Relay bytes sent.", "counter", relay.BytesOut},
		{"generals_relay_dropped_malformed_total", "Malformed relay datagrams dropped.", "counter", relay.DroppedMalformed},
		{"generals_relay_dropped_auth_total", "Relay datagrams dropped for authentication failure.", "counter", relay.DroppedAuth},
		{"generals_relay_dropped_rate_limit_total", "Relay datagrams dropped by rate limiting.", "counter", relay.DroppedRateLimit},
		{"generals_relay_dropped_no_endpoint_total", "Relay deliveries dropped when a recipient's pre-bind queue was full.", "counter", relay.DroppedNoEndpoint},
		{"generals_relay_buffered_until_bind_total", "Relay deliveries buffered until a recipient registered its UDP endpoint.", "counter", relay.BufferedUntilBind},
		{"generals_relay_active_games", "Current UDP relay allocations.", "gauge", uint64(relay.ActiveGames)},
	}
	for _, metric := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", metric.name, metric.help, metric.name, metric.kind, metric.name, strconv.FormatUint(metric.value, 10))
	}
}
