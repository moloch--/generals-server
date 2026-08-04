package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
)

const (
	passwordIterations           = 120_000
	passwordKeyBytes             = 32
	maxBuddies                   = 100
	defaultMaxProfiles           = 10_000
	maxSupportedProfiles         = 100_000
	profileDatabaseSchemaVersion = 1
	profileDatabaseApplicationID = 0x47585052 // "GXPR"
	sqliteBusyTimeoutMillis      = 5_000
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,24}$`)
var displayNamePattern = regexp.MustCompile(`^[A-Za-z0-9._ -]{1,24}$`)
var ErrProfileLimit = errors.New("persistent profile limit reached")
var memoryDatabaseID atomic.Uint64

type ProfileStore struct {
	db              *sql.DB
	maxProfiles     int
	visibleRevision atomic.Uint64
	closeOnce       sync.Once
	closeErr        error
}

// GeneralsX @feature OpenAI 02/08/2026 Identify the exact stored credential without retaining plaintext passwords.
type credentialStamp [sha256.Size]byte

type storedAdminProfile struct {
	Profile Profile
	Stats   PlayerStats
}

type sqlRowScanner interface {
	Scan(dest ...any) error
}

type sqlRowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func OpenProfileStore(path string) (*ProfileStore, error) {
	return OpenProfileStoreWithLimit(path, defaultMaxProfiles)
}

func OpenProfileStoreWithLimit(path string, maxProfiles int) (*ProfileStore, error) {
	if maxProfiles < 1 || maxProfiles > maxSupportedProfiles {
		return nil, fmt.Errorf("profile limit must be between 1 and %d", maxSupportedProfiles)
	}

	dsn, databasePath, err := profileDatabaseDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open profile database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	cleanup := func(openErr error) (*ProfileStore, error) {
		closeErr := db.Close()
		return nil, errors.Join(openErr, closeErr)
	}
	if err := db.Ping(); err != nil {
		return cleanup(fmt.Errorf("connect to profile database: %w", err))
	}
	if err := initializeProfileSchema(db); err != nil {
		return cleanup(err)
	}
	if err := configureProfileDatabase(db, databasePath); err != nil {
		return cleanup(err)
	}

	var profileCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&profileCount); err != nil {
		return cleanup(fmt.Errorf("count profiles: %w", err))
	}
	if profileCount > maxProfiles {
		return cleanup(fmt.Errorf("profile database contains %d profiles, exceeding configured limit %d", profileCount, maxProfiles))
	}
	if databasePath != "" {
		if err := secureSQLiteFiles(databasePath); err != nil {
			return cleanup(fmt.Errorf("secure profile database: %w", err))
		}
	}

	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	return &ProfileStore{db: db, maxProfiles: maxProfiles}, nil
}

func profileDatabaseDSN(path string) (dsn, databasePath string, err error) {
	query := url.Values{}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMillis))
	query.Add("_pragma", "foreign_keys(1)")
	query.Set("_txlock", "immediate")

	if path == "" {
		query.Set("mode", "memory")
		query.Set("cache", "shared")
		name := fmt.Sprintf("generals-server-%d", memoryDatabaseID.Add(1))
		return "file:" + name + "?" + query.Encode(), "", nil
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve profile database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		return "", "", fmt.Errorf("create profile database directory: %w", err)
	}
	databaseFile, err := os.OpenFile(absolutePath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("create profile database: %w", err)
	}
	if err := databaseFile.Close(); err != nil {
		return "", "", fmt.Errorf("close profile database: %w", err)
	}
	if err := secureSQLiteFiles(absolutePath); err != nil {
		return "", "", fmt.Errorf("secure profile database: %w", err)
	}

	query.Add("_pragma", "synchronous(FULL)")
	databaseURLPath := filepath.ToSlash(absolutePath)
	// GeneralsX @bugfix Codex 04/08/2026 Encode Windows drive paths as
	// file:///C:/... rather than file://C:/..., which SQLite interprets as an
	// invalid URI authority.
	if filepath.VolumeName(absolutePath) != "" && !strings.HasPrefix(databaseURLPath, "/") {
		databaseURLPath = "/" + databaseURLPath
	}
	databaseURL := url.URL{
		Scheme:   "file",
		Path:     databaseURLPath,
		RawQuery: query.Encode(),
	}
	return databaseURL.String(), absolutePath, nil
}

func configureProfileDatabase(db *sql.DB, databasePath string) error {
	if databasePath == "" {
		return nil
	}
	deadline := time.Now().Add(time.Duration(sqliteBusyTimeoutMillis) * time.Millisecond)
	for {
		var journalMode string
		err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode)
		if err == nil {
			if !strings.EqualFold(journalMode, "wal") {
				return fmt.Errorf("enable profile database WAL: SQLite selected %q mode", journalMode)
			}
			return nil
		}
		if !isSQLiteBusy(err) || time.Now().After(deadline) {
			return fmt.Errorf("enable profile database WAL: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 5
}

func secureSQLiteFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func initializeProfileSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin profile database migration: %w", err)
	}
	defer tx.Rollback()

	var applicationID int
	if err := tx.QueryRow(`PRAGMA application_id`).Scan(&applicationID); err != nil {
		return fmt.Errorf("read profile database application id: %w", err)
	}
	if applicationID != 0 && applicationID != profileDatabaseApplicationID {
		return fmt.Errorf("profile database has unexpected application id %d", applicationID)
	}

	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read profile database schema version: %w", err)
	}
	if version > profileDatabaseSchemaVersion {
		return fmt.Errorf("profile database schema version %d is newer than supported version %d", version, profileDatabaseSchemaVersion)
	}
	if version == profileDatabaseSchemaVersion {
		if applicationID != profileDatabaseApplicationID {
			return errors.New("profile database schema is missing the GeneralsX application id")
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("finish profile database validation: %w", err)
		}
		return nil
	}

	if version < 1 {
		var existingObjects int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE name NOT LIKE 'sqlite_%' AND type IN ('table', 'index', 'view', 'trigger')`).Scan(&existingObjects); err != nil {
			return fmt.Errorf("inspect unversioned profile database: %w", err)
		}
		if existingObjects != 0 {
			return errors.New("refusing to initialize a non-empty unversioned SQLite database")
		}
		for _, statement := range profileSchemaVersion1 {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("create profile database schema: %w", err)
			}
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA application_id = %d`, profileDatabaseApplicationID)); err != nil {
			return fmt.Errorf("set profile database application id: %w", err)
		}
		if _, err := tx.Exec(`PRAGMA user_version = 1`); err != nil {
			return fmt.Errorf("set profile database schema version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit profile database migration: %w", err)
	}
	return nil
}

var profileSchemaVersion1 = []string{
	`CREATE TABLE profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL,
		username_key TEXT NOT NULL UNIQUE,
		display_name TEXT NOT NULL,
		display_name_key TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		password_salt BLOB NOT NULL,
		password_hash BLOB NOT NULL,
		password_iterations INTEGER NOT NULL CHECK (password_iterations > 0),
		wins INTEGER NOT NULL DEFAULT 0 CHECK (wins >= 0),
		losses INTEGER NOT NULL DEFAULT 0 CHECK (losses >= 0),
		disconnects INTEGER NOT NULL DEFAULT 0 CHECK (disconnects >= 0),
		games INTEGER NOT NULL DEFAULT 0 CHECK (games >= 0),
		rating INTEGER NOT NULL DEFAULT 0 CHECK (rating >= 0)
	) STRICT`,
	`CREATE TABLE buddies (
		user_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
		buddy_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
		PRIMARY KEY (user_id, buddy_id),
		CHECK (user_id < buddy_id)
	) WITHOUT ROWID, STRICT`,
	`CREATE INDEX buddies_by_buddy_id ON buddies(buddy_id)`,
	`CREATE TABLE buddy_requests (
		requester_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
		recipient_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
		PRIMARY KEY (requester_id, recipient_id),
		CHECK (requester_id != recipient_id)
	) WITHOUT ROWID, STRICT`,
	`CREATE INDEX buddy_requests_by_recipient_id ON buddy_requests(recipient_id)`,
}

func (s *ProfileStore) Close() error {
	s.closeOnce.Do(func() {
		if s.db != nil {
			s.closeErr = s.db.Close()
		}
	})
	return s.closeErr
}

// GeneralsX @feature OpenAI 02/08/2026 Track profile-table invalidations for realtime administrators.
// VisibleRevision changes after a committed profile mutation that may require
// an administrator's profile view to be refreshed. It is process-local and is
// intended only as an invalidation token, not as durable profile data.
func (s *ProfileStore) VisibleRevision() uint64 {
	return s.visibleRevision.Load()
}

func (s *ProfileStore) profileCount(ctx context.Context) (uint64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM profiles`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count profiles: %w", err)
	}
	if count < 0 {
		return 0, errors.New("profile database returned a negative profile count")
	}
	return uint64(count), nil
}

