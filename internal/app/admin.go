package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/moloch--/generals-server/internal/app/adminui"
)

const (
	minimumAdminTokenBytes = 32
	maximumAdminTokenBytes = 4096
	defaultAdminPageLimit  = 50
	maximumAdminPageLimit  = 100
	maximumAdminBodyBytes  = 1024

	adminEventTicketTTL          = 30 * time.Second
	adminEventInterval           = time.Second
	adminEventWriteTimeout       = 5 * time.Second
	maximumAdminEventTickets     = 64
	maximumAdminEventConnections = 8
)

type adminTokenHash [sha256.Size]byte

type adminHandler struct {
	tokenHash adminTokenHash
	store     *ProfileStore
	hub       *Hub
	relay     *Relay
	log       *slog.Logger
	startedAt time.Time
	assets    fs.FS
	mux       *http.ServeMux

	eventsContext        context.Context
	eventsCancel         context.CancelFunc
	eventsMu             sync.Mutex
	eventsClosing        bool
	eventTickets         map[[sha256.Size]byte]time.Time
	eventConnections     map[*websocket.Conn]struct{}
	eventConnectionCount int
	eventConnectionWG    sync.WaitGroup
	eventSequence        atomic.Uint64
}

type adminDataEnvelope struct {
	Data any `json:"data"`
}

type adminErrorEnvelope struct {
	Error adminError `json:"error"`
}

type adminError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type adminOverview struct {
	Status        string          `json:"status"`
	Protocol      int             `json:"protocol"`
	StartedAt     string          `json:"started_at"`
	UptimeSeconds string          `json:"uptime_seconds"`
	ProfileCount  string          `json:"profile_count"`
	Hub           HubStats        `json:"hub"`
	Relay         adminRelayStats `json:"relay"`
}

type adminRelayStats struct {
	DatagramsIn       string `json:"datagrams_in"`
	DatagramsOut      string `json:"datagrams_out"`
	BytesIn           string `json:"bytes_in"`
	BytesOut          string `json:"bytes_out"`
	DroppedMalformed  string `json:"dropped_malformed"`
	DroppedAuth       string `json:"dropped_auth"`
	DroppedRateLimit  string `json:"dropped_rate_limit"`
	DroppedNoEndpoint string `json:"dropped_no_endpoint"`
	BufferedUntilBind string `json:"buffered_until_bind"`
	ActiveGames       int    `json:"active_games"`
}

type adminProfile struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
	Wins        string `json:"wins"`
	Losses      string `json:"losses"`
	Disconnects string `json:"disconnects"`
	Games       string `json:"games"`
	Rating      string `json:"rating"`
}

type adminProfilePage struct {
	Profiles []adminProfile `json:"profiles"`
	Total    string         `json:"total"`
	Limit    int            `json:"limit"`
	Offset   int            `json:"offset"`
}

type adminSession struct {
	UserID           string `json:"user_id"`
	Username         string `json:"username,omitempty"`
	DisplayName      string `json:"display_name"`
	Guest            bool   `json:"guest"`
	CreatedAt        string `json:"created_at"`
	Status           string `json:"status"`
	RoomID           string `json:"room_id,omitempty"`
	GameID           string `json:"game_id,omitempty"`
	QuickmatchQueued bool   `json:"quickmatch_queued"`
}

type adminMember struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Host        bool   `json:"host"`
	Ready       bool   `json:"ready"`
	Slot        int    `json:"slot"`
}

type adminGame struct {
	GameCompatibility
	GameID      string        `json:"game_id"`
	Name        string        `json:"name"`
	Map         string        `json:"map,omitempty"`
	HostName    string        `json:"host_name"`
	Players     int           `json:"players"`
	MaxPlayers  int           `json:"max_players"`
	HasPassword bool          `json:"has_password"`
	State       string        `json:"state"`
	Listed      bool          `json:"listed"`
	Members     []adminMember `json:"members"`
	Options     GameOptions   `json:"options"`
}

type adminHubSnapshot struct {
	Sessions []adminSession
	Games    []adminGame
}

type adminEventTicket struct {
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expires_at"`
}

type adminEventSnapshot struct {
	Type            string         `json:"type"`
	Sequence        string         `json:"sequence"`
	ProfileRevision string         `json:"profile_revision"`
	Overview        adminOverview  `json:"overview"`
	Sessions        []adminSession `json:"sessions"`
	Games           []adminGame    `json:"games"`
}

