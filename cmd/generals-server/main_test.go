package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moloch--/generals-server/internal/app"
)

var existingServerFlagNames = []string{
	"allow-insecure-password-auth",
	"control-listen",
	"data-file",
	"game-idle-timeout",
	"health-listen",
	"max-chat-messages-per-10s",
	"max-commands-per-second",
	"max-control-connections",
	"max-control-message",
	"max-online-players",
	"max-profiles",
	"max-staged-games",
	"public-host",
	"public-relay-port",
	"relay-bytes-per-second",
	"relay-listen",
	"relay-packets-per-second",
	"start-ready-timeout",
	"tls-cert",
	"tls-key",
}

type fakeOnlineServer struct {
	start       func(context.Context) error
	shutdown    func(context.Context) error
	controlAddr string
	relayAddr   string
	healthAddr  string
	errors      <-chan error
}

func (server *fakeOnlineServer) Start(ctx context.Context) error {
	if server.start != nil {
		return server.start(ctx)
	}
	return nil
}

func (server *fakeOnlineServer) Shutdown(ctx context.Context) error {
	if server.shutdown != nil {
		return server.shutdown(ctx)
	}
	return nil
}

func (server *fakeOnlineServer) ControlAddress() string { return server.controlAddr }
func (server *fakeOnlineServer) RelayAddress() string   { return server.relayAddr }
func (server *fakeOnlineServer) HealthAddress() string  { return server.healthAddr }
func (server *fakeOnlineServer) Errors() <-chan error   { return server.errors }

func TestRootCommandRegistersExistingFlags(t *testing.T) {
	runner := &commandRunner{stdout: io.Discard, stderr: io.Discard}
	command := newRootCommand(runner)

	for _, name := range existingServerFlagNames {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is not registered", name)
		}
	}
}

func TestRootCommandUsesDefaultConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got app.Config
	factory := func(cfg app.Config, _ *slog.Logger) (onlineServer, error) {
		got = cfg
		return &fakeOnlineServer{}, nil
	}

	code := execute(ctx, []string{}, io.Discard, io.Discard, factory)
	if code != 0 {
		t.Fatalf("execute() = %d, want 0", code)
	}
	if want := app.DefaultConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %#v, want %#v", got, want)
	}
}

func TestRootCommandBindsAllFlags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got app.Config
	factory := func(cfg app.Config, _ *slog.Logger) (onlineServer, error) {
		got = cfg
		return &fakeOnlineServer{}, nil
	}
	args := []string{
		"--control-listen=127.0.0.1:31000",
		"--relay-listen", "127.0.0.1:31001",
		"--health-listen=127.0.0.1:31002",
		"--public-host", "relay.example.net",
		"--public-relay-port", "32001",
		"--data-file=/var/lib/generals/profiles.db",
		"--tls-cert", "/run/tls/cert.pem",
		"--tls-key=/run/tls/key.pem",
		"--allow-insecure-password-auth",
		"--max-control-message=131072",
		"--max-control-connections", "512",
		"--max-commands-per-second=120",
		"--max-chat-messages-per-10s", "20",
		"--max-online-players=256",
		"--max-profiles", "20000",
		"--max-staged-games=128",
		"--relay-packets-per-second", "900",
		"--relay-bytes-per-second=4194304",
		"--game-idle-timeout", "20m",
		"--start-ready-timeout=45s",
	}

	code := execute(ctx, args, io.Discard, io.Discard, factory)
	if code != 0 {
		t.Fatalf("execute() = %d, want 0", code)
	}
	want := app.DefaultConfig()
	want.ControlAddr = "127.0.0.1:31000"
	want.RelayAddr = "127.0.0.1:31001"
	want.HealthAddr = "127.0.0.1:31002"
	want.PublicHost = "relay.example.net"
	want.PublicRelayPort = 32001
	want.DataFile = "/var/lib/generals/profiles.db"
	want.TLSCertFile = "/run/tls/cert.pem"
	want.TLSKeyFile = "/run/tls/key.pem"
	want.AllowInsecurePasswordAuth = true
	want.MaxControlLineBytes = 131072
	want.MaxControlConnections = 512
	want.MaxCommandsPerSecond = 120
	want.MaxChatMessagesPer10Secs = 20
	want.MaxOnlinePlayers = 256
	want.MaxProfiles = 20000
	want.MaxStagedGames = 128
	want.MaxRelayPacketsPerSecond = 900
	want.MaxRelayBytesPerSecond = 4194304
	want.GameIdleTimeout = 20 * time.Minute
	want.StartReadyTimeout = 45 * time.Second
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %#v, want %#v", got, want)
	}
}

