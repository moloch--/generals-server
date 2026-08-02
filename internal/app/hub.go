package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

type authSession struct {
	profile Profile
	expires time.Time
}

type chatRoom struct {
	id      string
	name    string
	members map[uint64]*controlClient
}

type gameMember struct {
	client     *controlClient
	ready      bool
	startReady bool
	slot       int
}

type stagedGame struct {
	id              uint64
	name            string
	password        string
	maxPlayers      int
	compatibility   GameCompatibility
	hostID          uint64
	state           string
	listed          bool
	options         GameOptions
	members         map[uint64]*gameMember
	startedRoster   map[uint64]Profile
	resultsReported bool
	startGeneration uint64
	startTimer      *time.Timer
}

type matchEntry struct {
	client        *controlClient
	mode          string
	compatibility GameCompatibility
	enqueuedAt    time.Time
}

type HubStats struct {
	OnlinePlayers int `json:"online_players"`
	OpenGames     int `json:"open_games"`
	ActiveGames   int `json:"active_games"`
	QueuedPlayers int `json:"queued_players"`
}

type Hub struct {
	cfg                  Config
	log                  *slog.Logger
	store                *ProfileStore
	relay                *Relay
	mu                   sync.RWMutex
	clients              map[uint64]*controlClient
	sessions             map[string]authSession
	sessionByUser        map[uint64]string
	rooms                map[string]*chatRoom
	userRoom             map[uint64]string
	games                map[uint64]*stagedGame
	userGame             map[uint64]uint64
	matchQueue           map[uint64]matchEntry
	displayOwners        map[string]uint64
	pendingAdmissions    map[uint64]string
	pendingRegistrations map[string]struct{}
}

func NewHub(cfg Config, logger *slog.Logger, store *ProfileStore, relay *Relay) *Hub {
	rooms := map[string]*chatRoom{
		"global": {id: "global", name: "Global", members: make(map[uint64]*controlClient)},
		"2v2":    {id: "2v2", name: "2 vs 2", members: make(map[uint64]*controlClient)},
		"3v3":    {id: "3v3", name: "3 vs 3", members: make(map[uint64]*controlClient)},
		"4v4":    {id: "4v4", name: "4 vs 4", members: make(map[uint64]*controlClient)},
	}
	return &Hub{
		cfg: cfg, log: logger, store: store, relay: relay,
		clients: make(map[uint64]*controlClient), sessions: make(map[string]authSession), sessionByUser: make(map[uint64]string),
		rooms: rooms, userRoom: make(map[uint64]string), games: make(map[uint64]*stagedGame),
		userGame: make(map[uint64]uint64), matchQueue: make(map[uint64]matchEntry),
		displayOwners: make(map[string]uint64), pendingAdmissions: make(map[uint64]string),
		pendingRegistrations: make(map[string]struct{}),
	}
}

func (h *Hub) ReserveProfile(profile Profile) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reserveProfileLocked(profile)
}

func (h *Hub) reserveProfileLocked(profile Profile) error {
	key := normalizeDisplayName(profile.DisplayName)
	_, active := h.clients[profile.UserID]
	_, pending := h.pendingAdmissions[profile.UserID]
	if !active && !pending && h.reservedPlayerCountLocked() >= h.cfg.MaxOnlinePlayers {
		return commandErr("server_full", "the Online server has reached its player limit")
	}
	if _, registering := h.pendingRegistrations[key]; registering {
		return commandErr("display_name_in_use", "that Online display name is being registered")
	}
	if owner, exists := h.displayOwners[key]; exists && owner != profile.UserID {
		return commandErr("display_name_in_use", "that Online display name is already in use")
	}
	if stored, exists := h.store.Find(profile.DisplayName); exists && stored.UserID != profile.UserID {
		return commandErr("display_name_in_use", "that Online display name belongs to another profile")
	}
	h.displayOwners[key] = profile.UserID
	h.pendingAdmissions[profile.UserID] = key
	return nil
}

func (h *Hub) reservedPlayerCountLocked() int {
	count := len(h.clients) + len(h.pendingRegistrations)
	for userID := range h.pendingAdmissions {
		if h.clients[userID] == nil {
			count++
		}
	}
	return count
}

func (h *Hub) ReserveRegistration(displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if err := validateDisplayName(displayName); err != nil {
		return commandErr("invalid_display_name", err.Error())
	}
	key := normalizeDisplayName(displayName)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reservedPlayerCountLocked() >= h.cfg.MaxOnlinePlayers {
		return commandErr("server_full", "the Online server has reached its player limit")
	}
	if _, exists := h.pendingRegistrations[key]; exists {
		return commandErr("display_name_in_use", "that Online display name is being registered")
	}
	if _, exists := h.displayOwners[key]; exists {
		return commandErr("display_name_in_use", "that Online display name is already in use")
	}
	if _, exists := h.store.Find(displayName); exists {
		return commandErr("display_name_in_use", "that Online display name belongs to another profile")
	}
	h.pendingRegistrations[key] = struct{}{}
	return nil
}

func (h *Hub) CommitRegistration(profile Profile) {
	key := normalizeDisplayName(profile.DisplayName)
	h.mu.Lock()
	delete(h.pendingRegistrations, key)
	h.displayOwners[key] = profile.UserID
	h.pendingAdmissions[profile.UserID] = key
	h.mu.Unlock()
}

func (h *Hub) ReleaseRegistration(displayName string) {
	h.mu.Lock()
	delete(h.pendingRegistrations, normalizeDisplayName(displayName))
	h.mu.Unlock()
}

func (h *Hub) ReleaseProfileReservation(profile Profile) {
	key := normalizeDisplayName(profile.DisplayName)
	h.mu.Lock()
	if h.pendingAdmissions[profile.UserID] == key {
		delete(h.pendingAdmissions, profile.UserID)
	}
	if h.clients[profile.UserID] == nil && h.displayOwners[key] == profile.UserID {
		delete(h.displayOwners, key)
	}
	h.mu.Unlock()
}

func (h *Hub) NewGuest(displayName string) (Profile, error) {
	displayName = strings.TrimSpace(displayName)
	if err := validateDisplayName(displayName); err != nil {
		return Profile{}, commandErr("invalid_display_name", err.Error())
	}
	for i := 0; i < 16; i++ {
		id, err := randomUint64()
		if err != nil {
			return Profile{}, err
		}
		id |= uint64(1) << 63
		h.mu.RLock()
		_, collision := h.clients[id]
		h.mu.RUnlock()
		if !collision {
			return Profile{UserID: id, DisplayName: displayName, Guest: true, CreatedAt: time.Now().UTC()}, nil
		}
	}
	return Profile{}, errors.New("could not allocate guest id")
}