func (s *ProfileStore) listAdminProfiles(ctx context.Context, search string, limit, offset int) ([]storedAdminProfile, uint64, error) {
	if limit < 1 || limit > 100 {
		return nil, 0, errors.New("profile page limit must be between 1 and 100")
	}
	if offset < 0 || offset > maxSupportedProfiles {
		return nil, 0, fmt.Errorf("profile page offset must be between 0 and %d", maxSupportedProfiles)
	}

	search = strings.TrimSpace(search)
	predicate := ""
	var filterArgs []any
	if search != "" {
		pattern := "%" + escapeSQLiteLike(strings.ToLower(search)) + "%"
		predicate = ` WHERE username_key LIKE ? ESCAPE '\' OR display_name_key LIKE ? ESCAPE '\'`
		filterArgs = []any{pattern, pattern}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, fmt.Errorf("begin admin profile query: %w", err)
	}
	defer tx.Rollback()

	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profiles`+predicate, filterArgs...).Scan(&count); err != nil {
		return nil, 0, fmt.Errorf("count admin profiles: %w", err)
	}
	if count < 0 {
		return nil, 0, errors.New("profile database returned a negative profile count")
	}

	queryArgs := append(append([]any(nil), filterArgs...), limit, offset)
	rows, err := tx.QueryContext(ctx, `
		SELECT id, username, display_name, created_at,
		       wins, losses, disconnects, games, rating
		FROM profiles`+predicate+`
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list admin profiles: %w", err)
	}
	profiles := make([]storedAdminProfile, 0, limit)
	for rows.Next() {
		var record storedAdminProfile
		var id int64
		var createdAt string
		if err := rows.Scan(
			&id, &record.Profile.Username, &record.Profile.DisplayName, &createdAt,
			&record.Stats.Wins, &record.Stats.Losses, &record.Stats.Disconnects,
			&record.Stats.Games, &record.Stats.Rating,
		); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan admin profile: %w", err)
		}
		if id < 1 {
			rows.Close()
			return nil, 0, errors.New("profile database contains an invalid id")
		}
		created, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			rows.Close()
			return nil, 0, errors.New("profile database contains an invalid creation time")
		}
		record.Profile.UserID = uint64(id)
		record.Profile.CreatedAt = created
		profiles = append(profiles, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("iterate admin profiles: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close admin profile query: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("finish admin profile query: %w", err)
	}
	return profiles, uint64(count), nil
}

func escapeSQLiteLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func (s *ProfileStore) Register(username, password, displayName string) (Profile, error) {
	profile, _, err := s.registerWithCredentialStamp(username, password, displayName)
	return profile, err
}

func (s *ProfileStore) registerWithCredentialStamp(username, password, displayName string) (Profile, credentialStamp, error) {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if !usernamePattern.MatchString(username) {
		return Profile{}, credentialStamp{}, errors.New("username must be 3-24 letters, digits, dots, dashes, or underscores")
	}
	if err := validatePassword(password); err != nil {
		return Profile{}, credentialStamp{}, err
	}
	if err := validateDisplayName(displayName); err != nil {
		return Profile{}, credentialStamp{}, err
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Profile{}, credentialStamp{}, fmt.Errorf("generate password salt: %w", err)
	}
	hash := derivePassword([]byte(password), salt, passwordIterations, passwordKeyBytes)
	profile := Profile{
		Username:    username,
		DisplayName: displayName,
		CreatedAt:   time.Now().UTC(),
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Profile{}, credentialStamp{}, fmt.Errorf("begin profile registration: %w", err)
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&count); err != nil {
		return Profile{}, credentialStamp{}, fmt.Errorf("count profiles: %w", err)
	}
	if count >= s.maxProfiles {
		return Profile{}, credentialStamp{}, ErrProfileLimit
	}
	if exists, err := profileValueExists(tx, "username_key", normalizeUsername(username)); err != nil {
		return Profile{}, credentialStamp{}, err
	} else if exists {
		return Profile{}, credentialStamp{}, errors.New("username already exists")
	}
	if exists, err := profileValueExists(tx, "display_name_key", normalizeDisplayName(displayName)); err != nil {
		return Profile{}, credentialStamp{}, err
	} else if exists {
		return Profile{}, credentialStamp{}, errors.New("display name already exists")
	}

	result, err := tx.Exec(`
		INSERT INTO profiles (
			username, username_key, display_name, display_name_key, created_at,
			password_salt, password_hash, password_iterations
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		username, normalizeUsername(username), displayName, normalizeDisplayName(displayName),
		profile.CreatedAt.Format(time.RFC3339Nano), salt, hash, passwordIterations)
	if err != nil {
		return Profile{}, credentialStamp{}, fmt.Errorf("insert profile: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Profile{}, credentialStamp{}, fmt.Errorf("read profile id: %w", err)
	}
	if id < 1 {
		return Profile{}, credentialStamp{}, errors.New("profile database generated an invalid id")
	}
	profile.UserID = uint64(id)
	if err := tx.Commit(); err != nil {
		return Profile{}, credentialStamp{}, fmt.Errorf("commit profile registration: %w", err)
	}
	s.visibleRevision.Add(1)
	return profile, makeCredentialStamp(profile.UserID, salt, hash, passwordIterations), nil
}

func profileValueExists(tx *sql.Tx, column, value string) (bool, error) {
	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM profiles WHERE ` + column + ` = ?)`
	if err := tx.QueryRow(query, value).Scan(&exists); err != nil {
		return false, fmt.Errorf("check profile uniqueness: %w", err)
	}
	return exists, nil
}

func (s *ProfileStore) Authenticate(username, password string) (Profile, error) {
	profile, _, err := s.authenticateWithCredentialStamp(username, password)
	return profile, err
}

func (s *ProfileStore) authenticateWithCredentialStamp(username, password string) (Profile, credentialStamp, error) {
	row := s.db.QueryRow(`
		SELECT id, username, display_name, created_at, password_salt, password_hash, password_iterations
		FROM profiles WHERE username_key = ?`, normalizeUsername(username))
	var profile Profile
	var id int64
	var createdAt string
	var salt, want []byte
	var iterations int
	if err := row.Scan(&id, &profile.Username, &profile.DisplayName, &createdAt, &salt, &want, &iterations); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			dummySalt := make([]byte, 16)
			_ = derivePassword([]byte(password), dummySalt, passwordIterations, passwordKeyBytes)
			return Profile{}, credentialStamp{}, errors.New("invalid username or password")
		}
		return Profile{}, credentialStamp{}, fmt.Errorf("read credentials: %w", err)
	}
	if id < 1 || len(salt) != 16 || len(want) < 1 || iterations < 1 || iterations > 10_000_000 {
		return Profile{}, credentialStamp{}, errors.New("stored credentials are corrupt")
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Profile{}, credentialStamp{}, errors.New("stored profile is corrupt")
	}
	profile.UserID = uint64(id)
	profile.CreatedAt = created
	got := derivePassword([]byte(password), salt, iterations, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return Profile{}, credentialStamp{}, errors.New("invalid username or password")
	}
	return profile, makeCredentialStamp(profile.UserID, salt, want, iterations), nil
}

