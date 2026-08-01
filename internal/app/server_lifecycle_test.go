package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServerConcurrentShutdownWaitsForSingleCleanup(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = ""
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs <- server.Shutdown(ctx)
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent shutdown: %v", err)
		}
	}
	if err := server.store.db.Ping(); err == nil {
		t.Fatal("profile database remained open after shutdown")
	}
}

func TestServerStartFailureClosesProfileStore(t *testing.T) {
	healthListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer healthListener.Close()

	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = healthListener.Addr().String()
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = ""
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("server started with an occupied health address")
	}
	if err := server.store.db.Ping(); err == nil {
		t.Fatal("profile database remained open after failed startup")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after failed startup: %v", err)
	}
}

func TestServerReportsFatalListenerFailures(t *testing.T) {
	tests := []struct {
		name       string
		want       string
		breakServe func(*Server) error
	}{
		{
			name: "control",
			want: "accept control connection",
			breakServe: func(server *Server) error {
				server.control.mu.RLock()
				listener := server.control.listener
				server.control.mu.RUnlock()
				return listener.Close()
			},
		},
		{
			name: "relay",
			want: "read UDP relay traffic",
			breakServe: func(server *Server) error {
				server.relay.mu.RLock()
				conn := server.relay.conn
				server.relay.mu.RUnlock()
				return conn.Close()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ControlAddr = "127.0.0.1:0"
			cfg.RelayAddr = "127.0.0.1:0"
			cfg.HealthAddr = "127.0.0.1:0"
			cfg.PublicHost = "127.0.0.1"
			cfg.DataFile = ""
			server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if err := server.Start(context.Background()); err != nil {
				t.Fatal(err)
			}

			if err := test.breakServe(server); err != nil {
				t.Fatalf("break listener: %v", err)
			}
			select {
			case err := <-server.Errors():
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("runtime error = %v, want text %q", err, test.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for runtime listener error")
			}

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				t.Fatalf("shutdown after listener failure: %v", err)
			}
		})
	}
}
