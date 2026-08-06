package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubPublicActivityReader struct {
	snapshot publicActivitySnapshot
}

func (s stubPublicActivityReader) PublicActivitySnapshot() publicActivitySnapshot {
	return s.snapshot
}

type stubPublicLeaderboardReader struct {
	records []publicLeaderboardRecord
	err     error
	load    func(context.Context) ([]publicLeaderboardRecord, error)
	calls   atomic.Int32
}

func (s *stubPublicLeaderboardReader) PublicLeaderboard(ctx context.Context) ([]publicLeaderboardRecord, error) {
	s.calls.Add(1)
	if s.load != nil {
		return s.load(ctx)
	}
	return append([]publicLeaderboardRecord(nil), s.records...), s.err
}

func testPublicHandler(activity publicActivitySnapshot, leaderboard *stubPublicLeaderboardReader) *publicHandler {
	return newPublicHandler(
		stubPublicActivityReader{snapshot: activity},
		leaderboard,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func TestPublicSnapshotUsesExplicitRedactedDTOs(t *testing.T) {
	activity := publicActivitySnapshot{
		Overview: publicOverview{OnlinePlayers: 3, OpenLobbies: 1, ActiveGames: 1, QueuedPlayers: 1},
		OnlinePlayers: []publicOnlinePlayer{
			{DisplayName: "Alice", Status: "online"},
			{DisplayName: "Bravo", Status: "in_lobby"},
		},
		Lobbies: []publicLobby{{
			Name: "Open Battle", Map: "Maps/Test.map", HostName: "Bravo", Players: 2,
			MaxPlayers: 4, HasPassword: true, Product: "zerohour",
		}},
		ActiveGames: []publicActiveGame{{
			Name: "Quick Match", Players: 2, MaxPlayers: 2, Product: "zerohour", State: "started",
		}},
	}
	leaderboard := &stubPublicLeaderboardReader{records: []publicLeaderboardRecord{{
		DisplayName: "Champion",
		Stats:       PlayerStats{Wins: 9007199254740993, Losses: 2, Games: 9007199254740995, Rating: 2400},
	}}}
	handler := testPublicHandler(activity, leaderboard)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://public.test/api/public/v1/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("public API unexpectedly enabled CORS: %q", response.Header().Get("Access-Control-Allow-Origin"))
	}
	if response.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HTTP public response unexpectedly enabled HSTS")
	}
	for _, header := range []string{
		"Content-Security-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
		"Permissions-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options",
	} {
		if response.Header().Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}

	var envelope publicDataEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	snapshot := envelope.Data
	if _, err := time.Parse(time.RFC3339Nano, snapshot.GeneratedAt); err != nil {
		t.Fatalf("generated_at = %q: %v", snapshot.GeneratedAt, err)
	}
	if snapshot.Overview != activity.Overview || len(snapshot.OnlinePlayers) != 2 || len(snapshot.Lobbies) != 1 || len(snapshot.ActiveGames) != 1 {
		t.Fatalf("unexpected public activity snapshot: %+v", snapshot)
	}
	if len(snapshot.Leaderboard) != 1 || snapshot.Leaderboard[0].Wins != "9007199254740993" ||
		snapshot.Leaderboard[0].Games != "9007199254740995" || snapshot.Leaderboard[0].Rating != "2400" {
		t.Fatalf("leaderboard counters lost precision: %+v", snapshot.Leaderboard)
	}

	var document any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]struct{}{
		"user_id": {}, "username": {}, "created_at": {}, "room_id": {}, "game_id": {},
		"members": {}, "options": {}, "ini_crc": {}, "compatibility_version": {}, "ready_key": {},
		"relay": {}, "profile_count": {}, "datagrams_in": {}, "disconnects": {},
	}
	assertNoPublicJSONKeys(t, document, forbidden)
}