func (h *Hub) IssueSession(profile Profile) (string, time.Time, error) {
	if profile.Guest {
		return "", time.Time{}, errors.New("guest profiles do not receive resumable sessions")
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	expires := time.Now().Add(h.cfg.SessionTTL)
	h.mu.Lock()
	h.pruneSessionsLocked(time.Now())
	if previous := h.sessionByUser[profile.UserID]; previous != "" {
		delete(h.sessions, previous)
	}
	h.sessions[token] = authSession{profile: profile, expires: expires}
	h.sessionByUser[profile.UserID] = token
	h.mu.Unlock()
	return token, expires, nil
}

func (h *Hub) ResumeAndReserve(token string) (Profile, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	h.pruneSessionsLocked(now)
	session, ok := h.sessions[token]
	if !ok || !session.expires.After(now) {
		return Profile{}, errors.New("session token is invalid or expired")
	}
	if !session.profile.Guest {
		if profile, exists := h.store.Get(session.profile.UserID); exists {
			session.profile = profile
		}
	}
	if err := h.reserveProfileLocked(session.profile); err != nil {
		return Profile{}, err
	}
	delete(h.sessions, token)
	if h.sessionByUser[session.profile.UserID] == token {
		delete(h.sessionByUser, session.profile.UserID)
	}
	return session.profile, nil
}

func (h *Hub) Connect(client *controlClient) {
	h.mu.Lock()
	delete(h.pendingAdmissions, client.profile.UserID)
	if previous := h.clients[client.profile.UserID]; previous != nil && previous != client {
		previous.event("session.replaced", map[string]any{"reason": "a newer connection authenticated"})
		previous.close()
	}
	h.clients[client.profile.UserID] = client
	roomID := h.userRoom[client.profile.UserID]
	if room := h.rooms[roomID]; room != nil {
		room.members[client.profile.UserID] = client
		h.broadcastRoomSnapshotLocked(room)
	} else {
		h.joinRoomLocked(client, "global")
		roomID = "global"
	}
	var currentGame *GameSnapshot
	if game := h.games[h.userGame[client.profile.UserID]]; game != nil {
		if member := game.members[client.profile.UserID]; member != nil {
			member.client = client
			snapshot := h.gameSnapshotLocked(game)
			currentGame = &snapshot
		}
	}
	h.notifyBuddyStatusLocked(client.profile.UserID, true, client.status)
	room := h.roomSnapshotLocked(roomID)
	games := h.gameListLocked()
	h.mu.Unlock()
	client.event("session.ready", map[string]any{"room": room, "games": games, "current_game": currentGame})
}

func (h *Hub) Disconnect(client *controlClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[client.profile.UserID] != client {
		return
	}
	delete(h.clients, client.profile.UserID)
	delete(h.displayOwners, normalizeDisplayName(client.profile.DisplayName))
	delete(h.matchQueue, client.profile.UserID)
	h.leaveRoomLocked(client)
	h.leaveGameLocked(client, "connection_closed")
	h.notifyBuddyStatusLocked(client.profile.UserID, false, "offline")
}

func (h *Hub) Command(client *controlClient, command string, raw json.RawMessage) (any, bool, error) {
	switch command {
	case "ping":
		return map[string]any{"type": "pong", "server_time": time.Now().UTC().Format(time.RFC3339Nano)}, false, nil
	case "session.close":
		return map[string]any{"closed": true}, true, nil
	case "profile.get":
		return map[string]any{"profile": client.profile}, false, nil
	case "profile.update":
		return h.updateProfile(client, raw)
	case "room.list":
		return h.listRooms(), false, nil
	case "room.join":
		return h.joinRoom(client, raw)
	case "room.leave":
		return h.leaveRoom(client), false, nil
	case "room.chat":
		return h.roomChat(client, raw)
	case "game.list":
		return h.listGames(), false, nil
	case "game.create":
		return h.createGame(client, raw)
	case "game.join":
		return h.joinGame(client, raw)
	case "game.leave":
		return h.leaveGame(client), false, nil
	case "game.options":
		return h.updateGameOptions(client, raw)
	case "game.ready":
		return h.markGameReady(client, raw)
	case "game.chat":
		return h.gameChat(client, raw)
	case "game.kick":
		return h.kickGameMember(client, raw)
	case "game.start":
		data, err := h.startGame(client)
		return data, false, err
	case "game.start_ready":
		return h.markGameStartReady(client, raw)
	case "game.end":
		data, err := h.endGame(client)
		return data, false, err
	case "player.chat":
		return h.playerChat(client, raw)
	case "buddy.list":
		return h.buddyList(client)
	case "buddy.request":
		return h.buddyRequest(client, raw)
	case "buddy.accept":
		return h.buddyAccept(client, raw)
	case "buddy.remove":
		return h.buddyRemove(client, raw)
	case "buddy.status":
		return h.buddyStatus(client, raw)
	case "stats.get":
		return h.statsGet(client, raw)
	case "stats.update":
		return h.statsUpdate(client, raw)
	case "stats.results":
		return h.statsResults(client, raw)
	case "quickmatch.enqueue":
		return h.quickmatchEnqueue(client, raw)
	case "quickmatch.cancel":
		return h.quickmatchCancel(client), false, nil
	default:
		return nil, false, commandErr("unknown_command", "unknown command type")
	}
}

func (h *Hub) Stats() HubStats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	stats := HubStats{OnlinePlayers: len(h.clients), QueuedPlayers: len(h.matchQueue)}
	for _, game := range h.games {
		if game.state == "starting" || game.state == "started" {
			stats.ActiveGames++
		} else if game.state == "open" {
			stats.OpenGames++
		}
	}
	return stats
}