func (s *ProfileStore) currentCredentialStamp(id uint64) (credentialStamp, bool, error) {
	databaseID, ok := sqliteID(id)
	if !ok {
		return credentialStamp{}, false, nil
	}
	var salt, hash []byte
	var iterations int
	if err := s.db.QueryRow(`
		SELECT password_salt, password_hash, password_iterations
		FROM profiles WHERE id = ?`, databaseID).Scan(&salt, &hash, &iterations); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return credentialStamp{}, false, nil
		}
		return credentialStamp{}, false, fmt.Errorf("read current credentials: %w", err)
	}
	if len(salt) != 16 || len(hash) < 1 || iterations < 1 || iterations > 10_000_000 {
		return credentialStamp{}, false, errors.New("stored credentials are corrupt")
	}
	return makeCredentialStamp(id, salt, hash, iterations), true, nil
}

func makeCredentialStamp(id uint64, salt, hash []byte, iterations int) credentialStamp {
	var header [16]byte
	binary.BigEndian.PutUint64(header[:8], id)
	binary.BigEndian.PutUint64(header[8:], uint64(iterations))
	digest := sha256.New()
	_, _ = digest.Write(header[:])
	_, _ = digest.Write(salt)
	_, _ = digest.Write(hash)
	var stamp credentialStamp
	copy(stamp[:], digest.Sum(nil))
	return stamp
}

