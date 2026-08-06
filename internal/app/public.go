package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moloch--/generals-server/internal/app/publicui"
)

const (
	publicLeaderboardLimit    = 25
	publicLeaderboardCacheTTL = 2 * time.Second
)

var publicPageRoutes = []string{
	"/leaderboard",
	"/game-lobbies",
	"/online-players",
	"/active-games",
	"/how-to-play",
}

// GeneralsX @feature OpenAI 06/08/2026 Define a fixed public schema that cannot inherit admin fields.
type publicOverview struct {
	OnlinePlayers int `json:"online_players"`
	OpenLobbies   int `json:"open_lobbies"`
	ActiveGames   int `json:"active_games"`
	QueuedPlayers int `json:"queued_players"`
}

type publicLeaderboardEntry struct {
	DisplayName string `json:"display_name"`
	Wins        string `json:"wins"`
	Losses      string `json:"losses"`
	Games       string `json:"games"`
	Rating      string `json:"rating"`
}

type publicOnlinePlayer struct {
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

type publicLobby struct {
	Name        string `json:"name"`
	Map         string `json:"map"`
	HostName    string `json:"host_name"`
	Players     int    `json:"players"`
	MaxPlayers  int    `json:"max_players"`
	HasPassword bool   `json:"has_password"`
	Product     string `json:"product"`
}

type publicActiveGame struct {
	Name       string `json:"name"`
	Map        string `json:"map"`
	Players    int    `json:"players"`
	MaxPlayers int    `json:"max_players"`
	Product    string `json:"product"`
	State      string `json:"state"`
}

type publicActivitySnapshot struct {
	Overview      publicOverview       `json:"overview"`
	OnlinePlayers []publicOnlinePlayer `json:"online_players"`
	Lobbies       []publicLobby        `json:"lobbies"`
	ActiveGames   []publicActiveGame   `json:"active_games"`
}

type publicLeaderboardRecord struct {
	DisplayName string
	Stats       PlayerStats
}

type publicSnapshot struct {
	GeneratedAt   string                   `json:"generated_at"`
	Overview      publicOverview           `json:"overview"`
	Leaderboard   []publicLeaderboardEntry `json:"leaderboard"`
	OnlinePlayers []publicOnlinePlayer     `json:"online_players"`
	Lobbies       []publicLobby            `json:"lobbies"`
	ActiveGames   []publicActiveGame       `json:"active_games"`
}

type publicDataEnvelope struct {
	Data publicSnapshot `json:"data"`
}

type publicActivityReader interface {
	PublicActivitySnapshot() publicActivitySnapshot
}

type publicLeaderboardReader interface {
	PublicLeaderboard(context.Context) ([]publicLeaderboardRecord, error)
}

// GeneralsX @feature OpenAI 06/08/2026 Serve a read-only public site through a capability-limited handler.
type publicHandler struct {
	activity    publicActivityReader
	leaderboard publicLeaderboardReader
	log         *slog.Logger
	assets      fs.FS
	mux         *http.ServeMux
	now         func() time.Time

	leaderboardMu       sync.Mutex
	leaderboardCachedAt time.Time
	leaderboardCache    []publicLeaderboardRecord
}

func newPublicHandler(activity publicActivityReader, leaderboard publicLeaderboardReader, logger *slog.Logger) *publicHandler {
	if logger == nil {
		logger = slog.Default()
	}
	handler := &publicHandler{
		activity:    activity,
		leaderboard: leaderboard,
		log:         logger,
		assets:      publicui.Files(),
		mux:         http.NewServeMux(),
		now:         time.Now,
	}
	handler.mux.HandleFunc("GET /api/public/v1/snapshot", handler.handleSnapshot)
	handler.mux.HandleFunc("GET /{$}", handler.handleIndex)
	for _, route := range publicPageRoutes {
		handler.mux.HandleFunc("GET "+route, handler.handleIndex)
	}
	handler.mux.HandleFunc("GET /assets/", handler.handleAsset)
	handler.mux.HandleFunc("GET /generalsx-zh-icon.png", handler.handleAsset)
	return handler
}

func (p *publicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setPublicSecurityHeaders(w)
	if r.TLS != nil {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if !isCanonicalPublicRequestPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	p.mux.ServeHTTP(w, r)
}

// GeneralsX @feature OpenAI 06/08/2026 Redirect only known public routes without sharing the public or admin mux.
func newPublicWebRedirectHandler(canonicalHost string) http.Handler {
	assets := publicui.Files()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setPublicSecurityHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		if !isAllowedPublicRedirectRequest(r, assets) {
			http.NotFound(w, r)
			return
		}
		target := (&url.URL{
			Scheme:   "https",
			Host:     canonicalHost,
			Path:     r.URL.Path,
			RawPath:  r.URL.RawPath,
			RawQuery: r.URL.RawQuery,
		}).String()
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

func isAllowedPublicRedirectRequest(r *http.Request, assets fs.FS) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	requestPath := r.URL.Path
	if !isCanonicalPublicRequestPath(requestPath) {
		return false
	}
	if isPublicPageRoute(requestPath) || requestPath == "/generalsx-zh-icon.png" || requestPath == "/api/public/v1/snapshot" {
		return true
	}
	if !strings.HasPrefix(requestPath, "/assets/") || len(requestPath) == len("/assets/") {
		return false
	}
	asset, err := fs.Stat(assets, strings.TrimPrefix(requestPath, "/"))
	return err == nil && !asset.IsDir()
}

func isPublicPageRoute(requestPath string) bool {
	if requestPath == "/" {
		return true
	}
	for _, route := range publicPageRoutes {
		if requestPath == route {
			return true
		}
	}
	return false
}

func isCanonicalPublicRequestPath(requestPath string) bool {
	return requestPath != "" && path.Clean(requestPath) == requestPath
}

func (p *publicHandler) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not supported", http.StatusBadRequest)
		return
	}
	queryContext, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	records, err := p.loadLeaderboard(queryContext)
	if err != nil {
		p.log.Error("public API request failed", "operation", "read leaderboard", "remote", r.RemoteAddr, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	activity := p.activity.PublicActivitySnapshot()
	leaderboard := make([]publicLeaderboardEntry, 0, len(records))
	for _, record := range records {
		leaderboard = append(leaderboard, publicLeaderboardEntry{
			DisplayName: record.DisplayName,
			Wins:        strconv.FormatUint(record.Stats.Wins, 10),
			Losses:      strconv.FormatUint(record.Stats.Losses, 10),
			Games:       strconv.FormatUint(record.Stats.Games, 10),
			Rating:      strconv.FormatInt(record.Stats.Rating, 10),
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=2, stale-while-revalidate=3")
	_ = json.NewEncoder(w).Encode(publicDataEnvelope{
		Data: publicSnapshot{
			GeneratedAt:   p.now().UTC().Format(time.RFC3339Nano),
			Overview:      activity.Overview,
			Leaderboard:   leaderboard,
			OnlinePlayers: activity.OnlinePlayers,
			Lobbies:       activity.Lobbies,
			ActiveGames:   activity.ActiveGames,
		},
	})
}

// GeneralsX @bugfix OpenAI 06/08/2026 Keep failed request contexts out of the shared leaderboard cache.
func (p *publicHandler) loadLeaderboard(ctx context.Context) ([]publicLeaderboardRecord, error) {
	p.leaderboardMu.Lock()
	defer p.leaderboardMu.Unlock()
	now := p.now()
	if !p.leaderboardCachedAt.IsZero() && now.Sub(p.leaderboardCachedAt) < publicLeaderboardCacheTTL {
		return append([]publicLeaderboardRecord(nil), p.leaderboardCache...), nil
	}
	records, err := p.leaderboard.PublicLeaderboard(ctx)
	if err != nil {
		return nil, err
	}
	p.leaderboardCachedAt = p.now()
	p.leaderboardCache = append(p.leaderboardCache[:0], records...)
	return append([]publicLeaderboardRecord(nil), p.leaderboardCache...), nil
}

func (p *publicHandler) handleIndex(w http.ResponseWriter, r *http.Request) {
	content, err := fs.ReadFile(p.assets, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (p *publicHandler) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name != "generalsx-zh-icon.png" && !strings.HasPrefix(name, "assets/") {
		http.NotFound(w, r)
		return
	}
	cleaned := strings.TrimPrefix(path.Clean("/"+name), "/")
	if cleaned != name {
		http.NotFound(w, r)
		return
	}
	content, err := fs.ReadFile(p.assets, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func setPublicSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'; img-src 'self' data:; font-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
