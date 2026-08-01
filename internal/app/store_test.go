package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileStoreProfileLimitIsAtomic(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "profiles.json")
	store, err := OpenProfileStoreWithLimit(path, 2)
	if err != nil {
		t.Fatal(err)
	}

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
	if succeeded != 2 || limited != 1 || len(store.byID) != 2 {
		t.Fatalf("limit race: succeeded=%d limited=%d profiles=%d", succeeded, limited, len(store.byID))
	}

	reopened, err := OpenProfileStoreWithLimit(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.byID) != 2 {
		t.Fatalf("persisted profile count = %d, want 2", len(reopened.byID))
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
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
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
	path := filepath.Join(t.TempDir(), "profiles.json")
	store, err := OpenProfileStore(path)
	if err != nil {
		t.Fatal(err)
	}
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

	reopened, err := OpenProfileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Authenticate("alice_1", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.DisplayName != "Alice Prime" {
		t.Fatalf("persisted display name = %q", persisted.DisplayName)
	}
	if found, ok := reopened.Find("  ALICE PRIME "); !ok || found.UserID != alice.UserID {
		t.Fatalf("case-insensitive display-name index failed: %+v ok=%v", found, ok)
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

func TestProfileStoreApplyStatsBatchIsAtomic(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
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
	if stats, _ := store.Stats(alice.UserID); stats != (PlayerStats{}) {
		t.Fatalf("failed validation mutated Alice stats: %+v", stats)
	}

	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blockedParent, "profiles.json")
	if _, err := store.ApplyStatsBatch(map[uint64]PlayerStats{
		alice.UserID: {Wins: 1, Games: 1, Rating: 10},
		bob.UserID:   {Losses: 1, Games: 1, Rating: -10},
	}); err == nil {
		t.Fatal("batch succeeded when persistence path was unwritable")
	}
	if stats, _ := store.Stats(alice.UserID); stats != (PlayerStats{}) {
		t.Fatalf("failed save mutated Alice stats: %+v", stats)
	}
	if stats, _ := store.Stats(bob.UserID); stats != (PlayerStats{}) {
		t.Fatalf("failed save mutated Bob stats: %+v", stats)
	}

	path := filepath.Join(t.TempDir(), "profiles.json")
	store.path = path
	result, err := store.ApplyStatsBatch(map[uint64]PlayerStats{
		alice.UserID: {Wins: 1, Games: 1, Rating: 10},
		bob.UserID:   {Losses: 1, Games: 1, Rating: -10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result[alice.UserID].Wins != 1 || result[bob.UserID].Losses != 1 {
		t.Fatalf("unexpected batch result: %+v", result)
	}
	reopened, err := OpenProfileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if stats, ok := reopened.Stats(alice.UserID); !ok || stats != result[alice.UserID] {
		t.Fatalf("Alice batch stats were not persisted: %+v ok=%v", stats, ok)
	}
	if stats, ok := reopened.Stats(bob.UserID); !ok || stats != result[bob.UserID] {
		t.Fatalf("Bob batch stats were not persisted: %+v ok=%v", stats, ok)
	}
}

func TestProfileStoreAcceptBuddyRollbackDoesNotAliasSlices(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.Register("accept_alice", "correct horse", "Accept Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.Register("accept_bob", "battery staple", "Accept Bob")
	if err != nil {
		t.Fatal(err)
	}
	charlie, err := store.Register("accept_charlie", "another password", "Accept Charlie")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RequestBuddy(alice.UserID, bob.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestBuddy(charlie.UserID, bob.UserID); err != nil {
		t.Fatal(err)
	}
	store.path = blockedProfilePath(t)
	if err := store.AcceptBuddy(bob.UserID, alice.UserID); err == nil {
		t.Fatal("buddy acceptance succeeded when persistence path was unwritable")
	}

	buddies, pending, ok := store.BuddyIDs(bob.UserID)
	if !ok || len(buddies) != 0 || len(pending) != 2 || pending[0] != alice.UserID || pending[1] != charlie.UserID {
		t.Fatalf("failed acceptance corrupted Bob state: buddies=%v pending=%v ok=%v", buddies, pending, ok)
	}
	aliceBuddies, _, _ := store.BuddyIDs(alice.UserID)
	if len(aliceBuddies) != 0 {
		t.Fatalf("failed acceptance corrupted Alice state: buddies=%v", aliceBuddies)
	}
}

func TestProfileStoreRemoveBuddyRollbackDoesNotAliasSlices(t *testing.T) {
	store, err := OpenProfileStore("")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.Register("remove_alice", "correct horse", "Remove Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.Register("remove_bob", "battery staple", "Remove Bob")
	if err != nil {
		t.Fatal(err)
	}
	charlie, err := store.Register("remove_charlie", "another password", "Remove Charlie")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RequestBuddy(alice.UserID, bob.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptBuddy(bob.UserID, alice.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestBuddy(alice.UserID, charlie.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptBuddy(charlie.UserID, alice.UserID); err != nil {
		t.Fatal(err)
	}
	store.path = blockedProfilePath(t)
	if err := store.RemoveBuddy(alice.UserID, bob.UserID); err == nil {
		t.Fatal("buddy removal succeeded when persistence path was unwritable")
	}

	aliceBuddies, _, ok := store.BuddyIDs(alice.UserID)
	if !ok || len(aliceBuddies) != 2 || aliceBuddies[0] != bob.UserID || aliceBuddies[1] != charlie.UserID {
		t.Fatalf("failed removal corrupted Alice state: buddies=%v ok=%v", aliceBuddies, ok)
	}
	bobBuddies, _, _ := store.BuddyIDs(bob.UserID)
	if len(bobBuddies) != 1 || bobBuddies[0] != alice.UserID {
		t.Fatalf("failed removal corrupted Bob state: buddies=%v", bobBuddies)
	}
	charlieBuddies, _, _ := store.BuddyIDs(charlie.UserID)
	if len(charlieBuddies) != 1 || charlieBuddies[0] != alice.UserID {
		t.Fatalf("failed removal corrupted Charlie state: buddies=%v", charlieBuddies)
	}
}

func blockedProfilePath(t *testing.T) string {
	t.Helper()
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blockedParent, "profiles.json")
}
