package usagelog

import (
	"fmt"
	"os"
	"path/filepath"
)

type RuntimeOptions struct {
	Enabled                           bool
	MemorySize, MaxSizeMB, MaxBackups int
}

// PrepareRuntime allocates the buffer and verifies storage before persistence.
// The returned commit only changes protected memory; rotation waits for a write.
func (l *Logger) PrepareRuntime(o RuntimeOptions) (func(), error) {
	if o.MemorySize < 1 || o.MaxSizeMB < 1 || o.MaxBackups < 0 {
		return nil, fmt.Errorf("invalid log limits")
	}
	buffer := make([]Entry, o.MemorySize)
	if o.Enabled && l.file != "" {
		dir := filepath.Dir(l.file)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("日志目录不可写: %w", err)
		}
		probe, err := os.CreateTemp(dir, ".usage-probe-*")
		if err != nil {
			return nil, fmt.Errorf("日志目录不可写: %w", err)
		}
		name := probe.Name()
		probe.Close()
		os.Remove(name)
		if _, err = os.Stat(l.file); err == nil {
			f, e := os.OpenFile(l.file, os.O_WRONLY|os.O_APPEND, 0600)
			if e != nil {
				return nil, fmt.Errorf("日志文件不可写: %w", e)
			}
			f.Close()
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		count := l.next
		if l.filled {
			count = l.size
		}
		if count > o.MemorySize {
			count = o.MemorySize
		}
		start := (l.next - count + l.size) % l.size
		for i := 0; i < count; i++ {
			buffer[i] = l.ring[(start+i)%l.size]
		}
		l.ring, l.size = buffer, o.MemorySize
		l.next, l.filled = count%o.MemorySize, count == o.MemorySize
		if l.maxBk > l.pruneBackups {
			l.pruneBackups = l.maxBk
		}
		if o.Enabled && !l.enabled {
			l.rescanSize = true
		}
		l.enabled, l.maxBytes, l.maxBk = o.Enabled, int64(o.MaxSizeMB)<<20, o.MaxBackups
		l.gen++
	}, nil
}
