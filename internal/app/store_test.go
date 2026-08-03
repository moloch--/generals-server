package app

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileStoreProfileLimitIsAtomic(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "profiles.db")
	store, err := OpenProfileStoreWithLimit(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	start := make(chan struct{})
	errs := make(chan error, 3)
	registrations := []struct {
		username string
		display  string
	}{
		{username: "limit_one", display: "Limit One"},
		{username: "limit_two", display: "Limit Two"},
		{username: "limit_three", display: "Limit Three"},
	}
	for _, registration := range registrations {
		registration := registration
		go func() {
			<-start
			_, registerErr := store.Register(registration.username, "correct horse", registration.display)
			errs <- registerErr
		}()
	}
	close(start)

	succeeded := 0
	limited := 0
	for range registrations {
		switch err := <-errs; {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrProfileLimit):
			limited++
		default:
			t.Fatalf("registration returned unexpected error: %v", err)
		}
	}
	if count := countProfiles(t, store); succeeded != 2 || limited != 1 || count != 2 {
		t.Fatalf("limit race: succeeded=%d limited=%d profiles=%d", succeeded, limited, count)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenProfileStoreWithLimit(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if count := countProfiles(t, reopened); count != 2 {
		t.Fatalf("persisted profile count = %d, want 2", count)
	}
	if _, err := OpenProfileStoreWithLimit(path, 1); err == nil {
		t.Fatal("store opened with a configured limit below its persisted profile count")
	}
	if _, err := OpenProfileStoreWithLimit(path, 0); err == nil {
		t.Fatal("zero profile limit was accepted")
	}
}

func TestDisplayNameRejectsRetailDelimiters(t *testing.T) {
	t.Parallel()
	store := openTestProfileStore(t, "")
	for index, displayName := range []string{"Comma,Name", "Colon:Name", `Slash\\Name`, "Semi;Name", "Equals=Name", "Unicode-玩家"} {
		username := fmt.Sprintf("unsafe_%d", index)
		if _, err := store.Register(username, "correct horse", displayName); err == nil {
			t.Errorf("unsafe display name %q was accepted", displayName)
		}
	}
	if _, err := store.Register("safe_name", "correct horse", "Safe Name-1.0"); err != nil {
		t.Fatalf("safe display name was rejected: %v", err)
	}
}

func TestProfileStorePersistsAuthBuddiesAndStats(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "profiles.db")
	store := openTestProfileStore(t, path)
	alice, err := store.Register("Alice_1", "correct horse", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.Register("bob-2", "battery staple", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register("charlie_3", "another password", "alice"); err == nil {
		t.Fatal("case-insensitive duplicate display name was accepted")
	}
	if _, err := store.UpdateDisplayName(bob.UserID, "ALICE"); err == nil {
		t.Fatal("display-name update impersonated another profile")
	}
	if _, err := store.Register("ALICE_1", "another password", "Duplicate"); err == nil {
		t.Fatal("case-insensitive duplicate username was accepted")
	}
	if _, err := store.Authenticate("alice_1", "wrong password"); err == nil {
		t.Fatal("invalid password was accepted")
	}
	authenticated, err := store.Authenticate("ALICE_1", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.UserID != alice.UserID {
		t.Fatalf("authenticated user id = %d, want %d", authenticated.UserID, alice.UserID)
	}
	if err := store.RequestBuddy(alice.UserID, bob.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptBuddy(bob.UserID, alice.UserID); err != nil {
		t.Fatal(err)
	}
	stats, err := store.ApplyStats(alice.UserID, PlayerStats{Wins: 1, Games: 1, Rating: 10})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Wins != 1 || stats.Games != 1 || stats.Rating != 10 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	updated, err := store.UpdateDisplayName(alice.UserID, "Alice Prime")
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "Alice Prime" {
		t.Fatalf("display name = %q", updated.DisplayName)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestProfileStore(t, path)
	persisted, err := reopened.Authenticate("alice_1", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.DisplayName != "Alice Prime" {
		t.Fatalf("persisted display name = %q", persisted.DisplayName)
	}
	if found, ok := reopened.Find("  ALICE PRIME "); !ok || found.UserID != alice.UserID {
		t.Fatalf("case-insensitive display-name lookup failed: %+v ok=%v", found, ok)
	}
	buddies, pending, ok := reopened.BuddyIDs(alice.UserID)
	if !ok || len(buddies) != 1 || buddies[0] != bob.UserID || len(pending) != 0 {
		t.Fatalf("persisted buddy state = buddies %v pending %v ok %v", buddies, pending, ok)
	}
	persistedStats, ok := reopened.Stats(alice.UserID)
	if !ok || persistedStats != stats {
		t.Fatalf("persisted stats = %+v, want %+v", persistedStats, stats)
	}
}

func TestProfileStoreVisibleRevisionTracksCommittedProfileMutations(t *testing.T) {
	store := openTestProfileStore(t, "")
	if got := store.VisibleRevision(); got != 0 {
		t.Fatalf("initial visible revision = %d, want 0", got)
	}

	profile, err := store.Register("revision_user", "original password", "Revision User")
	if err != nil {
		t.Fatal(err)
	}
	assertVisibleRevision(t, store, 1)
	originalStamp, exists, err := store.currentCredentialStamp(profile.UserID)
	if err != nil || !exists {
		t.Fatalf("read original credential stamp exists=%v error=%v", exists, err)
	}

	if _, err := store.Register("bad", "short", "Bad Password"); err == nil {
		t.Fatal("invalid registration password was accepted")
	}
	assertVisibleRevision(t, store, 1)

	if _, err := store.ApplyStatsBatch(map[uint64]PlayerStats{999999: {Wins: 1}}); err == nil {
		t.Fatal("stats update for an unknown profile succeeded")
	}
	assertVisibleRevision(t, store, 1)

	if _, err := store.ApplyStats(profile.UserID, PlayerStats{Wins: 1, Games: 1}); err != nil {
		t.Fatal(err)
	}
	assertVisibleRevision(t, store, 2)

	if _, err := store.UpdateDisplayName(profile.UserID, "Revision Prime"); err != nil {
		t.Fatal(err)
	}
	assertVisibleRevision(t, store, 3)
	nonAuthStamp, exists, err := store.currentCredentialStamp(profile.UserID)
	if err != nil || !exists || nonAuthStamp != originalStamp {
		t.Fatalf("non-auth mutations changed credential stamp exists=%v error=%v", exists, err)
	}

	if updated, err := store.ResetPassword(profile.UserID, "new secure password"); err != nil || !updated {
		t.Fatalf("ResetPassword() updated=%v error=%v", updated, err)
	}
	assertVisibleRevision(t, store, 4)
	resetStamp, exists, err := store.currentCredentialStamp(profile.UserID)
	if err != nil || !exists || resetStamp == originalStamp {
		t.Fatalf("password reset credential stamp exists=%v changed=%v error=%v", exists, resetStamp != originalStamp, err)
	}

	if updated, err := store.ResetPassword(999999, "another secure password"); err != nil || updated {
		t.Fatalf("missing ResetPassword() updated=%v error=%v", updated, err)
	}
	if _, err := store.ResetPassword(profile.UserID, "short"); err == nil {
		t.Fatal("invalid reset password was accepted")
	}
	assertVisibleRevision(t, store, 4)

	if deleted, err := store.DeleteProfile(999999); err != nil || deleted {
		t.Fatalf("missing DeleteProfile() deleted=%v error=%v", deleted, err)
	}
	assertVisibleRevision(t, store, 4)
	if deleted, err := store.DeleteProfile(profile.UserID); err != nil || !deleted {
		t.Fatalf("DeleteProfile() deleted=%v error=%v", deleted, err)
	}
	assertVisibleRevision(t, store, 5)
	if _, exists, err := store.currentCredentialStamp(profile.UserID); err != nil || exists {
		t.Fatalf("deleted credential stamp exists=%v error=%v", exists, err)
	}
}

func TestProfileStoreResetPasswordAndDeleteProfilePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.db")
	store := openTestProfileStore(t, path)
	alice, err := store.Register("admin_alice", "original password", "Admin Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.Register("admin_bob", "bob password", "Admin Bob")
	if err != nil {
		t.Fatal(err)
	}
	charlie, err := store.Register("admin_charlie", "charlie password", "Admin Charlie")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RequestBuddy(alice.UserID, bob.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptBuddy(bob.UserID, alice.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestBuddy(charlie.UserID, alice.UserID); err != nil {
		t.Fatal(err)
	}

	if updated, err := store.ResetPassword(alice.UserID, "replacement password"); err != nil || !updated {
		t.Fatalf("ResetPassword() updated=%v error=%v", updated, err)
	}
	if _, err := store.Authenticate("admin_alice", "original password"); err == nil {
		t.Fatal("original password remained valid after reset")
	}
	if _, err := store.Authenticate("admin_alice", "replacement password"); err != nil {
		t.Fatalf("replacement password was rejected: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestProfileStore(t, path)
	if _, err := reopened.Authenticate("admin_alice", "replacement password"); err != nil {
		t.Fatalf("replacement password did not persist: %v", err)
	}
	if deleted, err := reopened.DeleteProfile(alice.UserID); err != nil || !deleted {
		t.Fatalf("DeleteProfile() deleted=%v error=%v", deleted, err)
	}
	if _, ok := reopened.Get(alice.UserID); ok {
		t.Fatal("deleted profile remained readable")
	}
	if _, err := reopened.Authenticate("admin_alice", "replacement password"); err == nil {
		t.Fatal("deleted profile still authenticated")
	}
	if buddies, pending, ok := reopened.BuddyIDs(bob.UserID); !ok || len(buddies) != 0 || len(pending) != 0 {
		t.Fatalf("buddy cascade state = buddies %v pending %v ok %v", buddies, pending, ok)
	}
	var buddyRows, requestRows int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM buddies`).Scan(&buddyRows); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM buddy_requests`).Scan(&requestRows); err != nil {
		t.Fatal(err)
	}
	if buddyRows != 0 || requestRows != 0 {
		t.Fatalf("profile deletion left buddy rows=%d request rows=%d", buddyRows, requestRows)
	}
	if deleted, err := reopened.DeleteProfile(alice.UserID); err != nil || deleted {
		t.Fatalf("second DeleteProfile() deleted=%v error=%v", deleted, err)
	}
}

func TestProfileStoreApplyStatsBatchIsAtomic(t *testing.T) {
	store := openTestProfileStore(t, "")
	alice, err := store.Register("batch_alice", "correct horse", "Batch Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.Register("batch_bob", "battery staple", "Batch Bob")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.ApplyStatsBatch(map[uint64]PlayerStats{
		alice.UserID: {Wins: 1, Games: 1, Rating: 10},
		999999:       {Losses: 1, Games: 1},
	}); err == nil {
		t.Fatal("batch containing an unknown profile succeeded")
	}
	assertStats(t, store, alice.UserID, PlayerStats{})

	createFailureTrigger(t, store, "fail_stats_update", fmt.Sprintf(`
		CREATE TRIGGER fail_stats_update
		BEFORE UPDATE OF wins, losses, disconnects, games, rating ON profiles
		WHEN NEW.id = %d
		BEGIN SELECT RAISE(ABORT, 'forced stats failure'); END`, bob.UserID))
	if _, err := store.ApplyStatsBatch(map[uint64]PlayerStats{
		alice.UserID: {Wins: 1, Games: 1, Rating: 10},
		bob.UserID:   {Losses: 1, Games: 1, Rating: -10},
	}); err == nil {
		t.Fatal("batch succeeded when the second profile update failed")
	}
	assertStats(t, store, alice.UserID, PlayerStats{})
	assertStats(t, store, bob.UserID, PlayerStats{})
	if _, err := store.db.Exec(`DROP TRIGGER fail_stats_update`); err != nil {
		t.Fatal(err)
	}

	result, err := store.ApplyStatsBatch(map[uint64]PlayerStats{
		alice.UserID: {Wins: 1, Games: 1, Rating: 10},
		bob.UserID:   {Losses: 1, Games: 1, Rating: -10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result[alice.UserID].Wins != 1 || result[bob.UserID].Losses != 1 || result[bob.UserID].Rating != 0 {
		t.Fatalf("unexpected batch result: %+v", result)
	}
}

func TestProfileStoreAcceptBuddyRollsBackOnDatabaseFailure(t *testing.T) {
	store := openTestProfileStore(t, "")
	alice, _ := store.Register("accept_alice", "correct horse", "Accept Alice")
	bob, _ := store.Register("accept_bob", "battery staple", "Accept Bob")
	charlie, _ := store.Register("accept_charlie", "another password", "Accept Charlie")
	if err := store.RequestBuddy(alice.UserID, bob.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestBuddy(charlie.UserID, bob.UserID); err != nil {
		t.Fatal(err)
	}
	createFailureTrigger(t, store, "fail_buddy_insert", `
		CREATE TRIGGER fail_buddy_insert BEFORE INSERT ON buddies
		BEGIN SELECT RAISE(ABORT, 'forced buddy failure'); END`)
	if err := store.AcceptBuddy(bob.UserID, alice.UserID); err == nil {
		t.Fatal("buddy acceptance succeeded when the relationship insert failed")
	}

	buddies, pending, ok := store.BuddyIDs(bob.UserID)
	if !ok || len(buddies) != 0 || len(pending) != 2 || pending[0] != alice.UserID || pending[1] != charlie.UserID {
		t.Fatalf("failed acceptance corrupted state: buddies=%v pending=%v ok=%v", buddies, pending, ok)
	}
}

func TestProfileStoreRemoveBuddyRollsBackOnDatabaseFailure(t *testing.T) {
	store := openTestProfileStore(t, "")
	alice, _ := store.Register("remove_alice", "correct horse", "Remove Alice")
	bob, _ := store.Register("remove_bob", "battery staple", "Remove Bob")
	charlie, _ := store.Register("remove_charlie", "another password", "Remove Charlie")
	for _, buddy := range []Profile{bob, charlie} {
		if err := store.RequestBuddy(alice.UserID, buddy.UserID); err != nil {
			t.Fatal(err)
		}
		if err := store.AcceptBuddy(buddy.UserID, alice.UserID); err != nil {
			t.Fatal(err)
		}
	}
	createFailureTrigger(t, store, "fail_buddy_delete", `
		CREATE TRIGGER fail_buddy_delete BEFORE DELETE ON buddies
		BEGIN SELECT RAISE(ABORT, 'forced buddy failure'); END`)
	if err := store.RemoveBuddy(alice.UserID, bob.UserID); err == nil {
		t.Fatal("buddy removal succeeded when the relationship delete failed")
	}

	aliceBuddies, _, ok := store.BuddyIDs(alice.UserID)
	if !ok || len(aliceBuddies) != 2 || aliceBuddies[0] != bob.UserID || aliceBuddies[1] != charlie.UserID {
		t.Fatalf("failed removal corrupted Alice state: buddies=%v ok=%v", aliceBuddies, ok)
	}
	bobBuddies, _, _ := store.BuddyIDs(bob.UserID)
	if len(bobBuddies) != 1 || bobBuddies[0] != alice.UserID {
		t.Fatalf("failed removal corrupted Bob state: buddies=%v", bobBuddies)
	}
}

func TestProfileStoreInitializesSecureSQLiteDatabase(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "profiles.db")
	store := openTestProfileStore(t, path)

	var applicationID, version, foreignKeys int
	var journalMode, integrity string
	for query, destination := range map[string]any{
		`PRAGMA application_id`:  &applicationID,
		`PRAGMA user_version`:    &version,
		`PRAGMA foreign_keys`:    &foreignKeys,
		`PRAGMA journal_mode`:    &journalMode,
		`PRAGMA integrity_check`: &integrity,
	} {
		if err := store.db.QueryRow(query).Scan(destination); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if applicationID != profileDatabaseApplicationID || version != profileDatabaseSchemaVersion || foreignKeys != 1 {
		t.Fatalf("database metadata: application=%d version=%d foreign_keys=%d", applicationID, version, foreignKeys)
	}
	if journalMode != "wal" || integrity != "ok" {
		t.Fatalf("database state: journal=%q integrity=%q", journalMode, integrity)
	}
	if _, err := store.db.Exec(`INSERT INTO buddy_requests (requester_id, recipient_id) VALUES (100, 101)`); err == nil {
		t.Fatal("foreign-key constraint was not enforced")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database permissions = %o, want no group/other access", info.Mode().Perm())
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		info, err := os.Stat(sidecar)
		if err != nil {
			t.Fatalf("stat SQLite sidecar %s: %v", sidecar, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("SQLite sidecar %s permissions = %o, want no group/other access", sidecar, info.Mode().Perm())
		}
	}
}

func TestConcurrentProfileStoreInitializationDoesNotDeleteDatabase(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("concurrent-%d.db", iteration))
		start := make(chan struct{})
		type openResult struct {
			store *ProfileStore
			err   error
		}
		results := make(chan openResult, 2)
		for range 2 {
			go func() {
				<-start
				store, err := OpenProfileStore(path)
				results <- openResult{store: store, err: err}
			}()
		}
		close(start)
		var opened []*ProfileStore
		for range 2 {
			result := <-results
			if result.err != nil {
				for _, openedStore := range opened {
					_ = openedStore.Close()
				}
				t.Fatalf("iteration %d concurrent open failed: %v", iteration, result.err)
			}
			opened = append(opened, result.store)
		}
		if _, err := opened[0].Register("concurrent_user", "correct horse", "Concurrent User"); err != nil {
			t.Fatalf("iteration %d register after concurrent open: %v", iteration, err)
		}
		if _, ok := opened[1].Find("Concurrent User"); !ok {
			t.Fatalf("iteration %d second store did not observe committed profile", iteration)
		}
		for _, openedStore := range opened {
			if err := openedStore.Close(); err != nil {
				t.Fatalf("iteration %d close: %v", iteration, err)
			}
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("iteration %d database disappeared: %v", iteration, err)
		}
	}
}

func TestProfileStoreRejectsUnsupportedOrUnrelatedDatabase(t *testing.T) {
	t.Run("newer schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "future.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA application_id = %d`, profileDatabaseApplicationID)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenProfileStore(path); err == nil {
			t.Fatal("newer schema version was accepted")
		}
	})

	t.Run("unrelated sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "other.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA application_id = 1234`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenProfileStore(path); err == nil {
			t.Fatal("unrelated SQLite database was accepted")
		}
	})

	t.Run("unversioned database with user table", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unversioned.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE unrelated (value TEXT)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenProfileStore(path); err == nil {
			t.Fatal("non-empty unversioned SQLite database was adopted")
		}
	})

	t.Run("legacy json", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "profiles.db")
		original := []byte(`{"version":1,"profiles":[]}`)
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenProfileStore(path); err == nil {
			t.Fatal("JSON file was silently accepted as SQLite")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(original) {
			t.Fatal("failed open modified the existing file")
		}
	})
}

func TestProfileStoreCloseIsIdempotent(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Ping(); err == nil {
		t.Fatal("database remained open after Close")
	}
}

func openTestProfileStore(t *testing.T, path string) *ProfileStore {
	t.Helper()
	store, err := OpenProfileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close profile store: %v", err)
		}
	})
	return store
}

func countProfiles(t *testing.T, store *ProfileStore) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertStats(t *testing.T, store *ProfileStore, id uint64, want PlayerStats) {
	t.Helper()
	got, ok := store.Stats(id)
	if !ok || got != want {
		t.Fatalf("stats for %d = %+v ok=%v, want %+v", id, got, ok, want)
	}
}

func assertVisibleRevision(t *testing.T, store *ProfileStore, want uint64) {
	t.Helper()
	if got := store.VisibleRevision(); got != want {
		t.Fatalf("visible revision = %d, want %d", got, want)
	}
}

func createFailureTrigger(t *testing.T, store *ProfileStore, name, statement string) {
	t.Helper()
	if _, err := store.db.Exec(statement); err != nil {
		t.Fatalf("create trigger %s: %v", name, err)
	}
}
