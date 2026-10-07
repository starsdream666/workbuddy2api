package access

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/realm"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "access.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func setupAdmin(t *testing.T, s *Store) {
	t.Helper()
	proof, err := os.ReadFile(s.SetupFile())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(strings.TrimSpace(string(proof)), "admin", "memorable-password"); err != nil {
		t.Fatal(err)
	}
}
func testPolicy() Policy {
	return Policy{Name: "开发", Channels: []string{realm.WB}, DefaultChannel: realm.WB, Enabled: true}
}

func TestAdministratorLifecycle(t *testing.T) {
	s := testStore(t)
	if s.Ready() {
		t.Fatal("uninitialized store is ready")
	}
	if err := s.Setup("wrong", "admin", "memorable-password"); err == nil {
		t.Fatal("setup without local proof")
	}
	setupAdmin(t, s)
	if _, err := os.Stat(s.SetupFile()); !os.IsNotExist(err) {
		t.Fatal("setup proof not removed")
	}
	if err := s.Setup("anything", "other", "another-password"); err == nil {
		t.Fatal("setup reused")
	}
	if _, _, err := s.Login("wrong", "memorable-password"); !errors.Is(err, ErrCredentials) {
		t.Fatal("wrong username accepted")
	}
	token, sess, err := s.Login("admin", "memorable-password")
	if err != nil {
		t.Fatal(err)
	}
	if sess.CSRF == "" || sess.Username != "admin" {
		t.Fatal("incomplete session")
	}
	if _, ok := s.Session(token); !ok {
		t.Fatal("session unavailable")
	}
	raw, _ := os.ReadFile(s.path)
	for _, secret := range []string{"memorable-password", token, sess.CSRF} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("plaintext secret persisted")
		}
	}
	s.Logout(token)
	if _, ok := s.Session(token); ok {
		t.Fatal("logout ineffective")
	}
	select {
	case <-sess.Done:
	default:
		t.Fatal("active session not cancelled")
	}
	old, oldSession, _ := s.Login("admin", "memorable-password")
	if err := s.ChangePassword("wrong", "new-memorable-password"); err == nil {
		t.Fatal("change without current password")
	}
	if err := s.ChangePassword("memorable-password", "new-memorable-password"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Session(old); ok {
		t.Fatal("old session survived password change")
	}
	select {
	case <-oldSession.Done:
	default:
		t.Fatal("old session stream remains active")
	}
	if _, _, err := s.Login("admin", "memorable-password"); err == nil {
		t.Fatal("old password accepted")
	}
	if _, _, err := s.Login("admin", "new-memorable-password"); err != nil {
		t.Fatal(err)
	}
}

func TestKeyPersistenceRevocationAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.json")
	legacy := []LegacyKey{{Token: "old-key", Name: "旧 Key", Channels: []string{realm.WB}, DefaultChannel: realm.WB}, {Token: "old-key", Name: "旧 Key 2", Channels: []string{realm.CB}, DefaultChannel: realm.CB}}
	s, err := Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 1 {
		t.Fatal("duplicate legacy tokens")
	}
	old, err := s.Lookup("old-key")
	if err != nil || !old.Allows(realm.WB) || !old.Allows(realm.CB) {
		t.Fatal("legacy channels not merged")
	}
	key, token, err := s.Create(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "sk-wb2a-") {
		t.Fatal("invalid token format")
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "old-key") {
		t.Fatal("raw API key persisted")
	}
	list := s.List()
	list[1].Channels[0] = realm.CN
	if got, _ := s.Lookup(token); !got.Allows(realm.WB) || got.Allows(realm.CN) {
		t.Fatal("list mutation changed permissions")
	}
	if err = s.Revoke(old.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Lookup("old-key"); err == nil {
		t.Fatal("legacy config resurrected revoked key")
	}
	if got, err := s.Lookup(token); err != nil || got.ID != key.ID {
		t.Fatal("issued key not restored")
	}
	p := testPolicy()
	p.Enabled = false
	if _, err = s.Update(key.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Lookup(token); err == nil {
		t.Fatal("disabled key accepted")
	}
	p.Enabled = true
	p.Channels = []string{realm.CB}
	p.DefaultChannel = realm.CB
	if _, err = s.Update(key.ID, p); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Lookup(token)
	if got.Allows(realm.WB) || !got.Allows(realm.CB) {
		t.Fatal("channel restrictions stale")
	}
	if err = s.Revoke(key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(key.ID, p); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked key re-enabled")
	}
}

func TestLegacyKeyWithIssuedPrefix(t *testing.T) {
	for _, token := range []string{"sk-wb2a-", "sk-wb2a-a", "sk-wb2a-abcd", "sk-wb2a-abcdefg", "sk-wb2a-abcdefgh"} {
		t.Run(token, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "access.json")
			s, err := Open(path, []LegacyKey{{Token: token, Name: "迁移", Channels: []string{realm.WB}, DefaultChannel: realm.WB}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.Lookup(token); err != nil {
				t.Fatalf("legacy key did not survive import: %v", err)
			}
			if keys := s.List(); len(keys) != 1 || keys[0].Prefix != "已导入密钥" {
				t.Fatal("short legacy key must remain opaque in the list")
			}
			raw, err := os.ReadFile(path)
			if err != nil || strings.Contains(string(raw), token) {
				t.Fatal("legacy key stored as plaintext")
			}
		})
	}
}

func TestStoreLockAndFailedWrite(t *testing.T) {
	s := testStore(t)
	if duplicate, err := Open(s.path, nil); err == nil {
		duplicate.Close()
		t.Fatal("two writers opened same store")
	}
	key, token, err := s.Create(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	original := s.path
	s.path = filepath.Join(t.TempDir(), "missing", "access.json")
	if err = s.Revoke(key.ID); err == nil {
		t.Fatal("expected write failure")
	}
	if _, err = s.Lookup(token); err != nil {
		t.Fatal("failed revoke changed in-memory state")
	}
	s.path = original
}

func TestLimitsExpiryAndValidation(t *testing.T) {
	s := testStore(t)
	p := testPolicy()
	p.RPM = 7
	_, token, err := s.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Admit(token); err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrRateLimit) {
				t.Errorf("admit: %v", err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 7 {
		t.Fatalf("accepted=%d", accepted.Load())
	}
	s.mu.Lock()
	past := time.Now().Add(-time.Second)
	s.data.Keys[0].ExpiresAt = &past
	s.mu.Unlock()
	if _, err = s.Lookup(token); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("expired key accepted")
	}
	for _, p := range []Policy{{Name: "bad", Enabled: true}, {Name: "bad", Channels: []string{realm.WB}, DefaultChannel: realm.CN}, {Name: "bad", Channels: []string{"unknown"}, DefaultChannel: "unknown"}, {Name: "bad", Channels: []string{realm.WB}, DefaultChannel: realm.WB, RPM: -1}} {
		if _, _, err := s.Create(p); err == nil {
			t.Fatalf("invalid policy accepted: %+v", p)
		}
	}
}

func TestCorruptStoreFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.json")
	os.WriteFile(path, []byte(`{"version":1}`), 0600)
	if s, err := Open(path, nil); err == nil {
		s.Close()
		t.Fatal("corrupt store reset silently")
	}
	if _, err := os.Stat(path + ".setup-token"); !os.IsNotExist(err) {
		t.Fatal("corrupt store generated new setup proof")
	}
}
