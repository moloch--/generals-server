package app

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestRelayAuthenticatesAndPreservesPayload(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.PublicHost = "relay.test"
	cfg.GameIdleTimeout = time.Minute
	relay := NewRelay(cfg, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := relay.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	const gameID = uint64(0x1020304050607080)
	members := []Member{
		{UserID: 10, DisplayName: "Host", Host: true, Slot: 0},
		{UserID: 20, DisplayName: "Peer", Slot: 2},
	}
	credentials, err := relay.Allocate(gameID, members)
	if err != nil {
		t.Fatal(err)
	}
	if credentials[20].Slot != 2 {
		t.Fatalf("relay collapsed sparse slot to %d", credentials[20].Slot)
	}
	serverAddr, err := net.ResolveUDPAddr("udp", relay.Address())
	if err != nil {
		t.Fatal(err)
	}
	hostConn := listenTestUDP(t)
	defer hostConn.Close()
	peerConn := listenTestUDP(t)
	defer peerConn.Close()
	hostToken := decodeRelayToken(t, credentials[10].Token)
	peerToken := decodeRelayToken(t, credentials[20].Token)

	bindAndRequireAck(t, hostConn, serverAddr, gameID, 0, hostToken)
	bindAndRequireAck(t, peerConn, serverAddr, gameID, 2, peerToken)

	payload := []byte{0x44, 0x00, 0xf0, 0x0d, 0xaa, 0xbb, 0xcc}
	frame := makeRelayPacket(relayKindData, 0, 2, gameID, hostToken, payload)
	if _, err := hostConn.WriteToUDP(frame, serverAddr); err != nil {
		t.Fatal(err)
	}
	received := readTestUDP(t, peerConn)
	if received[5] != relayKindDataOut || received[6] != 0 || received[7] != 2 {
		t.Fatalf("unexpected relay header: %x", received[:relayHeaderSize])
	}
	var receivedToken relayToken
	copy(receivedToken[:], received[16:32])
	if receivedToken != peerToken {
		t.Fatal("server-to-client frame did not use the recipient token")
	}
	if string(received[relayHeaderSize:]) != string(payload) {
		t.Fatalf("payload changed: got %x want %x", received[relayHeaderSize:], payload)
	}

	badToken := hostToken
	badToken[0] ^= 0xff
	if _, err := hostConn.WriteToUDP(makeRelayPacket(relayKindData, 0, 2, gameID, badToken, payload), serverAddr); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	if err := peerConn.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	if _, _, err := peerConn.ReadFromUDP(buf); err == nil {
		t.Fatal("frame with a bad token was relayed")
	}
	if relay.Stats().DroppedAuth == 0 {
		t.Fatal("bad token was not counted as an authentication drop")
	}
}

func TestRelayBuffersInitialDatagramUntilRecipientBinds(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.PublicHost = "relay.test"
	relay := NewRelay(cfg, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := relay.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	const gameID = uint64(0x0102030405060708)
	credentials, err := relay.Allocate(gameID, []Member{
		{UserID: 1, DisplayName: "Early", Host: true, Slot: 0},
		{UserID: 2, DisplayName: "Delayed", Slot: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	serverAddr, err := net.ResolveUDPAddr("udp", relay.Address())
	if err != nil {
		t.Fatal(err)
	}
	early := listenTestUDP(t)
	defer early.Close()
	delayed := listenTestUDP(t)
	defer delayed.Close()
	earlyToken := decodeRelayToken(t, credentials[1].Token)
	delayedToken := decodeRelayToken(t, credentials[2].Token)
	bindAndRequireAck(t, early, serverAddr, gameID, 0, earlyToken)
	payload := []byte{0xde, 0xad, 0xfa, 0xce}
	if _, err := early.WriteToUDP(makeRelayPacket(relayKindData, 0, 1, gameID, earlyToken, payload), serverAddr); err != nil {
		t.Fatal(err)
	}

	bindAndRequireAck(t, delayed, serverAddr, gameID, 1, delayedToken)
	queued := readTestUDP(t, delayed)
	if queued[5] != relayKindDataOut || queued[6] != 0 || queued[7] != 1 ||
		string(queued[relayHeaderSize:]) != string(payload) {
		t.Fatalf("unexpected buffered relay frame: %x", queued)
	}
	if relay.Stats().BufferedUntilBind != 1 || relay.Stats().DroppedNoEndpoint != 0 {
		t.Fatalf("unexpected initial buffering stats: %+v", relay.Stats())
	}
}

func TestRelayAdvertisesConfiguredPublicPort(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		publicPort int
	}{
		{name: "bound port by default"},
		{name: "explicit public port", publicPort: 42001},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.RelayAddr = "127.0.0.1:0"
			cfg.PublicHost = "relay.test"
			cfg.PublicRelayPort = test.publicPort
			relay := NewRelay(cfg, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
			if err := relay.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer relay.Close()

			credentials, err := relay.Allocate(1, []Member{{UserID: 1, DisplayName: "Player", Slot: 0}})
			if err != nil {
				t.Fatal(err)
			}
			wantPort := test.publicPort
			if wantPort == 0 {
				wantPort = relay.Port()
			}
			if got := credentials[1].Port; got != wantPort {
				t.Fatalf("advertised relay port = %d, want %d", got, wantPort)
			}
		})
	}
}

func listenTestUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func decodeRelayToken(t *testing.T, value string) relayToken {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("invalid relay token %q: %v", value, err)
	}
	var token relayToken
	copy(token[:], decoded)
	return token
}

func bindAndRequireAck(t *testing.T, conn *net.UDPConn, server *net.UDPAddr, gameID uint64, slot uint8, token relayToken) {
	t.Helper()
	if _, err := conn.WriteToUDP(makeRelayPacket(relayKindBind, slot, slot, gameID, token, nil), server); err != nil {
		t.Fatal(err)
	}
	packet := readTestUDP(t, conn)
	if packet[5] != relayKindBindAck || packet[6] != slot || packet[7] != slot {
		t.Fatalf("unexpected bind ack: %x", packet)
	}
}

func readTestUDP(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buf[:n]...)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(stringsTrimSpace(string(p)))
	return len(p), nil
}

func stringsTrimSpace(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}
