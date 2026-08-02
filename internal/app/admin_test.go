package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testAdminToken = "0123456789abcdefghijklmnopqrstuvwxyz-ADMIN"

func writeTestAdminToken(t *testing.T) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "admin-token")
	if err := os.WriteFile(filename, []byte(testAdminToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func testAdminHandler(t *testing.T, store *ProfileStore, hub *Hub, relay *Relay) http.Handler {
	t.Helper()
	hash := adminTokenHash(sha256.Sum256([]byte(testAdminToken)))
	return newAdminHandler(hash, store, hub, relay, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now().Add(-time.Minute))
}

func adminRequest(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestLoadAdminTokenHashValidatesSecretFile(t *testing.T) {
	valid := writeTestAdminToken(t)
	want := adminTokenHash(sha256.Sum256([]byte(testAdminToken)))
	got, err := loadAdminTokenHash(valid)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("admin token hash does not match the file contents")
	}

	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    string
	}{
		{name: "short", content: "too-short", mode: 0o600, want: "between 32 and 4096 bytes"},
		{name: "whitespace", content: strings.Repeat("x", 31) + " y", mode: 0o600, want: "printable ASCII without whitespace"},
		{name: "permissions", content: testAdminToken, mode: 0o640, want: "must not be accessible by group or other users"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(filename, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filename, test.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAdminTokenHash(filename); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadAdminTokenHash() error = %v, want text %q", err, test.want)
			}
		})
	}

	if _, err := loadAdminTokenHash(t.TempDir()); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory token error = %v", err)
	}
}