func assertNoPublicJSONKeys(t *testing.T, value any, forbidden map[string]struct{}) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for name, child := range value {
			if _, exists := forbidden[name]; exists {
				t.Errorf("public JSON contains forbidden key %q", name)
			}
			assertNoPublicJSONKeys(t, child, forbidden)
		}
	case []any:
		for _, child := range value {
			assertNoPublicJSONKeys(t, child, forbidden)
		}
	}
}

func TestPublicHandlerStrictRouteAllowlist(t *testing.T) {
	handler := testPublicHandler(publicActivitySnapshot{}, &stubPublicLeaderboardReader{})
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin/"},
		{http.MethodGet, "/api/admin/v1/overview"},
		{http.MethodGet, "/api/admin/v1/profiles"},
		{http.MethodPut, "/api/admin/v1/profiles/1/password"},
		{http.MethodDelete, "/api/admin/v1/profiles/1"},
		{http.MethodGet, "/api/admin/v1/sessions"},
		{http.MethodDelete, "/api/admin/v1/sessions/1"},
		{http.MethodGet, "/api/admin/v1/games"},
		{http.MethodDelete, "/api/admin/v1/games/1"},
		{http.MethodPost, "/api/admin/v1/events/ticket"},
		{http.MethodGet, "/api/admin/v1/events"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/readyz"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/api/public/v1/unknown"},
		{http.MethodGet, "/not-a-public-route"},
		{http.MethodGet, "/assets/not-found.js"},
		{http.MethodGet, "/assets/../admin/"},
		{http.MethodGet, "/assets/%2e%2e/api/admin/v1/overview"},
	} {
		request := httptest.NewRequest(test.method, "http://public.test"+test.path, nil)
		request.Header.Set("Authorization", "Bearer "+testAdminToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s status=%d, want 404; body=%q", test.method, test.path, response.Code, response.Body.String())
		}
		if location := response.Header().Get("Location"); location != "" {
			t.Errorf("%s %s unexpectedly redirected to %q", test.method, test.path, location)
		}
	}

	query := httptest.NewRecorder()
	handler.ServeHTTP(query, httptest.NewRequest(http.MethodGet, "http://public.test/api/public/v1/snapshot?admin=true", nil))
	if query.Code != http.StatusBadRequest {
		t.Fatalf("snapshot query status=%d, want 400", query.Code)
	}
	if query.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot query error cache control=%q", query.Header().Get("Cache-Control"))
	}
	wrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodPost, "http://public.test/api/public/v1/snapshot", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("snapshot POST status=%d, want 405", wrongMethod.Code)
	}
	errorHandler := testPublicHandler(publicActivitySnapshot{}, &stubPublicLeaderboardReader{err: context.DeadlineExceeded})
	internalError := httptest.NewRecorder()
	errorHandler.ServeHTTP(internalError, httptest.NewRequest(http.MethodGet, "http://public.test/api/public/v1/snapshot", nil))
	if internalError.Code != http.StatusInternalServerError || internalError.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot internal error status=%d cache=%q", internalError.Code, internalError.Header().Get("Cache-Control"))
	}
	if handlerWithDefaultLogger := newPublicHandler(stubPublicActivityReader{}, &stubPublicLeaderboardReader{}, nil); handlerWithDefaultLogger.log == nil {
		t.Fatal("public handler retained a nil logger")
	}

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "http://public.test/", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Header().Get("Content-Type"), "text/html") || index.Body.Len() == 0 {
		t.Fatalf("public index status=%d type=%q body=%q", index.Code, index.Header().Get("Content-Type"), index.Body.String())
	}
	if index.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public index cache control = %q", index.Header().Get("Cache-Control"))
	}
	for _, route := range []string{"/leaderboard", "/game-lobbies", "/online-players", "/active-games", "/how-to-play"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://public.test"+route, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "text/html") {
			t.Errorf("approved public route %s status=%d type=%q", route, response.Code, response.Header().Get("Content-Type"))
		}
	}
}

