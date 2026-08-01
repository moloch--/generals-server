package app

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	relayHeaderSize          = 32
	relayMaxPayload          = 1100
	relayBroadcast           = 0xff
	relayVersion             = 1
	relayKindBind            = 1
	relayKindData            = 2
	relayKindKeepalive       = 3
	relayKindBindAck         = 4
	relayKindDataOut         = 5
	relayKindError           = 6
	relayInitialQueuePackets = 32
)

var relayMagic = [4]byte{'G', 'X', 'R', 'L'}

type relayToken [16]byte

type relayParticipant struct {
	userID      uint64
	slot        uint8
	token       relayToken
	endpoint    *net.UDPAddr
	lastSeen    time.Time
	windowStart time.Time
	packets     int
	bytes       int
	pending     [][]byte
}

type relayGame struct {
	id           uint64
	participants map[uint8]*relayParticipant
	byToken      map[relayToken]*relayParticipant
	lastActivity time.Time
}

type RelayStats struct {
	DatagramsIn       uint64 `json:"datagrams_in"`
	DatagramsOut      uint64 `json:"datagrams_out"`
	BytesIn           uint64 `json:"bytes_in"`
	BytesOut          uint64 `json:"bytes_out"`
	DroppedMalformed  uint64 `json:"dropped_malformed"`
	DroppedAuth       uint64 `json:"dropped_auth"`
	DroppedRateLimit  uint64 `json:"dropped_rate_limit"`
	DroppedNoEndpoint uint64 `json:"dropped_no_endpoint"`
	BufferedUntilBind uint64 `json:"buffered_until_bind"`
	ActiveGames       int    `json:"active_games"`
}

type Relay struct {
	cfg   Config
	log   *slog.Logger
	mu    sync.RWMutex
	conn  *net.UDPConn
	games map[uint64]*relayGame

	datagramsIn       atomic.Uint64
	datagramsOut      atomic.Uint64
	bytesIn           atomic.Uint64
	bytesOut          atomic.Uint64
	droppedMalformed  atomic.Uint64
	droppedAuth       atomic.Uint64
	droppedRateLimit  atomic.Uint64
	droppedNoEndpoint atomic.Uint64
	bufferedUntilBind atomic.Uint64
}

func NewRelay(cfg Config, logger *slog.Logger) *Relay {
	return &Relay{cfg: cfg, log: logger, games: make(map[uint64]*relayGame)}
}