func (h *Hub) CloseAll() {
	h.mu.RLock()
	clients := make([]*controlClient, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.RUnlock()
	for _, client := range clients {
		client.event("server.shutdown", map[string]any{"reason": "server is shutting down"})
		client.close()
	}
}

func (h *Hub) updateProfile(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		DisplayName string `json:"display_name"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	if err := validateDisplayName(request.DisplayName); err != nil {
		return nil, false, commandErr("invalid_display_name", err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	newKey := normalizeDisplayName(request.DisplayName)
	if _, registering := h.pendingRegistrations[newKey]; registering {
		return nil, false, commandErr("display_name_in_use", "that Online display name is being registered")
	}
	if owner, exists := h.displayOwners[newKey]; exists && owner != client.profile.UserID {
		return nil, false, commandErr("display_name_in_use", "that Online display name is already in use")
	}
	if stored, exists := h.store.Find(request.DisplayName); exists && stored.UserID != client.profile.UserID {
		return nil, false, commandErr("display_name_in_use", "that Online display name belongs to another profile")
	}
	var profile Profile
	var err error
	if client.profile.Guest {
		profile = client.profile
		profile.DisplayName = request.DisplayName
	} else {
		profile, err = h.store.UpdateDisplayName(client.profile.UserID, request.DisplayName)
		if err != nil {
			return nil, false, commandErr("profile_update_failed", err.Error())
		}
	}
	oldKey := normalizeDisplayName(client.profile.DisplayName)
	if h.displayOwners[oldKey] == client.profile.UserID {
		delete(h.displayOwners, oldKey)
	}
	h.displayOwners[newKey] = client.profile.UserID
	client.profile = profile
	h.refreshSnapshotsForUserLocked(client.profile.UserID)
	return map[string]any{"profile": profile}, false, nil
}

func (h *Hub) listRooms() map[string]any {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]string, 0, len(h.rooms))
	for id := range h.rooms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rooms := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		room := h.rooms[id]
		rooms = append(rooms, map[string]any{"room_id": id, "name": room.name, "players": len(room.members)})
	}
	return map[string]any{"rooms": rooms}
}

func (h *Hub) joinRoom(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		RoomID string `json:"room_id"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	h.mu.Lock()
	if h.rooms[request.RoomID] == nil {
		h.mu.Unlock()
		return nil, false, commandErr("room_not_found", "room not found")
	}
	h.leaveRoomLocked(client)
	h.joinRoomLocked(client, request.RoomID)
	snapshot := h.roomSnapshotLocked(request.RoomID)
	h.mu.Unlock()
	return map[string]any{"room": snapshot}, false, nil
}

func (h *Hub) leaveRoom(client *controlClient) map[string]any {
	h.mu.Lock()
	h.leaveRoomLocked(client)
	h.mu.Unlock()
	return map[string]any{"left": true}
}

func (h *Hub) roomChat(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		Message string `json:"message"`
		Action  bool   `json:"action,omitempty"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if err := validateChat(request.Message); err != nil {
		return nil, false, commandErr("invalid_message", err.Error())
	}
	h.mu.RLock()
	roomID := h.userRoom[client.profile.UserID]
	room := h.rooms[roomID]
	if room == nil {
		h.mu.RUnlock()
		return nil, false, commandErr("not_in_room", "join a room before chatting")
	}
	event := map[string]any{"room_id": roomID, "user_id": client.profile.UserID, "display_name": client.profile.DisplayName, "message": request.Message, "action": request.Action, "sent_at": time.Now().UTC().Format(time.RFC3339Nano)}
	for _, member := range room.members {
		member.event("room.chat", event)
	}
	h.mu.RUnlock()
	return map[string]any{"sent": true}, false, nil
}

func (h *Hub) listGames() map[string]any {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return map[string]any{"games": h.gameListLocked()}
}

func (h *Hub) createGame(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		GameCompatibility
		Name       string      `json:"name"`
		Password   string      `json:"password,omitempty"`
		MaxPlayers int         `json:"max_players"`
		Options    GameOptions `json:"options"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if err := validateGameCompatibility(request.GameCompatibility); err != nil {
		return nil, false, err
	}
	request.Name = strings.TrimSpace(request.Name)
	if len(request.Name) < 1 || len(request.Name) > 48 {
		return nil, false, commandErr("invalid_game", "game name must be 1-48 bytes")
	}
	if len(request.Password) > 64 {
		return nil, false, commandErr("invalid_game", "game password must be at most 64 bytes")
	}
	if request.MaxPlayers < 2 || request.MaxPlayers > 8 {
		return nil, false, commandErr("invalid_game", "max_players must be 2-8")
	}
	if err := validateOptions(request.Options); err != nil {
		return nil, false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, queued := h.matchQueue[client.profile.UserID]; queued {
		return nil, false, commandErr("quickmatch_queued", "cancel quickmatch before creating a game")
	}
	if _, exists := h.userGame[client.profile.UserID]; exists {
		return nil, false, commandErr("already_in_game", "leave the current game first")
	}
	if len(h.games) >= h.cfg.MaxStagedGames {
		return nil, false, commandErr("server_full", "the Online server has reached its staged game limit")
	}
	id, err := h.newGameIDLocked()
	if err != nil {
		return nil, false, commandErr("internal_error", err.Error())
	}
	game := &stagedGame{id: id, name: request.Name, password: request.Password, maxPlayers: request.MaxPlayers, compatibility: request.GameCompatibility, hostID: client.profile.UserID, state: "open", listed: true, options: request.Options, members: make(map[uint64]*gameMember)}
	game.members[client.profile.UserID] = &gameMember{client: client, slot: 0}
	h.games[id] = game
	h.userGame[client.profile.UserID] = id
	snapshot := h.gameSnapshotLocked(game)
	h.broadcastGameListLocked()
	return map[string]any{"game": snapshot}, false, nil
}

func (h *Hub) joinGame(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		GameCompatibility
		GameID   string `json:"game_id"`
		Password string `json:"password,omitempty"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	id, err := parseGameID(request.GameID)
	if err != nil {
		return nil, false, commandErr("invalid_game_id", err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, queued := h.matchQueue[client.profile.UserID]; queued {
		return nil, false, commandErr("quickmatch_queued", "cancel quickmatch before joining a game")
	}
	if _, exists := h.userGame[client.profile.UserID]; exists {
		return nil, false, commandErr("already_in_game", "leave the current game first")
	}
	game := h.games[id]
	if game == nil || !game.listed {
		return nil, false, commandErr("game_not_found", "game not found")
	}
	if game.state != "open" {
		return nil, false, commandErr("game_started", "game is no longer joinable")
	}
	if game.compatibility != request.GameCompatibility {
		return nil, false, commandErr("incompatible_game", "game product, compatibility version, or INI data differs from this client")
	}
	if game.password != request.Password {
		return nil, false, commandErr("wrong_password", "incorrect game password")
	}
	if len(game.members) >= game.maxPlayers {
		return nil, false, commandErr("game_full", "game is full")
	}
	slot := firstOpenSlot(game)
	game.members[client.profile.UserID] = &gameMember{client: client, slot: slot}
	h.userGame[client.profile.UserID] = id
	snapshot := h.gameSnapshotLocked(game)
	for userID, member := range game.members {
		if userID != client.profile.UserID {
			member.client.event("game.updated", map[string]any{"game": snapshot})
		}
	}
	h.broadcastGameListLocked()
	return map[string]any{"game": snapshot}, false, nil
}

func (h *Hub) leaveGame(client *controlClient) map[string]any {
	h.mu.Lock()
	h.leaveGameLocked(client, "player_left")
	h.mu.Unlock()
	return map[string]any{"left": true}
}

func (h *Hub) updateGameOptions(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		Name       *string      `json:"name,omitempty"`
		Password   *string      `json:"password,omitempty"`
		MaxPlayers *int         `json:"max_players,omitempty"`
		Options    *GameOptions `json:"options,omitempty"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.gameForClientLocked(client)
	if game == nil {
		return nil, false, commandErr("not_in_game", "not in a game")
	}
	if game.hostID != client.profile.UserID {
		return nil, false, commandErr("host_required", "only the host can change game options")
	}
	if game.state != "open" {
		return nil, false, commandErr("game_started", "game options are locked after start")
	}
	name := game.name
	password := game.password
	maxPlayers := game.maxPlayers
	options := game.options
	if request.Name != nil {
		name = strings.TrimSpace(*request.Name)
		if len(name) < 1 || len(name) > 48 {
			return nil, false, commandErr("invalid_game", "game name must be 1-48 bytes")
		}
	}
	if request.Password != nil {
		if len(*request.Password) > 64 {
			return nil, false, commandErr("invalid_game", "game password must be at most 64 bytes")
		}
		password = *request.Password
	}
	if request.MaxPlayers != nil {
		if *request.MaxPlayers < len(game.members) || *request.MaxPlayers < 2 || *request.MaxPlayers > 8 {
			return nil, false, commandErr("invalid_game", "max_players must be 2-8 and at least the current player count")
		}
		maxPlayers = *request.MaxPlayers
	}
	if request.Options != nil {
		if err := validateOptions(*request.Options); err != nil {
			return nil, false, err
		}
		options = *request.Options
	}
	resetReady := request.Options != nil && options.ReadyKey != game.options.ReadyKey
	changed := name != game.name || password != game.password || maxPlayers != game.maxPlayers || options != game.options
	if !changed {
		return map[string]any{"game": h.gameSnapshotLocked(game)}, false, nil
	}
	game.name = name
	game.password = password
	game.maxPlayers = maxPlayers
	game.options = options
	if resetReady {
		for userID, member := range game.members {
			if userID != game.hostID {
				member.ready = false
			}
		}
	}
	h.broadcastGameSnapshotLocked(game)
	h.broadcastGameListLocked()
	return map[string]any{"game": h.gameSnapshotLocked(game)}, false, nil
}

func (h *Hub) markGameReady(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		Ready bool `json:"ready"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.gameForClientLocked(client)
	if game == nil {
		return nil, false, commandErr("not_in_game", "not in a game")
	}
	if game.state != "open" {
		return nil, false, commandErr("game_started", "readiness is locked after start")
	}
	game.members[client.profile.UserID].ready = request.Ready
	h.broadcastGameSnapshotLocked(game)
	return map[string]any{"ready": request.Ready}, false, nil
}

func (h *Hub) gameChat(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		Message string `json:"message"`
		Action  bool   `json:"action,omitempty"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if err := validateChat(request.Message); err != nil {
		return nil, false, commandErr("invalid_message", err.Error())
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	game := h.gameForClientLocked(client)
	if game == nil {
		return nil, false, commandErr("not_in_game", "not in a game")
	}
	event := map[string]any{"game_id": formatGameID(game.id), "user_id": client.profile.UserID, "display_name": client.profile.DisplayName, "message": request.Message, "action": request.Action, "sent_at": time.Now().UTC().Format(time.RFC3339Nano)}
	for _, member := range game.members {
		member.client.event("game.chat", event)
	}
	return map[string]any{"sent": true}, false, nil
}

func (h *Hub) kickGameMember(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		UserID uint64 `json:"user_id"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.gameForClientLocked(client)
	if game == nil {
		return nil, false, commandErr("not_in_game", "not in a game")
	}
	if game.hostID != client.profile.UserID {
		return nil, false, commandErr("host_required", "only the host can kick a player")
	}
	if game.state != "open" {
		return nil, false, commandErr("game_started", "players cannot be kicked after the game starts")
	}
	if request.UserID == 0 || request.UserID == game.hostID {
		return nil, false, commandErr("invalid_kick_target", "select a non-host game member")
	}
	target := game.members[request.UserID]
	if target == nil {
		return nil, false, commandErr("player_not_found", "target player is not in this game")
	}
	target.client.event("game.kicked", map[string]any{
		"game_id": formatGameID(game.id), "reason": "host_kicked",
	})
	h.leaveGameLocked(target.client, "host_kicked")
	return map[string]any{"kicked": true, "user_id": request.UserID}, false, nil
}

func (h *Hub) startGame(client *controlClient) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.gameForClientLocked(client)
	if game == nil {
		return nil, commandErr("not_in_game", "not in a game")
	}
	if game.hostID != client.profile.UserID {
		return nil, commandErr("host_required", "only the host can start the game")
	}
	if game.state != "open" {
		return nil, commandErr("game_started", "game already started")
	}
	if len(game.members) < 2 {
		return nil, commandErr("not_enough_players", "at least two players are required")
	}
	for userID, member := range game.members {
		if userID != game.hostID && !member.ready {
			return nil, commandErr("players_not_ready", "all non-host players must be ready")
		}
	}
	members := h.membersLocked(game)
	credentials, err := h.relay.Allocate(game.id, members)
	if err != nil {
		return nil, commandErr("relay_unavailable", err.Error())
	}
	game.state = "starting"
	game.resultsReported = false
	for userID, member := range game.members {
		member.startReady = false
		member.client.event("game.started", credentials[userID])
		member.client.status = "in_game"
		h.notifyBuddyStatusLocked(userID, true, "in_game")
	}
	h.armStartTimerLocked(game)
	h.broadcastGameSnapshotLocked(game)
	h.broadcastGameListLocked()
	return map[string]any{"starting": true, "game_id": formatGameID(game.id)}, nil
}

func (h *Hub) markGameStartReady(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		GameID string `json:"game_id"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	id, err := parseGameID(request.GameID)
	if err != nil {
		return nil, false, commandErr("invalid_game_id", err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.gameForClientLocked(client)
	if game == nil || game.id != id {
		return nil, false, commandErr("game_not_found", "the starting game was not found")
	}
	member := game.members[client.profile.UserID]
	if member == nil {
		return nil, false, commandErr("not_in_game", "not in the starting game")
	}
	if game.state == "started" {
		return map[string]any{"ready": true, "go": true}, false, nil
	}
	if game.state != "starting" {
		return nil, false, commandErr("game_not_starting", "game credentials are not awaiting confirmation")
	}
	member.startReady = true
	allReady := true
	for _, candidate := range game.members {
		if !candidate.startReady {
			allReady = false
			break
		}
	}
	if allReady {
		h.stopStartTimerLocked(game)
		game.state = "started"
		game.startedRoster = make(map[uint64]Profile, len(game.members))
		for userID, candidate := range game.members {
			game.startedRoster[userID] = candidate.client.profile
		}
		event := map[string]any{"game_id": formatGameID(game.id)}
		for _, candidate := range game.members {
			candidate.client.event("game.go", event)
		}
	}
	return map[string]any{"ready": true, "go": allReady}, false, nil
}

func (h *Hub) endGame(client *controlClient) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.gameForClientLocked(client)
	if game == nil {
		return nil, commandErr("not_in_game", "not in a game")
	}
	_, originalHostPresent := game.members[game.hostID]
	if game.hostID != client.profile.UserID && originalHostPresent {
		return nil, commandErr("host_required", "only the host can end the game")
	}
	h.endGameLocked(game, "host_ended")
	return map[string]any{"ended": true}, nil
}

func (h *Hub) playerChat(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		UserID  uint64 `json:"user_id"`
		Message string `json:"message"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if err := validateChat(request.Message); err != nil {
		return nil, false, commandErr("invalid_message", err.Error())
	}
	h.mu.RLock()
	target := h.clients[request.UserID]
	h.mu.RUnlock()
	if target == nil {
		return nil, false, commandErr("player_offline", "target player is offline")
	}
	target.event("player.chat", map[string]any{"user_id": client.profile.UserID, "display_name": client.profile.DisplayName, "message": request.Message, "sent_at": time.Now().UTC().Format(time.RFC3339Nano)})
	return map[string]any{"sent": true}, false, nil
}

func (h *Hub) buddyList(client *controlClient) (any, bool, error) {
	if client.profile.Guest {
		return nil, false, commandErr("persistent_profile_required", "guest profiles do not have buddy lists")
	}
	buddyIDs, pendingIDs, ok := h.store.BuddyIDs(client.profile.UserID)
	if !ok {
		return nil, false, commandErr("profile_not_found", "profile not found")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	buddies := make([]Buddy, 0, len(buddyIDs))
	pending := make([]Buddy, 0, len(pendingIDs))
	for _, id := range buddyIDs {
		if p, found := h.store.Get(id); found {
			buddies = append(buddies, Buddy{UserID: id, DisplayName: p.DisplayName, Online: h.clients[id] != nil})
		}
	}
	for _, id := range pendingIDs {
		if p, found := h.store.Get(id); found {
			pending = append(pending, Buddy{UserID: id, DisplayName: p.DisplayName, Online: h.clients[id] != nil})
		}
	}
	sort.Slice(buddies, func(i, j int) bool { return buddies[i].UserID < buddies[j].UserID })
	sort.Slice(pending, func(i, j int) bool { return pending[i].UserID < pending[j].UserID })
	return map[string]any{"buddies": buddies, "pending": pending}, false, nil
}

func (h *Hub) buddyRequest(client *controlClient, raw json.RawMessage) (any, bool, error) {
	if client.profile.Guest {
		return nil, false, commandErr("persistent_profile_required", "guest profiles do not have buddy lists")
	}
	var request struct {
		UserID      uint64 `json:"user_id,omitempty"`
		DisplayName string `json:"display_name,omitempty"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	target, err := h.resolvePersistentTarget(request.UserID, request.DisplayName)
	if err != nil {
		return nil, false, err
	}
	if err := h.store.RequestBuddy(client.profile.UserID, target.UserID); err != nil {
		return nil, false, commandErr("buddy_request_failed", err.Error())
	}
	h.mu.RLock()
	online := h.clients[target.UserID]
	h.mu.RUnlock()
	if online != nil {
		online.event("buddy.requested", map[string]any{"user_id": client.profile.UserID, "display_name": client.profile.DisplayName})
	}
	return map[string]any{"requested": true, "user_id": target.UserID}, false, nil
}

func (h *Hub) buddyAccept(client *controlClient, raw json.RawMessage) (any, bool, error) {
	if client.profile.Guest {
		return nil, false, commandErr("persistent_profile_required", "guest profiles do not have buddy lists")
	}
	var request struct {
		UserID uint64 `json:"user_id"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if err := h.store.AcceptBuddy(client.profile.UserID, request.UserID); err != nil {
		return nil, false, commandErr("buddy_accept_failed", err.Error())
	}
	h.mu.RLock()
	online := h.clients[request.UserID]
	h.mu.RUnlock()
	if online != nil {
		online.event("buddy.accepted", map[string]any{"user_id": client.profile.UserID, "display_name": client.profile.DisplayName})
	}
	return map[string]any{"accepted": true}, false, nil
}

func (h *Hub) buddyRemove(client *controlClient, raw json.RawMessage) (any, bool, error) {
	if client.profile.Guest {
		return nil, false, commandErr("persistent_profile_required", "guest profiles do not have buddy lists")
	}
	var request struct {
		UserID uint64 `json:"user_id"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if err := h.store.RemoveBuddy(client.profile.UserID, request.UserID); err != nil {
		return nil, false, commandErr("buddy_remove_failed", err.Error())
	}
	return map[string]any{"removed": true}, false, nil
}

func (h *Hub) buddyStatus(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		Status string `json:"status"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if request.Status != "online" && request.Status != "away" && request.Status != "in_game" {
		return nil, false, commandErr("invalid_status", "status must be online, away, or in_game")
	}
	h.mu.Lock()
	client.status = request.Status
	h.notifyBuddyStatusLocked(client.profile.UserID, true, request.Status)
	h.mu.Unlock()
	return map[string]any{"status": request.Status}, false, nil
}

func (h *Hub) statsGet(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		UserID uint64 `json:"user_id,omitempty"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if request.UserID == 0 {
		request.UserID = client.profile.UserID
	}
	guest := request.UserID == client.profile.UserID && client.profile.Guest
	if !guest {
		h.mu.RLock()
		target := h.clients[request.UserID]
		guest = target != nil && target.profile.Guest
		h.mu.RUnlock()
	}
	if guest {
		return map[string]any{"user_id": request.UserID, "stats": PlayerStats{}}, false, nil
	}
	stats, ok := h.store.Stats(request.UserID)
	if !ok {
		return nil, false, commandErr("profile_not_found", "profile not found")
	}
	return map[string]any{"user_id": request.UserID, "stats": stats}, false, nil
}

func (h *Hub) statsUpdate(client *controlClient, raw json.RawMessage) (any, bool, error) {
	if client.profile.Guest {
		return nil, false, commandErr("persistent_profile_required", "guest profiles do not have stats")
	}
	var request struct {
		Delta PlayerStats `json:"delta"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if request.Delta.Wins > 1 || request.Delta.Losses > 1 || request.Delta.Disconnects > 1 || request.Delta.Games > 1 || request.Delta.Rating < -100 || request.Delta.Rating > 100 {
		return nil, false, commandErr("invalid_stats", "stats delta exceeds per-request limits")
	}
	stats, err := h.store.ApplyStats(client.profile.UserID, request.Delta)
	if err != nil {
		return nil, false, commandErr("stats_update_failed", err.Error())
	}
	return map[string]any{"stats": stats}, false, nil
}

func (h *Hub) statsResults(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		GameID  string `json:"game_id"`
		Results []struct {
			UserID  uint64 `json:"user_id"`
			Outcome string `json:"outcome"`
		} `json:"results"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	id, err := parseGameID(request.GameID)
	if err != nil {
		return nil, false, commandErr("invalid_game_id", err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.games[id]
	originalHostPresent := game != nil && game.members[game.hostID] != nil
	reporterIsSurvivor := game != nil && game.members[client.profile.UserID] != nil
	if game == nil || !reporterIsSurvivor || (game.hostID != client.profile.UserID && originalHostPresent) || game.state != "started" || game.resultsReported {
		return nil, false, commandErr("results_rejected", "results require the active host or a survivor after host departure and may be reported once")
	}
	if !game.options.UseStats {
		return nil, false, commandErr("stats_disabled", "this game was not created with statistics enabled")
	}
	if len(request.Results) != len(game.startedRoster) {
		return nil, false, commandErr("invalid_results", "results must include every player from the started roster exactly once")
	}
	seen := make(map[uint64]bool)
	updates := make(map[uint64]PlayerStats)
	for _, result := range request.Results {
		if _, launched := game.startedRoster[result.UserID]; !launched || seen[result.UserID] || (result.Outcome != "win" && result.Outcome != "loss" && result.Outcome != "disconnect") {
			return nil, false, commandErr("invalid_results", "results contain an invalid player or outcome")
		}
		seen[result.UserID] = true
		if profile, ok := h.store.Get(result.UserID); ok && !profile.Guest {
			delta := PlayerStats{Games: 1}
			switch result.Outcome {
			case "win":
				delta.Wins = 1
				delta.Rating = 10
			case "loss":
				delta.Losses = 1
				delta.Rating = -10
			case "disconnect":
				delta.Disconnects = 1
				delta.Losses = 1
				delta.Rating = -15
			}
			updates[result.UserID] = delta
		}
	}
	if _, err := h.store.ApplyStatsBatch(updates); err != nil {
		return nil, false, commandErr("stats_update_failed", err.Error())
	}
	game.resultsReported = true
	return map[string]any{"recorded": true}, false, nil
}

func (h *Hub) quickmatchEnqueue(client *controlClient, raw json.RawMessage) (any, bool, error) {
	var request struct {
		GameCompatibility
		Mode string `json:"mode,omitempty"`
	}
	if err := decodeData(raw, &request); err != nil {
		return nil, false, err
	}
	if err := validateGameCompatibility(request.GameCompatibility); err != nil {
		return nil, false, err
	}
	request.Mode = strings.TrimSpace(request.Mode)
	if request.Mode == "" {
		request.Mode = "1v1"
	}
	if len(request.Mode) > 24 {
		return nil, false, commandErr("invalid_mode", "mode must be at most 24 bytes")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.userGame[client.profile.UserID]; exists {
		return nil, false, commandErr("already_in_game", "leave the current game first")
	}
	if entry, exists := h.matchQueue[client.profile.UserID]; exists {
		return map[string]any{
			"queued": true, "mode": entry.mode,
			"product": entry.compatibility.Product, "compatibility_version": entry.compatibility.CompatibilityVersion, "ini_crc": entry.compatibility.INICRC,
		}, false, nil
	}
	var opponent *matchEntry
	var opponentID uint64
	for userID, entry := range h.matchQueue {
		_, inGame := h.userGame[userID]
		if h.clients[userID] != entry.client || inGame {
			delete(h.matchQueue, userID)
			continue
		}
		if entry.mode == request.Mode && entry.compatibility == request.GameCompatibility && (opponent == nil || entry.enqueuedAt.Before(opponent.enqueuedAt)) {
			copy := entry
			opponent = &copy
			opponentID = userID
		}
	}
	if opponent == nil {
		h.matchQueue[client.profile.UserID] = matchEntry{client: client, mode: request.Mode, compatibility: request.GameCompatibility, enqueuedAt: time.Now()}
		return map[string]any{
			"queued": true, "mode": request.Mode,
			"product": request.Product, "compatibility_version": request.CompatibilityVersion, "ini_crc": request.INICRC,
		}, false, nil
	}
	if len(h.games) >= h.cfg.MaxStagedGames {
		return nil, false, commandErr("server_full", "the Online server has reached its staged game limit")
	}
	delete(h.matchQueue, opponentID)
	id, err := h.newGameIDLocked()
	if err != nil {
		return nil, false, commandErr("internal_error", err.Error())
	}
	game := &stagedGame{id: id, name: "Quick Match", maxPlayers: 2, compatibility: request.GameCompatibility, hostID: opponent.client.profile.UserID, state: "starting", listed: false, options: GameOptions{UseStats: false}, members: make(map[uint64]*gameMember)}
	game.members[opponent.client.profile.UserID] = &gameMember{client: opponent.client, ready: true, slot: 0}
	game.members[client.profile.UserID] = &gameMember{client: client, ready: true, slot: 1}
	credentials, err := h.relay.Allocate(game.id, h.membersLocked(game))
	if err != nil {
		h.matchQueue[opponentID] = *opponent
		return nil, false, commandErr("relay_unavailable", err.Error())
	}
	h.games[id] = game
	h.userGame[opponent.client.profile.UserID] = id
	h.userGame[client.profile.UserID] = id
	h.armStartTimerLocked(game)
	snapshot := h.gameSnapshotLocked(game)
	opponent.client.event("quickmatch.matched", map[string]any{"game": snapshot})
	client.event("quickmatch.matched", map[string]any{"game": snapshot})
	for userID, member := range game.members {
		member.client.status = "in_game"
		h.notifyBuddyStatusLocked(userID, true, "in_game")
		member.client.event("game.started", credentials[userID])
	}
	return map[string]any{"queued": false, "matched": true, "game": snapshot}, false, nil
}

func (h *Hub) quickmatchCancel(client *controlClient) map[string]any {
	h.mu.Lock()
	_, existed := h.matchQueue[client.profile.UserID]
	delete(h.matchQueue, client.profile.UserID)
	h.mu.Unlock()
	return map[string]any{"cancelled": existed}
}

func (h *Hub) joinRoomLocked(client *controlClient, roomID string) {
	room := h.rooms[roomID]
	if room == nil {
		return
	}
	room.members[client.profile.UserID] = client
	h.userRoom[client.profile.UserID] = roomID
	h.broadcastRoomSnapshotLocked(room)
}

func (h *Hub) leaveRoomLocked(client *controlClient) {
	roomID := h.userRoom[client.profile.UserID]
	if roomID == "" {
		return
	}
	delete(h.userRoom, client.profile.UserID)
	if room := h.rooms[roomID]; room != nil {
		delete(room.members, client.profile.UserID)
		h.broadcastRoomSnapshotLocked(room)
	}
}

func (h *Hub) roomSnapshotLocked(roomID string) RoomSnapshot {
	room := h.rooms[roomID]
	snapshot := RoomSnapshot{RoomID: roomID}
	if room == nil {
		return snapshot
	}
	snapshot.Name = room.name
	ids := make([]uint64, 0, len(room.members))
	for id := range room.members {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		c := room.members[id]
		snapshot.Members = append(snapshot.Members, Member{UserID: id, DisplayName: c.profile.DisplayName, Slot: -1})
	}
	return snapshot
}

func (h *Hub) broadcastRoomSnapshotLocked(room *chatRoom) {
	snapshot := h.roomSnapshotLocked(room.id)
	for _, member := range room.members {
		member.event("room.updated", map[string]any{"room": snapshot})
	}
}

func (h *Hub) gameForClientLocked(client *controlClient) *stagedGame {
	return h.games[h.userGame[client.profile.UserID]]
}

func (h *Hub) gameSnapshotLocked(game *stagedGame) GameSnapshot {
	hostName := ""
	if host := game.members[game.hostID]; host != nil {
		hostName = host.client.profile.DisplayName
	}
	summary := GameSummary{GameCompatibility: game.compatibility, GameID: formatGameID(game.id), Name: game.name, Map: game.options.Map, HostName: hostName, Players: len(game.members), MaxPlayers: game.maxPlayers, HasPassword: game.password != "", State: game.state}
	return GameSnapshot{GameSummary: summary, Members: h.membersLocked(game), Options: game.options}
}

func (h *Hub) membersLocked(game *stagedGame) []Member {
	members := make([]Member, 0, len(game.members))
	for id, member := range game.members {
		members = append(members, Member{UserID: id, DisplayName: member.client.profile.DisplayName, Host: id == game.hostID, Ready: member.ready, Slot: member.slot})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Slot < members[j].Slot })
	return members
}

func (h *Hub) gameListLocked() []GameSummary {
	games := make([]GameSummary, 0, len(h.games))
	for _, game := range h.games {
		if game.listed && game.state == "open" {
			games = append(games, h.gameSnapshotLocked(game).GameSummary)
		}
	}
	sort.Slice(games, func(i, j int) bool { return games[i].GameID < games[j].GameID })
	return games
}

func (h *Hub) broadcastGameSnapshotLocked(game *stagedGame) {
	snapshot := h.gameSnapshotLocked(game)
	for _, member := range game.members {
		member.client.event("game.updated", map[string]any{"game": snapshot})
	}
}

func (h *Hub) broadcastGameListLocked() {
	list := h.gameListLocked()
	for _, client := range h.clients {
		client.event("game.list", map[string]any{"games": list})
	}
}

func (h *Hub) leaveGameLocked(client *controlClient, reason string) {
	id, ok := h.userGame[client.profile.UserID]
	if !ok {
		return
	}
	delete(h.userGame, client.profile.UserID)
	game := h.games[id]
	if game == nil {
		return
	}
	departed := game.members[client.profile.UserID]
	delete(game.members, client.profile.UserID)
	client.status = "online"
	if h.clients[client.profile.UserID] == client {
		h.notifyBuddyStatusLocked(client.profile.UserID, true, "online")
	}
	if !game.listed {
		h.dissolveGameLocked(game, reason, nil)
		return
	}
	departure := map[string]any{
		"departed_user_id":      client.profile.UserID,
		"departed_display_name": client.profile.DisplayName,
	}
	if game.state == "starting" {
		h.dissolveGameLocked(game, "player_left", departure)
		return
	}
	if game.state == "started" {
		departedSlot := -1
		if departed != nil {
			departedSlot = departed.slot
			h.relay.RemoveParticipant(game.id, client.profile.UserID, departed.slot)
		}
		if len(game.members) == 0 {
			h.dissolveGameLocked(game, "player_left", departure)
			return
		}
		peerLeft := map[string]any{
			"game_id":               formatGameID(game.id),
			"departed_user_id":      client.profile.UserID,
			"departed_display_name": client.profile.DisplayName,
			"departed_slot":         departedSlot,
			"departed_host":         game.hostID == client.profile.UserID,
		}
		for _, member := range game.members {
			member.client.event("game.peer_left", peerLeft)
		}
		return
	}
	if game.hostID == client.profile.UserID {
		// Retail exits a staged lobby when its slot-zero host disappears. Send
		// that membership diff before the terminal event clears adapter state,
		// without replaying the departed host's opaque slot ownership.
		game.options.Opaque = ""
		game.options.SlotList = ""
		game.options.ReadyKey = ""
		h.broadcastGameSnapshotLocked(game)
		h.dissolveGameLocked(game, "host_left", departure)
		return
	}
	if len(game.members) == 0 {
		delete(h.games, id)
		h.broadcastGameListLocked()
		return
	}
	h.broadcastGameSnapshotLocked(game)
	h.broadcastGameListLocked()
}

func (h *Hub) endGameLocked(game *stagedGame, reason string) {
	h.dissolveGameLocked(game, reason, nil)
}

func (h *Hub) dissolveGameLocked(game *stagedGame, reason string, details map[string]any) {
	h.stopStartTimerLocked(game)
	h.relay.EndGame(game.id)
	event := map[string]any{"game_id": formatGameID(game.id), "reason": reason}
	for key, value := range details {
		event[key] = value
	}
	for userID, member := range game.members {
		delete(h.userGame, userID)
		member.ready = false
		member.startReady = false
		member.client.status = "online"
		h.notifyBuddyStatusLocked(userID, true, "online")
		member.client.event("game.ended", event)
	}
	delete(h.games, game.id)
	h.broadcastGameListLocked()
}

func (h *Hub) armStartTimerLocked(game *stagedGame) {
	h.stopStartTimerLocked(game)
	generation := game.startGeneration
	game.startTimer = time.AfterFunc(h.cfg.StartReadyTimeout, func() {
		h.handleStartReadyTimeout(game.id, generation)
	})
}

func (h *Hub) stopStartTimerLocked(game *stagedGame) {
	game.startGeneration++
	if game.startTimer != nil {
		game.startTimer.Stop()
		game.startTimer = nil
	}
}

func (h *Hub) handleStartReadyTimeout(gameID, generation uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.games[gameID]
	if game == nil || game.state != "starting" || game.startGeneration != generation {
		return
	}
	h.dissolveGameLocked(game, "start_timeout", nil)
}

func (h *Hub) handleRelayGameExpired(gameID uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	game := h.games[gameID]
	if game == nil || (game.state != "starting" && game.state != "started") {
		return
	}
	h.dissolveGameLocked(game, "relay_idle_timeout", nil)
}

func (h *Hub) refreshSnapshotsForUserLocked(userID uint64) {
	if roomID := h.userRoom[userID]; roomID != "" {
		h.broadcastRoomSnapshotLocked(h.rooms[roomID])
	}
	if game := h.games[h.userGame[userID]]; game != nil {
		h.broadcastGameSnapshotLocked(game)
	}
}

func (h *Hub) notifyBuddyStatusLocked(userID uint64, online bool, status string) {
	buddies, _, ok := h.store.BuddyIDs(userID)
	if !ok {
		return
	}
	profile, _ := h.store.Get(userID)
	event := map[string]any{"user_id": userID, "display_name": profile.DisplayName, "online": online, "status": status}
	for _, buddyID := range buddies {
		if c := h.clients[buddyID]; c != nil {
			c.event("buddy.status", event)
		}
	}
}

func (h *Hub) resolvePersistentTarget(userID uint64, displayName string) (Profile, error) {
	if userID != 0 {
		if p, ok := h.store.Get(userID); ok {
			return p, nil
		}
	}
	if displayName != "" {
		if p, ok := h.store.Find(displayName); ok {
			return p, nil
		}
	}
	return Profile{}, commandErr("profile_not_found", "target profile not found")
}

func (h *Hub) newGameIDLocked() (uint64, error) {
	for i := 0; i < 16; i++ {
		id, err := randomUint64()
		if err != nil {
			return 0, err
		}
		if id != 0 && h.games[id] == nil {
			return id, nil
		}
	}
	return 0, errors.New("could not allocate game id")
}

func (h *Hub) pruneSessionsLocked(now time.Time) {
	for token, session := range h.sessions {
		if !session.expires.After(now) {
			delete(h.sessions, token)
			if h.sessionByUser[session.profile.UserID] == token {
				delete(h.sessionByUser, session.profile.UserID)
			}
		}
	}
}

func firstOpenSlot(game *stagedGame) int {
	used := [8]bool{}
	for _, m := range game.members {
		if m.slot >= 0 && m.slot < 8 {
			used[m.slot] = true
		}
	}
	for i := 0; i < 8; i++ {
		if !used[i] {
			return i
		}
	}
	return -1
}

func randomUint64() (uint64, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value[:]), nil
}

func validateChat(message string) error {
	if len(message) < 1 || len(message) > 512 {
		return errors.New("message must be 1-512 bytes")
	}
	for _, r := range message {
		if r < 0x20 || r == 0x7f {
			return errors.New("message contains a control character")
		}
	}
	return nil
}

func validateOptions(options GameOptions) error {
	if len(options.Map) > 128 {
		return commandErr("invalid_options", "map must be at most 128 bytes")
	}
	if options.StartingCash != 0 && (options.StartingCash < 5000 || options.StartingCash > 1000000) {
		return commandErr("invalid_options", "starting_cash must be 5000-1000000")
	}
	if len(options.Opaque) > 4096 {
		return commandErr("invalid_options", "opaque options must be at most 4096 bytes")
	}
	if len(options.SlotList) > 4096 {
		return commandErr("invalid_options", "slot_list must be at most 4096 bytes")
	}
	if len(options.ReadyKey) > 4096 {
		return commandErr("invalid_options", "ready_key must be at most 4096 bytes")
	}
	return nil
}

func validateGameCompatibility(compatibility GameCompatibility) error {
	if compatibility.Product != "generals" && compatibility.Product != "zerohour" {
		return commandErr("invalid_compatibility", "product must be generals or zerohour")
	}
	if compatibility.CompatibilityVersion != GameCompatibilityVersion {
		return commandErr("invalid_compatibility", fmt.Sprintf("compatibility_version must be %d", GameCompatibilityVersion))
	}
	return nil
}

func (h *Hub) debugString() string {
	stats := h.Stats()
	return fmt.Sprintf("players=%d games=%d/%d queued=%d", stats.OnlinePlayers, stats.OpenGames, stats.ActiveGames, stats.QueuedPlayers)
}