func TestPublicWebRedirectPreservesCanonicalHostPathAndQuery(t *testing.T) {
	handler := newPublicWebRedirectHandler("generals.network")
	assetEntries, err := fs.ReadDir(testPublicHandler(publicActivitySnapshot{}, &stubPublicLeaderboardReader{}).assets, "assets")
	if err != nil {
		t.Fatal(err)
	}
	embeddedAssetPath := ""
	for _, entry := range assetEntries {
		if !entry.IsDir() {
			embeddedAssetPath = "/assets/" + entry.Name()
			break
		}
	}
	if embeddedAssetPath == "" {
		t.Fatal("embedded public assets directory contains no files")
	}
	for _, test := range []struct {
		method string
		target string
		want   string
	}{
		{http.MethodGet, "http://attacker.example/", "https://generals.network/"},
		{http.MethodGet, "http://attacker.example/leaderboard?season=all&player=Alice%20Bob", "https://generals.network/leaderboard?season=all&player=Alice%20Bob"},
		{http.MethodGet, "http://attacker.example/how-to-play?source=nav", "https://generals.network/how-to-play?source=nav"},
		{http.MethodHead, "http://attacker.example" + embeddedAssetPath + "?cache=a%2Fb", "https://generals.network" + embeddedAssetPath + "?cache=a%2Fb"},
		{http.MethodGet, "http://attacker.example/generalsx%2Dzh-icon.png?cache=icon", "https://generals.network/generalsx%2Dzh-icon.png?cache=icon"},
		{http.MethodGet, "http://attacker.example/api/public/v1/snapshot?source=http%3A%2F%2Fevil.example", "https://generals.network/api/public/v1/snapshot?source=http%3A%2F%2Fevil.example"},
	} {
		request := httptest.NewRequest(test.method, test.target, nil)
		request.Host = "poisoned.example:8083"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusPermanentRedirect {
			t.Errorf("%s %s status=%d, want 308", test.method, test.target, response.Code)
		}
		if got := response.Header().Get("Location"); got != test.want {
			t.Errorf("%s %s Location=%q, want %q", test.method, test.target, got, test.want)
		}
	}
}

func TestPublicWebRedirectRejectsPrivateUnknownAndNoncanonicalRoutes(t *testing.T) {
	handler := newPublicWebRedirectHandler("generals.network")
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin"},
		{http.MethodGet, "/admin/"},
		{http.MethodGet, "/api/admin"},
		{http.MethodGet, "/api/admin/v1/overview?redirect=true"},
		{http.MethodGet, "/%61dmin/"},
		{http.MethodGet, "/assets/../admin/"},
		{http.MethodGet, "/assets/%2e%2e/api/admin/v1/overview"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/readyz"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/unknown"},
		{http.MethodGet, "/how-to-play/"},
		{http.MethodGet, "/assets/"},
		{http.MethodGet, "/assets/not-found.js"},
		{http.MethodGet, "/assets/admin/secret.js"},
		{http.MethodPost, "/leaderboard"},
	} {
		request := httptest.NewRequest(test.method, "http://public.test"+test.path, nil)
		request.Header.Set("Authorization", "Bearer "+testAdminToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s status=%d, want 404", test.method, test.path, response.Code)
		}
		if location := response.Header().Get("Location"); location != "" {
			t.Errorf("%s %s unexpectedly redirected to %q", test.method, test.path, location)
		}
	}
}

