package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

type inboundEnvelope struct {
	Version   int             `json:"v"`
	Type      string          `json:"type"`
	RequestID string          `json:"id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type outboundEnvelope struct {
	Version   int    `json:"v"`
	Type      string `json:"type"`
	RequestID string `json:"id,omitempty"`
	OK        *bool  `json:"ok,omitempty"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
	Data      any    `json:"data,omitempty"`
}

type commandError struct {
	code string
	err  error
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

func commandErr(code, message string) error {
	return &commandError{code: code, err: errors.New(message)}
}

type controlClient struct {
	conn                 net.Conn
	server               *ControlServer
	send                 chan outboundEnvelope
	done                 chan struct{}
	closeOnce            sync.Once
	profile              Profile
	authed               bool
	status               string
	commandWindow        time.Time
	commandsInWindow     int
	chatWindow           time.Time
	chatMessagesInWindow int
}

func (c *controlClient) enqueue(message outboundEnvelope) bool {
	encoded, err := json.Marshal(message)
	if err != nil || len(encoded)+1 > c.server.cfg.MaxControlLineBytes {
		c.close()
		return false
	}
	select {
	case <-c.done:
		return false
	case c.send <- message:
		return true
	default:
		c.close()
		return false
	}
}

func (c *controlClient) event(eventType string, data any) bool {
	return c.enqueue(outboundEnvelope{Version: ProtocolVersion, Type: eventType, Data: data})
}

func (c *controlClient) respond(requestID string, data any) bool {
	ok := true
	return c.enqueue(outboundEnvelope{Version: ProtocolVersion, Type: "response", RequestID: requestID, OK: &ok, Data: data})
}

func (c *controlClient) reject(requestID string, err error) bool {
	ok := false
	code := "bad_request"
	var ce *commandError
	if errors.As(err, &ce) {
		code = ce.code
	}
	return c.enqueue(outboundEnvelope{
		Version: ProtocolVersion, Type: "response", RequestID: requestID,
		OK: &ok, Code: code, Error: err.Error(),
	})
}

func (c *controlClient) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *controlClient) allowCommand(now time.Time) bool {
	if c.commandWindow.IsZero() || now.Sub(c.commandWindow) >= time.Second {
		c.commandWindow = now
		c.commandsInWindow = 0
	}
	if c.commandsInWindow >= c.server.cfg.MaxCommandsPerSecond {
		return false
	}
	c.commandsInWindow++
	return true
}

func (c *controlClient) allowChat(now time.Time) bool {
	if c.chatWindow.IsZero() || now.Sub(c.chatWindow) >= 10*time.Second {
		c.chatWindow = now
		c.chatMessagesInWindow = 0
	}
	if c.chatMessagesInWindow >= c.server.cfg.MaxChatMessagesPer10Secs {
		return false
	}
	c.chatMessagesInWindow++
	return true
}

type ControlServer struct {
	cfg      Config
	log      *slog.Logger
	store    *ProfileStore
	hub      *Hub
	mu       sync.RWMutex
	listener net.Listener
	clients  map[*controlClient]struct{}
	closing  bool
	wg       sync.WaitGroup
}

func NewControlServer(cfg Config, logger *slog.Logger, store *ProfileStore, hub *Hub) *ControlServer {
	return &ControlServer{
		cfg: cfg, log: logger, store: store, hub: hub,
		clients: make(map[*controlClient]struct{}),
	}
}

func (s *ControlServer) Start(ctx context.Context) error {
	var listener net.Listener
	var err error
	if (s.cfg.TLSCertFile == "") != (s.cfg.TLSKeyFile == "") {
		return errors.New("both -tls-cert and -tls-key are required when TLS is enabled")
	}
	if s.cfg.TLSCertFile != "" {
		cert, loadErr := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if loadErr != nil {
			return fmt.Errorf("load control TLS certificate: %w", loadErr)
		}
		listener, err = tls.Listen("tcp", s.cfg.ControlAddr, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
	} else {
		listener, err = net.Listen("tcp", s.cfg.ControlAddr)
	}
	if err != nil {
		return fmt.Errorf("listen for control connections: %w", err)
	}
	s.mu.Lock()
	s.listener = listener
	s.closing = false
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.acceptLoop(ctx, listener)
	}()
	return nil
}

func (s *ControlServer) Close() error {
	s.mu.Lock()
	s.closing = true
	listener := s.listener
	s.listener = nil
	clients := make([]*controlClient, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	s.mu.Unlock()
	var err error
	if listener != nil {
		err = listener.Close()
	}
	for _, client := range clients {
		client.close()
	}
	return err
}

func (s *ControlServer) Wait() { s.wg.Wait() }

func (s *ControlServer) Address() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return s.cfg.ControlAddr
	}
	return s.listener.Addr().String()
}

func (s *ControlServer) acceptLoop(ctx context.Context, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !errors.Is(err, net.ErrClosed) {
				s.log.Warn("control accept failed", "error", err)
			}
			return
		}
		client := &controlClient{
			conn: conn, server: s, send: make(chan outboundEnvelope, 128), done: make(chan struct{}), status: "online",
		}
		s.mu.Lock()
		if s.closing || len(s.clients) >= s.cfg.MaxControlConnections {
			s.mu.Unlock()
			client.close()
			continue
		}
		s.clients[client] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveClient(ctx, client)
		}()
	}
}

