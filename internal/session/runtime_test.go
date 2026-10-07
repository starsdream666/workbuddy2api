package session

import (
	"sync"
	"testing"
	"time"
)

func TestRuntimeKeepsBindingAndReschedulesGC(t *testing.T) {
	r := New(Config{TTL: time.Hour, GCInterval: time.Hour, Available: func() []string { return []string{"a"} }})
	r.Bind("key", "a")
	r.StartGC()
	defer r.StopGC()
	r.ApplyRuntime(time.Hour, time.Millisecond)
	if uid, ok := r.Resolve("key"); !ok || uid != "a" {
		t.Fatal("binding lost")
	}
	r.mu.Lock()
	r.entries["key"] = entry{uid: "a", lastActive: time.Now().Add(-2 * time.Hour)}
	r.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for r.Count() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.Count() != 0 {
		t.Fatal("GC timer did not update")
	}
	r.StopGC()
	r.StartGC()
	r.StopGC()
}

func TestConcurrentSessionRuntimeUpdates(t *testing.T) {
	r := New(Config{Available: func() []string { return []string{"a"} }})
	r.StartGC()
	defer r.StopGC()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.ApplyRuntime(time.Hour, time.Second)
				r.Bind("key", "a")
				r.Resolve("key")
			}
		}()
	}
	wg.Wait()
}