func TestPublicAssetsDoNotContainAdminAPICapabilities(t *testing.T) {
	handler := testPublicHandler(publicActivitySnapshot{}, &stubPublicLeaderboardReader{})
	if err := fs.WalkDir(handler.assets, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		switch pathExtension := path.Ext(name); pathExtension {
		case ".html", ".js", ".css":
		default:
			return nil
		}
		content, err := fs.ReadFile(handler.assets, name)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), "/api/admin/v1/") {
			t.Errorf("public asset %s contains an admin API path", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicLeaderboardCacheCoalescesBursts(t *testing.T) {
	leaderboard := &stubPublicLeaderboardReader{records: []publicLeaderboardRecord{{DisplayName: "Alice"}}}
	handler := testPublicHandler(publicActivitySnapshot{}, leaderboard)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	handler.now = func() time.Time { return now }

	const requests = 32
	start := make(chan struct{})
	statuses := make(chan int, requests)
	var wait sync.WaitGroup
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://public.test/api/public/v1/snapshot", nil))
			statuses <- response.Code
		}()
	}
	close(start)
	wait.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Errorf("burst response status=%d", status)
		}
	}
	if calls := leaderboard.calls.Load(); calls != 1 {
		t.Fatalf("leaderboard query calls=%d after burst, want 1", calls)
	}

	now = now.Add(publicLeaderboardCacheTTL)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://public.test/api/public/v1/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("post-expiry response status=%d", response.Code)
	}
	if calls := leaderboard.calls.Load(); calls != 2 {
		t.Fatalf("leaderboard query calls=%d after cache expiry, want 2", calls)
	}
}

func TestPublicLeaderboardCancellationDoesNotPoisonCache(t *testing.T) {
	leaderboard := &stubPublicLeaderboardReader{}
	leaderboard.load = func(ctx context.Context) ([]publicLeaderboardRecord, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []publicLeaderboardRecord{{DisplayName: "Healthy Player", Stats: PlayerStats{Games: 1}}}, nil
	}
	handler := testPublicHandler(publicActivitySnapshot{}, leaderboard)
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	handler.now = func() time.Time { return now }

	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	cancelledRequest := httptest.NewRequest(http.MethodGet, "http://public.test/api/public/v1/snapshot", nil).WithContext(cancelledContext)
	cancelledResponse := httptest.NewRecorder()
	handler.ServeHTTP(cancelledResponse, cancelledRequest)
	if cancelledResponse.Code != http.StatusInternalServerError {
		t.Fatalf("cancelled request status=%d, want 500", cancelledResponse.Code)
	}
	if calls := leaderboard.calls.Load(); calls != 1 {
		t.Fatalf("leaderboard calls after cancellation=%d, want 1", calls)
	}

	healthyResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthyResponse, httptest.NewRequest(http.MethodGet, "http://public.test/api/public/v1/snapshot", nil))
	if healthyResponse.Code != http.StatusOK {
		t.Fatalf("healthy request status=%d body=%q", healthyResponse.Code, healthyResponse.Body.String())
	}
	if calls := leaderboard.calls.Load(); calls != 2 {
		t.Fatalf("healthy request did not retry leaderboard query; calls=%d", calls)
	}
	var envelope publicDataEnvelope
	if err := json.Unmarshal(healthyResponse.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Leaderboard) != 1 || envelope.Data.Leaderboard[0].DisplayName != "Healthy Player" {
		t.Fatalf("healthy leaderboard response=%+v", envelope.Data.Leaderboard)
	}

	cachedResponse := httptest.NewRecorder()
	handler.ServeHTTP(cachedResponse, httptest.NewRequest(http.MethodGet, "http://public.test/api/public/v1/snapshot", nil))
	if cachedResponse.Code != http.StatusOK {
		t.Fatalf("cached healthy request status=%d", cachedResponse.Code)
	}
	if calls := leaderboard.calls.Load(); calls != 2 {
		t.Fatalf("successful retry was not cached; calls=%d", calls)
	}
}

