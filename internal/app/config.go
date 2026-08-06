package app

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ControlAddr               string
	RelayAddr                 string
	HealthAddr                string
	AdminAddr                 string
	AdminTokenFile            string
	PublicWebAddr             string
	PublicHost                string
	PublicRelayPort           int
	DataFile                  string
	TLSCertFile               string
	TLSKeyFile                string
	AllowInsecurePasswordAuth bool
	MaxControlLineBytes       int
	MaxControlConnections     int
	MaxCommandsPerSecond      int
	MaxChatMessagesPer10Secs  int
	MaxOnlinePlayers          int
	MaxProfiles               int
	MaxStagedGames            int
	MaxRelayPacketsPerSecond  int
	MaxRelayBytesPerSecond    int
	GameIdleTimeout           time.Duration
	StartReadyTimeout         time.Duration
	ControlWriteTimeout       time.Duration
	ControlReadTimeout        time.Duration
	SessionTTL                time.Duration
}

func DefaultConfig() Config {
	return Config{
		ControlAddr:              ":29900",
		RelayAddr:                ":27901",
		HealthAddr:               ":8080",
		PublicHost:               "localhost",
		DataFile:                 "data/profiles.db",
		MaxControlLineBytes:      64 * 1024,
		MaxControlConnections:    256,
		MaxCommandsPerSecond:     60,
		MaxChatMessagesPer10Secs: 10,
		MaxOnlinePlayers:         128,
		MaxProfiles:              defaultMaxProfiles,
		MaxStagedGames:           64,
		MaxRelayPacketsPerSecond: 600,
		MaxRelayBytesPerSecond:   2 * 1024 * 1024,
		GameIdleTimeout:          15 * time.Minute,
		StartReadyTimeout:        15 * time.Second,
		ControlWriteTimeout:      10 * time.Second,
		ControlReadTimeout:       90 * time.Second,
		SessionTTL:               24 * time.Hour,
	}
}

// GeneralsX @feature OpenAI 06/08/2026 Keep the public web listener off every private HTTP port.
func validatePublicWebListenerPorts(cfg Config) error {
	publicPort, enabled := configuredNonzeroTCPPort(cfg.PublicWebAddr)
	if !enabled {
		return nil
	}
	if adminPort, ok := configuredNonzeroTCPPort(cfg.AdminAddr); ok && adminPort == publicPort {
		return errors.New("public web and admin listeners must use different nonzero TCP ports")
	}
	if healthPort, ok := configuredNonzeroTCPPort(cfg.HealthAddr); ok && healthPort == publicPort {
		return errors.New("public web and health listeners must use different nonzero TCP ports")
	}
	return nil
}

func configuredNonzeroTCPPort(address string) (int, bool) {
	if address == "" {
		return 0, false
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

func validatePublicHost(host string) error {
	if host == "" || host != strings.TrimSpace(host) || len(host) > 253 {
		return errors.New("public host must be a non-empty DNS name or IPv4 address")
	}
	for _, character := range host {
		if character > 0x7f {
			return errors.New("public host must contain ASCII characters only")
		}
	}
	if strings.Contains(host, ":") {
		return errors.New("public host must not contain a scheme, port, or IPv6 syntax")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return errors.New("public host must be an IPv4 address or DNS name")
		}
		return nil
	}

	numeric := true
	for _, character := range host {
		if (character < '0' || character > '9') && character != '.' {
			numeric = false
			break
		}
	}
	if numeric {
		return errors.New("public host contains an invalid IPv4 address")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("public host contains an invalid DNS label")
		}
		for _, character := range label {
			if (character < 'A' || character > 'Z') &&
				(character < 'a' || character > 'z') &&
				(character < '0' || character > '9') && character != '-' {
				return errors.New("public host contains an invalid DNS character")
			}
		}
	}
	return nil
}