func loadAdminTokenHash(filename string) (adminTokenHash, error) {
	file, err := os.Open(filename)
	if err != nil {
		return adminTokenHash{}, fmt.Errorf("open admin token file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return adminTokenHash{}, fmt.Errorf("inspect admin token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return adminTokenHash{}, errors.New("admin token file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return adminTokenHash{}, errors.New("admin token file must not be accessible by group or other users")
	}

	raw, err := io.ReadAll(io.LimitReader(file, maximumAdminTokenBytes+2))
	if err != nil {
		return adminTokenHash{}, fmt.Errorf("read admin token file: %w", err)
	}
	token := bytes.TrimSpace(raw)
	if len(token) < minimumAdminTokenBytes || len(token) > maximumAdminTokenBytes {
		clear(raw)
		return adminTokenHash{}, fmt.Errorf("admin token must contain between %d and %d bytes", minimumAdminTokenBytes, maximumAdminTokenBytes)
	}
	for _, value := range token {
		if value < 0x21 || value > 0x7e {
			clear(raw)
			return adminTokenHash{}, errors.New("admin token must contain printable ASCII without whitespace")
		}
	}
	hash := adminTokenHash(sha256.Sum256(token))
	clear(raw)
	return hash, nil
}

func newAdminHandler(tokenHash adminTokenHash, store *ProfileStore, hub *Hub, relay *Relay, logger *slog.Logger, startedAt time.Time) *adminHandler {
	eventsContext, eventsCancel := context.WithCancel(context.Background())
	handler := &adminHandler{
		tokenHash:        tokenHash,
		store:            store,
		hub:              hub,
		relay:            relay,
		log:              logger,
		startedAt:        startedAt.UTC(),
		assets:           adminui.Files(),
		mux:              http.NewServeMux(),
		eventsContext:    eventsContext,
		eventsCancel:     eventsCancel,
		eventTickets:     make(map[[sha256.Size]byte]time.Time),
		eventConnections: make(map[*websocket.Conn]struct{}),
	}
	handler.mux.HandleFunc("GET /api/admin/v1/overview", handler.handleOverview)
	handler.mux.HandleFunc("GET /api/admin/v1/profiles", handler.handleProfiles)
	handler.mux.HandleFunc("PUT /api/admin/v1/profiles/{userID}/password", handler.handleResetPassword)
	handler.mux.HandleFunc("DELETE /api/admin/v1/profiles/{userID}", handler.handleDeleteProfile)
	handler.mux.HandleFunc("GET /api/admin/v1/sessions", handler.handleSessions)
	handler.mux.HandleFunc("DELETE /api/admin/v1/sessions/{userID}", handler.handleDeleteSession)
	handler.mux.HandleFunc("GET /api/admin/v1/games", handler.handleGames)
	handler.mux.HandleFunc("DELETE /api/admin/v1/games/{gameID}", handler.handleDeleteGame)
	handler.mux.HandleFunc("POST /api/admin/v1/events/ticket", handler.handleEventTicket)
	handler.mux.HandleFunc("GET /api/admin/v1/events", handler.handleEvents)
	handler.mux.HandleFunc("GET /admin/", handler.handleUI)
	handler.mux.HandleFunc("GET /", handler.handleRoot)
	return handler
}

func (a *adminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setAdminSecurityHeaders(w)
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodGet && r.URL.Path == "/api/admin/v1/events" {
			a.mux.ServeHTTP(w, r)
			return
		}
		if !a.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="generals-admin"`)
			writeAdminError(w, http.StatusUnauthorized, "unauthorized", "a valid admin bearer token is required")
			return
		}
	}
	a.mux.ServeHTTP(w, r)
}

func (a *adminHandler) authorized(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maximumAdminTokenBytes+16 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], a.tokenHash[:]) == 1
}

func setAdminSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; img-src 'self' data:; font-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func (a *adminHandler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
}

func (a *adminHandler) handleUI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	content, err := fs.ReadFile(a.assets, name)
	if err != nil && path.Ext(name) == "" {
		name = "index.html"
		content, err = fs.ReadFile(a.assets, name)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (a *adminHandler) handleOverview(w http.ResponseWriter, r *http.Request) {
	overview, err := a.makeOverview(r.Context())
	if err != nil {
		a.internalError(w, r, "read overview", err)
		return
	}
	writeAdminData(w, http.StatusOK, overview)
}

func (a *adminHandler) makeOverview(ctx context.Context) (adminOverview, error) {
	count, err := a.store.profileCount(ctx)
	if err != nil {
		return adminOverview{}, err
	}
	uptime := time.Since(a.startedAt)
	if uptime < 0 {
		uptime = 0
	}
	relay := a.relay.Stats()
	return adminOverview{
		Status:        "ok",
		Protocol:      ProtocolVersion,
		StartedAt:     a.startedAt.Format(time.RFC3339Nano),
		UptimeSeconds: strconv.FormatUint(uint64(uptime/time.Second), 10),
		ProfileCount:  strconv.FormatUint(count, 10),
		Hub:           a.hub.Stats(),
		Relay:         makeAdminRelayStats(relay),
	}, nil
}

func makeAdminRelayStats(stats RelayStats) adminRelayStats {
	return adminRelayStats{
		DatagramsIn:       strconv.FormatUint(stats.DatagramsIn, 10),
		DatagramsOut:      strconv.FormatUint(stats.DatagramsOut, 10),
		BytesIn:           strconv.FormatUint(stats.BytesIn, 10),
		BytesOut:          strconv.FormatUint(stats.BytesOut, 10),
		DroppedMalformed:  strconv.FormatUint(stats.DroppedMalformed, 10),
		DroppedAuth:       strconv.FormatUint(stats.DroppedAuth, 10),
		DroppedRateLimit:  strconv.FormatUint(stats.DroppedRateLimit, 10),
		DroppedNoEndpoint: strconv.FormatUint(stats.DroppedNoEndpoint, 10),
		BufferedUntilBind: strconv.FormatUint(stats.BufferedUntilBind, 10),
		ActiveGames:       stats.ActiveGames,
	}
}

func (a *adminHandler) handleProfiles(w http.ResponseWriter, r *http.Request) {
	queryValues := r.URL.Query()
	for name := range queryValues {
		if name != "query" && name != "limit" && name != "offset" {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "unknown query parameter: "+name)
			return
		}
	}
	queries := queryValues["query"]
	if len(queries) > 1 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "query must be provided at most once")
		return
	}
	query := ""
	if len(queries) == 1 {
		query = strings.TrimSpace(queries[0])
	}
	if !utf8.ValidString(query) || len(query) > 64 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "query must be valid UTF-8 and at most 64 bytes")
		return
	}
	limit, err := adminQueryInt(queryValues, "limit", defaultAdminPageLimit, 1, maximumAdminPageLimit)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	offset, err := adminQueryInt(queryValues, "offset", 0, 0, maxSupportedProfiles)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	records, total, err := a.store.listAdminProfiles(r.Context(), query, limit, offset)
	if err != nil {
		a.internalError(w, r, "list profiles", err)
		return
	}
	profiles := make([]adminProfile, 0, len(records))
	for _, record := range records {
		profiles = append(profiles, adminProfile{
			UserID:      strconv.FormatUint(record.Profile.UserID, 10),
			Username:    record.Profile.Username,
			DisplayName: record.Profile.DisplayName,
			CreatedAt:   record.Profile.CreatedAt.UTC().Format(time.RFC3339Nano),
			Wins:        strconv.FormatUint(record.Stats.Wins, 10),
			Losses:      strconv.FormatUint(record.Stats.Losses, 10),
			Disconnects: strconv.FormatUint(record.Stats.Disconnects, 10),
			Games:       strconv.FormatUint(record.Stats.Games, 10),
			Rating:      strconv.FormatInt(record.Stats.Rating, 10),
		})
	}
	writeAdminData(w, http.StatusOK, adminProfilePage{
		Profiles: profiles,
		Total:    strconv.FormatUint(total, 10),
		Limit:    limit,
		Offset:   offset,
	})
}

// GeneralsX @feature OpenAI 02/08/2026 Add authenticated password-reset and profile-delete administration.
func (a *adminHandler) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("userID")
	userID, err := parseAdminUserID(value)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_user_id", err.Error())
		return
	}
	var request struct {
		Password string `json:"password"`
	}
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	defer func() { request.Password = "" }()
	if err := validatePassword(request.Password); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_password", err.Error())
		return
	}
	updated, err := a.hub.AdminResetPassword(userID, request.Password)
	if err != nil {
		a.internalError(w, r, "reset profile password", err)
		return
	}
	if !updated {
		writeAdminError(w, http.StatusNotFound, "profile_not_found", "profile not found")
		return
	}
	a.log.Info("admin reset profile password", "user_id", value, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

func (a *adminHandler) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("userID")
	userID, err := parseAdminUserID(value)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_user_id", err.Error())
		return
	}
	deleted, err := a.hub.AdminDeleteProfile(userID)
	if err != nil {
		a.internalError(w, r, "delete profile", err)
		return
	}
	if !deleted {
		writeAdminError(w, http.StatusNotFound, "profile_not_found", "profile not found")
		return
	}
	a.log.Info("admin deleted profile", "user_id", value, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximumAdminBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("request body must be one valid JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON object")
	}
	return nil
}

func adminQueryInt(values map[string][]string, name string, defaultValue, minimum, maximum int) (int, error) {
	items := values[name]
	if len(items) == 0 {
		return defaultValue, nil
	}
	if len(items) != 1 || items[0] == "" {
		return 0, fmt.Errorf("%s must be provided exactly once", name)
	}
	value, err := strconv.Atoi(items[0])
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func (a *adminHandler) handleSessions(w http.ResponseWriter, _ *http.Request) {
	snapshot := a.hub.AdminSnapshot()
	writeAdminData(w, http.StatusOK, struct {
		Sessions []adminSession `json:"sessions"`
	}{Sessions: snapshot.Sessions})
}

func (a *adminHandler) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("userID")
	userID, err := parseAdminUserID(value)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_user_id", err.Error())
		return
	}
	if !a.hub.AdminDisconnect(userID) {
		writeAdminError(w, http.StatusNotFound, "session_not_found", "online session not found")
		return
	}
	a.log.Info("admin disconnected online session", "user_id", value, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

func parseAdminUserID(value string) (uint64, error) {
	if value == "" || len(value) > 20 {
		return 0, errors.New("user ID must be a decimal unsigned integer")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("user ID must be a decimal unsigned integer")
		}
	}
	userID, err := strconv.ParseUint(value, 10, 64)
	if err != nil || userID == 0 {
		return 0, errors.New("user ID must be a non-zero decimal unsigned integer")
	}
	return userID, nil
}

func (a *adminHandler) handleGames(w http.ResponseWriter, _ *http.Request) {
	snapshot := a.hub.AdminSnapshot()
	writeAdminData(w, http.StatusOK, struct {
		Games []adminGame `json:"games"`
	}{Games: snapshot.Games})
}

func (a *adminHandler) handleDeleteGame(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("gameID")
	gameID, err := parseGameID(value)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_game_id", err.Error())
		return
	}
	if !a.hub.AdminCloseGame(gameID) {
		writeAdminError(w, http.StatusNotFound, "game_not_found", "game not found")
		return
	}
	a.log.Info("admin closed game", "game_id", value, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

// GeneralsX @feature OpenAI 02/08/2026 Stream near-realtime admin snapshots through single-use WebSocket tickets.
func (a *adminHandler) handleEventTicket(w http.ResponseWriter, r *http.Request) {
	ticket, expiresAt, err := a.issueEventTicket(time.Now().UTC())
	if err != nil {
		if errors.Is(err, errAdminEventCapacity) {
			writeAdminError(w, http.StatusTooManyRequests, "event_capacity", "too many pending realtime connection requests")
			return
		}
		a.internalError(w, r, "issue realtime ticket", err)
		return
	}
	writeAdminData(w, http.StatusCreated, adminEventTicket{
		Ticket:    ticket,
		ExpiresAt: expiresAt.Format(time.RFC3339Nano),
	})
}

var errAdminEventCapacity = errors.New("admin realtime ticket capacity reached")

func (a *adminHandler) issueEventTicket(now time.Time) (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("generate realtime ticket: %w", err)
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw[:])
	clear(raw[:])
	hash := sha256.Sum256([]byte(ticket))
	expiresAt := now.Add(adminEventTicketTTL)

	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	if a.eventsClosing {
		return "", time.Time{}, errors.New("admin realtime service is shutting down")
	}
	for existing, expiration := range a.eventTickets {
		if !expiration.After(now) {
			delete(a.eventTickets, existing)
		}
	}
	if len(a.eventTickets) >= maximumAdminEventTickets {
		return "", time.Time{}, errAdminEventCapacity
	}
	if _, collision := a.eventTickets[hash]; collision {
		return "", time.Time{}, errors.New("generated a duplicate realtime ticket")
	}
	a.eventTickets[hash] = expiresAt
	return ticket, expiresAt, nil
}

func (a *adminHandler) consumeEventTicket(ticket string, now time.Time) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil || len(decoded) != 32 {
		clear(decoded)
		return false
	}
	clear(decoded)
	hash := sha256.Sum256([]byte(ticket))
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	expiresAt, exists := a.eventTickets[hash]
	delete(a.eventTickets, hash)
	return exists && expiresAt.After(now) && !a.eventsClosing
}

func (a *adminHandler) handleEvents(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	if len(values) != 1 || len(values["ticket"]) != 1 || values.Get("ticket") == "" {
		writeAdminError(w, http.StatusUnauthorized, "invalid_event_ticket", "a valid realtime connection ticket is required")
		return
	}
	if !a.consumeEventTicket(values.Get("ticket"), time.Now().UTC()) {
		writeAdminError(w, http.StatusUnauthorized, "invalid_event_ticket", "the realtime connection ticket is invalid or expired")
		return
	}
	if !a.beginEventConnection() {
		writeAdminError(w, http.StatusServiceUnavailable, "event_capacity", "the realtime connection limit has been reached")
		return
	}
	defer a.finishEventConnection(nil)

	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Time{}); err != nil {
		a.internalError(w, r, "prepare realtime connection", err)
		return
	}
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		a.internalError(w, r, "prepare realtime connection", err)
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionNoContextTakeover,
	})
	if err != nil {
		return
	}
	a.trackEventConnection(connection)
	defer func() {
		a.finishEventConnection(connection)
		connection.CloseNow()
	}()

	connectionContext := connection.CloseRead(a.eventsContext)
	if err := a.writeEventSnapshot(connectionContext, connection); err != nil {
		return
	}
	ticker := time.NewTicker(adminEventInterval)
	defer ticker.Stop()
	for {
		select {
		case <-connectionContext.Done():
			return
		case <-ticker.C:
			if err := a.writeEventSnapshot(connectionContext, connection); err != nil {
				return
			}
		}
	}
}

func (a *adminHandler) beginEventConnection() bool {
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	if a.eventsClosing || a.eventConnectionCount >= maximumAdminEventConnections {
		return false
	}
	a.eventConnectionCount++
	a.eventConnectionWG.Add(1)
	return true
}

func (a *adminHandler) trackEventConnection(connection *websocket.Conn) {
	a.eventsMu.Lock()
	a.eventConnections[connection] = struct{}{}
	a.eventsMu.Unlock()
}

func (a *adminHandler) finishEventConnection(connection *websocket.Conn) {
	a.eventsMu.Lock()
	if connection != nil {
		delete(a.eventConnections, connection)
	} else {
		a.eventConnectionCount--
	}
	a.eventsMu.Unlock()
	if connection == nil {
		a.eventConnectionWG.Done()
	}
}

func (a *adminHandler) writeEventSnapshot(parent context.Context, connection *websocket.Conn) error {
	writeContext, cancel := context.WithTimeout(parent, adminEventWriteTimeout)
	defer cancel()
	overview, err := a.makeOverview(writeContext)
	if err != nil {
		return err
	}
	hub := a.hub.AdminSnapshot()
	snapshot := adminEventSnapshot{
		Type:            "snapshot",
		Sequence:        strconv.FormatUint(a.eventSequence.Add(1), 10),
		ProfileRevision: strconv.FormatUint(a.store.VisibleRevision(), 10),
		Overview:        overview,
		Sessions:        hub.Sessions,
		Games:           hub.Games,
	}
	return wsjson.Write(writeContext, connection, snapshot)
}

func (a *adminHandler) shutdownEvents(ctx context.Context) error {
	a.eventsMu.Lock()
	if !a.eventsClosing {
		a.eventsClosing = true
		a.eventsCancel()
	}
	connections := make([]*websocket.Conn, 0, len(a.eventConnections))
	for connection := range a.eventConnections {
		connections = append(connections, connection)
	}
	a.eventsMu.Unlock()
	for _, connection := range connections {
		connection.CloseNow()
	}
	done := make(chan struct{})
	go func() {
		a.eventConnectionWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *adminHandler) internalError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	a.log.Error("admin API request failed", "operation", operation, "path", r.URL.Path, "remote", r.RemoteAddr, "error", err)
	writeAdminError(w, http.StatusInternalServerError, "internal_error", "the server could not complete the request")
}

func writeAdminData(w http.ResponseWriter, status int, data any) {
	writeAdminJSON(w, status, adminDataEnvelope{Data: data})
}

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	writeAdminJSON(w, status, adminErrorEnvelope{Error: adminError{Code: code, Message: message}})
}

func writeAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
