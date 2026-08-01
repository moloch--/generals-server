package app

import (
	"context"
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