func TestNewServerRequiresCompleteAdminConfiguration(t *testing.T) {
	tokenFile := writeTestAdminToken(t)
	tests := []struct {
		name      string
		adminAddr string
		tokenFile string
	}{
		{name: "address only", adminAddr: "127.0.0.1:0"},
		{name: "token only", tokenFile: tokenFile},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.DataFile = ""
			cfg.AdminAddr = test.adminAddr
			cfg.AdminTokenFile = test.tokenFile
			if _, err := NewServer(cfg, nil); err == nil || !strings.Contains(err.Error(), "both --admin-listen and --admin-token-file") {
				t.Fatalf("NewServer() error = %v", err)
			}
		})
	}

	cfg := DefaultConfig()
	cfg.DataFile = ""
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.AdminTokenFile = tokenFile
	server, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAdminHTTPAuthenticationSecurityAndEmbeddedUI(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	relay := NewRelay(cfg, nil)
	hub := NewHub(cfg, nil, store, relay)
	handler := testAdminHandler(t, store, hub, relay)

	ui := httptest.NewRecorder()
	handler.ServeHTTP(ui, httptest.NewRequest(http.MethodGet, "http://admin.test/admin/", nil))
	if ui.Code != http.StatusOK || !strings.Contains(ui.Header().Get("Content-Type"), "text/html") || ui.Body.Len() == 0 {
		t.Fatalf("embedded UI response: status=%d type=%q body=%q", ui.Code, ui.Header().Get("Content-Type"), ui.Body.String())
	}
	if ui.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("UI cache control = %q", ui.Header().Get("Cache-Control"))
	}

	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "http://admin.test/", nil))
	if root.Code != http.StatusTemporaryRedirect || root.Header().Get("Location") != "/admin/" {
		t.Fatalf("root redirect: status=%d location=%q", root.Code, root.Header().Get("Location"))
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "http://admin.test/api/admin/v1/overview", nil))
	if missing.Code != http.StatusUnauthorized || missing.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("missing auth: status=%d authenticate=%q", missing.Code, missing.Header().Get("WWW-Authenticate"))
	}
	wrongRequest := httptest.NewRequest(http.MethodGet, "http://admin.test/api/admin/v1/overview", nil)
	wrongRequest.Header.Set("Authorization", "Bearer definitely-not-the-token")
	wrong := httptest.NewRecorder()
	handler.ServeHTTP(wrong, wrongRequest)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong auth status = %d", wrong.Code)
	}

	overview := adminRequest(t, handler, http.MethodGet, "http://admin.test/api/admin/v1/overview")
	if overview.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", overview.Code, overview.Body.String())
	}
	for header, want := range map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := overview.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if overview.Header().Get("Content-Security-Policy") == "" {
		t.Error("Content-Security-Policy was not set")
	}
	if overview.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("admin handler unexpectedly enabled cross-origin access")
	}
	var envelope struct {
		Data adminOverview `json:"data"`
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Status != "ok" || envelope.Data.Protocol != ProtocolVersion || envelope.Data.ProfileCount != "0" || envelope.Data.UptimeSeconds == "" {
		t.Fatalf("unexpected overview: %+v", envelope.Data)
	}

	options := adminRequest(t, handler, http.MethodOptions, "http://admin.test/api/admin/v1/overview")
	if options.Code != http.StatusMethodNotAllowed || options.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("OPTIONS response: status=%d CORS=%q", options.Code, options.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestAdminProfilesPaginationSearchAndExactCounters(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	relay := NewRelay(cfg, nil)
	hub := NewHub(cfg, nil, store, relay)
	handler := testAdminHandler(t, store, hub, relay)

	first, err := store.Register("player_one", "correct horse battery staple", "Player One")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register("playerXone", "correct horse battery staple", "Player Xone"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register("third_player", "correct horse battery staple", "Third Player"); err != nil {
		t.Fatal(err)
	}
	const exactCounter = uint64(9_007_199_254_740_993)
	if _, err := store.ApplyStats(first.UserID, PlayerStats{Wins: exactCounter, Losses: 2, Disconnects: 3, Games: 4, Rating: 55}); err != nil {
		t.Fatal(err)
	}

	response := adminRequest(t, handler, http.MethodGet, "http://admin.test/api/admin/v1/profiles?query=player_&limit=10&offset=0")
	if response.Code != http.StatusOK {
		t.Fatalf("profiles status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data adminProfilePage `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Total != "1" || len(envelope.Data.Profiles) != 1 {
		t.Fatalf("literal underscore search returned %+v", envelope.Data)
	}
	profile := envelope.Data.Profiles[0]
	if profile.UserID != strconv.FormatUint(first.UserID, 10) || profile.Wins != strconv.FormatUint(exactCounter, 10) || profile.Rating != "55" {
		t.Fatalf("profile counters or ID were not exact strings: %+v", profile)
	}

	page := adminRequest(t, handler, http.MethodGet, "http://admin.test/api/admin/v1/profiles?limit=2&offset=1")
	if page.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", page.Code, page.Body.String())
	}
	if err := json.Unmarshal(page.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Total != "3" || envelope.Data.Limit != 2 || envelope.Data.Offset != 1 || len(envelope.Data.Profiles) != 2 {
		t.Fatalf("unexpected profile page: %+v", envelope.Data)
	}

	for _, target := range []string{
		"http://admin.test/api/admin/v1/profiles?limit=101",
		"http://admin.test/api/admin/v1/profiles?unknown=value",
		"http://admin.test/api/admin/v1/profiles?offset=-1",
	} {
		invalid := adminRequest(t, handler, http.MethodGet, target)
		if invalid.Code != http.StatusBadRequest {
			t.Errorf("invalid profile query %q status = %d", target, invalid.Code)
		}
	}
}

func TestAdminLiveSnapshotsAndActionsPreserveExactGuestID(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	relay := NewRelay(cfg, nil)
	hub := NewHub(cfg, nil, store, relay)
	handler := testAdminHandler(t, store, hub, relay)

	serverSide, peerSide := net.Pipe()
	defer peerSide.Close()
	guestID := uint64(1)<<63 | 7
	client := &controlClient{
		conn: serverSide,
		server: &ControlServer{
			cfg: cfg,
		},
		send:    make(chan outboundEnvelope, 8),
		done:    make(chan struct{}),
		profile: Profile{UserID: guestID, DisplayName: "Exact Guest", Guest: true, CreatedAt: time.Now().UTC()},
		authed:  true,
		status:  "online",
	}
	const gameID uint64 = 0xabc
	hub.mu.Lock()
	hub.clients[guestID] = client
	hub.displayOwners[normalizeDisplayName(client.profile.DisplayName)] = guestID
	hub.userRoom[guestID] = "global"
	hub.rooms["global"].members[guestID] = client
	hub.userGame[guestID] = gameID
	hub.matchQueue[guestID] = matchEntry{client: client, mode: "1v1", enqueuedAt: time.Now()}
	hub.games[gameID] = &stagedGame{
		id: gameID, name: "Admin Test", password: "do-not-expose", maxPlayers: 2,
		compatibility: testGameCompatibility, hostID: guestID, state: "open", listed: true,
		options: GameOptions{Map: "Maps/Test/Test.map"},
		members: map[uint64]*gameMember{guestID: {client: client, ready: true, slot: 0}},
	}
	hub.mu.Unlock()

	sessionsResponse := adminRequest(t, handler, http.MethodGet, "http://admin.test/api/admin/v1/sessions")
	var sessionsEnvelope struct {
		Data struct {
			Sessions []adminSession `json:"sessions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(sessionsResponse.Body.Bytes(), &sessionsEnvelope); err != nil {
		t.Fatal(err)
	}
	if sessionsResponse.Code != http.StatusOK || len(sessionsEnvelope.Data.Sessions) != 1 {
		t.Fatalf("sessions response status=%d body=%s", sessionsResponse.Code, sessionsResponse.Body.String())
	}
	session := sessionsEnvelope.Data.Sessions[0]
	if session.UserID != strconv.FormatUint(guestID, 10) || session.GameID != formatGameID(gameID) || !session.QuickmatchQueued {
		t.Fatalf("guest session lost exact state: %+v", session)
	}

	gamesResponse := adminRequest(t, handler, http.MethodGet, "http://admin.test/api/admin/v1/games")
	var gamesEnvelope struct {
		Data struct {
			Games []adminGame `json:"games"`
		} `json:"data"`
	}
	if err := json.Unmarshal(gamesResponse.Body.Bytes(), &gamesEnvelope); err != nil {
		t.Fatal(err)
	}
	if gamesResponse.Code != http.StatusOK || len(gamesEnvelope.Data.Games) != 1 || len(gamesEnvelope.Data.Games[0].Members) != 1 {
		t.Fatalf("games response status=%d body=%s", gamesResponse.Code, gamesResponse.Body.String())
	}
	game := gamesEnvelope.Data.Games[0]
	if game.Members[0].UserID != strconv.FormatUint(guestID, 10) || !game.HasPassword || strings.Contains(gamesResponse.Body.String(), "do-not-expose") {
		t.Fatalf("game response lost exact ID or exposed password: %+v", game)
	}

	closeGame := adminRequest(t, handler, http.MethodDelete, "http://admin.test/api/admin/v1/games/"+formatGameID(gameID))
	if closeGame.Code != http.StatusNoContent {
		t.Fatalf("close game status=%d body=%s", closeGame.Code, closeGame.Body.String())
	}
	if hub.AdminCloseGame(gameID) {
		t.Fatal("admin-closed game remained in the hub")
	}

	disconnect := adminRequest(t, handler, http.MethodDelete, "http://admin.test/api/admin/v1/sessions/"+strconv.FormatUint(guestID, 10))
	if disconnect.Code != http.StatusNoContent {
		t.Fatalf("disconnect status=%d body=%s", disconnect.Code, disconnect.Body.String())
	}
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("admin disconnect did not close the client")
	}
	if len(hub.AdminSnapshot().Sessions) != 0 {
		t.Fatal("admin-disconnected session remained in the hub")
	}

	for _, target := range []string{
		"http://admin.test/api/admin/v1/sessions/not-a-number",
		"http://admin.test/api/admin/v1/games/ABC",
	} {
		invalid := adminRequest(t, handler, http.MethodDelete, target)
		if invalid.Code != http.StatusBadRequest {
			t.Errorf("invalid action %q status = %d", target, invalid.Code)
		}
	}
}

func TestServerAdminLifecycleAndBindFailureCleanup(t *testing.T) {
	tokenFile := writeTestAdminToken(t)
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.AdminTokenFile = tokenFile
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = ""
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	request, err := http.NewRequest(http.MethodGet, "http://"+server.AdminAddress()+"/api/admin/v1/overview", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("live admin status = %s", response.Status)
	}
	address := server.AdminAddress()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
		t.Fatal("admin listener remained open after shutdown")
	}

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	failedCfg := cfg
	failedCfg.AdminAddr = occupied.Addr().String()
	failed, err := NewServer(failedCfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "listen for admin requests") {
		t.Fatalf("admin bind error = %v", err)
	}
	if err := failed.store.db.Ping(); err == nil {
		t.Fatal("profile store remained open after admin bind failure")
	}
	if err := failed.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after admin bind failure: %v", err)
	}
}

func TestServerReportsAdminListenerFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.AdminTokenFile = writeTestAdminToken(t)
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = ""
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.adminLn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server.Errors():
		if err == nil || !strings.Contains(err.Error(), "serve admin requests") {
			t.Fatalf("runtime error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for admin listener error")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}
