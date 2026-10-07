package usagelog

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRuntimeResizePreservesNewestAndTotals(t *testing.T) {
	l := New(Config{Enabled: true, MemorySize: 4})
	for i := 1; i <= 6; i++ {
		l.Record(Entry{Seq: int64(i)})
	}
	apply, err := l.PrepareRuntime(RuntimeOptions{Enabled: true, MemorySize: 2, MaxSizeMB: 1, MaxBackups: 0})
	if err != nil {
		t.Fatal(err)
	}
	apply()
	l.mu.Lock()
	if l.total != 6 || l.ring[0].Seq != 5 || l.ring[1].Seq != 6 || !l.filled {
		t.Fatal("resize lost recent records or totals")
	}
	l.mu.Unlock()
	apply, err = l.PrepareRuntime(RuntimeOptions{Enabled: false, MemorySize: 8, MaxSizeMB: 1})
	if err != nil {
		t.Fatal(err)
	}
	apply()
	l.Record(Entry{})
	if total, kept, _, enabled := l.Stats(); total != 6 || kept != 2 || enabled {
		t.Fatal("disable changed history")
	}
}

func TestRuntimeBackupShrinkAndReenable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: false, File: file, MaxBackups: 3, BackupsSet: true})
	for i := 1; i <= 3; i++ {
		if err := os.WriteFile(backupPath(file, i), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	apply, err := l.PrepareRuntime(RuntimeOptions{Enabled: true, MemorySize: 8, MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	apply()
	if l.SizeInfo().BackupBytes != 9 {
		t.Fatal("pending cleanup hidden from disk size")
	}
	l.Record(Entry{})
	if _, err := os.Stat(backupPath(file, 2)); !os.IsNotExist(err) {
		t.Fatal("obsolete backup retained")
	}
	if _, err := os.Stat(backupPath(file, 1)); err != nil {
		t.Fatal("kept backup removed")
	}
}

func TestConcurrentLogRuntimeUpdates(t *testing.T) {
	l := New(Config{Enabled: true})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				apply, err := l.PrepareRuntime(RuntimeOptions{Enabled: j%2 == 0, MemorySize: 10 + j%5, MaxSizeMB: 1})
				if err != nil {
					t.Error(err)
					return
				}
				apply()
				l.Record(Entry{})
				l.Enabled()
				l.Stats()
			}
		}()
	}
	wg.Wait()
}
