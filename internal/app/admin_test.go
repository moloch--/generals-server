package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
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

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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
	return adminRequestBody(t, handler, method, target, "", "")
}

func adminRequestBody(t *testing.T, handler http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
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
	if overview.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HTTP admin response unexpectedly enabled HSTS")
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

func TestAdminProfileManagementResetsAndDeletesAccountsWithoutLeakingSecrets(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	relay := NewRelay(cfg, nil)
	hub := NewHub(cfg, nil, store, relay)
	var audit bytes.Buffer
	hash := adminTokenHash(sha256.Sum256([]byte(testAdminToken)))
	handler := newAdminHandler(hash, store, hub, relay, slog.New(slog.NewTextHandler(&audit, nil)), time.Now().Add(-time.Minute))
	t.Cleanup(func() { _ = handler.shutdownEvents(context.Background()) })

	profile, err := store.Register("managed_user", "original password", "Managed User")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.ReserveProfile(profile); err != nil {
		t.Fatal(err)
	}
	resumeToken, _, err := hub.IssueSession(profile)
	if err != nil {
		t.Fatal(err)
	}
	client := newPersistentHubClient(t, cfg, profile)
	if _, err := hub.Connect(client); err != nil {
		t.Fatal(err)
	}

	const replacementPassword = "replacement password"
	reset := adminRequestBody(t, handler, http.MethodPut,
		"http://admin.test/api/admin/v1/profiles/"+strconv.FormatUint(profile.UserID, 10)+"/password",
		"application/json", `{"password":"`+replacementPassword+`"}`)
	if reset.Code != http.StatusNoContent || reset.Body.Len() != 0 {
		t.Fatalf("reset response status=%d body=%q", reset.Code, reset.Body.String())
	}
	requireClientClosed(t, client)
	if _, err := store.Authenticate(profile.Username, "original password"); err == nil {
		t.Fatal("original password remained valid after admin reset")
	}
	updatedProfile, err := store.Authenticate(profile.Username, replacementPassword)
	if err != nil {
		t.Fatalf("replacement password was rejected: %v", err)
	}
	if _, err := hub.ResumeAndReserve(resumeToken); err == nil {
		t.Fatal("resume token survived admin password reset")
	}

	invalidRequests := []struct {
		target      string
		contentType string
		body        string
	}{
		{target: "http://admin.test/api/admin/v1/profiles/not-a-number/password", contentType: "application/json", body: `{"password":"valid password"}`},
		{target: "http://admin.test/api/admin/v1/profiles/" + strconv.FormatUint(profile.UserID, 10) + "/password", contentType: "text/plain", body: `{"password":"valid password"}`},
		{target: "http://admin.test/api/admin/v1/profiles/" + strconv.FormatUint(profile.UserID, 10) + "/password", contentType: "application/json", body: `{"password":"short"}`},
		{target: "http://admin.test/api/admin/v1/profiles/" + strconv.FormatUint(profile.UserID, 10) + "/password", contentType: "application/json", body: `{"password":"valid password","unexpected":true}`},
		{target: "http://admin.test/api/admin/v1/profiles/" + strconv.FormatUint(profile.UserID, 10) + "/password", contentType: "application/json", body: `{"password":"valid password"}{}`},
	}
	for _, test := range invalidRequests {
		response := adminRequestBody(t, handler, http.MethodPut, test.target, test.contentType, test.body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid reset %q status=%d body=%s", test.body, response.Code, response.Body.String())
		}
	}
	missingReset := adminRequestBody(t, handler, http.MethodPut,
		"http://admin.test/api/admin/v1/profiles/999999/password", "application/json", `{"password":"valid password"}`)
	if missingReset.Code != http.StatusNotFound {
		t.Fatalf("missing reset status=%d body=%s", missingReset.Code, missingReset.Body.String())
	}

	if err := hub.ReserveProfile(updatedProfile); err != nil {
		t.Fatal(err)
	}
	deleteToken, _, err := hub.IssueSession(updatedProfile)
	if err != nil {
		t.Fatal(err)
	}
	deleteClient := newPersistentHubClient(t, cfg, updatedProfile)
	if _, err := hub.Connect(deleteClient); err != nil {
		t.Fatal(err)
	}
	deleteTarget := "http://admin.test/api/admin/v1/profiles/" + strconv.FormatUint(profile.UserID, 10)
	deleted := adminRequest(t, handler, http.MethodDelete, deleteTarget)
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 {
		t.Fatalf("delete response status=%d body=%q", deleted.Code, deleted.Body.String())
	}
	requireClientClosed(t, deleteClient)
	if _, exists := store.Get(profile.UserID); exists {
		t.Fatal("admin-deleted profile remained in SQLite")
	}
	if _, err := hub.ResumeAndReserve(deleteToken); err == nil {
		t.Fatal("resume token survived admin profile deletion")
	}
	if second := adminRequest(t, handler, http.MethodDelete, deleteTarget); second.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d body=%s", second.Code, second.Body.String())
	}

	logs := audit.String()
	if strings.Contains(logs, replacementPassword) || strings.Contains(logs, "original password") {
		t.Fatalf("admin audit log exposed a password: %s", logs)
	}
}