func TestLegacyLongFlagNormalization(t *testing.T) {
	command := newRootCommand(&commandRunner{stdout: io.Discard, stderr: io.Discard})
	for _, name := range append(existingServerFlagNames, "help") {
		argument := "-" + name + "=value"
		if name == "help" {
			argument = "-help"
		}
		got := normalizeLegacyArgs([]string{argument}, command)
		want := []string{"-" + argument}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("normalizeLegacyArgs(%q) = %q, want %q", argument, got, want)
		}
	}

	got := normalizeLegacyArgs([]string{"-tls-cert", "-public-host", "--", "-data-file=x"}, command)
	want := []string{"--tls-cert", "-public-host", "--", "-data-file=x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("value/terminator normalization = %q, want %q", got, want)
	}
	if got := normalizeLegacyArgs([]string{"-unknown=value"}, command); !reflect.DeepEqual(got, []string{"-unknown=value"}) {
		t.Errorf("unknown flag normalization = %q, want unchanged", got)
	}
}

func TestLegacyLongFlagsReachConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got app.Config
	factory := func(cfg app.Config, _ *slog.Logger) (onlineServer, error) {
		got = cfg
		return &fakeOnlineServer{}, nil
	}
	args := []string{
		"-public-host", "legacy.example.net",
		"-public-relay-port", "32002",
		"-data-file=/tmp/legacy.db",
		"-allow-insecure-password-auth=true",
		"-max-profiles", "4321",
		"-game-idle-timeout=2m",
	}

	if code := execute(ctx, args, io.Discard, io.Discard, factory); code != 0 {
		t.Fatalf("execute() = %d, want 0", code)
	}
	if got.PublicHost != "legacy.example.net" || got.PublicRelayPort != 32002 || got.DataFile != "/tmp/legacy.db" ||
		!got.AllowInsecurePasswordAuth || got.MaxProfiles != 4321 || got.GameIdleTimeout != 2*time.Minute {
		t.Fatalf("legacy flags produced unexpected config: %#v", got)
	}
}

