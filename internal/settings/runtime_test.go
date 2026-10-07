package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedSavePublishesAfterPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	resolve := func(raw []byte) (Values, Values, error) {
		var object struct {
			Pool struct {
				Max float64 `json:"max_in_flight"`
			} `json:"pool"`
		}
		err := json.Unmarshal(raw, &object)
		v := Values{}
		for _, f := range Catalog() {
			switch f.Kind {
			case "integer", "number":
				v[f.Key] = f.Min
			case "duration":
				v[f.Key] = "1h"
			case "boolean":
				v[f.Key] = false
			case "hours":
				v[f.Key] = []any{float64(9)}
			case "select":
				v[f.Key] = f.Options[0]
			}
		}
		v["pool.max_in_flight"] = object.Pool.Max
		return v, v, err
	}
	s := New(path, Values{"pool.max_in_flight": float64(0)}, nil, nil, resolve)
	live := float64(0)
	s.SetApplier(func(v Values) (Prepared, error) {
		return Prepared{Commit: func() {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, disk, _ := resolve(raw)
			if disk["pool.max_in_flight"] != v["pool.max_in_flight"] {
				t.Fatal("published before persistence")
			}
			live = v["pool.max_in_flight"].(float64)
		}}, nil
	})
	before, _ := s.Read()
	after, err := s.Save(before.Revision, map[string]json.RawMessage{"pool.max_in_flight": json.RawMessage(`7`)})
	if err != nil || live != 7 || !after.HotReload || after.RestartRequired || len(after.Pending) != 0 || after.AppliedVersion != 1 || after.Current["pool.max_in_flight"] != float64(7) {
		t.Fatalf("save: %+v %v live=%v", after, err, live)
	}
	if _, err = s.Save(before.Revision, map[string]json.RawMessage{"pool.max_in_flight": json.RawMessage(`9`)}); !errors.Is(err, ErrConflict) || live != 7 {
		t.Fatal("stale save applied")
	}
	aborted := false
	s.SetApplier(func(v Values) (Prepared, error) {
		return Prepared{Commit: func() { live = 99 }, Abort: func() { aborted = true }}, errors.New("prepare failed")
	})
	if _, err = s.Save(after.Revision, map[string]json.RawMessage{"pool.max_in_flight": json.RawMessage(`9`)}); err == nil || !aborted || live != 7 {
		t.Fatal("failed prepare applied")
	}
	s.SetApplier(func(v Values) (Prepared, error) {
		// Simulate an external edit after preparation, before atomic replacement.
		if err := os.WriteFile(path, []byte(`{"pool":{"max_in_flight":8}}`), 0600); err != nil {
			t.Fatal(err)
		}
		return Prepared{Commit: func() { live = 99 }, Abort: func() { aborted = true }}, nil
	})
	aborted = false
	if _, err = s.Save(after.Revision, map[string]json.RawMessage{"pool.max_in_flight": json.RawMessage(`9`)}); !errors.Is(err, ErrConflict) || !aborted || live != 7 {
		t.Fatal("write conflict applied")
	}
}
