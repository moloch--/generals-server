package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	passwordIterations   = 120_000
	passwordKeyBytes     = 32
	maxBuddies           = 100
	defaultMaxProfiles   = 10_000
	maxSupportedProfiles = 100_000
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,24}$`)
var displayNamePattern = regexp.MustCompile(`^[A-Za-z0-9._ -]{1,24}$`)
var ErrProfileLimit = errors.New("persistent profile limit reached")

type storedProfile struct {
	Profile
	PasswordSalt string      `json:"password_salt"`
	PasswordHash string      `json:"password_hash"`
	Stats        PlayerStats `json:"stats"`
	Buddies      []uint64    `json:"buddies,omitempty"`
	Pending      []uint64    `json:"pending_buddy_requests,omitempty"`
}

type profileDatabase struct {
	Version  int             `json:"version"`
	NextID   uint64          `json:"next_id"`
	Profiles []storedProfile `json:"profiles"`
}

type ProfileStore struct {
	mu          sync.RWMutex
	path        string
	maxProfiles int
	nextID      uint64
	byID        map[uint64]storedProfile
	byName      map[string]uint64
	byDisplay   map[string]uint64
}

func OpenProfileStore(path string) (*ProfileStore, error) {
	return OpenProfileStoreWithLimit(path, defaultMaxProfiles)
}

func OpenProfileStoreWithLimit(path string, maxProfiles int) (*ProfileStore, error) {
	if maxProfiles < 1 || maxProfiles > maxSupportedProfiles {
		return nil, fmt.Errorf("profile limit must be between 1 and %d", maxSupportedProfiles)
	}
	s := &ProfileStore{
		path:        path,
		maxProfiles: maxProfiles,
		nextID:      1,
		byID:        make(map[uint64]storedProfile),
		byName:      make(map[string]uint64),
		byDisplay:   make(map[string]uint64),
	}
	if path == "" {
		return s, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read profile database: %w", err)
	}

	var db profileDatabase
	if err := json.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("decode profile database: %w", err)
	}
	if db.Version != 1 {
		return nil, fmt.Errorf("unsupported profile database version %d", db.Version)
	}
	if len(db.Profiles) > s.maxProfiles {
		return nil, fmt.Errorf("profile database contains %d profiles, exceeding configured limit %d", len(db.Profiles), s.maxProfiles)
	}
	if db.NextID > 0 {
		s.nextID = db.NextID
	}
	for _, p := range db.Profiles {
		name := normalizeUsername(p.Username)
		if p.UserID == 0 || !usernamePattern.MatchString(p.Username) {
			return nil, errors.New("profile database contains an invalid profile")
		}
		if _, exists := s.byID[p.UserID]; exists {
			return nil, fmt.Errorf("duplicate profile id %d", p.UserID)
		}
		if _, exists := s.byName[name]; exists {
			return nil, fmt.Errorf("duplicate username %q", p.Username)
		}
		if err := validateDisplayName(p.DisplayName); err != nil {
			return nil, fmt.Errorf("profile %d has an invalid display name", p.UserID)
		}
		display := normalizeDisplayName(p.DisplayName)
		if _, exists := s.byDisplay[display]; exists {
			return nil, fmt.Errorf("duplicate display name %q", p.DisplayName)
		}
		s.byID[p.UserID] = p
		s.byName[name] = p.UserID
		s.byDisplay[display] = p.UserID
		if p.UserID >= s.nextID {
			s.nextID = p.UserID + 1
		}
	}
	return s, nil
}

func (s *ProfileStore) Register(username, password, displayName string) (Profile, error) {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if !usernamePattern.MatchString(username) {
		return Profile{}, errors.New("username must be 3-24 letters, digits, dots, dashes, or underscores")
	}
	if err := validatePassword(password); err != nil {
		return Profile{}, err
	}
	if err := validateDisplayName(displayName); err != nil {
		return Profile{}, err
	}
	key := normalizeUsername(username)
	displayKey := normalizeDisplayName(displayName)
	s.mu.RLock()
	if len(s.byID) >= s.maxProfiles {
		s.mu.RUnlock()
		return Profile{}, ErrProfileLimit
	}
	if _, exists := s.byName[key]; exists {
		s.mu.RUnlock()
		return Profile{}, errors.New("username already exists")
	}
	if _, exists := s.byDisplay[displayKey]; exists {
		s.mu.RUnlock()
		return Profile{}, errors.New("display name already exists")
	}
	s.mu.RUnlock()

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Profile{}, fmt.Errorf("generate password salt: %w", err)
	}
	hash := derivePassword([]byte(password), salt, passwordIterations, passwordKeyBytes)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.byID) >= s.maxProfiles {
		return Profile{}, ErrProfileLimit
	}
	if _, exists := s.byName[key]; exists {
		return Profile{}, errors.New("username already exists")
	}
	if _, exists := s.byDisplay[displayKey]; exists {
		return Profile{}, errors.New("display name already exists")
	}
	p := storedProfile{
		Profile: Profile{
			UserID:      s.nextID,
			Username:    username,
			DisplayName: displayName,
			CreatedAt:   time.Now().UTC(),
		},
		PasswordSalt: base64.RawStdEncoding.EncodeToString(salt),
		PasswordHash: base64.RawStdEncoding.EncodeToString(hash),
	}
	s.nextID++
	s.byID[p.UserID] = p
	s.byName[key] = p.UserID
	s.byDisplay[displayKey] = p.UserID
	if err := s.saveLocked(); err != nil {
		delete(s.byID, p.UserID)
		delete(s.byName, key)
		delete(s.byDisplay, displayKey)
		s.nextID--
		return Profile{}, err
	}
	return p.Profile, nil
}

func (s *ProfileStore) Authenticate(username, password string) (Profile, error) {
	s.mu.RLock()
	id, ok := s.byName[normalizeUsername(username)]
	p := s.byID[id]
	s.mu.RUnlock()
	if !ok {
		// Do comparable work for unknown accounts to reduce username timing leaks.
		dummySalt := make([]byte, 16)
		_ = derivePassword([]byte(password), dummySalt, passwordIterations, passwordKeyBytes)
		return Profile{}, errors.New("invalid username or password")
	}
	salt, err := base64.RawStdEncoding.DecodeString(p.PasswordSalt)
	if err != nil {
		return Profile{}, errors.New("stored credentials are corrupt")
	}
	want, err := base64.RawStdEncoding.DecodeString(p.PasswordHash)
	if err != nil {
		return Profile{}, errors.New("stored credentials are corrupt")
	}
	got := derivePassword([]byte(password), salt, passwordIterations, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return Profile{}, errors.New("invalid username or password")
	}
	return p.Profile, nil
}

func (s *ProfileStore) Get(id uint64) (Profile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	return p.Profile, ok
}

func (s *ProfileStore) Find(displayName string) (Profile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byDisplay[normalizeDisplayName(displayName)]
	p := s.byID[id]
	return p.Profile, ok
}

func (s *ProfileStore) Stats(id uint64) (PlayerStats, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	return p.Stats, ok
}

func (s *ProfileStore) ApplyStats(id uint64, update PlayerStats) (PlayerStats, error) {
	stats, err := s.ApplyStatsBatch(map[uint64]PlayerStats{id: update})
	if err != nil {
		return PlayerStats{}, err
	}
	return stats[id], nil
}

func (s *ProfileStore) ApplyStatsBatch(updates map[uint64]PlayerStats) (map[uint64]PlayerStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := make(map[uint64]storedProfile, len(updates))
	for id := range updates {
		p, ok := s.byID[id]
		if !ok {
			return nil, fmt.Errorf("profile %d not found", id)
		}
		old[id] = p
	}
	result := make(map[uint64]PlayerStats, len(updates))
	for id, update := range updates {
		p := s.byID[id]
		p.Stats.Wins += update.Wins
		p.Stats.Losses += update.Losses
		p.Stats.Disconnects += update.Disconnects
		p.Stats.Games += update.Games
		p.Stats.Rating += update.Rating
		if p.Stats.Rating < 0 {
			p.Stats.Rating = 0
		}
		s.byID[id] = p
		result[id] = p.Stats
	}
	if err := s.saveLocked(); err != nil {
		for id, profile := range old {
			s.byID[id] = profile
		}
		return nil, err
	}
	return result, nil
}

func (s *ProfileStore) BuddyIDs(id uint64) (buddies, pending []uint64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	if !ok {
		return nil, nil, false
	}
	return append([]uint64(nil), p.Buddies...), append([]uint64(nil), p.Pending...), true
}

func (s *ProfileStore) RequestBuddy(from, target uint64) error {
	if from == target {
		return errors.New("cannot add yourself as a buddy")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sender, senderOK := s.byID[from]
	receiver, receiverOK := s.byID[target]
	if !senderOK || !receiverOK {
		return errors.New("profile not found")
	}
	if containsID(sender.Buddies, target) {
		return errors.New("player is already a buddy")
	}
	if len(sender.Buddies) >= maxBuddies || len(receiver.Buddies) >= maxBuddies || len(receiver.Pending) >= maxBuddies {
		return errors.New("buddy list limit reached")
	}
	if containsID(receiver.Pending, from) {
		return nil
	}
	receiver.Pending = append(receiver.Pending, from)
	s.byID[target] = receiver
	if err := s.saveLocked(); err != nil {
		receiver.Pending = receiver.Pending[:len(receiver.Pending)-1]
		s.byID[target] = receiver
		return err
	}
	return nil
}

func (s *ProfileStore) AcceptBuddy(user, requester uint64) error {
	if user == requester {
		return errors.New("cannot accept yourself as a buddy")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, userOK := s.byID[user]
	r, requesterOK := s.byID[requester]
	if !userOK || !requesterOK {
		return errors.New("profile not found")
	}
	if !containsID(u.Pending, requester) {
		return errors.New("buddy request not found")
	}
	oldU, oldR := u, r
	u.Pending = removeID(u.Pending, requester)
	if !containsID(u.Buddies, requester) {
		u.Buddies = append(u.Buddies, requester)
	}
	if !containsID(r.Buddies, user) {
		r.Buddies = append(r.Buddies, user)
	}
	s.byID[user], s.byID[requester] = u, r
	if err := s.saveLocked(); err != nil {
		s.byID[user], s.byID[requester] = oldU, oldR
		return err
	}
	return nil
}

func (s *ProfileStore) RemoveBuddy(user, buddy uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, userOK := s.byID[user]
	b, buddyOK := s.byID[buddy]
	if !userOK || !buddyOK {
		return errors.New("profile not found")
	}
	oldU, oldB := u, b
	u.Buddies = removeID(u.Buddies, buddy)
	u.Pending = removeID(u.Pending, buddy)
	b.Buddies = removeID(b.Buddies, user)
	b.Pending = removeID(b.Pending, user)
	s.byID[user], s.byID[buddy] = u, b
	if err := s.saveLocked(); err != nil {
		s.byID[user], s.byID[buddy] = oldU, oldB
		return err
	}
	return nil
}

func (s *ProfileStore) UpdateDisplayName(id uint64, displayName string) (Profile, error) {
	displayName = strings.TrimSpace(displayName)
	if err := validateDisplayName(displayName); err != nil {
		return Profile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return Profile{}, errors.New("profile not found")
	}
	old := p
	newKey := normalizeDisplayName(displayName)
	if owner, exists := s.byDisplay[newKey]; exists && owner != id {
		return Profile{}, errors.New("display name already exists")
	}
	oldKey := normalizeDisplayName(p.DisplayName)
	p.DisplayName = displayName
	s.byID[id] = p
	delete(s.byDisplay, oldKey)
	s.byDisplay[newKey] = id
	if err := s.saveLocked(); err != nil {
		s.byID[id] = old
		delete(s.byDisplay, newKey)
		s.byDisplay[oldKey] = id
		return Profile{}, err
	}
	return p.Profile, nil
}

func (s *ProfileStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	db := profileDatabase{Version: 1, NextID: s.nextID, Profiles: make([]storedProfile, 0, len(s.byID))}
	for _, p := range s.byID {
		sort.Slice(p.Buddies, func(i, j int) bool { return p.Buddies[i] < p.Buddies[j] })
		sort.Slice(p.Pending, func(i, j int) bool { return p.Pending[i] < p.Pending[j] })
		db.Profiles = append(db.Profiles, p)
	}
	sort.Slice(db.Profiles, func(i, j int) bool { return db.Profiles[i].UserID < db.Profiles[j].UserID })
	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile database: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create profile database directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".profiles-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary profile database: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("publish profile database: %w", err)
	}
	return nil
}

func containsID(values []uint64, id uint64) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
}

func removeID(values []uint64, id uint64) []uint64 {
	for i, value := range values {
		if value == id {
			result := make([]uint64, 0, len(values)-1)
			result = append(result, values[:i]...)
			result = append(result, values[i+1:]...)
			return result
		}
	}
	return values
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

// derivePassword implements PBKDF2-HMAC-SHA256 so the server has no non-standard
// runtime dependencies. Existing database versions record their iteration count
// implicitly; changing it requires a database format migration.
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