func (r *Relay) Start(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", r.cfg.RelayAddr)
	if err != nil {
		return fmt.Errorf("resolve UDP relay address: %w", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen for UDP relay traffic: %w", err)
	}
	r.mu.Lock()
	r.conn = conn
	r.mu.Unlock()
	go r.serve(ctx, conn)
	go r.expireLoop(ctx)
	return nil
}

func (r *Relay) Close() error {
	r.mu.Lock()
	conn := r.conn
	r.conn = nil
	r.games = make(map[uint64]*relayGame)
	r.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (r *Relay) Address() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.conn == nil {
		return r.cfg.RelayAddr
	}
	return r.conn.LocalAddr().String()
}

func (r *Relay) Port() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.conn != nil {
		return r.conn.LocalAddr().(*net.UDPAddr).Port
	}
	_, port, err := net.SplitHostPort(r.cfg.RelayAddr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

func (r *Relay) Allocate(gameID uint64, members []Member) (map[uint64]RelayCredential, error) {
	if gameID == 0 {
		return nil, errors.New("game id must not be zero")
	}
	if len(members) == 0 || len(members) > 8 {
		return nil, errors.New("relay games require 1-8 members")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return nil, errors.New("relay is not running")
	}
	if _, exists := r.games[gameID]; exists {
		return nil, errors.New("game already has a relay allocation")
	}
	g := &relayGame{
		id:           gameID,
		participants: make(map[uint8]*relayParticipant, len(members)),
		byToken:      make(map[relayToken]*relayParticipant, len(members)),
		lastActivity: time.Now(),
	}
	credentials := make(map[uint64]RelayCredential, len(members))
	for _, member := range members {
		if member.Slot < 0 || member.Slot > 7 {
			return nil, errors.New("relay slot is out of range")
		}
		var token relayToken
		if _, err := rand.Read(token[:]); err != nil {
			return nil, fmt.Errorf("generate relay token: %w", err)
		}
		p := &relayParticipant{userID: member.UserID, slot: uint8(member.Slot), token: token}
		if g.participants[p.slot] != nil {
			return nil, errors.New("relay slots must be unique")
		}
		g.participants[p.slot] = p
		g.byToken[token] = p
		credentials[member.UserID] = RelayCredential{
			GameID:   formatGameID(gameID),
			Host:     r.cfg.PublicHost,
			Port:     r.conn.LocalAddr().(*net.UDPAddr).Port,
			Slot:     member.Slot,
			Token:    hex.EncodeToString(token[:]),
			Peers:    append([]Member(nil), members...),
			Protocol: relayVersion,
		}
	}
	r.games[gameID] = g
	return credentials, nil
}

func (r *Relay) EndGame(gameID uint64) {
	r.mu.Lock()
	delete(r.games, gameID)
	r.mu.Unlock()
}

func (r *Relay) RemoveParticipant(gameID, userID uint64, slot int) bool {
	if slot < 0 || slot > 7 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	game := r.games[gameID]
	if game == nil {
		return false
	}
	participant := game.participants[uint8(slot)]
	if participant == nil || participant.userID != userID {
		return false
	}
	delete(game.participants, participant.slot)
	delete(game.byToken, participant.token)
	participant.endpoint = nil
	participant.pending = nil
	participant.token = relayToken{}
	game.lastActivity = time.Now()
	return true
}

func (r *Relay) Stats() RelayStats {
	r.mu.RLock()
	active := len(r.games)
	r.mu.RUnlock()
	return RelayStats{
		DatagramsIn:       r.datagramsIn.Load(),
		DatagramsOut:      r.datagramsOut.Load(),
		BytesIn:           r.bytesIn.Load(),
		BytesOut:          r.bytesOut.Load(),
		DroppedMalformed:  r.droppedMalformed.Load(),
		DroppedAuth:       r.droppedAuth.Load(),
		DroppedRateLimit:  r.droppedRateLimit.Load(),
		DroppedNoEndpoint: r.droppedNoEndpoint.Load(),
		BufferedUntilBind: r.bufferedUntilBind.Load(),
		ActiveGames:       active,
	}
}

func (r *Relay) serve(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, relayHeaderSize+relayMaxPayload+1)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !errors.Is(err, net.ErrClosed) {
				r.log.Warn("UDP relay read failed", "error", err)
			}
			return
		}
		r.datagramsIn.Add(1)
		r.bytesIn.Add(uint64(n))
		r.handleDatagram(buf[:n], addr)
	}
}

