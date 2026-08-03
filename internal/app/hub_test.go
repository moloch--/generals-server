package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
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
