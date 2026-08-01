package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/moloch--/generals-server/internal/app"
)

func main() {
	cfg := app.DefaultConfig()
	flag.StringVar(&cfg.ControlAddr, "control-listen", cfg.ControlAddr, "TCP control listen address")
	flag.StringVar(&cfg.RelayAddr, "relay-listen", cfg.RelayAddr, "UDP relay listen address")
	flag.StringVar(&cfg.HealthAddr, "health-listen", cfg.HealthAddr, "HTTP health/metrics listen address")
	flag.StringVar(&cfg.PublicHost, "public-host", cfg.PublicHost, "public relay hostname advertised to clients")
	flag.StringVar(&cfg.DataFile, "data-file", cfg.DataFile, "persistent profile database path")
	flag.StringVar(&cfg.TLSCertFile, "tls-cert", "", "PEM certificate for TLS control connections")
	flag.StringVar(&cfg.TLSKeyFile, "tls-key", "", "PEM private key for TLS control connections")
	flag.BoolVar(&cfg.AllowInsecurePasswordAuth, "allow-insecure-password-auth", false, "allow password login and persistent session tokens on non-TLS control connections")
	flag.IntVar(&cfg.MaxControlLineBytes, "max-control-message", cfg.MaxControlLineBytes, "maximum newline-delimited JSON control message size")
	flag.IntVar(&cfg.MaxControlConnections, "max-control-connections", cfg.MaxControlConnections, "maximum simultaneous authenticated and unauthenticated control connections")
	flag.IntVar(&cfg.MaxCommandsPerSecond, "max-commands-per-second", cfg.MaxCommandsPerSecond, "per-connection control command rate limit")
	flag.IntVar(&cfg.MaxChatMessagesPer10Secs, "max-chat-messages-per-10s", cfg.MaxChatMessagesPer10Secs, "per-player chat message limit per ten seconds")
	flag.IntVar(&cfg.MaxOnlinePlayers, "max-online-players", cfg.MaxOnlinePlayers, "maximum concurrent authenticated players")
	flag.IntVar(&cfg.MaxProfiles, "max-profiles", cfg.MaxProfiles, "maximum persistent profiles stored by this server")
	flag.IntVar(&cfg.MaxStagedGames, "max-staged-games", cfg.MaxStagedGames, "maximum concurrent open and active games")
	flag.IntVar(&cfg.MaxRelayPacketsPerSecond, "relay-packets-per-second", cfg.MaxRelayPacketsPerSecond, "per-player UDP packet rate limit")
	flag.IntVar(&cfg.MaxRelayBytesPerSecond, "relay-bytes-per-second", cfg.MaxRelayBytesPerSecond, "per-player UDP byte rate limit")
	flag.DurationVar(&cfg.GameIdleTimeout, "game-idle-timeout", cfg.GameIdleTimeout, "idle time before a relay allocation expires")
	flag.DurationVar(&cfg.StartReadyTimeout, "start-ready-timeout", cfg.StartReadyTimeout, "time allowed for every player to parse relay credentials")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server, err := app.NewServer(cfg, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Start(ctx); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
	logger.Info("Generals online server started",
		"control", server.ControlAddress(),
		"relay", server.RelayAddress(),
		"health", server.HealthAddress(),
		"public_host", cfg.PublicHost,
	)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
}