func (r *Relay) handleDatagram(packet []byte, addr *net.UDPAddr) {
	if len(packet) < relayHeaderSize || len(packet) > relayHeaderSize+relayMaxPayload ||
		packet[0] != relayMagic[0] || packet[1] != relayMagic[1] ||
		packet[2] != relayMagic[2] || packet[3] != relayMagic[3] || packet[4] != relayVersion {
		r.droppedMalformed.Add(1)
		return
	}
	kind := packet[5]
	if kind != relayKindBind && kind != relayKindData && kind != relayKindKeepalive {
		r.droppedMalformed.Add(1)
		return
	}
	source := packet[6]
	target := packet[7]
	gameID := binary.BigEndian.Uint64(packet[8:16])
	var token relayToken
	copy(token[:], packet[16:32])
	now := time.Now()

	r.mu.Lock()
	g := r.games[gameID]
	if g == nil {
		r.mu.Unlock()
		r.droppedAuth.Add(1)
		return
	}
	p := g.byToken[token]
	if p == nil || p.slot != source {
		r.mu.Unlock()
		r.droppedAuth.Add(1)
		return
	}
	if now.Sub(p.windowStart) >= time.Second {
		p.windowStart = now
		p.packets = 0
		p.bytes = 0
	}
	p.packets++
	p.bytes += len(packet)
	if p.packets > r.cfg.MaxRelayPacketsPerSecond || p.bytes > r.cfg.MaxRelayBytesPerSecond {
		r.mu.Unlock()
		r.droppedRateLimit.Add(1)
		return
	}
	p.endpoint = cloneUDPAddr(addr)
	p.lastSeen = now
	g.lastActivity = now
	conn := r.conn

	if kind == relayKindBind || kind == relayKindKeepalive {
		ack := makeRelayPacket(relayKindBindAck, p.slot, p.slot, gameID, p.token, nil)
		pending := p.pending
		p.pending = nil
		boundAddress := cloneUDPAddr(addr)
		r.mu.Unlock()
		r.writeTo(conn, ack, boundAddress)
		for _, frame := range pending {
			r.writeTo(conn, frame, boundAddress)
		}
		return
	}

	payload := append([]byte(nil), packet[relayHeaderSize:]...)
	recipients := make([]*relayParticipant, 0, len(g.participants))
	if target == relayBroadcast {
		for slot, candidate := range g.participants {
			if slot != source {
				recipients = append(recipients, candidate)
			}
		}
	} else if candidate := g.participants[target]; candidate != nil && target != source {
		recipients = append(recipients, candidate)
	}
	type delivery struct {
		addr  *net.UDPAddr
		frame []byte
	}
	deliveries := make([]delivery, 0, len(recipients))
	for _, recipient := range recipients {
		frame := makeRelayPacket(relayKindDataOut, source, recipient.slot, gameID, recipient.token, payload)
		if recipient.endpoint == nil {
			if len(recipient.pending) >= relayInitialQueuePackets {
				copy(recipient.pending, recipient.pending[1:])
				recipient.pending[len(recipient.pending)-1] = frame
				r.droppedNoEndpoint.Add(1)
			} else {
				recipient.pending = append(recipient.pending, frame)
			}
			r.bufferedUntilBind.Add(1)
			continue
		}
		deliveries = append(deliveries, delivery{
			addr:  cloneUDPAddr(recipient.endpoint),
			frame: frame,
		})
	}
	r.mu.Unlock()
	for _, d := range deliveries {
		r.writeTo(conn, d.frame, d.addr)
	}
}

func (r *Relay) writeTo(conn *net.UDPConn, packet []byte, addr *net.UDPAddr) {
	if conn == nil || addr == nil {
		return
	}
	n, err := conn.WriteToUDP(packet, addr)
	if err != nil {
		return
	}
	r.datagramsOut.Add(1)
	r.bytesOut.Add(uint64(n))
}

func (r *Relay) expireLoop(ctx context.Context) {
	interval := time.Minute
	if r.cfg.GameIdleTimeout > 0 && r.cfg.GameIdleTimeout/2 < interval {
		interval = r.cfg.GameIdleTimeout / 2
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.mu.Lock()
			for id, game := range r.games {
				if now.Sub(game.lastActivity) > r.cfg.GameIdleTimeout {
					delete(r.games, id)
				}
			}
			r.mu.Unlock()
		}
	}
}

func makeRelayPacket(kind, source, target uint8, gameID uint64, token relayToken, payload []byte) []byte {
	packet := make([]byte, relayHeaderSize+len(payload))
	copy(packet[:4], relayMagic[:])
	packet[4] = relayVersion
	packet[5] = kind
	packet[6] = source
	packet[7] = target
	binary.BigEndian.PutUint64(packet[8:16], gameID)
	copy(packet[16:32], token[:])
	copy(packet[32:], payload)
	return packet
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), addr.IP...), Port: addr.Port, Zone: addr.Zone}
}

func formatGameID(id uint64) string {
	return fmt.Sprintf("%016x", id)
}

func parseGameID(value string) (uint64, error) {
	if len(value) != 16 {
		return 0, errors.New("game_id must contain exactly 16 lowercase hexadecimal characters")
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return 0, errors.New("game_id must contain exactly 16 lowercase hexadecimal characters")
		}
	}
	return strconv.ParseUint(value, 16, 64)
}