func TestAdminRealtimeTicketsAreSingleUseAndPushSnapshots(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := DefaultConfig()
	relay := NewRelay(cfg, nil)
	hub := NewHub(cfg, nil, store, relay)
	hash := adminTokenHash(sha256.Sum256([]byte(testAdminToken)))
	handler := newAdminHandler(hash, store, hub, relay, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now().Add(-time.Minute))
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		_ = handler.shutdownEvents(context.Background())
		server.Close()
	})

	unauthorized, err := http.Post(server.URL+"/api/admin/v1/events/ticket", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized ticket status=%d", unauthorized.StatusCode)
	}

	crossOriginTicket := requestAdminEventTicket(t, server.URL)
	crossOriginURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/admin/v1/events?ticket=" + crossOriginTicket
	crossOriginContext, cancelCrossOrigin := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelCrossOrigin()
	crossOriginConnection, crossOriginResponse, err := websocket.Dial(crossOriginContext, crossOriginURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://attacker.example"}},
	})
	if crossOriginConnection != nil {
		crossOriginConnection.CloseNow()
		t.Fatal("cross-origin realtime connection was accepted")
	}
	if err == nil || crossOriginResponse == nil {
		t.Fatalf("cross-origin dial error=%v response=%v", err, crossOriginResponse)
	}
	crossOriginResponse.Body.Close()
	if crossOriginResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin realtime status=%d, want %d", crossOriginResponse.StatusCode, http.StatusForbidden)
	}
	handler.eventsMu.Lock()
	crossOriginCount := handler.eventConnectionCount
	crossOriginTracked := len(handler.eventConnections)
	handler.eventsMu.Unlock()
	if crossOriginCount != 0 || crossOriginTracked != 0 {
		t.Fatalf("rejected cross-origin connection left count=%d tracked=%d", crossOriginCount, crossOriginTracked)
	}

	ticket := requestAdminEventTicket(t, server.URL)
	websocketURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/admin/v1/events?ticket=" + ticket
	dialContext, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDial()
	connection, response, err := websocket.Dial(dialContext, websocketURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{server.URL}},
	})
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.CloseNow()

	readContext, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRead()
	var initial adminEventSnapshot
	if err := wsjson.Read(readContext, connection, &initial); err != nil {
		t.Fatal(err)
	}
	if initial.Type != "snapshot" || initial.Sequence == "" || initial.ProfileRevision != "0" || initial.Overview.Status != "ok" {
		t.Fatalf("unexpected initial realtime snapshot: %+v", initial)
	}

	if _, err := store.Register("realtime_user", "realtime password", "Realtime User"); err != nil {
		t.Fatal(err)
	}
	var updated adminEventSnapshot
	for updated.ProfileRevision != "1" {
		if err := wsjson.Read(readContext, connection, &updated); err != nil {
			t.Fatal(err)
		}
	}
	if updated.ProfileRevision != "1" || updated.Overview.ProfileCount != "1" {
		t.Fatalf("profile change was not pushed: %+v", updated)
	}

	reused, err := http.Get(server.URL + "/api/admin/v1/events?ticket=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	reused.Body.Close()
	if reused.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused ticket status=%d", reused.StatusCode)
	}
	expiredTicket, _, err := handler.issueEventTicket(time.Now().Add(-2 * adminEventTicketTTL))
	if err != nil {
		t.Fatal(err)
	}
	expired, err := http.Get(server.URL + "/api/admin/v1/events?ticket=" + expiredTicket)
	if err != nil {
		t.Fatal(err)
	}
	expired.Body.Close()
	if expired.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired ticket status=%d", expired.StatusCode)
	}
}

