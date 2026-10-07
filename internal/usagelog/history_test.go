package usagelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistoryIncludesBackupsBeyondMemoryAfterRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.jsonl")
	cfg := Config{Enabled: true, File: file, MemorySize: 3, MaxBytes: 12000, MaxBackups: 30, BackupsSet: true}
	logger := New(cfg)
	for i := 0; i < 650; i++ {
		logger.Record(Entry{Time: time.Unix(int64(i+1), 0), TotalTokens: i, Status: 200})
	}
	for _, l := range []*Logger{logger, New(cfg)} {
		entries, incomplete := l.History()
		if incomplete || len(entries) != 650 {
			t.Fatalf("history=%d incomplete=%v, want 650 complete records", len(entries), incomplete)
		}
		for i, entry := range entries {
			if entry.TotalTokens != i {
				t.Fatalf("entry %d out of order: %+v", i, entry)
			}
		}
		if len(l.Recent(0)) != 3 {
			t.Fatal("history must not resize the write buffer")
		}
	}
}

func TestHistoryCacheInvalidatesOnRecordRotationAndClear(t *testing.T) {
	l := New(Config{Enabled: true, File: filepath.Join(t.TempDir(), "usage.jsonl"), MaxBytes: 1, MaxBackups: 1, BackupsSet: true})
	for i := 1; i <= 4; i++ {
		l.Record(Entry{Time: time.Unix(int64(i), 0), TotalTokens: i})
		entries, incomplete := l.History()
		if incomplete || len(entries) > 2 || entries[len(entries)-1].TotalTokens != i {
			t.Fatalf("stale history after record/rotation: %+v, incomplete=%v", entries, incomplete)
		}
	}
	l.Clear()
	entries, incomplete := l.History()
	if incomplete || len(entries) != 0 {
		t.Fatalf("history after clear=%v incomplete=%v", entries, incomplete)
	}
}

func TestHistoryIncompleteScanRetriesAndPreservesDuplicateSequences(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.jsonl")
	older, _ := json.Marshal(Entry{Seq: 1, Time: time.Unix(1, 0)})
	newer, _ := json.Marshal(Entry{Seq: 1, Time: time.Unix(2, 0)})
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(string(older) + "\n" + strings.Repeat("x", summaryMaxScanBufBytes+1))
	l := New(Config{Enabled: true, File: file})
	if _, incomplete := l.History(); !incomplete {
		t.Fatal("oversized line must report an incomplete scan")
	}
	write(string(newer) + "\nbroken JSON\n" + string(older) + "\n")
	entries, incomplete := l.History()
	if incomplete || len(entries) != 2 || !entries[0].Time.Before(entries[1].Time) {
		t.Fatalf("retry history=%v incomplete=%v", entries, incomplete)
	}
}

func TestHistoryMemoryAndDisabledWithRetainedFiles(t *testing.T) {
	var nilLogger *Logger
	if entries, incomplete := nilLogger.History(); entries == nil || len(entries) > 0 || incomplete {
		t.Fatal("nil logger must return an empty complete history")
	}
	l := New(Config{Enabled: true, MemorySize: 2})
	for i := 0; i < 3; i++ {
		l.Record(Entry{TotalTokens: i})
	}
	if entries, incomplete := l.History(); incomplete || len(entries) != 2 {
		t.Fatal("memory fallback lost records")
	}
	file := filepath.Join(t.TempDir(), "usage.jsonl")
	New(Config{Enabled: true, File: file}).Record(Entry{TotalTokens: 42})
	entries, incomplete := New(Config{Enabled: false, File: file}).History()
	if incomplete || len(entries) != 1 || entries[0].TotalTokens != 42 {
		t.Fatal("disabled logging must preserve access to retained history")
	}
}
