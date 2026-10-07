package auth

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDeleteFileBlocksConcurrentSave(t *testing.T) {
	a := &Auth{UID: "u", AccessToken: "test", FilePath: filepath.Join(t.TempDir(), "workbuddy-u.json")}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = a.SaveAtomic() }()
	}
	if err := a.DeleteFile(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if _, err := os.Stat(a.FilePath); !os.IsNotExist(err) {
		t.Fatalf("file recreated: %v", err)
	}
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("save after deletion should fail")
	}
	if err := a.DeleteFile(); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
}

func TestDeleteFileFailureKeepsSaveEnabled(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{UID: "u", AccessToken: "test", FilePath: dir}
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteFile(); err == nil {
		t.Fatal("nonempty directory should not delete")
	}
	a.FilePath = filepath.Join(dir, "workbuddy-u.json")
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("failed delete must not mark deleted: %v", err)
	}
}