// GeneralsX @feature OpenAI 02/08/2026 Let authenticated administrators reset persistent credentials.
func (s *ProfileStore) ResetPassword(id uint64, password string) (bool, error) {
	if err := validatePassword(password); err != nil {
		return false, err
	}
	databaseID, ok := sqliteID(id)
	if !ok {
		return false, nil
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return false, fmt.Errorf("generate password salt: %w", err)
	}
	hash := derivePassword([]byte(password), salt, passwordIterations, passwordKeyBytes)
	result, err := s.db.Exec(`
		UPDATE profiles
		SET password_salt = ?, password_hash = ?, password_iterations = ?
		WHERE id = ?`, salt, hash, passwordIterations, databaseID)
	if err != nil {
		return false, fmt.Errorf("reset profile password: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect profile password reset: %w", err)
	}
	if updated == 0 {
		return false, nil
	}
	if updated != 1 {
		return false, fmt.Errorf("profile password reset affected %d rows", updated)
	}
	s.visibleRevision.Add(1)
	return true, nil
}

// GeneralsX @feature OpenAI 02/08/2026 Let authenticated administrators delete persistent accounts atomically.
func (s *ProfileStore) DeleteProfile(id uint64) (bool, error) {
	databaseID, ok := sqliteID(id)
	if !ok {
		return false, nil
	}
	result, err := s.db.Exec(`DELETE FROM profiles WHERE id = ?`, databaseID)
	if err != nil {
		return false, fmt.Errorf("delete profile: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect profile deletion: %w", err)
	}
	if deleted == 0 {
		return false, nil
	}
	if deleted != 1 {
		return false, fmt.Errorf("profile deletion affected %d rows", deleted)
	}
	s.visibleRevision.Add(1)
	return true, nil
}

func (s *ProfileStore) Get(id uint64) (Profile, bool) {
	databaseID, ok := sqliteID(id)
	if !ok {
		return Profile{}, false
	}
	profile, err := queryProfile(s.db, `WHERE id = ?`, databaseID)
	return profile, err == nil
}

func (s *ProfileStore) Find(displayName string) (Profile, bool) {
	profile, err := queryProfile(s.db, `WHERE display_name_key = ?`, normalizeDisplayName(displayName))
	return profile, err == nil
}

func queryProfile(queryer sqlRowQuerier, predicate string, args ...any) (Profile, error) {
	row := queryer.QueryRow(`
		SELECT id, username, display_name, created_at
		FROM profiles `+predicate, args...)
	return scanProfile(row)
}

func scanProfile(row sqlRowScanner) (Profile, error) {
	var profile Profile
	var id int64
	var createdAt string
	if err := row.Scan(&id, &profile.Username, &profile.DisplayName, &createdAt); err != nil {
		return Profile{}, err
	}
	if id < 1 {
		return Profile{}, errors.New("stored profile has invalid id")
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Profile{}, errors.New("stored profile has invalid creation time")
	}
	profile.UserID = uint64(id)
	profile.CreatedAt = created
	return profile, nil
}

