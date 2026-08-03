package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/moloch--/generals-server/internal/app"
	"github.com/spf13/cobra"
)

const gracefulShutdownTimeout = 10 * time.Second

type onlineServer interface {
	Start(context.Context) error
	Shutdown(context.Context) error
	ControlAddress() string
	RelayAddress() string
	HealthAddress() string
	AdminAddress() string
	Errors() <-chan error
}

type serverFactory func(app.Config, *slog.Logger) (onlineServer, error)

type commandRunner struct {
	newServer serverFactory
	stdout    io.Writer
	stderr    io.Writer
	exitCode  int
}

func main() {
	os.Exit(runMain())
}

func runMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return execute(ctx, os.Args[1:], os.Stdout, os.Stderr, newAppServer)
}

func newAppServer(cfg app.Config, logger *slog.Logger) (onlineServer, error) {
	return app.NewServer(cfg, logger)
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer, newServer serverFactory) int {
	runner := &commandRunner{
		newServer: newServer,
		stdout:    stdout,
		stderr:    stderr,
	}
	command := newRootCommand(runner)
	command.SetArgs(normalizeLegacyArgs(args, command))

	executed, err := command.ExecuteContextC(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if executed == nil {
			executed = command
		}
		fmt.Fprint(stderr, executed.UsageString())
		return 2
	}
	return runner.exitCode
}

func newRootCommand(runner *commandRunner) *cobra.Command {
	cfg := app.DefaultConfig()
	command := &cobra.Command{
		Use:           "generals-server",
		Short:         "Run the GeneralsX Online multiplayer server",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		Run: func(command *cobra.Command, _ []string) {
			runner.run(command.Context(), cfg)
		},
	}
	command.SetOut(runner.stdout)
	command.SetErr(runner.stderr)

	flags := command.Flags()
	flags.StringVar(&cfg.ControlAddr, "control-listen", cfg.ControlAddr, "TCP control listen address")
	flags.StringVar(&cfg.RelayAddr, "relay-listen", cfg.RelayAddr, "UDP relay listen address")
	flags.StringVar(&cfg.HealthAddr, "health-listen", cfg.HealthAddr, "HTTP health/metrics listen address")
	flags.StringVar(&cfg.AdminAddr, "admin-listen", cfg.AdminAddr, "HTTP admin API and web interface listen address (disabled by default)")
	flags.StringVar(&cfg.AdminTokenFile, "admin-token-file", cfg.AdminTokenFile, "file containing the admin bearer token")
	flags.StringVar(&cfg.PublicHost, "public-host", cfg.PublicHost, "public relay hostname advertised to clients")
	flags.IntVar(&cfg.PublicRelayPort, "public-relay-port", cfg.PublicRelayPort, "public UDP relay port advertised to clients (zero uses the bound port)")
	flags.StringVar(&cfg.DataFile, "data-file", cfg.DataFile, "SQLite profile database path")
	flags.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "PEM certificate for TLS control connections")
	flags.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "PEM private key for TLS control connections")
	flags.BoolVar(&cfg.AllowInsecurePasswordAuth, "allow-insecure-password-auth", cfg.AllowInsecurePasswordAuth, "allow password login and persistent session tokens on non-TLS control connections")
	flags.IntVar(&cfg.MaxControlLineBytes, "max-control-message", cfg.MaxControlLineBytes, "maximum newline-delimited JSON control message size")
	flags.IntVar(&cfg.MaxControlConnections, "max-control-connections", cfg.MaxControlConnections, "maximum simultaneous authenticated and unauthenticated control connections")
	flags.IntVar(&cfg.MaxCommandsPerSecond, "max-commands-per-second", cfg.MaxCommandsPerSecond, "per-connection control command rate limit")
	flags.IntVar(&cfg.MaxChatMessagesPer10Secs, "max-chat-messages-per-10s", cfg.MaxChatMessagesPer10Secs, "per-player chat message limit per ten seconds")
	flags.IntVar(&cfg.MaxOnlinePlayers, "max-online-players", cfg.MaxOnlinePlayers, "maximum concurrent authenticated players")
	flags.IntVar(&cfg.MaxProfiles, "max-profiles", cfg.MaxProfiles, "maximum persistent profiles stored by this server")
	flags.IntVar(&cfg.MaxStagedGames, "max-staged-games", cfg.MaxStagedGames, "maximum concurrent open and active games")
	flags.IntVar(&cfg.MaxRelayPacketsPerSecond, "relay-packets-per-second", cfg.MaxRelayPacketsPerSecond, "per-player UDP packet rate limit")
	flags.IntVar(&cfg.MaxRelayBytesPerSecond, "relay-bytes-per-second", cfg.MaxRelayBytesPerSecond, "per-player UDP byte rate limit")
	flags.DurationVar(&cfg.GameIdleTimeout, "game-idle-timeout", cfg.GameIdleTimeout, "idle time before a relay allocation expires")
	flags.DurationVar(&cfg.StartReadyTimeout, "start-ready-timeout", cfg.StartReadyTimeout, "time allowed for every player to parse relay credentials")
	return command
}

func normalizeLegacyArgs(args []string, command *cobra.Command) []string {
	normalized := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			normalized = append(normalized, args[index:]...)
			break
		}

		name, hasValue, legacy := flagToken(argument)
		flag := command.Flags().Lookup(name)
		if legacy && (flag != nil || name == "help") {
			argument = "-" + argument
		}
		normalized = append(normalized, argument)

		if flag != nil && !hasValue && flag.NoOptDefVal == "" && index+1 < len(args) {
			index++
			normalized = append(normalized, args[index])
		}
	}
	if normalized == nil {
		return []string{}
	}
	return normalized
}

func flagToken(argument string) (name string, hasValue bool, legacy bool) {
	switch {
	case strings.HasPrefix(argument, "--") && len(argument) > 2:
		name = argument[2:]
	case strings.HasPrefix(argument, "-") && len(argument) > 2:
		name = argument[1:]
		legacy = true
	default:
		return "", false, false
	}
	if separator := strings.IndexByte(name, '='); separator >= 0 {
		name = name[:separator]
		hasValue = true
	}
	return name, hasValue, legacy
}

func (runner *commandRunner) run(ctx context.Context, cfg app.Config) {
	logger := slog.New(slog.NewTextHandler(runner.stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server, err := runner.newServer(cfg, logger)
	if err != nil {
		fmt.Fprintln(runner.stderr, err)
		runner.exitCode = 2
		return
	}

	if err := server.Start(ctx); err != nil {
		logger.Error("server failed", "error", err)
		runner.exitCode = 1
		return
	}
	logger.Info("Generals online server started",
		"control", server.ControlAddress(),
		"relay", server.RelayAddress(),
		"health", server.HealthAddress(),
		"admin", server.AdminAddress(),
		"public_host", cfg.PublicHost,
	)

	select {
	case <-ctx.Done():
	case err := <-server.Errors():
		if err != nil {
			logger.Error("server failed", "error", err)
			runner.exitCode = 1
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		runner.exitCode = 1
	}
}
