package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewServerValidatesPublicHost(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"relay.example.net", "localhost", "a-b.example", "127.0.0.1"} {
		cfg := DefaultConfig()
		cfg.PublicHost = host
		cfg.DataFile = ""
		server, err := NewServer(cfg, nil)
		if err != nil {
			t.Errorf("valid public host %q was rejected: %v", host, err)
			continue
		}
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("close server for public host %q: %v", host, err)
		}
	}

	for _, host := range []string{
		"", " relay.example.net", "relay.example.net ", "tls://relay.example.net",
		"relay.example.net:27901", "relay host", "bad_name.example", "-bad.example",
		"bad-.example", "bad..example", "relay.example.net.", "[::1]", "2001:db8::1", "::ffff:127.0.0.1",
		"999.999.999.999", "rélay.example",
	} {
		cfg := DefaultConfig()
		cfg.PublicHost = host
		cfg.DataFile = ""
		if _, err := NewServer(cfg, nil); err == nil {
			t.Errorf("invalid public host %q was accepted", host)
		}
	}
}

func TestNewServerValidatesStartReadyTimeout(t *testing.T) {
	t.Parallel()

	valid := DefaultConfig()
	valid.DataFile = ""
	valid.StartReadyTimeout = 5 * time.Minute
	server, err := NewServer(valid, nil)
	if err != nil {
		t.Fatalf("maximum start-ready timeout was rejected: %v", err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("close server: %v", err)
	}

	for _, timeout := range []time.Duration{0, -time.Second, 5*time.Minute + time.Nanosecond} {
		cfg := DefaultConfig()
		cfg.DataFile = ""
		cfg.StartReadyTimeout = timeout
		if _, err := NewServer(cfg, nil); err == nil {
			t.Errorf("invalid start-ready timeout %s was accepted", timeout)
		}
	}
}

func TestNewServerValidatesPublicRelayPort(t *testing.T) {
	t.Parallel()

	for _, port := range []int{0, 1, 27901, 65535} {
		cfg := DefaultConfig()
		cfg.DataFile = ""
		cfg.PublicRelayPort = port
		server, err := NewServer(cfg, nil)
		if err != nil {
			t.Errorf("valid public relay port %d was rejected: %v", port, err)
			continue
		}
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("close server for public relay port %d: %v", port, err)
		}
	}

	for _, port := range []int{-1, 65536} {
		cfg := DefaultConfig()
		cfg.DataFile = ""
		cfg.PublicRelayPort = port
		if _, err := NewServer(cfg, nil); err == nil {
			t.Errorf("invalid public relay port %d was accepted", port)
		}
	}
}

func TestNewServerRequiresPublicWebToUseDedicatedNonzeroPort(t *testing.T) {
	tokenFile := writeTestAdminToken(t)
	for _, test := range []struct {
		name       string
		publicAddr string
		healthAddr string
		adminAddr  string
		want       string
	}{
		{
			name: "health", publicAddr: "0.0.0.0:8080", healthAddr: "127.0.0.1:8080",
			want: "public web and health listeners must use different nonzero TCP ports",
		},
		{
			name: "admin", publicAddr: "[::]:8081", healthAddr: "127.0.0.1:8080", adminAddr: "127.0.0.1:8081",
			want: "public web and admin listeners must use different nonzero TCP ports",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.DataFile = ""
			cfg.PublicWebAddr = test.publicAddr
			cfg.HealthAddr = test.healthAddr
			cfg.AdminAddr = test.adminAddr
			if test.adminAddr != "" {
				cfg.AdminTokenFile = tokenFile
			}
			if _, err := NewServer(cfg, nil); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewServer() error=%v, want text %q", err, test.want)
			}
		})
	}

	cfg := DefaultConfig()
	cfg.DataFile = ""
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicWebAddr = "127.0.0.1:0"
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.AdminTokenFile = tokenFile
	server, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatalf("ephemeral listener ports were rejected: %v", err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