func (s *ProfileStore) Stats(id uint64) (PlayerStats, bool) {
	databaseID, ok := sqliteID(id)
	if !ok {
		return PlayerStats{}, false
	}
	stats, err := queryStats(s.db.QueryRow(`
		SELECT wins, losses, disconnects, games, rating
		FROM profiles WHERE id = ?`, databaseID))
	return stats, err == nil
}

func queryStats(row sqlRowScanner) (PlayerStats, error) {
	var stats PlayerStats
	if err := row.Scan(&stats.Wins, &stats.Losses, &stats.Disconnects, &stats.Games, &stats.Rating); err != nil {
		return PlayerStats{}, err
	}
	return stats, nil
}

func (s *ProfileStore) ApplyStats(id uint64, update PlayerStats) (PlayerStats, error) {
	stats, err := s.ApplyStatsBatch(map[uint64]PlayerStats{id: update})
	if err != nil {
		return PlayerStats{}, err
	}
	return stats[id], nil
}

func (s *ProfileStore) ApplyStatsBatch(updates map[uint64]PlayerStats) (map[uint64]PlayerStats, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin stats update: %w", err)
	}
	defer tx.Rollback()

	ids := make([]uint64, 0, len(updates))
	for id := range updates {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := make(map[uint64]PlayerStats, len(updates))
	for _, id := range ids {
		databaseID, ok := sqliteID(id)
		if !ok {
			return nil, fmt.Errorf("profile %d not found", id)
		}
		current, err := queryStats(tx.QueryRow(`
			SELECT wins, losses, disconnects, games, rating
			FROM profiles WHERE id = ?`, databaseID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("profile %d not found", id)
		}
		if err != nil {
			return nil, fmt.Errorf("read profile %d stats: %w", id, err)
		}
		next, err := addStats(current, updates[id])
		if err != nil {
			return nil, fmt.Errorf("update profile %d stats: %w", id, err)
		}
		if _, err := tx.Exec(`
			UPDATE profiles
			SET wins = ?, losses = ?, disconnects = ?, games = ?, rating = ?
			WHERE id = ?`, next.Wins, next.Losses, next.Disconnects, next.Games, next.Rating, databaseID); err != nil {
			return nil, fmt.Errorf("write profile %d stats: %w", id, err)
		}
		result[id] = next
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit stats update: %w", err)
	}
	if len(updates) != 0 {
		s.visibleRevision.Add(1)
	}
	return result, nil
}

func addStats(current, update PlayerStats) (PlayerStats, error) {
	var err error
	if current.Wins, err = addSQLiteCounter(current.Wins, update.Wins); err != nil {
		return PlayerStats{}, fmt.Errorf("wins: %w", err)
	}
	if current.Losses, err = addSQLiteCounter(current.Losses, update.Losses); err != nil {
		return PlayerStats{}, fmt.Errorf("losses: %w", err)
	}
	if current.Disconnects, err = addSQLiteCounter(current.Disconnects, update.Disconnects); err != nil {
		return PlayerStats{}, fmt.Errorf("disconnects: %w", err)
	}
	if current.Games, err = addSQLiteCounter(current.Games, update.Games); err != nil {
		return PlayerStats{}, fmt.Errorf("games: %w", err)
	}
	if update.Rating > 0 && current.Rating > math.MaxInt64-update.Rating {
		return PlayerStats{}, errors.New("rating overflow")
	}
	if update.Rating < 0 && current.Rating < math.MinInt64-update.Rating {
		current.Rating = 0
	} else {
		current.Rating += update.Rating
		if current.Rating < 0 {
			current.Rating = 0
		}
	}
	return current, nil
}

func addSQLiteCounter(current, update uint64) (uint64, error) {
	if current > math.MaxInt64 || update > math.MaxInt64 || current > uint64(math.MaxInt64)-update {
		return 0, errors.New("counter overflow")
	}
	return current + update, nil
}

func (s *ProfileStore) BuddyIDs(id uint64) (buddies, pending []uint64, ok bool) {
	databaseID, valid := sqliteID(id)
	if !valid {
		return nil, nil, false
	}
	exists, err := profileExists(s.db, databaseID)
	if err != nil || !exists {
		return nil, nil, false
	}
	buddies, err = queryIDs(s.db, `
		SELECT CASE WHEN user_id = ? THEN buddy_id ELSE user_id END AS other_id
		FROM buddies
		WHERE user_id = ? OR buddy_id = ?
		ORDER BY other_id`, databaseID, databaseID, databaseID)
	if err != nil {
		return nil, nil, false
	}
	pending, err = queryIDs(s.db, `
		SELECT requester_id FROM buddy_requests
		WHERE recipient_id = ? ORDER BY requester_id`, databaseID)
	if err != nil {
		return nil, nil, false
	}
	return buddies, pending, true
}

type sqlQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func queryIDs(queryer sqlQueryer, query string, args ...any) ([]uint64, error) {
	rows, err := queryer.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id < 1 {
			return nil, errors.New("profile database contains an invalid id")
		}
		ids = append(ids, uint64(id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *ProfileStore) RequestBuddy(from, target uint64) error {
	if from == target {
		return errors.New("cannot add yourself as a buddy")
	}
	fromID, fromOK := sqliteID(from)
	targetID, targetOK := sqliteID(target)
	if !fromOK || !targetOK {
		return errors.New("profile not found")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin buddy request: %w", err)
	}
	defer tx.Rollback()
	if err := requireProfiles(tx, fromID, targetID); err != nil {
		return err
	}
	first, second := orderedPair(fromID, targetID)
	var buddies bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM buddies WHERE user_id = ? AND buddy_id = ?)`, first, second).Scan(&buddies); err != nil {
		return fmt.Errorf("check buddy relationship: %w", err)
	}
	if buddies {
		return errors.New("player is already a buddy")
	}

	senderBuddies, err := buddyCount(tx, fromID)
	if err != nil {
		return err
	}
	receiverBuddies, err := buddyCount(tx, targetID)
	if err != nil {
		return err
	}
	var pendingCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM buddy_requests WHERE recipient_id = ?`, targetID).Scan(&pendingCount); err != nil {
		return fmt.Errorf("count pending buddy requests: %w", err)
	}
	if senderBuddies >= maxBuddies || receiverBuddies >= maxBuddies || pendingCount >= maxBuddies {
		return errors.New("buddy list limit reached")
	}

	var pending bool
	if err := tx.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM buddy_requests WHERE requester_id = ? AND recipient_id = ?)`,
		fromID, targetID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending buddy request: %w", err)
	}
	if pending {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO buddy_requests (requester_id, recipient_id) VALUES (?, ?)`, fromID, targetID); err != nil {
		return fmt.Errorf("insert buddy request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit buddy request: %w", err)
	}
	return nil
}