func TestHubPublicActivitySnapshotRedactsAndClassifiesState(t *testing.T) {
	hub := NewHub(DefaultConfig(), nil, nil, nil)
	clients := map[uint64]*controlClient{
		1: {profile: Profile{UserID: 1, DisplayName: "Lobby Host"}},
		2: {profile: Profile{UserID: 2, DisplayName: "Active Player"}},
		3: {profile: Profile{UserID: 3, DisplayName: "Queued Player"}},
		4: {profile: Profile{UserID: 4, DisplayName: "Away Player"}, status: "away"},
		5: {profile: Profile{UserID: 5, DisplayName: "Online Player"}, status: "unexpected"},
		6: {profile: Profile{UserID: 6, DisplayName: "Quickmatch Player"}},
	}
	for userID, client := range clients {
		hub.clients[userID] = client
	}
	hub.games[10] = &stagedGame{
		id: 10, name: "Open Battle", password: "secret", maxPlayers: 4,
		compatibility: GameCompatibility{Product: "zerohour"}, hostID: 1, state: "open", listed: true,
		options: GameOptions{Map: "Maps/Open.map"}, members: map[uint64]*gameMember{1: {client: clients[1]}},
	}
	hub.games[20] = &stagedGame{
		id: 20, name: "Started Battle", maxPlayers: 4,
		compatibility: GameCompatibility{Product: "generals"}, hostID: 2, state: "started", listed: true,
		options: GameOptions{Map: "Maps/Started.map", ReadyKey: "must-not-leak"}, members: map[uint64]*gameMember{2: {client: clients[2]}},
	}
	hub.games[30] = &stagedGame{
		id: 30, name: "Private Quickmatch Name", maxPlayers: 2,
		compatibility: GameCompatibility{Product: "zerohour"}, hostID: 6, state: "starting", listed: false,
		options: GameOptions{Map: "Private/Map", Opaque: "private-options"}, members: map[uint64]*gameMember{6: {client: clients[6]}},
	}
	hub.userGame[1] = 10
	hub.userGame[2] = 20
	hub.userGame[6] = 30
	hub.matchQueue[3] = matchEntry{client: clients[3]}

	snapshot := hub.PublicActivitySnapshot()
	if snapshot.Overview != (publicOverview{OnlinePlayers: 6, OpenLobbies: 1, ActiveGames: 2, QueuedPlayers: 1}) {
		t.Fatalf("overview = %+v", snapshot.Overview)
	}
	statuses := make(map[string]string, len(snapshot.OnlinePlayers))
	for _, player := range snapshot.OnlinePlayers {
		statuses[player.DisplayName] = player.Status
	}
	for name, want := range map[string]string{
		"Lobby Host": "in_lobby", "Active Player": "in_game", "Queued Player": "quick_match",
		"Away Player": "away", "Online Player": "online", "Quickmatch Player": "in_game",
	} {
		if got := statuses[name]; got != want {
			t.Errorf("status for %q = %q, want %q", name, got, want)
		}
	}
	if len(snapshot.Lobbies) != 1 || snapshot.Lobbies[0] != (publicLobby{
		Name: "Open Battle", Map: "Maps/Open.map", HostName: "Lobby Host", Players: 1,
		MaxPlayers: 4, HasPassword: true, Product: "zerohour",
	}) {
		t.Fatalf("public lobbies = %+v", snapshot.Lobbies)
	}
	if len(snapshot.ActiveGames) != 2 {
		t.Fatalf("public active games = %+v", snapshot.ActiveGames)
	}
	var quickmatch *publicActiveGame
	for index := range snapshot.ActiveGames {
		if snapshot.ActiveGames[index].Name == "Quick Match" {
			quickmatch = &snapshot.ActiveGames[index]
		}
	}
	if quickmatch == nil || quickmatch.Map != "" || quickmatch.Players != 1 || quickmatch.MaxPlayers != 2 ||
		quickmatch.Product != "zerohour" || quickmatch.State != "starting" {
		t.Fatalf("redacted quickmatch = %+v", quickmatch)
	}
	encoded, err := json.Marshal(snapshot.ActiveGames)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Private Quickmatch Name", "Private/Map", "private-options", "must-not-leak"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("active-game JSON leaked %q: %s", secret, encoded)
		}
	}
}

