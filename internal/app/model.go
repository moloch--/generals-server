package app

import "time"

const (
	ProtocolVersion          = 1
	GameCompatibilityVersion = 2
)

type GameCompatibility struct {
	Product              string `json:"product"`
	CompatibilityVersion int    `json:"compatibility_version"`
	INICRC               uint32 `json:"ini_crc"`
}

type Profile struct {
	UserID      uint64    `json:"user_id"`
	Username    string    `json:"username,omitempty"`
	DisplayName string    `json:"display_name"`
	Guest       bool      `json:"guest"`
	CreatedAt   time.Time `json:"created_at"`
}

type Member struct {
	UserID      uint64 `json:"user_id"`
	DisplayName string `json:"display_name"`
	Host        bool   `json:"host"`
	Ready       bool   `json:"ready"`
	Slot        int    `json:"slot"`
}

type GameSummary struct {
	GameCompatibility
	GameID      string `json:"game_id"`
	Name        string `json:"name"`
	Map         string `json:"map,omitempty"`
	HostName    string `json:"host_name"`
	Players     int    `json:"players"`
	MaxPlayers  int    `json:"max_players"`
	HasPassword bool   `json:"has_password"`
	State       string `json:"state"`
}

type GameSnapshot struct {
	GameSummary
	Members []Member    `json:"members"`
	Options GameOptions `json:"options"`
}

type GameOptions struct {
	Map            string `json:"map,omitempty"`
	StartingCash   int    `json:"starting_cash,omitempty"`
	UseStats       bool   `json:"use_stats"`
	AllowObservers bool   `json:"allow_observers"`
	Opaque         string `json:"opaque,omitempty"`
	SlotList       string `json:"slot_list,omitempty"`
	ReadyKey       string `json:"ready_key,omitempty"`
}

type RoomSnapshot struct {
	RoomID  string   `json:"room_id"`
	Name    string   `json:"name"`
	Members []Member `json:"members"`
}

type RelayCredential struct {
	GameID   string   `json:"game_id"`
	Host     string   `json:"relay_host"`
	Port     int      `json:"relay_port"`
	Slot     int      `json:"slot"`
	Token    string   `json:"relay_token"`
	Peers    []Member `json:"peers"`
	Protocol int      `json:"relay_protocol"`
}

type PlayerStats struct {
	Wins        uint64 `json:"wins"`
	Losses      uint64 `json:"losses"`
	Disconnects uint64 `json:"disconnects"`
	Games       uint64 `json:"games"`
	Rating      int64  `json:"rating"`
}

type Buddy struct {
	UserID      uint64 `json:"user_id"`
	DisplayName string `json:"display_name"`
	Online      bool   `json:"online"`
}