func (s *ControlServer) serveClient(ctx context.Context, client *controlClient) {
	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
	}()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		encoder := json.NewEncoder(client.conn)
		encoder.SetEscapeHTML(false)
		for {
			select {
			case <-client.done:
				return
			case message := <-client.send:
				_ = client.conn.SetWriteDeadline(time.Now().Add(s.cfg.ControlWriteTimeout))
				if err := encoder.Encode(message); err != nil {
					client.close()
					return
				}
			}
		}
	}()

	client.event("server.hello", map[string]any{
		"protocol":                   ProtocolVersion,
		"server_time":                time.Now().UTC().Format(time.RFC3339Nano),
		"password_auth_requires_tls": !s.cfg.AllowInsecurePasswordAuth,
	})
	scanner := bufio.NewScanner(client.conn)
	scanner.Buffer(make([]byte, 4096), s.cfg.MaxControlLineBytes)

readLoop:
	for {
		_ = client.conn.SetReadDeadline(time.Now().Add(s.cfg.ControlReadTimeout))
		if !scanner.Scan() {
			break
		}
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var message inboundEnvelope
		if err := json.Unmarshal(line, &message); err != nil {
			client.reject("", commandErr("invalid_json", "message is not valid JSON"))
			continue
		}
		if message.Version != ProtocolVersion {
			client.reject(message.RequestID, commandErr("unsupported_protocol", "unsupported control protocol version"))
			continue
		}
		if message.Type == "" || message.RequestID == "" {
			client.reject(message.RequestID, commandErr("invalid_envelope", "type and id are required"))
			continue
		}
		closeAfter, err := s.dispatch(client, message)
		if err != nil {
			client.reject(message.RequestID, err)
		}
		if closeAfter {
			break
		}
		select {
		case <-ctx.Done():
			break readLoop
		default:
		}
	}
	if client.authed {
		s.hub.Disconnect(client)
	}
	client.close()
	<-writerDone
}

func (s *ControlServer) dispatch(client *controlClient, message inboundEnvelope) (bool, error) {
	now := time.Now()
	if !client.allowCommand(now) {
		return false, commandErr("rate_limited", "control command rate limit exceeded")
	}
	if (message.Type == "room.chat" || message.Type == "game.chat" || message.Type == "player.chat") && !client.allowChat(now) {
		return false, commandErr("rate_limited", "chat message rate limit exceeded")
	}
	if !client.authed {
		return false, s.dispatchAuth(client, message)
	}
	if strings.HasPrefix(message.Type, "auth.") {
		return false, commandErr("already_authenticated", "connection is already authenticated")
	}
	data, closeAfter, err := s.hub.Command(client, message.Type, message.Data)
	if err != nil {
		return false, err
	}
	client.respond(message.RequestID, data)
	return closeAfter, nil
}

func (s *ControlServer) dispatchAuth(client *controlClient, message inboundEnvelope) error {
	var profile Profile
	var err error
	profileReserved := false
	switch message.Type {
	case "auth.guest":
		var request struct {
			DisplayName string `json:"display_name"`
		}
		if err = decodeData(message.Data, &request); err == nil {
			profile, err = s.hub.NewGuest(request.DisplayName)
		}
	case "auth.register":
		if !s.passwordAuthAllowed(client) {
			return commandErr("tls_required", "password authentication requires a TLS control connection")
		}
		var request struct {
			Username    string `json:"username"`
			Password    string `json:"password"`
			DisplayName string `json:"display_name"`
		}
		if err = decodeData(message.Data, &request); err == nil {
			if err = s.hub.ReserveRegistration(request.DisplayName); err == nil {
				profile, err = s.store.Register(request.Username, request.Password, request.DisplayName)
				if err != nil {
					s.hub.ReleaseRegistration(request.DisplayName)
				} else {
					s.hub.CommitRegistration(profile)
					profileReserved = true
				}
			}
		}
	case "auth.login":
		if !s.passwordAuthAllowed(client) {
			return commandErr("tls_required", "password authentication requires a TLS control connection")
		}
		var request struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err = decodeData(message.Data, &request); err == nil {
			profile, err = s.store.Authenticate(request.Username, request.Password)
		}
	case "auth.resume":
		if !s.passwordAuthAllowed(client) {
			return commandErr("tls_required", "persistent session resumption requires a TLS control connection")
		}
		var request struct {
			Token string `json:"token"`
		}
		if err = decodeData(message.Data, &request); err == nil {
			profile, err = s.hub.ResumeAndReserve(request.Token)
			profileReserved = err == nil
		}
	default:
		return commandErr("authentication_required", "authenticate before using this command")
	}
	if err != nil {
		if errors.Is(err, ErrProfileLimit) {
			return commandErr("server_full", "the Online server has reached its persistent profile limit")
		}
		var ce *commandError
		if errors.As(err, &ce) {
			return err
		}
		return commandErr("authentication_failed", err.Error())
	}
	if !profileReserved {
		if err := s.hub.ReserveProfile(profile); err != nil {
			return err
		}
	}
	client.profile = profile
	client.authed = true
	authData := map[string]any{"profile": profile}
	if !profile.Guest {
		token, expires, sessionErr := s.hub.IssueSession(profile)
		if sessionErr != nil {
			client.authed = false
			s.hub.ReleaseProfileReservation(profile)
			return commandErr("internal_error", sessionErr.Error())
		}
		authData["token"] = token
		authData["expires_at"] = expires.UTC().Format(time.RFC3339Nano)
	}
	client.respond(message.RequestID, authData)
	s.hub.Connect(client)
	return nil
}

func (s *ControlServer) passwordAuthAllowed(client *controlClient) bool {
	if s.cfg.AllowInsecurePasswordAuth {
		return true
	}
	_, ok := client.conn.(*tls.Conn)
	return ok
}

func decodeData(raw json.RawMessage, destination any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return commandErr("invalid_data", err.Error())
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return commandErr("invalid_data", err.Error())
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("data contains multiple JSON values")
	}
	return err
}