func requestAdminEventTicket(t *testing.T, baseURL string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/admin/v1/events/ticket", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("ticket status=%d body=%s", response.StatusCode, body)
	}
	var envelope struct {
		Data adminEventTicket `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Ticket == "" || envelope.Data.ExpiresAt == "" {
		t.Fatalf("invalid ticket response: %+v", envelope.Data)
	}
	return envelope.Data.Ticket
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
	baseURL := "http://" + server.AdminAddress()
	ticket := requestAdminEventTicket(t, baseURL)
	websocketContext, cancelWebsocket := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelWebsocket()
	events, upgradeResponse, err := websocket.Dial(websocketContext,
		"ws://"+server.AdminAddress()+"/api/admin/v1/events?ticket="+ticket, nil)
	if err != nil {
		if upgradeResponse != nil {
			upgradeResponse.Body.Close()
		}
		t.Fatal(err)
	}
	defer events.CloseNow()
	var liveSnapshot adminEventSnapshot
	if err := wsjson.Read(websocketContext, events, &liveSnapshot); err != nil {
		t.Fatal(err)
	}
	if liveSnapshot.Type != "snapshot" {
		t.Fatalf("live snapshot type = %q", liveSnapshot.Type)
	}
	address := server.AdminAddress()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	closedContext, cancelClosed := context.WithTimeout(context.Background(), time.Second)
	defer cancelClosed()
	if err := wsjson.Read(closedContext, events, &liveSnapshot); err == nil {
		t.Fatal("admin WebSocket remained readable after server shutdown")
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

func TestServerAdminTLSAuthenticatesAndRejectsPlaintext(t *testing.T) {
	tokenFile := writeTestAdminToken(t)
	certFile, keyFile, roots := writeTestTLSCertificate(t)
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.AdminTokenFile = tokenFile
	cfg.AdminTLSCertFile = certFile
	cfg.AdminTLSKeyFile = keyFile
	cfg.PublicHost = "127.0.0.1"
	cfg.DataFile = ""
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

	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "127.0.0.1",
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	baseURL := "https://" + server.AdminAddress()

	unauthorized, err := client.Get(baseURL + "/api/admin/v1/overview")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized HTTPS admin status = %s", unauthorized.Status)
	}
	if unauthorized.TLS == nil || unauthorized.TLS.Version != tls.VersionTLS12 {
		t.Fatalf("admin TLS state = %#v", unauthorized.TLS)
	}
	if got := unauthorized.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("Strict-Transport-Security = %q", got)
	}

	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/admin/v1/overview", nil)
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
		t.Fatalf("authenticated HTTPS admin status = %s", response.Status)
	}

	plaintextRequest, err := http.NewRequest(http.MethodGet, "http://"+server.AdminAddress()+"/api/admin/v1/overview", nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintextRequest.Header.Set("Authorization", "Bearer "+testAdminToken)
	plaintextResponse, plaintextErr := (&http.Client{Timeout: 2 * time.Second}).Do(plaintextRequest)
	if plaintextErr == nil {
		plaintextResponse.Body.Close()
		if plaintextResponse.StatusCode == http.StatusOK {
			t.Fatal("TLS-enabled admin listener accepted an authenticated plaintext request")
		}
	}
}

func TestNewServerValidatesAdminTLSConfiguration(t *testing.T) {
	tokenFile := writeTestAdminToken(t)
	tests := []struct {
		name      string
		configure func(*Config)
		want      string
	}{
		{
			name: "certificate without key",
			configure: func(cfg *Config) {
				cfg.AdminAddr = "127.0.0.1:0"
				cfg.AdminTokenFile = tokenFile
				cfg.AdminTLSCertFile = "admin-cert.pem"
			},
			want: "both --admin-tls-cert and --admin-tls-key",
		},
		{
			name: "key without certificate",
			configure: func(cfg *Config) {
				cfg.AdminAddr = "127.0.0.1:0"
				cfg.AdminTokenFile = tokenFile
				cfg.AdminTLSKeyFile = "admin-key.pem"
			},
			want: "both --admin-tls-cert and --admin-tls-key",
		},
		{
			name: "TLS without admin listener",
			configure: func(cfg *Config) {
				cfg.AdminTLSCertFile = "admin-cert.pem"
				cfg.AdminTLSKeyFile = "admin-key.pem"
			},
			want: "require the admin server to be enabled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.DataFile = ""
			test.configure(&cfg)
			if _, err := NewServer(cfg, nil); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewServer() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestServerAdminTLSCertificateLoadFailureClosesStore(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.AdminTokenFile = writeTestAdminToken(t)
	cfg.AdminTLSCertFile = filepath.Join(t.TempDir(), "missing-cert.pem")
	cfg.AdminTLSKeyFile = filepath.Join(t.TempDir(), "missing-key.pem")
	cfg.DataFile = ""
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "load admin TLS certificate") {
		t.Fatalf("Start() error = %v", err)
	}
	if err := server.store.db.Ping(); err == nil {
		t.Fatal("profile store remained open after admin TLS certificate load failure")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after admin TLS certificate load failure: %v", err)
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
