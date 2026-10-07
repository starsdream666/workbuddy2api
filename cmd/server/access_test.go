package main

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/realm"
)

func TestManagedAccessBootstrapAndNoLegacyResurrection(t *testing.T) {
	t.Setenv("WB2A_ADMIN_USERNAME", "admin")
	t.Setenv("WB2A_ADMIN_PASSWORD", "memorable-password")
	cfg := Default()
	cfg.Security.StoreFile = filepath.Join(t.TempDir(), "access.json")
	cfg.APIKey = "legacy-key"
	cfg.Upstream.Realm = realm.WB
	s, err := openAccess(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Ready() {
		t.Fatal("environment did not initialize admin")
	}
	key, err := s.Lookup("legacy-key")
	if err != nil || key.DefaultChannel != realm.WB {
		t.Fatal("legacy key default route lost")
	}
	if _, _, err = s.Login("admin", "memorable-password"); err != nil {
		t.Fatal(err)
	}
	s.Revoke(key.ID)
	s.Close()
	t.Setenv("WB2A_ADMIN_PASSWORD", "different-password")
	s, err = openAccess(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Lookup("legacy-key"); err == nil {
		t.Fatal("revoked key reimported")
	}
	if _, _, err = s.Login("admin", "memorable-password"); err != nil {
		t.Fatal("environment unexpectedly replaced password")
	}
	s.Close()
	s, err = openAccess(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err = s.Login("admin", "different-password"); err != nil {
		t.Fatal("explicit recovery failed")
	}
	if _, err = s.Lookup("legacy-key"); err == nil {
		t.Fatal("recovery resurrected key")
	}
	if _, err = os.Stat(s.SetupFile()); !os.IsNotExist(err) {
		t.Fatal("bootstrap proof remains after admin creation")
	}
}
