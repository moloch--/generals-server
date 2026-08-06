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
	cfg          Config
	log          *slog.Logger
	store        *ProfileStore
	relay        *Relay
	hub          *Hub
	control      *ControlServer
	health       *http.Server
	healthLn     net.Listener
	publicWeb    *http.Server
	publicWebLn  net.Listener
	admin        *http.Server
	adminLn      net.Listener
	adminHandler *adminHandler
	adminToken   adminTokenHash
	cancel       context.CancelFunc
	errors       chan error
	mu           sync.Mutex
	started      bool
	closed       bool
	shutdownDone chan struct{}
	shutdownErr  error
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := validatePublicHost(cfg.PublicHost); err != nil {
		return nil, fmt.Errorf("invalid public host: %w", err)
	}
	if cfg.PublicRelayPort < 0 || cfg.PublicRelayPort > 65535 {
		return nil, errors.New("public relay port must be zero or a valid UDP port")
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
	if err := validatePublicWebListenerPorts(cfg); err != nil {
		return nil, err
	}
	if (cfg.AdminAddr == "") != (cfg.AdminTokenFile == "") {
		return nil, errors.New("both --admin-listen and --admin-token-file are required when the admin server is enabled")
	}
	var adminToken adminTokenHash
	if cfg.AdminAddr != "" {
		var err error
		adminToken, err = loadAdminTokenHash(cfg.AdminTokenFile)
		if err != nil {
			return nil, err
		}
	}
	store, err := OpenProfileStoreWithLimit(cfg.DataFile, cfg.MaxProfiles)
	if err != nil {
		return nil, err
	}
	relay := NewRelay(cfg, logger)
	hub := NewHub(cfg, logger, store, relay)
	relay.SetGameExpiredHandler(hub.handleRelayGameExpired)
	control := NewControlServer(cfg, logger, store, hub)
	return &Server{
		cfg: cfg, log: logger, store: store, relay: relay, hub: hub, control: control,
		adminToken: adminToken, errors: make(chan error, 1),
	}, nil
}

func (s *Server) Start(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("server is closed")
	}
	if s.started {
		return errors.New("server is already started")
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	reportRuntimeError := func(err error) {
		select {
		case s.errors <- err:
		default:
		}
		cancel()
	}
	s.relay.runtimeError = reportRuntimeError
	s.control.runtimeError = reportRuntimeError
	if err := s.relay.Start(ctx); err != nil {
		cancel()
		s.closed = true
		return errors.Join(err, s.store.Close())
	}
	if err := s.control.Start(ctx); err != nil {
		_ = s.relay.Close()
		cancel()
		s.closed = true
		return errors.Join(err, s.store.Close())
	}
	listener, err := net.Listen("tcp", s.cfg.HealthAddr)
	if err != nil {
		_ = s.control.Close()
		_ = s.relay.Close()
		cancel()
		s.control.Wait()
		s.closed = true
		return errors.Join(fmt.Errorf("listen for health requests: %w", err), s.store.Close())
	}
	// GeneralsX @feature OpenAI 06/08/2026 Own the public HTTP listener independently from private handlers.
	var publicWebListener net.Listener
	if s.cfg.PublicWebAddr != "" {
		publicWebListener, err = net.Listen("tcp", s.cfg.PublicWebAddr)
		if err != nil {
			_ = listener.Close()
			_ = s.control.Close()
			_ = s.relay.Close()
			cancel()
			s.control.Wait()
			s.closed = true
			return errors.Join(fmt.Errorf("listen for public web requests: %w", err), s.store.Close())
		}
		s.publicWebLn = publicWebListener
	}
	var adminListener net.Listener
	if s.cfg.AdminAddr != "" {
		adminListener, err = net.Listen("tcp", s.cfg.AdminAddr)
		if err != nil {
			if publicWebListener != nil {
				_ = publicWebListener.Close()
			}
			_ = listener.Close()
			_ = s.control.Close()
			_ = s.relay.Close()
			cancel()
			s.control.Wait()
			s.closed = true
			return errors.Join(fmt.Errorf("listen for admin requests: %w", err), s.store.Close())
		}
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
		MaxHeaderBytes:    16 * 1024,
	}
	if publicWebListener != nil {
		s.publicWeb = &http.Server{
			Handler:           newPublicHandler(s.hub, s.store, s.log),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    16 * 1024,
		}
	}
	startedAt := time.Now().UTC()
	if adminListener != nil {
		s.adminLn = adminListener
		// GeneralsX @feature OpenAI 02/08/2026 Own realtime admin connections so shutdown closes every hijacked socket.
		s.adminHandler = newAdminHandler(s.adminToken, s.store, s.hub, s.relay, s.log, startedAt)
		s.admin = &http.Server{
			Handler:           s.adminHandler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    16 * 1024,
		}
	}
	go func() {
		if err := s.health.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			reportRuntimeError(fmt.Errorf("serve health requests: %w", err))
		}
	}()
	if s.publicWeb != nil {
		go func() {
			if err := s.publicWeb.Serve(publicWebListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				reportRuntimeError(fmt.Errorf("serve public web requests: %w", err))
			}
		}()
	}
	if s.admin != nil {
		go func() {
			if err := s.admin.Serve(adminListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				reportRuntimeError(fmt.Errorf("serve admin requests: %w", err))
			}
		}()
	}
	s.started = true
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.shutdownDone != nil {
		done := s.shutdownDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.shutdownErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	s.shutdownDone = done
	wasStarted := s.started
	s.started = false
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	health := s.health
	publicWeb := s.publicWeb
	admin := s.admin
	adminHandler := s.adminHandler
	s.mu.Unlock()

	var errs []error
	if wasStarted {
		if publicWeb != nil {
			if err := publicWeb.Shutdown(ctx); err != nil {
				errs = append(errs, err)
				_ = publicWeb.Close()
			}
		}
		if admin != nil {
			if err := adminHandler.shutdownEvents(ctx); err != nil {
				errs = append(errs, err)
			}
			if err := admin.Shutdown(ctx); err != nil {
				errs = append(errs, err)
				_ = admin.Close()
			}
		}
		s.hub.CloseAll()
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
		controlDone := make(chan struct{})
		go func() { s.control.Wait(); close(controlDone) }()
		select {
		case <-controlDone:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}
	if err := s.store.Close(); err != nil {
		errs = append(errs, err)
	}
	shutdownErr := errors.Join(errs...)
	s.mu.Lock()
	s.shutdownErr = shutdownErr
	close(done)
	s.mu.Unlock()
	return shutdownErr
}

func (s *Server) Errors() <-chan error { return s.errors }

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

// GeneralsX @feature OpenAI 06/08/2026 Report the independently bound public web address.
func (s *Server) PublicWebAddress() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicWebLn != nil {
		return s.publicWebLn.Addr().String()
	}
	return s.cfg.PublicWebAddr
}

func (s *Server) AdminAddress() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adminLn != nil {
		return s.adminLn.Addr().String()
	}
	return s.cfg.AdminAddr
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