func TestServerPublicWebTLSRedirectLifecycleAndIsolation(t *testing.T) {
	certFile, keyFile, roots := writeTestTLSCertificate(t)
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicWebAddr = "127.0.0.1:0"
	cfg.PublicWebTLSCertFile = certFile
	cfg.PublicWebTLSKeyFile = keyFile
	cfg.PublicWebRedirectAddr = "127.0.0.1:0"
	cfg.PublicWebCanonicalHost = "generals.network"
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.AdminTokenFile = writeTestAdminToken(t)
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

	addresses := map[string]string{
		"health":   server.HealthAddress(),
		"public":   server.PublicWebAddress(),
		"redirect": server.PublicWebRedirectAddress(),
		"admin":    server.AdminAddress(),
	}
	ports := make(map[string]string, len(addresses))
	for name, address := range addresses {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatalf("%s address %q: %v", name, address, err)
		}
		if previous, exists := ports[port]; exists {
			t.Fatalf("%s and %s resolved to the same TCP port %s", previous, name, port)
		}
		ports[port] = name
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "127.0.0.1",
	}}
	defer transport.CloseIdleConnections()
	httpsClient := &http.Client{Transport: transport, Timeout: 2 * time.Second}

	publicResponse, err := httpsClient.Get("https://" + server.PublicWebAddress() + "/api/public/v1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	publicResponse.Body.Close()
	if publicResponse.StatusCode != http.StatusOK || publicResponse.TLS == nil || publicResponse.TLS.Version != tls.VersionTLS12 {
		t.Fatalf("public HTTPS response status=%s TLS=%#v", publicResponse.Status, publicResponse.TLS)
	}
	if got := publicResponse.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("public Strict-Transport-Security = %q", got)
	}
	legacyTransport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS10,
		MaxVersion: tls.VersionTLS11,
		RootCAs:    roots,
		ServerName: "127.0.0.1",
	}}
	defer legacyTransport.CloseIdleConnections()
	legacyResponse, legacyErr := (&http.Client{Transport: legacyTransport, Timeout: 2 * time.Second}).Get(
		"https://" + server.PublicWebAddress() + "/",
	)
	if legacyErr == nil {
		legacyResponse.Body.Close()
		t.Fatalf("public TLS listener accepted legacy TLS version %x", legacyResponse.TLS.Version)
	}

	for _, requestTarget := range []string{"/admin/", "/api/admin/v1/overview", "/healthz"} {
		request, err := http.NewRequest(http.MethodGet, "https://"+server.PublicWebAddress()+requestTarget, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+testAdminToken)
		response, err := httpsClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("public HTTPS %s status=%s, want 404", requestTarget, response.Status)
		}
	}

	adminRequest, err := http.NewRequest(http.MethodGet, "https://"+server.AdminAddress()+"/api/public/v1/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	adminRequest.Header.Set("Authorization", "Bearer "+testAdminToken)
	adminResponse, err := httpsClient.Do(adminRequest)
	if err != nil {
		t.Fatal(err)
	}
	adminResponse.Body.Close()
	if adminResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("admin HTTPS exposed public snapshot: status=%s", adminResponse.Status)
	}

	plaintextResponse, plaintextErr := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + server.PublicWebAddress() + "/")
	if plaintextErr == nil {
		plaintextResponse.Body.Close()
		if plaintextResponse.StatusCode == http.StatusOK || plaintextResponse.StatusCode == http.StatusPermanentRedirect {
			t.Fatalf("TLS public listener accepted plaintext: status=%s", plaintextResponse.Status)
		}
	}

	redirectClient := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	redirectRequest, err := http.NewRequest(http.MethodGet,
		"http://"+server.PublicWebRedirectAddress()+"/leaderboard?season=all%2Btime", nil)
	if err != nil {
		t.Fatal(err)
	}
	redirectRequest.Host = "attacker.example"
	redirectResponse, err := redirectClient.Do(redirectRequest)
	if err != nil {
		t.Fatal(err)
	}
	redirectResponse.Body.Close()
	if redirectResponse.StatusCode != http.StatusPermanentRedirect ||
		redirectResponse.Header.Get("Location") != "https://generals.network/leaderboard?season=all%2Btime" {
		t.Fatalf("live redirect status=%s Location=%q", redirectResponse.Status, redirectResponse.Header.Get("Location"))
	}

	privateRedirectRequest, err := http.NewRequest(http.MethodGet,
		"http://"+server.PublicWebRedirectAddress()+"/api/admin/v1/overview", nil)
	if err != nil {
		t.Fatal(err)
	}
	privateRedirectRequest.Header.Set("Authorization", "Bearer "+testAdminToken)
	privateRedirectResponse, err := redirectClient.Do(privateRedirectRequest)
	if err != nil {
		t.Fatal(err)
	}
	privateRedirectResponse.Body.Close()
	if privateRedirectResponse.StatusCode != http.StatusNotFound || privateRedirectResponse.Header.Get("Location") != "" {
		t.Fatalf("admin redirect isolation status=%s Location=%q", privateRedirectResponse.Status, privateRedirectResponse.Header.Get("Location"))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	for name, address := range map[string]string{
		"public": server.PublicWebAddress(), "redirect": server.PublicWebRedirectAddress(), "admin": server.AdminAddress(),
	} {
		if _, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
			t.Errorf("%s listener remained open after shutdown", name)
		}
	}
}