func TestHelpAndCommandLineErrors(t *testing.T) {
	neverStart := func(app.Config, *slog.Logger) (onlineServer, error) {
		t.Fatal("server factory was called")
		return nil, nil
	}

	for _, helpFlag := range []string{"--help", "-help", "-h"} {
		t.Run("help "+helpFlag, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := execute(context.Background(), []string{helpFlag}, &stdout, &stderr, neverStart); code != 0 {
				t.Fatalf("execute() = %d, want 0; stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Usage:") || !strings.Contains(stdout.String(), "--control-listen") {
				t.Fatalf("help output missing usage or flags: %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("help stderr = %q, want empty", stderr.String())
			}
		})
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown flag", args: []string{"--unknown"}, want: "unknown flag"},
		{name: "invalid duration", args: []string{"--game-idle-timeout=soon"}, want: "invalid argument"},
		{name: "positional argument", args: []string{"unexpected"}, want: "unknown command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := execute(context.Background(), test.args, &stdout, &stderr, neverStart); code != 2 {
				t.Fatalf("execute() = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), test.want) || !strings.Contains(stderr.String(), "Usage:") {
				t.Fatalf("stderr = %q, want %q and usage", stderr.String(), test.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestServerLifecycleUsesIndependentShutdownContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan context.Context, 1)
	type shutdownState struct {
		err       error
		remaining time.Duration
		deadline  bool
	}
	shutdown := make(chan shutdownState, 1)
	server := &fakeOnlineServer{
		start: func(startCtx context.Context) error {
			started <- startCtx
			return nil
		},
		shutdown: func(shutdownCtx context.Context) error {
			deadline, ok := shutdownCtx.Deadline()
			shutdown <- shutdownState{err: shutdownCtx.Err(), remaining: time.Until(deadline), deadline: ok}
			return nil
		},
		controlAddr: "127.0.0.1:29900",
		relayAddr:   "127.0.0.1:27901",
		healthAddr:  "127.0.0.1:8080",
	}
	factory := func(app.Config, *slog.Logger) (onlineServer, error) { return server, nil }
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- execute(ctx, []string{"--public-host=relay.example.net"}, &stdout, &stderr, factory)
	}()

	var startCtx context.Context
	select {
	case startCtx = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start")
	}
	cancel()
	select {
	case <-startCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Start context was not canceled")
	}

	var state shutdownState
	select {
	case state = <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
	if state.err != nil {
		t.Fatalf("shutdown context was already canceled: %v", state.err)
	}
	if !state.deadline || state.remaining < 8*time.Second || state.remaining > gracefulShutdownTimeout {
		t.Fatalf("shutdown deadline remaining = %s, deadline=%v", state.remaining, state.deadline)
	}
	select {
	case code := <-result:
		if code != 0 {
			t.Fatalf("execute() = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command did not return after shutdown")
	}
	for _, text := range []string{
		`msg="Generals online server started"`,
		"control=127.0.0.1:29900",
		"relay=127.0.0.1:27901",
		"health=127.0.0.1:8080",
		"public_host=relay.example.net",
	} {
		if !strings.Contains(stdout.String(), text) {
			t.Errorf("startup log %q does not contain %q", stdout.String(), text)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestServerExitClasses(t *testing.T) {
	t.Run("construction error", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		factory := func(app.Config, *slog.Logger) (onlineServer, error) {
			return nil, errors.New("open database")
		}
		if code := execute(context.Background(), []string{}, &stdout, &stderr, factory); code != 2 {
			t.Fatalf("execute() = %d, want 2", code)
		}
		if stderr.String() != "open database\n" || stdout.Len() != 0 {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	t.Run("start error", func(t *testing.T) {
		var shutdownCalls atomic.Int32
		server := &fakeOnlineServer{
			start: func(context.Context) error { return errors.New("bind failed") },
			shutdown: func(context.Context) error {
				shutdownCalls.Add(1)
				return nil
			},
		}
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		factory := func(app.Config, *slog.Logger) (onlineServer, error) { return server, nil }
		if code := execute(context.Background(), []string{}, &stdout, &stderr, factory); code != 1 {
			t.Fatalf("execute() = %d, want 1", code)
		}
		if !strings.Contains(stdout.String(), `level=ERROR msg="server failed" error="bind failed"`) {
			t.Fatalf("stdout = %q", stdout.String())
		}
		if stderr.Len() != 0 || shutdownCalls.Load() != 0 {
			t.Fatalf("stderr=%q shutdown calls=%d", stderr.String(), shutdownCalls.Load())
		}
	})

	t.Run("shutdown error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		server := &fakeOnlineServer{
			shutdown: func(context.Context) error { return errors.New("drain failed") },
		}
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		factory := func(app.Config, *slog.Logger) (onlineServer, error) { return server, nil }
		if code := execute(ctx, []string{}, &stdout, &stderr, factory); code != 1 {
			t.Fatalf("execute() = %d, want 1", code)
		}
		if !strings.Contains(stdout.String(), `level=ERROR msg="graceful shutdown failed" error="drain failed"`) {
			t.Fatalf("stdout = %q", stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})

	t.Run("runtime error", func(t *testing.T) {
		runtimeErrors := make(chan error, 1)
		runtimeErrors <- errors.New("health failed")
		var shutdownCalls atomic.Int32
		server := &fakeOnlineServer{
			errors: runtimeErrors,
			shutdown: func(ctx context.Context) error {
				shutdownCalls.Add(1)
				if ctx.Err() != nil {
					t.Fatalf("shutdown context was already canceled: %v", ctx.Err())
				}
				return nil
			},
		}
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		factory := func(app.Config, *slog.Logger) (onlineServer, error) { return server, nil }
		if code := execute(context.Background(), []string{}, &stdout, &stderr, factory); code != 1 {
			t.Fatalf("execute() = %d, want 1", code)
		}
		if !strings.Contains(stdout.String(), `level=ERROR msg="server failed" error="health failed"`) {
			t.Fatalf("stdout = %q", stdout.String())
		}
		if stderr.Len() != 0 || shutdownCalls.Load() != 1 {
			t.Fatalf("stderr=%q shutdown calls=%d", stderr.String(), shutdownCalls.Load())
		}
	})
}
