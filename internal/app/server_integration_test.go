package app

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testINICRC uint32 = 0x6f2a93c1

var testGameCompatibility = GameCompatibility{
	Product:              "zerohour",
	CompatibilityVersion: GameCompatibilityVersion,
	INICRC:               testINICRC,
}

func testCompatibleRequest(data map[string]any) map[string]any {
	return requestWithCompatibility(data, testGameCompatibility)
}

func requestWithCompatibility(data map[string]any, compatibility GameCompatibility) map[string]any {
	data["product"] = compatibility.Product
	data["compatibility_version"] = compatibility.CompatibilityVersion
	data["ini_crc"] = compatibility.INICRC
	return data
}

func TestServerTwoClientOnlineFlowAndUDPRelay(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = filepath.Join(t.TempDir(), "profiles.db")
	cfg.AllowInsecurePasswordAuth = true
	cfg.ControlReadTimeout = 10 * time.Second
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	host := newTestPeer(t, server.ControlAddress())
	defer host.Close()
	peer := newTestPeer(t, server.ControlAddress())
	defer peer.Close()
	host.requireEvent("server.hello")
	peer.requireEvent("server.hello")

	hostAuth := host.command("auth.guest", map[string]any{"display_name": "Host"})
	peerAuth := peer.command("auth.guest", map[string]any{"display_name": "Peer"})
	var hostAuthData struct {
		Profile Profile `json:"profile"`
	}
	var peerAuthData struct {
		Profile Profile `json:"profile"`
	}
	decodeWireData(t, hostAuth, &hostAuthData)
	decodeWireData(t, peerAuth, &peerAuthData)
	if hostAuthData.Profile.UserID == peerAuthData.Profile.UserID {
		t.Fatal("guest IDs collided")
	}

	selfStats := host.command("stats.get", map[string]any{})
	var selfStatsData struct {
		UserID uint64      `json:"user_id"`
		Stats  PlayerStats `json:"stats"`
	}
	decodeWireData(t, selfStats, &selfStatsData)
	if selfStatsData.UserID != hostAuthData.Profile.UserID || selfStatsData.Stats != (PlayerStats{}) {
		t.Fatalf("unexpected guest self stats: %+v", selfStatsData)
	}
	peerStats := host.command("stats.get", map[string]any{"user_id": peerAuthData.Profile.UserID})
	var peerStatsData struct {
		UserID uint64      `json:"user_id"`
		Stats  PlayerStats `json:"stats"`
	}
	decodeWireData(t, peerStats, &peerStatsData)
	if peerStatsData.UserID != peerAuthData.Profile.UserID || peerStatsData.Stats != (PlayerStats{}) {
		t.Fatalf("unexpected guest peer stats: %+v", peerStatsData)
	}
	guestUpdate := host.commandResponse("stats.update", map[string]any{"delta": PlayerStats{Wins: 1, Games: 1}})
	if guestUpdate.OK == nil || *guestUpdate.OK || guestUpdate.Code != "persistent_profile_required" {
		t.Fatalf("guest stats update response: %+v", guestUpdate)
	}

	host.command("room.chat", map[string]any{"message": "hello room", "action": false})
	roomChat := peer.requireEvent("room.chat")
	var roomChatData struct {
		Message string `json:"message"`
		UserID  uint64 `json:"user_id"`
	}
	decodeWireData(t, roomChat, &roomChatData)
	if roomChatData.Message != "hello room" || roomChatData.UserID != hostAuthData.Profile.UserID {
		t.Fatalf("unexpected room chat: %+v", roomChatData)
	}

	create := host.command("game.create", testCompatibleRequest(map[string]any{
		"name": "Internet Test", "password": "", "max_players": 2,
		"options": map[string]any{"map": "Maps/Test/Test.map", "use_stats": false, "allow_observers": false, "opaque": "legacy-options", "slot_list": "legacy-slots"},
	}))
	var created struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, create, &created)
	if created.Game.Options.Opaque != "legacy-options" || created.Game.Options.SlotList != "legacy-slots" {
		t.Fatalf("opaque options were not preserved: %+v", created.Game.Options)
	}
	peer.nextID++
	joinRequestID := fmt.Sprintf("request-%d", peer.nextID)
	if err := peer.encoder.Encode(map[string]any{
		"v": ProtocolVersion, "type": "game.join", "id": joinRequestID,
		"data": testCompatibleRequest(map[string]any{"game_id": created.Game.GameID, "password": ""}),
	}); err != nil {
		t.Fatal(err)
	}
	for {
		message := peer.read()
		if message.Type == "game.updated" {
			t.Fatalf("joining client received game.updated before game.join response: %+v", message)
		}
		if message.Type == "response" && message.RequestID == joinRequestID {
			if message.OK == nil || !*message.OK {
				t.Fatalf("game.join failed: %+v", message)
			}
			break
		}
		peer.pending = append(peer.pending, message)
	}
	badOptions := host.commandResponse("game.options", map[string]any{
		"name": "Mutated Despite Error", "max_players": 1,
	})
	if badOptions.OK == nil || *badOptions.OK || badOptions.Code != "invalid_game" {
		t.Fatalf("invalid game.options response: %+v", badOptions)
	}
	listed := peer.command("game.list", map[string]any{})
	var listedData struct {
		Games []GameSummary `json:"games"`
	}
	decodeWireData(t, listed, &listedData)
	if len(listedData.Games) != 1 || listedData.Games[0].Name != "Internet Test" {
		t.Fatalf("invalid game.options partially mutated state: %+v", listedData.Games)
	}
	unauthorizedKick := peer.commandResponse("game.kick", map[string]any{"user_id": hostAuthData.Profile.UserID})
	if unauthorizedKick.OK == nil || *unauthorizedKick.OK || unauthorizedKick.Code != "host_required" {
		t.Fatalf("non-host game.kick response: %+v", unauthorizedKick)
	}
	host.command("game.kick", map[string]any{"user_id": peerAuthData.Profile.UserID})
	peer.requireEvent("game.kicked")
	peer.command("game.join", testCompatibleRequest(map[string]any{"game_id": created.Game.GameID, "password": ""}))
	peer.command("game.ready", map[string]any{"ready": true})
	host.command("game.options", map[string]any{
		"options": map[string]any{
			"map": "Maps/Test/Changed.map", "use_stats": false, "allow_observers": false,
			"opaque": "changed-options", "slot_list": "changed-slots", "ready_key": "configuration-2",
		},
	})
	notReady := host.commandResponse("game.start", map[string]any{})
	if notReady.OK == nil || *notReady.OK || notReady.Code != "players_not_ready" {
		t.Fatalf("game.start did not honor readiness reset: %+v", notReady)
	}
	peer.command("game.ready", map[string]any{"ready": true})
	// A host slot-list echo after receiving accept carries the same semantic
	// ready key and must not clear the player's acceptance again.
	host.command("game.options", map[string]any{
		"options": map[string]any{
			"map": "Maps/Test/Changed.map", "use_stats": false, "allow_observers": false,
			"opaque": "acceptance-only-change", "slot_list": "accepted-slots", "ready_key": "configuration-2",
		},
	})
	host.command("game.start", map[string]any{})

	hostStarted := host.requireEvent("game.started")
	peerStarted := peer.requireEvent("game.started")
	var hostRelay, peerRelay RelayCredential
	decodeWireData(t, hostStarted, &hostRelay)
	decodeWireData(t, peerStarted, &peerRelay)
	if hostRelay.GameID != created.Game.GameID || peerRelay.GameID != created.Game.GameID {
		t.Fatalf("relay game IDs do not match staged game: host %q peer %q staged %q", hostRelay.GameID, peerRelay.GameID, created.Game.GameID)
	}
	if hostRelay.Token == peerRelay.Token || len(hostRelay.Peers) != 2 || len(peerRelay.Peers) != 2 {
		t.Fatal("relay credentials did not contain distinct tokens and all peers")
	}
	completeStartBarrier(t, created.Game.GameID, host, peer)

	relayAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: hostRelay.Port}
	hostUDP := listenTestUDP(t)
	defer hostUDP.Close()
	peerUDP := listenTestUDP(t)
	defer peerUDP.Close()
	gameID, err := parseGameID(created.Game.GameID)
	if err != nil {
		t.Fatal(err)
	}
	hostToken := decodeRelayToken(t, hostRelay.Token)
	peerToken := decodeRelayToken(t, peerRelay.Token)
	bindAndRequireAck(t, hostUDP, relayAddr, gameID, uint8(hostRelay.Slot), hostToken)
	bindAndRequireAck(t, peerUDP, relayAddr, gameID, uint8(peerRelay.Slot), peerToken)
	nativeDatagram := []byte{0xde, 0xad, 0xbe, 0xef, 0x0d, 0xf0, 0x01, 0x02}
	if _, err := hostUDP.WriteToUDP(makeRelayPacket(relayKindData, uint8(hostRelay.Slot), uint8(peerRelay.Slot), gameID, hostToken, nativeDatagram), relayAddr); err != nil {
		t.Fatal(err)
	}
	relayed := readTestUDP(t, peerUDP)
	if string(relayed[relayHeaderSize:]) != string(nativeDatagram) {
		t.Fatalf("native datagram changed through relay: got %x want %x", relayed[relayHeaderSize:], nativeDatagram)
	}

	response, err := http.Get("http://" + server.HealthAddress() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %s", response.Status)
	}
	var health struct {
		Status string     `json:"status"`
		Hub    HubStats   `json:"hub"`
		UDP    RelayStats `json:"udp"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || health.Hub.OnlinePlayers != 2 || health.Hub.ActiveGames != 1 || health.UDP.DatagramsOut < 3 {
		t.Fatalf("unexpected health snapshot: %+v", health)
	}

	metricsResponse, err := http.Get("http://" + server.HealthAddress() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer metricsResponse.Body.Close()
	metrics, err := io.ReadAll(metricsResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	metricsText := string(metrics)
	if !strings.Contains(metricsText, "# TYPE generals_online_players gauge") ||
		!strings.Contains(metricsText, "# TYPE generals_relay_datagrams_in_total counter") ||
		!strings.Contains(metricsText, "# TYPE generals_relay_dropped_no_endpoint_total counter") {
		t.Fatalf("metrics do not distinguish gauges and counters:\n%s", metricsText)
	}
}

func TestGameCompatibilityIsReportedAndRejectsMismatchedJoin(t *testing.T) {
	cfg := hardeningTestConfig(t)
	server := startHardeningTestServer(t, cfg)

	host := newTestPeer(t, server.ControlAddress())
	defer host.Close()
	joiner := newTestPeer(t, server.ControlAddress())
	defer joiner.Close()
	host.requireEvent("server.hello")
	joiner.requireEvent("server.hello")
	host.command("auth.guest", map[string]any{"display_name": "Compatibility Host"})
	joiner.command("auth.guest", map[string]any{"display_name": "Compatibility Joiner"})

	created := host.command("game.create", testCompatibleRequest(map[string]any{
		"name": "Compatible Game", "password": "secret", "max_players": 2, "options": map[string]any{},
	}))
	var createdData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, created, &createdData)
	if createdData.Game.GameCompatibility != testGameCompatibility {
		t.Fatalf("created game compatibility = %+v, want %+v", createdData.Game.GameCompatibility, testGameCompatibility)
	}

	listed := joiner.command("game.list", map[string]any{})
	var listedData struct {
		Games []GameSummary `json:"games"`
	}
	decodeWireData(t, listed, &listedData)
	if len(listedData.Games) != 1 || listedData.Games[0].GameCompatibility != testGameCompatibility {
		t.Fatalf("listed game compatibility was not preserved: %+v", listedData.Games)
	}
	invalidCreate := joiner.commandResponse("game.create", map[string]any{
		"name": "Missing Tuple", "password": "", "max_players": 2, "options": map[string]any{},
	})
	if invalidCreate.OK == nil || *invalidCreate.OK || invalidCreate.Code != "invalid_compatibility" {
		t.Fatalf("game.create without compatibility response: %+v", invalidCreate)
	}
	unsupportedVersion := testGameCompatibility
	unsupportedVersion.CompatibilityVersion++
	invalidQuickmatch := joiner.commandResponse("quickmatch.enqueue", requestWithCompatibility(map[string]any{"mode": "1v1"}, unsupportedVersion))
	if invalidQuickmatch.OK == nil || *invalidQuickmatch.OK || invalidQuickmatch.Code != "invalid_compatibility" {
		t.Fatalf("quickmatch with unsupported compatibility version response: %+v", invalidQuickmatch)
	}

	mismatches := []GameCompatibility{
		{Product: "generals", CompatibilityVersion: GameCompatibilityVersion, INICRC: testINICRC},
		{Product: "zerohour", CompatibilityVersion: GameCompatibilityVersion + 1, INICRC: testINICRC},
		{Product: "zerohour", CompatibilityVersion: GameCompatibilityVersion, INICRC: testINICRC + 1},
		{},
	}
	for _, mismatch := range mismatches {
		response := joiner.commandResponse("game.join", requestWithCompatibility(map[string]any{
			"game_id": createdData.Game.GameID, "password": "secret",
		}, mismatch))
		if response.OK == nil || *response.OK || response.Code != "incompatible_game" {
			t.Fatalf("mismatched join %+v response: %+v", mismatch, response)
		}
	}

	joined := joiner.command("game.join", testCompatibleRequest(map[string]any{
		"game_id": createdData.Game.GameID, "password": "secret",
	}))
	var joinedData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, joined, &joinedData)
	if joinedData.Game.GameCompatibility != testGameCompatibility || len(joinedData.Game.Members) != 2 {
		t.Fatalf("compatible join snapshot: %+v", joinedData.Game)
	}
}

func TestQuickmatchRequiresExactCompatibilityAndMode(t *testing.T) {
	cfg := hardeningTestConfig(t)
	server := startHardeningTestServer(t, cfg)

	peers := []*testPeer{
		newTestPeer(t, server.ControlAddress()),
		newTestPeer(t, server.ControlAddress()),
		newTestPeer(t, server.ControlAddress()),
		newTestPeer(t, server.ControlAddress()),
	}
	for _, peer := range peers {
		defer peer.Close()
		peer.requireEvent("server.hello")
	}
	profiles := make([]Profile, len(peers))
	for index, peer := range peers {
		auth := peer.command("auth.guest", map[string]any{"display_name": fmt.Sprintf("Match Compatibility %d", index+1)})
		var data struct {
			Profile Profile `json:"profile"`
		}
		decodeWireData(t, auth, &data)
		profiles[index] = data.Profile
	}

	otherCompatibility := testGameCompatibility
	otherCompatibility.INICRC++
	firstQueued := peers[0].command("quickmatch.enqueue", requestWithCompatibility(map[string]any{"mode": "1v1"}, testGameCompatibility))
	peers[1].command("quickmatch.enqueue", requestWithCompatibility(map[string]any{"mode": "1v1"}, otherCompatibility))
	peers[2].command("quickmatch.enqueue", requestWithCompatibility(map[string]any{"mode": "2v2"}, testGameCompatibility))
	var firstQueueData struct {
		GameCompatibility
		Queued bool `json:"queued"`
	}
	decodeWireData(t, firstQueued, &firstQueueData)
	if !firstQueueData.Queued || firstQueueData.GameCompatibility != testGameCompatibility {
		t.Fatalf("quickmatch queue did not echo its immutable compatibility: %+v", firstQueueData)
	}
	if stats := server.hub.Stats(); stats.QueuedPlayers != 3 || stats.ActiveGames != 0 {
		t.Fatalf("incompatible quickmatch entries paired: %+v", stats)
	}

	// Re-enqueue is idempotent and must not mutate the tuple already occupying
	// the queue. Changing it requires an explicit quickmatch.cancel first.
	repeated := peers[1].command("quickmatch.enqueue", requestWithCompatibility(map[string]any{"mode": "2v2"}, testGameCompatibility))
	var repeatedData struct {
		GameCompatibility
		Queued bool   `json:"queued"`
		Mode   string `json:"mode"`
	}
	decodeWireData(t, repeated, &repeatedData)
	if !repeatedData.Queued || repeatedData.Mode != "1v1" || repeatedData.GameCompatibility != otherCompatibility {
		t.Fatalf("quickmatch re-enqueue mutated the queued key: %+v", repeatedData)
	}

	matched := peers[3].command("quickmatch.enqueue", requestWithCompatibility(map[string]any{"mode": "1v1"}, testGameCompatibility))
	var matchedData struct {
		Matched bool         `json:"matched"`
		Game    GameSnapshot `json:"game"`
	}
	decodeWireData(t, matched, &matchedData)
	if !matchedData.Matched || matchedData.Game.GameCompatibility != testGameCompatibility {
		t.Fatalf("exact quickmatch did not preserve compatibility: %+v", matchedData)
	}
	server.hub.mu.RLock()
	_, otherQueued := server.hub.matchQueue[profiles[1].UserID]
	_, wrongModeQueued := server.hub.matchQueue[profiles[2].UserID]
	queueLength := len(server.hub.matchQueue)
	server.hub.mu.RUnlock()
	if !otherQueued || !wrongModeQueued || queueLength != 2 {
		t.Fatalf("quickmatch removed incompatible waiters: other=%v mode=%v queue=%d", otherQueued, wrongModeQueued, queueLength)
	}

	peers[0].requireEvent("quickmatch.matched")
	peers[0].requireEvent("game.started")
	peers[3].requireEvent("game.started")
	completeStartBarrier(t, matchedData.Game.GameID, peers[0], peers[3])
	peers[0].command("game.end", map[string]any{})
	peers[0].requireEvent("game.ended")
	peers[3].requireEvent("game.ended")
}

func TestServerShutdownClosesUnauthenticatedIdleClient(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = filepath.Join(t.TempDir(), "profiles.db")
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", server.ControlAddress(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadBytes('\n'); err != nil {
		t.Fatalf("read server hello: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown with idle unauthenticated client: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("idle unauthenticated connection remained open after shutdown")
	}
}

func TestServerTLSAuthenticatesPersistentProfiles(t *testing.T) {
	t.Parallel()
	certFile, keyFile, roots := writeTestTLSCertificate(t)
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = filepath.Join(t.TempDir(), "profiles.db")
	cfg.TLSCertFile = certFile
	cfg.TLSKeyFile = keyFile
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	clientConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "127.0.0.1",
	}
	conn, err := tls.Dial("tcp", server.ControlAddress(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	peer := newTestPeerFromConn(t, conn)
	peer.requireEvent("server.hello")
	registered := peer.command("auth.register", map[string]any{
		"username": "tls-player", "password": "correct horse battery staple", "display_name": "TLS Player",
	})
	var auth struct {
		Profile Profile `json:"profile"`
		Token   string  `json:"token"`
	}
	decodeWireData(t, registered, &auth)
	if auth.Profile.Guest || auth.Profile.Username != "tls-player" || auth.Token == "" {
		t.Fatalf("unexpected persistent TLS auth result: %+v", auth)
	}
	peer.Close()

	loginConn, err := tls.Dial("tcp", server.ControlAddress(), clientConfig.Clone())
	if err != nil {
		t.Fatal(err)
	}
	login := newTestPeerFromConn(t, loginConn)
	defer login.Close()
	login.requireEvent("server.hello")
	loggedIn := login.command("auth.login", map[string]any{
		"username": "tls-player", "password": "correct horse battery staple",
	})
	decodeWireData(t, loggedIn, &auth)
	if auth.Profile.Username != "tls-player" || auth.Token == "" {
		t.Fatalf("unexpected TLS login result: %+v", auth)
	}

	wrongName := clientConfig.Clone()
	wrongName.ServerName = "wrong.example.invalid"
	if wrongConn, dialErr := tls.Dial("tcp", server.ControlAddress(), wrongName); dialErr == nil {
		_ = wrongConn.Close()
		t.Fatal("TLS connection unexpectedly accepted a certificate for the wrong hostname")
	}
}

func TestServerGuestIdentityAndCapacityLimits(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = filepath.Join(t.TempDir(), "profiles.db")
	cfg.MaxOnlinePlayers = 2
	cfg.MaxStagedGames = 1
	cfg.MaxChatMessagesPer10Secs = 1
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	first := newTestPeer(t, server.ControlAddress())
	defer first.Close()
	second := newTestPeer(t, server.ControlAddress())
	defer second.Close()
	third := newTestPeer(t, server.ControlAddress())
	defer third.Close()
	first.requireEvent("server.hello")
	second.requireEvent("server.hello")
	third.requireEvent("server.hello")
	blockedLogin := third.commandResponse("auth.login", map[string]any{"username": "player", "password": "do not send"})
	if blockedLogin.OK == nil || *blockedLogin.OK || blockedLogin.Code != "tls_required" {
		t.Fatalf("plaintext password-auth response: %+v", blockedLogin)
	}
	blockedResume := third.commandResponse("auth.resume", map[string]any{"token": "plaintext-bearer"})
	if blockedResume.OK == nil || *blockedResume.OK || blockedResume.Code != "tls_required" {
		t.Fatalf("plaintext persistent-resume response: %+v", blockedResume)
	}
	firstAuth := first.command("auth.guest", map[string]any{"display_name": "Player"})
	var firstAuthData struct {
		Token string `json:"token"`
	}
	decodeWireData(t, firstAuth, &firstAuthData)
	if firstAuthData.Token != "" {
		t.Fatal("guest authentication created a resumable session token")
	}
	duplicate := second.commandResponse("auth.guest", map[string]any{"display_name": " player "})
	if duplicate.OK == nil || *duplicate.OK || duplicate.Code != "display_name_in_use" {
		t.Fatalf("duplicate guest display-name response: %+v", duplicate)
	}
	second.command("auth.guest", map[string]any{"display_name": "Other"})
	first.command("room.chat", map[string]any{"message": "first", "action": false})
	chatLimited := first.commandResponse("room.chat", map[string]any{"message": "second", "action": false})
	if chatLimited.OK == nil || *chatLimited.OK || chatLimited.Code != "rate_limited" {
		t.Fatalf("chat rate-limit response: %+v", chatLimited)
	}
	full := third.commandResponse("auth.guest", map[string]any{"display_name": "Third"})
	if full.OK == nil || *full.OK || full.Code != "server_full" {
		t.Fatalf("player-capacity response: %+v", full)
	}
	rename := second.commandResponse("profile.update", map[string]any{"display_name": "PLAYER"})
	if rename.OK == nil || *rename.OK || rename.Code != "display_name_in_use" {
		t.Fatalf("duplicate profile.update response: %+v", rename)
	}
	first.command("game.create", testCompatibleRequest(map[string]any{
		"name": "Only Game", "password": "", "max_players": 2, "options": map[string]any{},
	}))
	gameFull := second.commandResponse("game.create", testCompatibleRequest(map[string]any{
		"name": "Too Many", "password": "", "max_players": 2, "options": map[string]any{},
	}))
	if gameFull.OK == nil || *gameFull.OK || gameFull.Code != "server_full" {
		t.Fatalf("staged-game-capacity response: %+v", gameFull)
	}
}

func TestServerPersistentSocialStatsAndQuickmatchFlow(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = filepath.Join(t.TempDir(), "profiles.db")
	cfg.AllowInsecurePasswordAuth = true
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	alice := newTestPeer(t, server.ControlAddress())
	defer alice.Close()
	bob := newTestPeer(t, server.ControlAddress())
	defer bob.Close()
	alice.requireEvent("server.hello")
	bob.requireEvent("server.hello")
	aliceAuth := alice.command("auth.register", map[string]any{"username": "alice_1", "password": "correct horse", "display_name": "Alice"})
	bobAuth := bob.command("auth.register", map[string]any{"username": "bob_2", "password": "battery staple", "display_name": "Bob"})
	var aliceData struct {
		Profile Profile `json:"profile"`
		Token   string  `json:"token"`
	}
	var bobData struct {
		Profile Profile `json:"profile"`
	}
	decodeWireData(t, aliceAuth, &aliceData)
	decodeWireData(t, bobAuth, &bobData)
	if aliceData.Token == "" || aliceData.Profile.Guest || bobData.Profile.Guest {
		t.Fatal("persistent registration did not return persistent profiles and a token")
	}

	alice.command("buddy.request", map[string]any{"user_id": bobData.Profile.UserID})
	bob.requireEvent("buddy.requested")
	bob.command("buddy.accept", map[string]any{"user_id": aliceData.Profile.UserID})
	alice.requireEvent("buddy.accepted")
	buddyList := alice.command("buddy.list", map[string]any{})
	var buddyData struct {
		Buddies []Buddy `json:"buddies"`
	}
	decodeWireData(t, buddyList, &buddyData)
	if len(buddyData.Buddies) != 1 || buddyData.Buddies[0].UserID != bobData.Profile.UserID || !buddyData.Buddies[0].Online {
		t.Fatalf("unexpected buddy list: %+v", buddyData.Buddies)
	}
	bob.command("buddy.status", map[string]any{"status": "away"})
	statusEvent := alice.requireEvent("buddy.status")
	var statusData struct {
		UserID uint64 `json:"user_id"`
		Status string `json:"status"`
	}
	decodeWireData(t, statusEvent, &statusData)
	if statusData.UserID != bobData.Profile.UserID || statusData.Status != "away" {
		t.Fatalf("unexpected buddy status: %+v", statusData)
	}

	alice.command("player.chat", map[string]any{"user_id": bobData.Profile.UserID, "message": "direct hello"})
	direct := bob.requireEvent("player.chat")
	var directData struct {
		Message string `json:"message"`
	}
	decodeWireData(t, direct, &directData)
	if directData.Message != "direct hello" {
		t.Fatalf("direct message = %q", directData.Message)
	}

	alice.command("stats.update", map[string]any{"delta": PlayerStats{Wins: 1, Games: 1, Rating: 10}})
	statsResponse := alice.command("stats.get", map[string]any{})
	var statsData struct {
		Stats PlayerStats `json:"stats"`
	}
	decodeWireData(t, statsResponse, &statsData)
	if statsData.Stats.Wins != 1 || statsData.Stats.Games != 1 || statsData.Stats.Rating != 10 {
		t.Fatalf("unexpected stats: %+v", statsData.Stats)
	}

	queued := alice.command("quickmatch.enqueue", testCompatibleRequest(map[string]any{"mode": "1v1"}))
	var queuedData struct {
		Queued bool `json:"queued"`
	}
	decodeWireData(t, queued, &queuedData)
	if !queuedData.Queued {
		t.Fatal("first quickmatch player was not queued")
	}
	rejectedCreate := alice.commandResponse("game.create", testCompatibleRequest(map[string]any{
		"name": "Must Not Exist", "password": "", "max_players": 2, "options": map[string]any{},
	}))
	if rejectedCreate.OK == nil || *rejectedCreate.OK || rejectedCreate.Code != "quickmatch_queued" {
		t.Fatalf("queued player game.create response: %+v", rejectedCreate)
	}
	manual := bob.command("game.create", testCompatibleRequest(map[string]any{
		"name": "Join Guard", "password": "", "max_players": 2, "options": map[string]any{},
	}))
	var manualData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, manual, &manualData)
	rejectedJoin := alice.commandResponse("game.join", testCompatibleRequest(map[string]any{"game_id": manualData.Game.GameID, "password": ""}))
	if rejectedJoin.OK == nil || *rejectedJoin.OK || rejectedJoin.Code != "quickmatch_queued" {
		t.Fatalf("queued player game.join response: %+v", rejectedJoin)
	}
	bob.command("game.leave", map[string]any{})
	alice.command("quickmatch.cancel", map[string]any{})
	createdAfterCancel := alice.command("game.create", testCompatibleRequest(map[string]any{
		"name": "After Cancel", "password": "", "max_players": 2, "options": map[string]any{},
	}))
	var afterCancelData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, createdAfterCancel, &afterCancelData)
	if afterCancelData.Game.GameID == "" {
		t.Fatal("game.create did not succeed after quickmatch cancellation")
	}
	alice.command("game.leave", map[string]any{})
	alice.command("quickmatch.enqueue", testCompatibleRequest(map[string]any{"mode": "1v1"}))
	matched := bob.command("quickmatch.enqueue", testCompatibleRequest(map[string]any{"mode": "1v1"}))
	var matchedData struct {
		Matched bool         `json:"matched"`
		Game    GameSnapshot `json:"game"`
	}
	decodeWireData(t, matched, &matchedData)
	if !matchedData.Matched || len(matchedData.Game.Members) != 2 || matchedData.Game.GameID == "" {
		t.Fatalf("unexpected quickmatch result: %+v", matchedData)
	}
	if matchedData.Game.Options.UseStats {
		t.Fatal("quickmatch enabled stats without a pre-launch profile identity exchange")
	}
	aliceMatch := alice.requireEvent("quickmatch.matched")
	var aliceMatchData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, aliceMatch, &aliceMatchData)
	if aliceMatchData.Game.GameID != matchedData.Game.GameID {
		t.Fatalf("quickmatch game mismatch: %q != %q", aliceMatchData.Game.GameID, matchedData.Game.GameID)
	}
	alice.requireEvent("game.started")
	bob.requireEvent("game.started")
	completeStartBarrier(t, matchedData.Game.GameID, alice, bob)
	disabledResults := alice.commandResponse("stats.results", map[string]any{
		"game_id": matchedData.Game.GameID,
		"results": []map[string]any{
			{"user_id": aliceData.Profile.UserID, "outcome": "win"},
			{"user_id": bobData.Profile.UserID, "outcome": "loss"},
		},
	})
	if disabledResults.OK == nil || *disabledResults.OK || disabledResults.Code != "stats_disabled" {
		t.Fatalf("quickmatch stats.results response: %+v", disabledResults)
	}

	// A resume token is single-use: the first connection rotates it, and the
	// second attempt must be rejected instead of creating another session.
	resume := newTestPeer(t, server.ControlAddress())
	defer resume.Close()
	resume.requireEvent("server.hello")
	resume.command("auth.resume", map[string]any{"token": aliceData.Token})
	secondResume := newTestPeer(t, server.ControlAddress())
	defer secondResume.Close()
	secondResume.requireEvent("server.hello")
	secondResume.nextID++
	id := fmt.Sprintf("request-%d", secondResume.nextID)
	if err := secondResume.encoder.Encode(map[string]any{"v": ProtocolVersion, "type": "auth.resume", "id": id, "data": map[string]any{"token": aliceData.Token}}); err != nil {
		t.Fatal(err)
	}
	response := secondResume.read()
	if response.OK == nil || *response.OK || response.Code != "authentication_failed" {
		t.Fatalf("reused resume token response: %+v", response)
	}
}

func TestStatsResultsUsesImmutableStartedRoster(t *testing.T) {
	cfg := hardeningTestConfig(t)
	server := startHardeningTestServer(t, cfg)
	peers := []*testPeer{
		newTestPeer(t, server.ControlAddress()),
		newTestPeer(t, server.ControlAddress()),
		newTestPeer(t, server.ControlAddress()),
	}
	for _, peer := range peers {
		defer peer.Close()
		peer.requireEvent("server.hello")
	}
	accounts := []struct {
		username string
		display  string
	}{
		{username: "roster_host", display: "Roster Host"},
		{username: "roster_left", display: "Roster Left"},
		{username: "roster_peer", display: "Roster Peer"},
	}
	profiles := make([]Profile, len(peers))
	for index, peer := range peers {
		auth := peer.command("auth.register", map[string]any{
			"username": accounts[index].username, "password": "correct horse", "display_name": accounts[index].display,
		})
		var data struct {
			Profile Profile `json:"profile"`
		}
		decodeWireData(t, auth, &data)
		profiles[index] = data.Profile
	}
	created := peers[0].command("game.create", testCompatibleRequest(map[string]any{
		"name": "Roster Stats", "password": "", "max_players": 3,
		"options": map[string]any{"use_stats": true},
	}))
	var createdData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, created, &createdData)
	for _, peer := range peers[1:] {
		peer.command("game.join", testCompatibleRequest(map[string]any{"game_id": createdData.Game.GameID, "password": ""}))
		peer.command("game.ready", map[string]any{"ready": true})
	}
	peers[0].command("game.start", map[string]any{})
	for _, peer := range peers {
		peer.requireEvent("game.started")
	}
	completeStartBarrier(t, createdData.Game.GameID, peers...)

	peers[0].command("game.leave", map[string]any{})
	peers[1].requireEvent("game.peer_left")
	peers[2].requireEvent("game.peer_left")
	results := map[string]any{
		"game_id": createdData.Game.GameID,
		"results": []map[string]any{
			{"user_id": profiles[0].UserID, "outcome": "disconnect"},
			{"user_id": profiles[1].UserID, "outcome": "loss"},
			{"user_id": profiles[2].UserID, "outcome": "win"},
		},
	}
	rejectedDepartedHost := peers[0].commandResponse("stats.results", results)
	if rejectedDepartedHost.OK == nil || *rejectedDepartedHost.OK || rejectedDepartedHost.Code != "results_rejected" {
		t.Fatalf("departed host remained authorized to report results: %+v", rejectedDepartedHost)
	}
	peers[2].command("stats.results", results)
	departedStats := peers[0].command("stats.get", map[string]any{})
	var statsData struct {
		Stats PlayerStats `json:"stats"`
	}
	decodeWireData(t, departedStats, &statsData)
	if statsData.Stats.Games != 1 || statsData.Stats.Disconnects != 1 || statsData.Stats.Losses != 1 {
		t.Fatalf("departed launch-roster stats were not recorded: %+v", statsData.Stats)
	}
	peers[2].command("game.end", map[string]any{})
	peers[1].requireEvent("game.ended")
	peers[2].requireEvent("game.ended")
}

func TestFailedRegistrationAdmissionDoesNotPersistAccount(t *testing.T) {
	cfg := hardeningTestConfig(t)
	server := startHardeningTestServer(t, cfg)

	owner := newTestPeer(t, server.ControlAddress())
	defer owner.Close()
	registrar := newTestPeer(t, server.ControlAddress())
	defer registrar.Close()
	owner.requireEvent("server.hello")
	registrar.requireEvent("server.hello")
	unsafe := registrar.commandResponse("auth.guest", map[string]any{"display_name": `Bad,Name\\Injected`})
	if unsafe.OK == nil || *unsafe.OK || unsafe.Code != "invalid_display_name" {
		t.Fatalf("unsafe retail display-name response: %+v", unsafe)
	}
	owner.command("auth.guest", map[string]any{"display_name": "Taken"})

	failed := registrar.commandResponse("auth.register", map[string]any{
		"username": "orphan_1", "password": "correct horse", "display_name": "Taken",
	})
	if failed.OK == nil || *failed.OK || failed.Code != "display_name_in_use" {
		t.Fatalf("registration admission response: %+v", failed)
	}
	owner.Close()
	waitForOnlinePlayers(t, server, 0)

	login := registrar.commandResponse("auth.login", map[string]any{"username": "orphan_1", "password": "correct horse"})
	if login.OK == nil || *login.OK || login.Code != "authentication_failed" {
		t.Fatalf("failed registration left a login-capable account: %+v", login)
	}
	registrar.command("auth.register", map[string]any{
		"username": "orphan_1", "password": "correct horse", "display_name": "Taken",
	})
}

func TestPersistentProfileLimitRejectsSequentialRegistrations(t *testing.T) {
	cfg := hardeningTestConfig(t)
	cfg.MaxProfiles = 2
	cfg.MaxOnlinePlayers = 2
	server := startHardeningTestServer(t, cfg)

	for _, account := range []struct {
		username string
		display  string
	}{
		{username: "limit_1", display: "Limit One"},
		{username: "limit_2", display: "Limit Two"},
	} {
		peer := newTestPeer(t, server.ControlAddress())
		peer.requireEvent("server.hello")
		peer.command("auth.register", map[string]any{
			"username": account.username, "password": "correct horse", "display_name": account.display,
		})
		peer.Close()
		waitForOnlinePlayers(t, server, 0)
	}

	third := newTestPeer(t, server.ControlAddress())
	defer third.Close()
	third.requireEvent("server.hello")
	full := third.commandResponse("auth.register", map[string]any{
		"username": "limit_3", "password": "correct horse", "display_name": "Limit Three",
	})
	if full.OK == nil || *full.OK || full.Code != "server_full" {
		t.Fatalf("profile-limit response: %+v", full)
	}
	if _, exists := server.store.Find("Limit Three"); exists {
		t.Fatal("profile-limit rejection persisted the third account")
	}
	third.command("auth.guest", map[string]any{"display_name": "Limit Three"})
}

func TestFailedResumeAdmissionDoesNotConsumeToken(t *testing.T) {
	cfg := hardeningTestConfig(t)
	cfg.MaxOnlinePlayers = 2
	server := startHardeningTestServer(t, cfg)

	alice := newTestPeer(t, server.ControlAddress())
	bob := newTestPeer(t, server.ControlAddress())
	defer bob.Close()
	alice.requireEvent("server.hello")
	bob.requireEvent("server.hello")
	aliceAuth := alice.command("auth.register", map[string]any{
		"username": "resume_alice", "password": "correct horse", "display_name": "Resume Alice",
	})
	bob.command("auth.register", map[string]any{
		"username": "resume_bob", "password": "battery staple", "display_name": "Resume Bob",
	})
	var aliceData struct {
		Token string `json:"token"`
	}
	decodeWireData(t, aliceAuth, &aliceData)
	if aliceData.Token == "" {
		t.Fatal("registration did not issue a resume token")
	}
	alice.Close()
	waitForOnlinePlayers(t, server, 1)

	occupant := newTestPeer(t, server.ControlAddress())
	occupant.requireEvent("server.hello")
	occupant.command("auth.guest", map[string]any{"display_name": "Capacity Guest"})
	blocked := newTestPeer(t, server.ControlAddress())
	blocked.requireEvent("server.hello")
	failed := blocked.commandResponse("auth.resume", map[string]any{"token": aliceData.Token})
	if failed.OK == nil || *failed.OK || failed.Code != "server_full" {
		t.Fatalf("full-capacity resume response: %+v", failed)
	}
	blocked.Close()
	occupant.Close()
	waitForOnlinePlayers(t, server, 1)

	retry := newTestPeer(t, server.ControlAddress())
	defer retry.Close()
	retry.requireEvent("server.hello")
	retry.command("auth.resume", map[string]any{"token": aliceData.Token})
}

func TestQuickmatchDepartureFullyReleasesSurvivor(t *testing.T) {
	cfg := hardeningTestConfig(t)
	server := startHardeningTestServer(t, cfg)

	alice := newTestPeer(t, server.ControlAddress())
	defer alice.Close()
	bob := newTestPeer(t, server.ControlAddress())
	defer bob.Close()
	alice.requireEvent("server.hello")
	bob.requireEvent("server.hello")
	alice.command("auth.guest", map[string]any{"display_name": "Quick Alice"})
	bob.command("auth.guest", map[string]any{"display_name": "Quick Bob"})
	alice.command("quickmatch.enqueue", testCompatibleRequest(map[string]any{"mode": "1v1"}))
	matched := bob.command("quickmatch.enqueue", testCompatibleRequest(map[string]any{"mode": "1v1"}))
	var match struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, matched, &match)
	if match.Game.Options.UseStats {
		t.Fatal("quickmatch unexpectedly enabled unreliable retail stats")
	}
	alice.requireEvent("quickmatch.matched")
	alice.requireEvent("game.started")
	bob.requireEvent("game.started")
	completeStartBarrier(t, match.Game.GameID, alice, bob)

	bob.command("game.leave", map[string]any{})
	ended := alice.requireEvent("game.ended")
	var endedData struct {
		GameID string `json:"game_id"`
	}
	decodeWireData(t, ended, &endedData)
	if endedData.GameID != match.Game.GameID {
		t.Fatalf("ended game id = %q, want %q", endedData.GameID, match.Game.GameID)
	}
	requeued := alice.command("quickmatch.enqueue", testCompatibleRequest(map[string]any{"mode": "1v1"}))
	var queueData struct {
		Queued bool `json:"queued"`
	}
	decodeWireData(t, requeued, &queueData)
	if !queueData.Queued {
		t.Fatal("quickmatch survivor could not return to the queue")
	}
}

func TestStartedCustomGameRetainsSurvivorRelayAfterHostDisconnect(t *testing.T) {
	cfg := hardeningTestConfig(t)
	server := startHardeningTestServer(t, cfg)

	host := newTestPeer(t, server.ControlAddress())
	defer host.Close()
	first := newTestPeer(t, server.ControlAddress())
	defer first.Close()
	second := newTestPeer(t, server.ControlAddress())
	defer second.Close()
	for _, peer := range []*testPeer{host, first, second} {
		peer.requireEvent("server.hello")
	}
	hostAuth := host.command("auth.guest", map[string]any{"display_name": "Started Host"})
	first.command("auth.guest", map[string]any{"display_name": "Started First"})
	second.command("auth.guest", map[string]any{"display_name": "Started Second"})
	var hostData struct {
		Profile Profile `json:"profile"`
	}
	decodeWireData(t, hostAuth, &hostData)

	created := host.command("game.create", testCompatibleRequest(map[string]any{
		"name": "Survivor Relay", "password": "", "max_players": 3, "options": map[string]any{},
	}))
	var createdData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, created, &createdData)
	first.command("game.join", testCompatibleRequest(map[string]any{"game_id": createdData.Game.GameID, "password": ""}))
	second.command("game.join", testCompatibleRequest(map[string]any{"game_id": createdData.Game.GameID, "password": ""}))
	first.command("game.ready", map[string]any{"ready": true})
	second.command("game.ready", map[string]any{"ready": true})
	host.command("game.start", map[string]any{})
	credentialEvents := []wireMessage{
		host.requireEvent("game.started"),
		first.requireEvent("game.started"),
		second.requireEvent("game.started"),
	}
	credentials := make([]RelayCredential, len(credentialEvents))
	for index, event := range credentialEvents {
		decodeWireData(t, event, &credentials[index])
	}
	completeStartBarrier(t, createdData.Game.GameID, host, first, second)

	gameID, err := parseGameID(createdData.Game.GameID)
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: credentials[0].Port}
	udpPeers := []*net.UDPConn{listenTestUDP(t), listenTestUDP(t), listenTestUDP(t)}
	for _, conn := range udpPeers {
		defer conn.Close()
	}
	tokens := make([]relayToken, len(credentials))
	for index, credential := range credentials {
		tokens[index] = decodeRelayToken(t, credential.Token)
		bindAndRequireAck(t, udpPeers[index], relayAddr, gameID, uint8(credential.Slot), tokens[index])
	}

	host.Close()
	for _, peer := range []*testPeer{first, second} {
		left := peer.requireEvent("game.peer_left")
		var data struct {
			GameID              string `json:"game_id"`
			DepartedUserID      uint64 `json:"departed_user_id"`
			DepartedDisplayName string `json:"departed_display_name"`
			DepartedSlot        int    `json:"departed_slot"`
			DepartedHost        bool   `json:"departed_host"`
		}
		decodeWireData(t, left, &data)
		if data.GameID != createdData.Game.GameID || data.DepartedUserID != hostData.Profile.UserID ||
			data.DepartedDisplayName != hostData.Profile.DisplayName || data.DepartedSlot != credentials[0].Slot || !data.DepartedHost {
			t.Fatalf("started-game peer-left event: %+v", data)
		}
	}
	if stats := server.hub.Stats(); stats.ActiveGames != 1 || stats.OpenGames != 0 {
		t.Fatalf("survivor game was not retained: %+v", stats)
	}
	if active := server.relay.Stats().ActiveGames; active != 1 {
		t.Fatalf("survivor relay allocation count = %d, want 1", active)
	}

	assertRelayedPayload(t, udpPeers[1], udpPeers[2], relayAddr, gameID, credentials[1], credentials[2], tokens[1], []byte("first-to-second"))
	assertRelayedPayload(t, udpPeers[2], udpPeers[1], relayAddr, gameID, credentials[2], credentials[1], tokens[2], []byte("second-to-first"))

	droppedBefore := server.relay.Stats().DroppedAuth
	if _, err := udpPeers[0].WriteToUDP(makeRelayPacket(
		relayKindData, uint8(credentials[0].Slot), uint8(credentials[1].Slot), gameID, tokens[0], []byte("departed"),
	), relayAddr); err != nil {
		t.Fatal(err)
	}
	requireNoUDPDatagram(t, udpPeers[1], 100*time.Millisecond)
	if dropped := server.relay.Stats().DroppedAuth; dropped <= droppedBefore {
		t.Fatalf("departed relay token was not rejected: before=%d after=%d", droppedBefore, dropped)
	}

	first.command("game.end", map[string]any{})
	for _, peer := range []*testPeer{first, second} {
		ended := peer.requireEvent("game.ended")
		var data struct {
			GameID string `json:"game_id"`
			Reason string `json:"reason"`
		}
		decodeWireData(t, ended, &data)
		if data.GameID != createdData.Game.GameID || data.Reason != "host_ended" {
			t.Fatalf("survivor cleanup event: %+v", data)
		}
	}
	if server.relay.Stats().ActiveGames != 0 || server.hub.Stats().ActiveGames != 0 {
		t.Fatal("explicit survivor game.end did not release the game and relay")
	}

	replacement := first.command("game.create", testCompatibleRequest(map[string]any{
		"name": "After Departure", "password": "", "max_players": 3, "options": map[string]any{},
	}))
	var replacementData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, replacement, &replacementData)
	joined := second.command("game.join", testCompatibleRequest(map[string]any{"game_id": replacementData.Game.GameID, "password": ""}))
	var joinedData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, joined, &joinedData)
	if len(joinedData.Game.Members) != 2 {
		t.Fatalf("relay survivors did not create/join replacement: %+v", joinedData.Game)
	}
}

func TestStartReadyBarrierTimeoutDepartureAndGenerationGuard(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		cfg := hardeningTestConfig(t)
		cfg.StartReadyTimeout = 50 * time.Millisecond
		server, host, peer, gameID := startTwoPlayerCredentialPhase(t, cfg)
		host.command("game.start_ready", map[string]any{"game_id": gameID})
		for _, client := range []*testPeer{host, peer} {
			ended := client.requireEvent("game.ended")
			var data struct {
				GameID string `json:"game_id"`
				Reason string `json:"reason"`
			}
			decodeWireData(t, ended, &data)
			if data.GameID != gameID || data.Reason != "start_timeout" {
				t.Fatalf("start timeout event: %+v", data)
			}
		}
		if server.hub.Stats().ActiveGames != 0 || server.relay.Stats().ActiveGames != 0 {
			t.Fatal("start-ready timeout retained game or relay state")
		}
		host.command("game.create", testCompatibleRequest(map[string]any{
			"name": "After Timeout", "password": "", "max_players": 2, "options": map[string]any{},
		}))
	})

	t.Run("departure_before_go", func(t *testing.T) {
		cfg := hardeningTestConfig(t)
		server, host, peer, gameID := startTwoPlayerCredentialPhase(t, cfg)
		peer.Close()
		ended := host.requireEvent("game.ended")
		var data struct {
			GameID string `json:"game_id"`
			Reason string `json:"reason"`
		}
		decodeWireData(t, ended, &data)
		if data.GameID != gameID || data.Reason != "player_left" {
			t.Fatalf("pre-go departure event: %+v", data)
		}
		if server.hub.Stats().ActiveGames != 0 || server.relay.Stats().ActiveGames != 0 {
			t.Fatal("pre-go departure retained game or relay state")
		}
	})

	t.Run("completed_barrier_invalidates_timer", func(t *testing.T) {
		cfg := hardeningTestConfig(t)
		cfg.StartReadyTimeout = 75 * time.Millisecond
		server, host, peer, gameID := startTwoPlayerCredentialPhase(t, cfg)
		completeStartBarrier(t, gameID, host, peer)
		time.Sleep(150 * time.Millisecond)
		if server.hub.Stats().ActiveGames != 1 || server.relay.Stats().ActiveGames != 1 {
			t.Fatal("stale start-ready timer cancelled a completed game")
		}
		host.command("game.end", map[string]any{})
		host.requireEvent("game.ended")
		peer.requireEvent("game.ended")
	})
}

func TestRelayIdleExpiryDissolvesStartedGame(t *testing.T) {
	cfg := hardeningTestConfig(t)
	cfg.GameIdleTimeout = time.Minute
	server, host, peer, gameIDText := startTwoPlayerCredentialPhase(t, cfg)
	completeStartBarrier(t, gameIDText, host, peer)

	gameID, err := parseGameID(gameIDText)
	if err != nil {
		t.Fatal(err)
	}
	server.relay.mu.Lock()
	server.relay.games[gameID].lastActivity = time.Now().Add(-2 * cfg.GameIdleTimeout)
	server.relay.mu.Unlock()

	expired := make(chan struct{})
	go func() {
		server.relay.expireIdleGames(time.Now())
		close(expired)
	}()
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("relay expiry deadlocked while coordinating Hub cleanup")
	}

	for _, client := range []*testPeer{host, peer} {
		ended := client.requireEvent("game.ended")
		var data struct {
			GameID string `json:"game_id"`
			Reason string `json:"reason"`
		}
		decodeWireData(t, ended, &data)
		if data.GameID != gameIDText || data.Reason != "relay_idle_timeout" {
			t.Fatalf("relay idle expiry event: %+v", data)
		}
	}
	if server.relay.Stats().ActiveGames != 0 || server.hub.Stats().ActiveGames != 0 {
		t.Fatal("relay idle expiry retained game state")
	}
	server.hub.mu.RLock()
	remainingGames := len(server.hub.games)
	remainingUserGames := len(server.hub.userGame)
	statuses := make([]string, 0, len(server.hub.clients))
	for _, client := range server.hub.clients {
		statuses = append(statuses, client.status)
	}
	server.hub.mu.RUnlock()
	if remainingGames != 0 || remainingUserGames != 0 {
		t.Fatalf("relay idle expiry retained %d games and %d user-to-game mappings", remainingGames, remainingUserGames)
	}
	for _, status := range statuses {
		if status != "online" {
			t.Fatalf("relay idle expiry left participant status %q, want online", status)
		}
	}

	replacement := host.command("game.create", testCompatibleRequest(map[string]any{
		"name": "After Relay Expiry", "password": "", "max_players": 2, "options": map[string]any{},
	}))
	var replacementData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, replacement, &replacementData)
	joined := peer.command("game.join", testCompatibleRequest(map[string]any{
		"game_id": replacementData.Game.GameID, "password": "",
	}))
	var joinedData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, joined, &joinedData)
	if len(joinedData.Game.Members) != 2 {
		t.Fatalf("expired-game participants could not stage a replacement: %+v", joinedData.Game)
	}
}

func TestHostDepartureDissolvesStagedGame(t *testing.T) {
	cfg := hardeningTestConfig(t)
	server := startHardeningTestServer(t, cfg)

	peers := []*testPeer{
		newTestPeer(t, server.ControlAddress()),
		newTestPeer(t, server.ControlAddress()),
		newTestPeer(t, server.ControlAddress()),
		newTestPeer(t, server.ControlAddress()),
	}
	for _, peer := range peers {
		defer peer.Close()
		peer.requireEvent("server.hello")
	}
	profiles := make([]Profile, len(peers))
	for index, peer := range peers {
		auth := peer.command("auth.guest", map[string]any{"display_name": fmt.Sprintf("Slot Player %d", index+1)})
		var data struct {
			Profile Profile `json:"profile"`
		}
		decodeWireData(t, auth, &data)
		profiles[index] = data.Profile
	}
	created := peers[0].command("game.create", testCompatibleRequest(map[string]any{
		"name": "Departing Host", "password": "", "max_players": 4, "options": map[string]any{
			"opaque": "M=Old,S=Host-owned-slot-list;", "slot_list": "M=Old,S=Host-owned-slot-list;",
		},
	}))
	var createdData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, created, &createdData)
	for _, peer := range peers[1:3] {
		peer.command("game.join", testCompatibleRequest(map[string]any{"game_id": createdData.Game.GameID, "password": ""}))
	}

	peers[0].command("game.leave", map[string]any{})
	for _, peer := range peers[1:3] {
		finalSnapshot := requireGameUpdateWithoutMember(t, peer, createdData.Game.GameID, profiles[0].UserID)
		if finalSnapshot.Options.Opaque != "" || finalSnapshot.Options.SlotList != "" || finalSnapshot.Options.ReadyKey != "" {
			t.Fatalf("dissolving game retained departed host options: %+v", finalSnapshot.Options)
		}
		for _, member := range finalSnapshot.Members {
			if member.Host {
				t.Fatalf("dissolving game advertised replacement host: %+v", finalSnapshot.Members)
			}
		}
		ended := peer.requireEvent("game.ended")
		var endedData struct {
			GameID string `json:"game_id"`
			Reason string `json:"reason"`
		}
		decodeWireData(t, ended, &endedData)
		if endedData.GameID != createdData.Game.GameID || endedData.Reason != "host_left" {
			t.Fatalf("host departure event: %+v", endedData)
		}
	}

	removed := peers[3].commandResponse("game.join", testCompatibleRequest(map[string]any{"game_id": createdData.Game.GameID, "password": ""}))
	if removed.OK == nil || *removed.OK || removed.Code != "game_not_found" {
		t.Fatalf("dissolved game remained joinable: %+v", removed)
	}
	replacement := peers[1].command("game.create", testCompatibleRequest(map[string]any{
		"name": "Replacement", "password": "", "max_players": 4, "options": map[string]any{},
	}))
	var replacementData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, replacement, &replacementData)
	joined := peers[2].command("game.join", testCompatibleRequest(map[string]any{"game_id": replacementData.Game.GameID, "password": ""}))
	var joinedData struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, joined, &joinedData)
	if len(joinedData.Game.Members) != 2 {
		t.Fatalf("survivors did not create/join replacement game: %+v", joinedData.Game)
	}
}

func hardeningTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = filepath.Join(t.TempDir(), "profiles.db")
	cfg.AllowInsecurePasswordAuth = true
	return cfg
}

func startHardeningTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return server
}

func startTwoPlayerCredentialPhase(t *testing.T, cfg Config) (*Server, *testPeer, *testPeer, string) {
	t.Helper()
	server := startHardeningTestServer(t, cfg)
	host := newTestPeer(t, server.ControlAddress())
	peer := newTestPeer(t, server.ControlAddress())
	t.Cleanup(host.Close)
	t.Cleanup(peer.Close)
	host.requireEvent("server.hello")
	peer.requireEvent("server.hello")
	host.command("auth.guest", map[string]any{"display_name": "Barrier Host"})
	peer.command("auth.guest", map[string]any{"display_name": "Barrier Peer"})
	created := host.command("game.create", testCompatibleRequest(map[string]any{
		"name": "Barrier", "password": "", "max_players": 2, "options": map[string]any{},
	}))
	var data struct {
		Game GameSnapshot `json:"game"`
	}
	decodeWireData(t, created, &data)
	peer.command("game.join", testCompatibleRequest(map[string]any{"game_id": data.Game.GameID, "password": ""}))
	peer.command("game.ready", map[string]any{"ready": true})
	host.command("game.start", map[string]any{})
	host.requireEvent("game.started")
	peer.requireEvent("game.started")
	return server, host, peer, data.Game.GameID
}

func waitForOnlinePlayers(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := server.hub.Stats().OnlinePlayers; got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("online players = %d, want %d", server.hub.Stats().OnlinePlayers, want)
}

func completeStartBarrier(t *testing.T, gameID string, peers ...*testPeer) {
	t.Helper()
	for index, peer := range peers {
		response := peer.command("game.start_ready", map[string]any{"game_id": gameID})
		var data struct {
			Ready bool `json:"ready"`
			Go    bool `json:"go"`
		}
		decodeWireData(t, response, &data)
		wantGo := index == len(peers)-1
		if !data.Ready || data.Go != wantGo {
			t.Fatalf("start-ready %d = ready %v go %v, want ready true go %v", index, data.Ready, data.Go, wantGo)
		}
	}
	for _, peer := range peers {
		event := peer.requireEvent("game.go")
		var data struct {
			GameID string `json:"game_id"`
		}
		decodeWireData(t, event, &data)
		if data.GameID != gameID {
			t.Fatalf("game.go id = %q, want %q", data.GameID, gameID)
		}
	}
}

func assertRelayedPayload(
	t *testing.T,
	sender, recipient *net.UDPConn,
	relayAddr *net.UDPAddr,
	gameID uint64,
	senderCredential, recipientCredential RelayCredential,
	senderToken relayToken,
	payload []byte,
) {
	t.Helper()
	frame := makeRelayPacket(
		relayKindData,
		uint8(senderCredential.Slot),
		uint8(recipientCredential.Slot),
		gameID,
		senderToken,
		payload,
	)
	if _, err := sender.WriteToUDP(frame, relayAddr); err != nil {
		t.Fatal(err)
	}
	received := readTestUDP(t, recipient)
	if received[5] != relayKindDataOut || received[6] != uint8(senderCredential.Slot) ||
		received[7] != uint8(recipientCredential.Slot) || string(received[relayHeaderSize:]) != string(payload) {
		t.Fatalf("unexpected survivor relay frame: %x", received)
	}
}

func requireNoUDPDatagram(t *testing.T, conn *net.UDPConn, wait time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2048)
	if _, _, err := conn.ReadFromUDP(buffer); err == nil {
		t.Fatal("unexpected UDP datagram was delivered")
	}
}

func requireGameUpdateWithoutMember(t *testing.T, peer *testPeer, gameID string, departedUserID uint64) GameSnapshot {
	t.Helper()
	for attempt := 0; attempt < 8; attempt++ {
		message := peer.requireEvent("game.updated")
		var data struct {
			Game GameSnapshot `json:"game"`
		}
		decodeWireData(t, message, &data)
		if data.Game.GameID != gameID {
			continue
		}
		departedPresent := false
		for _, member := range data.Game.Members {
			if member.UserID == departedUserID {
				departedPresent = true
				break
			}
		}
		if !departedPresent {
			return data.Game
		}
	}
	t.Fatalf("game %s never removed departed host %d", gameID, departedUserID)
	return GameSnapshot{}
}

type wireMessage struct {
	Version   int             `json:"v"`
	Type      string          `json:"type"`
	RequestID string          `json:"id"`
	OK        *bool           `json:"ok"`
	Code      string          `json:"code"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

type testPeer struct {
	t       *testing.T
	conn    net.Conn
	reader  *bufio.Reader
	encoder *json.Encoder
	pending []wireMessage
	nextID  int
}

func newTestPeer(t *testing.T, address string) *testPeer {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return newTestPeerFromConn(t, conn)
}

func newTestPeerFromConn(t *testing.T, conn net.Conn) *testPeer {
	t.Helper()
	return &testPeer{t: t, conn: conn, reader: bufio.NewReader(conn), encoder: json.NewEncoder(conn)}
}

func (p *testPeer) Close() { _ = p.conn.Close() }

func (p *testPeer) command(commandType string, data any) wireMessage {
	p.t.Helper()
	message := p.commandResponse(commandType, data)
	if message.OK == nil || !*message.OK {
		p.t.Fatalf("%s failed: code=%s error=%s", commandType, message.Code, message.Error)
	}
	return message
}

func (p *testPeer) commandResponse(commandType string, data any) wireMessage {
	p.t.Helper()
	p.nextID++
	id := fmt.Sprintf("request-%d", p.nextID)
	if err := p.encoder.Encode(map[string]any{"v": ProtocolVersion, "type": commandType, "id": id, "data": data}); err != nil {
		p.t.Fatal(err)
	}
	for {
		message := p.read()
		if message.Type == "response" && message.RequestID == id {
			return message
		}
		p.pending = append(p.pending, message)
	}
}

func (p *testPeer) requireEvent(eventType string) wireMessage {
	p.t.Helper()
	for i, message := range p.pending {
		if message.Type == eventType {
			p.pending = append(p.pending[:i], p.pending[i+1:]...)
			return message
		}
	}
	for {
		message := p.read()
		if message.Type == eventType {
			return message
		}
		p.pending = append(p.pending, message)
	}
}

func (p *testPeer) read() wireMessage {
	p.t.Helper()
	if err := p.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		p.t.Fatal(err)
	}
	line, err := p.reader.ReadBytes('\n')
	if err != nil {
		p.t.Fatal(err)
	}
	var message wireMessage
	if err := json.Unmarshal(line, &message); err != nil {
		p.t.Fatalf("decode %q: %v", line, err)
	}
	return message
}

func decodeWireData(t *testing.T, message wireMessage, destination any) {
	t.Helper()
	if err := json.Unmarshal(message.Data, destination); err != nil {
		t.Fatalf("decode %s data %s: %v", message.Type, message.Data, err)
	}
}

func writeTestTLSCertificate(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "GeneralsX test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	certFile := filepath.Join(directory, "server.pem")
	keyFile := filepath.Join(directory, "server-key.pem")
	certificatePEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...,
	)
	if err := os.WriteFile(certFile, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})) {
		t.Fatal("could not add generated test CA")
	}
	return certFile, keyFile, roots
}
