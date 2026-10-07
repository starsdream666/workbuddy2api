package usagelog

import (
	"os"
	"sort"
)

// History returns every retained record in chronological order. The bool reports
// incomplete reads. Callers must treat entries (including Usage maps) as read-only.
func (l *Logger) History() ([]Entry, bool) {
	if l == nil {
		return []Entry{}, false
	}
	l.historyMu.Lock()
	defer l.historyMu.Unlock()
	l.mu.Lock()
	if l.file == "" {
		entries := l.recentLocked(0)
		l.mu.Unlock()
		return entries, false
	}
	if l.historyCache != nil && l.historyCacheTotal == l.total && l.historyCacheGen == l.gen {
		entries := l.historyCache
		l.mu.Unlock()
		return entries, false
	}
	gen, total := l.gen, l.total
	memory := l.recentLocked(0)
	paths := make([]string, 0, l.maxBk+1)
	for i := l.maxBk; i > 0; i-- {
		paths = append(paths, backupPath(l.file, i))
	}
	paths = append(paths, l.file)
	l.mu.Unlock()

	entries := []Entry{}
	incomplete, found := false, false
	// Scan outside the writer lock; a concurrent rotation invalidates this snapshot.
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			if !os.IsNotExist(err) {
				incomplete = true
			}
			continue
		}
		found = true
		if !scanFile(path, func(e Entry) { entries = append(entries, e) }) {
			incomplete = true
		}
	}
	if !found && len(memory) > 0 {
		entries = memory
		incomplete = true
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.Before(entries[j].Time) })
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gen != gen {
		// A clear or rotation may have removed files during the scan. Never expose
		// a mixed generation as complete or retain it in the cache.
		return []Entry{}, true
	}
	if !incomplete && l.total == total {
		l.historyCache, l.historyCacheTotal, l.historyCacheGen = entries, total, gen
	}
	return entries, incomplete
}