func (s *ProfileStore) AcceptBuddy(user, requester uint64) error {
	if user == requester {
		return errors.New("cannot accept yourself as a buddy")
	}
	userID, userOK := sqliteID(user)
	requesterID, requesterOK := sqliteID(requester)
	if !userOK || !requesterOK {
		return errors.New("profile not found")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin buddy acceptance: %w", err)
	}
	defer tx.Rollback()
	if err := requireProfiles(tx, userID, requesterID); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM buddy_requests WHERE requester_id = ? AND recipient_id = ?`, requesterID, userID)
	if err != nil {
		return fmt.Errorf("remove buddy request: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect buddy request removal: %w", err)
	}
	if removed == 0 {
		return errors.New("buddy request not found")
	}
	first, second := orderedPair(userID, requesterID)
	if _, err := tx.Exec(`INSERT OR IGNORE INTO buddies (user_id, buddy_id) VALUES (?, ?)`, first, second); err != nil {
		return fmt.Errorf("insert buddy relationship: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit buddy acceptance: %w", err)
	}
	return nil
}

func (s *ProfileStore) RemoveBuddy(user, buddy uint64) error {
	userID, userOK := sqliteID(user)
	buddyID, buddyOK := sqliteID(buddy)
	if !userOK || !buddyOK {
		return errors.New("profile not found")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin buddy removal: %w", err)
	}
	defer tx.Rollback()
	if err := requireProfiles(tx, userID, buddyID); err != nil {
		return err
	}
	first, second := orderedPair(userID, buddyID)
	if _, err := tx.Exec(`DELETE FROM buddies WHERE user_id = ? AND buddy_id = ?`, first, second); err != nil {
		return fmt.Errorf("remove buddy relationship: %w", err)
	}
	if _, err := tx.Exec(`
		DELETE FROM buddy_requests
		WHERE (requester_id = ? AND recipient_id = ?)
		   OR (requester_id = ? AND recipient_id = ?)`, userID, buddyID, buddyID, userID); err != nil {
		return fmt.Errorf("remove pending buddy requests: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit buddy removal: %w", err)
	}
	return nil
}

func (s *ProfileStore) UpdateDisplayName(id uint64, displayName string) (Profile, error) {
	displayName = strings.TrimSpace(displayName)
	if err := validateDisplayName(displayName); err != nil {
		return Profile{}, err
	}
	databaseID, ok := sqliteID(id)
	if !ok {
		return Profile{}, errors.New("profile not found")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Profile{}, fmt.Errorf("begin display name update: %w", err)
	}
	defer tx.Rollback()
	profile, err := queryProfile(tx, `WHERE id = ?`, databaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, errors.New("profile not found")
	}
	if err != nil {
		return Profile{}, fmt.Errorf("read profile: %w", err)
	}
	var ownerID int64
	err = tx.QueryRow(`SELECT id FROM profiles WHERE display_name_key = ?`, normalizeDisplayName(displayName)).Scan(&ownerID)
	switch {
	case err == nil && ownerID != databaseID:
		return Profile{}, errors.New("display name already exists")
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return Profile{}, fmt.Errorf("check display name: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE profiles SET display_name = ?, display_name_key = ? WHERE id = ?`,
		displayName, normalizeDisplayName(displayName), databaseID); err != nil {
		return Profile{}, fmt.Errorf("update display name: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Profile{}, fmt.Errorf("commit display name update: %w", err)
	}
	profile.DisplayName = displayName
	s.visibleRevision.Add(1)
	return profile, nil
}