func TestServerPublicWebLifecycleAndPortIsolation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.RelayAddr = "127.0.0.1:0"
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.PublicWebAddr = "127.0.0.1:0"
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
	_, publicPort, err := net.SplitHostPort(server.PublicWebAddress())
	if err != nil {
		t.Fatal(err)
	}
	_, adminPort, err := net.SplitHostPort(server.AdminAddress())
	if err != nil {
		t.Fatal(err)
	}
	if publicPort == adminPort {
		t.Fatalf("public web and admin listeners resolved to the same TCP port %s", publicPort)
	}

	client := &http.Client{Timeout: time.Second}
	request := func(address, method, path string, adminToken bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+address+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if adminToken {
			req.Header.Set("Authorization", "Bearer "+testAdminToken)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	for _, test := range []struct {
		address string
		path    string
		admin   bool
		want    int
	}{
		{server.PublicWebAddress(), "/api/public/v1/snapshot", false, http.StatusOK},
		{server.PublicWebAddress(), "/api/admin/v1/overview", true, http.StatusNotFound},
		{server.PublicWebAddress(), "/admin/", true, http.StatusNotFound},
		{server.PublicWebAddress(), "/healthz", false, http.StatusNotFound},
		{server.AdminAddress(), "/api/public/v1/snapshot", true, http.StatusNotFound},
	} {
		response := request(test.address, http.MethodGet, test.path, test.admin)
		response.Body.Close()
		if response.StatusCode != test.want {
			t.Errorf("GET %s on %s status=%d, want %d", test.path, test.address, response.StatusCode, test.want)
		}
	}

	address := server.PublicWebAddress()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
		t.Fatal("public web listener remained open after shutdown")
	}
}

