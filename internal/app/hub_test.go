package app

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestUpdateGameOptionsDoesNotBroadcastUnchangedSnapshot(t *testing.T) {
	t.Parallel()
	hub, host, peer := newHubGameTest(t)

	request, err := json.Marshal(map[string]any{
		"name":        "Stable game",
		"password":    "",
		"max_players": 2,
		"options": map[string]any{
			"map":             "Maps/Test/Test.map",
			"use_stats":       false,
			"allow_observers": false,
			"opaque":          "stable-options",
			"slot_list":       "stable-slots",
			"ready_key":       "configuration-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hub.updateGameOptions(host, request); err != nil {
		t.Fatal(err)
	}
	if got := len(host.send); got != 0 {
		t.Fatalf("host received %d events for unchanged options, want 0", got)
	}
	if got := len(peer.send); got != 0 {
		t.Fatalf("peer received %d events for unchanged options, want 0", got)
	}

	changed, err := json.Marshal(map[string]any{"name": "Changed game"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hub.updateGameOptions(host, changed); err != nil {
		t.Fatal(err)
	}
	assertQueuedEvent(t, host, "game.updated")
	assertQueuedEvent(t, host, "game.list")
	assertQueuedEvent(t, peer, "game.updated")
	assertQueuedEvent(t, peer, "game.list")
}

func TestHubAdminResetPasswordRevokesSessionsAdmissionsAndActiveClient(t *testing.T) {
	store := openTestProfileStore(t, "")
	cfg := DefaultConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(cfg, logger, store, NewRelay(cfg, logger))

	active, err := store.Register("reset_active", "original password", "Reset Active")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.ReserveProfile(active); err != nil {
		t.Fatal(err)
	}
	token, _, err := hub.IssueSession(active)
	if err != nil {
		t.Fatal(err)
	}
	client := newPersistentHubClient(t, cfg, active)
	hub.Connect(client)

	updated, err := hub.AdminResetPassword(active.UserID, "replacement password")
	if err != nil || !updated {
		t.Fatalf("AdminResetPassword() updated=%v error=%v", updated, err)
	}
	requireClientClosed(t, client)
	hub.mu.RLock()
	_, clientPresent := hub.clients[active.UserID]
	_, tokenPresent := hub.sessions[token]
	_, sessionPresent := hub.sessionByUser[active.UserID]
	hub.mu.RUnlock()
	if clientPresent || tokenPresent || sessionPresent {
		t.Fatalf("reset left client=%v token=%v session index=%v", clientPresent, tokenPresent, sessionPresent)
	}
	if _, err := store.Authenticate(active.Username, "original password"); err == nil {
		t.Fatal("original password remained valid after admin reset")
	}
	if _, err := store.Authenticate(active.Username, "replacement password"); err != nil {
		t.Fatalf("replacement password was rejected: %v", err)
	}

	pending, err := store.Register("reset_pending", "pending password", "Reset Pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.ReserveProfile(pending); err != nil {
		t.Fatal(err)
	}
	pendingToken, _, err := hub.IssueSession(pending)
	if err != nil {
		t.Fatal(err)
	}
	updated, err = hub.AdminResetPassword(pending.UserID, "pending replacement")
	if err != nil || !updated {
		t.Fatalf("pending AdminResetPassword() updated=%v error=%v", updated, err)
	}
	hub.mu.RLock()
	_, admissionPresent := hub.pendingAdmissions[pending.UserID]
	_, ownerPresent := hub.displayOwners[normalizeDisplayName(pending.DisplayName)]
	_, pendingTokenPresent := hub.sessions[pendingToken]
	hub.mu.RUnlock()
	if admissionPresent || ownerPresent || pendingTokenPresent {
		t.Fatalf("reset left admission=%v display owner=%v token=%v", admissionPresent, ownerPresent, pendingTokenPresent)
	}
	pendingClient := newPersistentHubClient(t, cfg, pending)
	if _, err := hub.Connect(pendingClient); err == nil {
		t.Fatal("authentication connected after its admission was revoked by password reset")
	}
	if _, _, err := hub.IssueSession(pending); err == nil {
		t.Fatal("authentication issued a session after its admission was revoked by password reset")
	}

	if updated, err := hub.AdminResetPassword(999999, "missing password"); err != nil || updated {
		t.Fatalf("missing AdminResetPassword() updated=%v error=%v", updated, err)
	}
}

func TestHubRejectsAuthenticationVerifiedBeforeCredentialMutation(t *testing.T) {
	store := openTestProfileStore(t, "")
	cfg := DefaultConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(cfg, logger, store, NewRelay(cfg, logger))

	profile, err := store.Register("racing_login", "original password", "Racing Login")
	if err != nil {
		t.Fatal(err)
	}
	verified, stamp, err := store.authenticateWithCredentialStamp(profile.Username, "original password")
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := hub.AdminResetPassword(profile.UserID, "replacement password"); err != nil || !updated {
		t.Fatalf("AdminResetPassword() updated=%v error=%v", updated, err)
	}
	if err := hub.reserveAuthenticatedProfile(verified, stamp); !errors.Is(err, errAuthenticationCredentialsChanged) {
		t.Fatalf("stale authentication reservation error = %v, want credential change", err)
	}
	hub.mu.RLock()
	_, admitted := hub.pendingAdmissions[profile.UserID]
	hub.mu.RUnlock()
	if admitted {
		t.Fatal("stale authentication result created a pending admission")
	}
	if _, err := hub.AuthenticateAndReserve(profile.Username, "original password"); err == nil {
		t.Fatal("old password authenticated through the coordinated admission path")
	}
	authenticated, err := hub.AuthenticateAndReserve(profile.Username, "replacement password")
	if err != nil {
		t.Fatalf("replacement password did not authenticate: %v", err)
	}
	hub.ReleaseProfileReservation(authenticated)

	other, err := store.Register("unrelated_auth", "unrelated password", "Unrelated Auth")
	if err != nil {
		t.Fatal(err)
	}
	verified, stamp, err = store.authenticateWithCredentialStamp(profile.Username, "replacement password")
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := hub.AdminResetPassword(other.UserID, "unrelated replacement"); err != nil || !updated {
		t.Fatalf("unrelated AdminResetPassword() updated=%v error=%v", updated, err)
	}
	if err := hub.reserveAuthenticatedProfile(verified, stamp); err != nil {
		t.Fatalf("unrelated credential mutation rejected valid login: %v", err)
	}
	hub.ReleaseProfileReservation(verified)

	if err := hub.ReserveRegistration("Racing Register"); err != nil {
		t.Fatal(err)
	}
	registered, registrationStamp, err := store.registerWithCredentialStamp("racing_register", "registration password", "Racing Register")
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := hub.AdminResetPassword(other.UserID, "second unrelated replacement"); err != nil || !updated {
		t.Fatalf("second unrelated AdminResetPassword() updated=%v error=%v", updated, err)
	}
	if err := hub.CommitRegistration(registered, registrationStamp); err != nil {
		t.Fatalf("unrelated credential mutation rejected valid registration: %v", err)
	}
	hub.ReleaseProfileReservation(registered)

	if err := hub.ReserveRegistration("Racing Stale Register"); err != nil {
		t.Fatal(err)
	}
	staleRegistered, staleRegistrationStamp, err := store.registerWithCredentialStamp("racing_stale_register", "registration password", "Racing Stale Register")
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := hub.AdminResetPassword(staleRegistered.UserID, "registration replacement"); err != nil || !updated {
		t.Fatalf("registered AdminResetPassword() updated=%v error=%v", updated, err)
	}
	if err := hub.CommitRegistration(staleRegistered, staleRegistrationStamp); !errors.Is(err, errAuthenticationCredentialsChanged) {
		t.Fatalf("stale registration commit error = %v, want credential change", err)
	}
	hub.mu.RLock()
	_, admitted = hub.pendingAdmissions[staleRegistered.UserID]
	_, registering := hub.pendingRegistrations[normalizeDisplayName(staleRegistered.DisplayName)]
	hub.mu.RUnlock()
	if admitted || registering {
		t.Fatalf("stale registration left admission=%v registration=%v", admitted, registering)
	}
}

func TestHubAdminDeleteWaitsForInFlightCommandAndRemovesItsState(t *testing.T) {
	store := openTestProfileStore(t, "")
	cfg := DefaultConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(cfg, logger, store, NewRelay(cfg, logger))
	profile, err := store.Register("revoke_command", "command password", "Revoke Command")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.ReserveProfile(profile); err != nil {
		t.Fatal(err)
	}
	client := newPersistentHubClient(t, cfg, profile)
	if _, err := hub.Connect(client); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(testCompatibleRequest(map[string]any{
		"name":        "Revocation race",
		"max_players": 2,
		"options":     map[string]any{},
	}))
	if err != nil {
		t.Fatal(err)
	}

	hub.mu.Lock()
	commandDone := make(chan error, 1)
	go func() {
		_, _, commandErr := hub.Command(client, "game.create", raw)
		commandDone <- commandErr
	}()
	readerDeadline := time.Now().Add(time.Second)
	for hub.commandGate.TryLock() {
		hub.commandGate.Unlock()
		if time.Now().After(readerDeadline) {
			hub.mu.Unlock()
			t.Fatal("command did not enter the revocation gate")
		}
		time.Sleep(time.Millisecond)
	}
	deleteDone := make(chan error, 1)
	go func() {
		deleted, deleteErr := hub.AdminDeleteProfile(profile.UserID)
		if deleteErr == nil && !deleted {
			deleteErr = errors.New("profile was not deleted")
		}
		deleteDone <- deleteErr
	}()
	writerDeadline := time.Now().Add(time.Second)
	for hub.commandGate.TryRLock() {
		hub.commandGate.RUnlock()
		if time.Now().After(writerDeadline) {
			hub.mu.Unlock()
			t.Fatal("admin deletion did not wait on the revocation gate")
		}
		time.Sleep(time.Millisecond)
	}
	hub.mu.Unlock()

	if err := <-commandDone; err != nil {
		t.Fatalf("in-flight command failed before revocation: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	hub.mu.RLock()
	_, clientPresent := hub.clients[profile.UserID]
	_, gamePresent := hub.userGame[profile.UserID]
	gameCount := len(hub.games)
	queueCount := len(hub.matchQueue)
	hub.mu.RUnlock()
	if clientPresent || gamePresent || gameCount != 0 || queueCount != 0 {
		t.Fatalf("revoked command left client=%v user_game=%v games=%d queue=%d", clientPresent, gamePresent, gameCount, queueCount)
	}
}

func TestHubAdminDeleteProfileRevokesAccessAndRejectsDeletedResume(t *testing.T) {
	store := openTestProfileStore(t, "")
	cfg := DefaultConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(cfg, logger, store, NewRelay(cfg, logger))

	profile, err := store.Register("delete_active", "delete password", "Delete Active")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.ReserveProfile(profile); err != nil {
		t.Fatal(err)
	}
	token, _, err := hub.IssueSession(profile)
	if err != nil {
		t.Fatal(err)
	}
	client := newPersistentHubClient(t, cfg, profile)
	hub.Connect(client)

	deleted, err := hub.AdminDeleteProfile(profile.UserID)
	if err != nil || !deleted {
		t.Fatalf("AdminDeleteProfile() deleted=%v error=%v", deleted, err)
	}
	requireClientClosed(t, client)
	if _, ok := store.Get(profile.UserID); ok {
		t.Fatal("admin-deleted profile remained in the store")
	}
	hub.mu.RLock()
	_, clientPresent := hub.clients[profile.UserID]
	_, tokenPresent := hub.sessions[token]
	_, sessionPresent := hub.sessionByUser[profile.UserID]
	hub.mu.RUnlock()
	if clientPresent || tokenPresent || sessionPresent {
		t.Fatalf("delete left client=%v token=%v session index=%v", clientPresent, tokenPresent, sessionPresent)
	}
	if deleted, err := hub.AdminDeleteProfile(profile.UserID); err != nil || deleted {
		t.Fatalf("second AdminDeleteProfile() deleted=%v error=%v", deleted, err)
	}

	stale, err := store.Register("delete_stale", "stale password", "Delete Stale")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.ReserveProfile(stale); err != nil {
		t.Fatal(err)
	}
	staleToken, _, err := hub.IssueSession(stale)
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.DeleteProfile(stale.UserID); err != nil || !deleted {
		t.Fatalf("direct DeleteProfile() deleted=%v error=%v", deleted, err)
	}
	if _, err := hub.ResumeAndReserve(staleToken); err == nil {
		t.Fatal("resumable session restored a deleted persistent profile")
	}
	hub.mu.RLock()
	_, staleTokenPresent := hub.sessions[staleToken]
	_, staleSessionPresent := hub.sessionByUser[stale.UserID]
	hub.mu.RUnlock()
	if staleTokenPresent || staleSessionPresent {
		t.Fatalf("deleted resume left token=%v session index=%v", staleTokenPresent, staleSessionPresent)
	}
}

func newHubGameTest(t *testing.T) (*Hub, *controlClient, *controlClient) {
	t.Helper()
	cfg := DefaultConfig()
	server := &ControlServer{cfg: cfg}
	newClient := func(userID uint64, displayName string) *controlClient {
		return &controlClient{
			server:  server,
			send:    make(chan outboundEnvelope, 8),
			done:    make(chan struct{}),
			profile: Profile{UserID: userID, DisplayName: displayName, Guest: true},
			authed:  true,
			status:  "in_game",
		}
	}
	host := newClient(1, "Host")
	peer := newClient(2, "Peer")
	hub := NewHub(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	hub.clients[1] = host
	hub.clients[2] = peer
	hub.userGame[1] = 1
	hub.userGame[2] = 1
	hub.games[1] = &stagedGame{
		id:            1,
		name:          "Stable game",
		maxPlayers:    2,
		compatibility: testGameCompatibility,
		hostID:        1,
		state:         "open",
		listed:        true,
		options: GameOptions{
			Map:      "Maps/Test/Test.map",
			Opaque:   "stable-options",
			SlotList: "stable-slots",
			ReadyKey: "configuration-1",
		},
		members: map[uint64]*gameMember{
			1: {client: host, slot: 0},
			2: {client: peer, slot: 1},
		},
	}
	return hub, host, peer
}

func assertQueuedEvent(t *testing.T, client *controlClient, eventType string) outboundEnvelope {
	t.Helper()
	select {
	case message := <-client.send:
		if message.Type != eventType {
			t.Fatalf("queued event type = %q, want %q", message.Type, eventType)
		}
		return message
	default:
		t.Fatalf("missing queued %s event", eventType)
		return outboundEnvelope{}
	}
}

func newPersistentHubClient(t *testing.T, cfg Config, profile Profile) *controlClient {
	t.Helper()
	serverSide, peerSide := net.Pipe()
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = peerSide.Close()
	})
	return &controlClient{
		conn:    serverSide,
		server:  &ControlServer{cfg: cfg},
		send:    make(chan outboundEnvelope, 16),
		done:    make(chan struct{}),
		profile: profile,
		authed:  true,
		status:  "online",
	}
}

func requireClientClosed(t *testing.T, client *controlClient) {
	t.Helper()
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("admin profile mutation did not close the active client")
	}
}