func profileExists(queryer sqlRowQuerier, id int64) (bool, error) {
	var exists bool
	if err := queryer.QueryRow(`SELECT EXISTS(SELECT 1 FROM profiles WHERE id = ?)`, id).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func requireProfiles(tx *sql.Tx, ids ...int64) error {
	for _, id := range ids {
		exists, err := profileExists(tx, id)
		if err != nil {
			return fmt.Errorf("check profile: %w", err)
		}
		if !exists {
			return errors.New("profile not found")
		}
	}
	return nil
}

func buddyCount(tx *sql.Tx, id int64) (int, error) {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM buddies WHERE user_id = ? OR buddy_id = ?`, id, id).Scan(&count); err != nil {
		return 0, fmt.Errorf("count buddies: %w", err)
	}
	return count, nil
}

func orderedPair(first, second int64) (int64, int64) {
	if first > second {
		return second, first
	}
	return first, second
}

func sqliteID(id uint64) (int64, bool) {
	if id < 1 || id > math.MaxInt64 {
		return 0, false
	}
	return int64(id), true
}

func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

func normalizeDisplayName(displayName string) string {
	return strings.ToLower(strings.TrimSpace(displayName))
}

func validateDisplayName(name string) error {
	if !displayNamePattern.MatchString(name) {
		return errors.New("display name must be 1-24 ASCII letters, digits, spaces, dots, dashes, or underscores")
	}
	return nil
}

func validatePassword(password string) error {
	if len(password) < 8 || len(password) > 128 {
		return errors.New("password must be 8-128 bytes")
	}
	return nil
}

// derivePassword implements PBKDF2-HMAC-SHA256. The iteration count is stored
// with each profile so future schema versions can raise the cost safely.
func derivePassword(password, salt []byte, iterations, keyLen int) []byte {
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	for block := 1; block <= blocks; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		var counter [4]byte
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		mac.Write(counter[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
