package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileDatabaseDSNUsesValidWindowsFileURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.db")
	dsn, databasePath, err := profileDatabaseDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dsn, "file:///") {
		t.Fatalf("profile database DSN = %q, want an absolute file URL", dsn)
	}
	if databasePath == "" || filepath.VolumeName(databasePath) == "" {
		t.Fatalf("profile database path = %q, want an absolute Windows drive path", databasePath)
	}

	store, err := OpenProfileStore(path)
	if err != nil {
		t.Fatalf("open profile store through Windows file URL: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close profile store: %v", err)
	}
}