func TestServerPublicWebBindAndRuntimeFailuresCleanUp(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	failedConfig := DefaultConfig()
	failedConfig.ControlAddr = "127.0.0.1:0"
	failedConfig.RelayAddr = "127.0.0.1:0"
	failedConfig.HealthAddr = "127.0.0.1:0"
	failedConfig.PublicWebAddr = occupied.Addr().String()
	failedConfig.PublicHost = "127.0.0.1"
	failedConfig.DataFile = ""
	failed, err := NewServer(failedConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "listen for public web requests") {
		t.Fatalf("public bind error = %v", err)
	}
	if err := failed.store.db.Ping(); err == nil {
		t.Fatal("profile store remained open after public bind failure")
	}
	if err := failed.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after public bind failure: %v", err)
	}

	occupiedAdmin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupiedAdmin.Close()
	adminFailureConfig := DefaultConfig()
	adminFailureConfig.ControlAddr = "127.0.0.1:0"
	adminFailureConfig.RelayAddr = "127.0.0.1:0"
	adminFailureConfig.HealthAddr = "127.0.0.1:0"
	adminFailureConfig.PublicWebAddr = "127.0.0.1:0"
	adminFailureConfig.AdminAddr = occupiedAdmin.Addr().String()
	adminFailureConfig.AdminTokenFile = writeTestAdminToken(t)
	adminFailureConfig.PublicHost = "127.0.0.1"
	adminFailureConfig.DataFile = ""
	adminFailure, err := NewServer(adminFailureConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := adminFailure.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "listen for admin requests") {
		t.Fatalf("admin bind error = %v", err)
	}
	publicAddress := adminFailure.PublicWebAddress()
	if _, err := net.DialTimeout("tcp", publicAddress, 200*time.Millisecond); err == nil {
		t.Fatal("public web listener remained open after admin bind failure")
	}
	if err := adminFailure.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after admin bind failure: %v", err)
	}

	runtimeConfig := DefaultConfig()
	runtimeConfig.ControlAddr = "127.0.0.1:0"
	runtimeConfig.RelayAddr = "127.0.0.1:0"
	runtimeConfig.HealthAddr = "127.0.0.1:0"
	runtimeConfig.PublicWebAddr = "127.0.0.1:0"
	runtimeConfig.PublicHost = "127.0.0.1"
	runtimeConfig.DataFile = ""
	server, err := NewServer(runtimeConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.publicWebLn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server.Errors():
		if err == nil || !strings.Contains(err.Error(), "serve public web requests") {
			t.Fatalf("runtime error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for public web listener error")
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

func TestServerPublicWebTLSCertificateLoadFailureClosesStore(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PublicWebAddr = "127.0.0.1:0"
	cfg.PublicWebTLSCertFile = filepath.Join(t.TempDir(), "missing-cert.pem")
	cfg.PublicWebTLSKeyFile = filepath.Join(t.TempDir(), "missing-key.pem")
	cfg.DataFile = ""
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "load public web TLS certificate") {
		t.Fatalf("Start() error = %v", err)
	}
	if err := server.store.db.Ping(); err == nil {
		t.Fatal("profile store remained open after public TLS certificate load failure")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after public TLS certificate load failure: %v", err)
	}
}

func TestServerPublicWebRedirectBindAndRuntimeFailuresCleanUp(t *testing.T) {
	certFile, keyFile, _ := writeTestTLSCertificate(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	failedConfig := DefaultConfig()
	failedConfig.ControlAddr = "127.0.0.1:0"
	failedConfig.RelayAddr = "127.0.0.1:0"
	failedConfig.HealthAddr = "127.0.0.1:0"
	failedConfig.PublicWebAddr = "127.0.0.1:0"
	failedConfig.PublicWebTLSCertFile = certFile
	failedConfig.PublicWebTLSKeyFile = keyFile
	failedConfig.PublicWebRedirectAddr = occupied.Addr().String()
	failedConfig.PublicWebCanonicalHost = "generals.network"
	failedConfig.PublicHost = "127.0.0.1"
	failedConfig.DataFile = ""
	failed, err := NewServer(failedConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "listen for public web redirect requests") {
		t.Fatalf("redirect bind error = %v", err)
	}
	if _, err := net.DialTimeout("tcp", failed.PublicWebAddress(), 200*time.Millisecond); err == nil {
		t.Fatal("public web listener remained open after redirect bind failure")
	}
	if err := failed.store.db.Ping(); err == nil {
		t.Fatal("profile store remained open after redirect bind failure")
	}
	if err := failed.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after redirect bind failure: %v", err)
	}

	runtimeConfig := failedConfig
	runtimeConfig.PublicWebRedirectAddr = "127.0.0.1:0"
	server, err := NewServer(runtimeConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.publicWebRedirectLn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server.Errors():
		if err == nil || !strings.Contains(err.Error(), "serve public web redirect requests") {
			t.Fatalf("runtime error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for public web redirect listener error")
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}
