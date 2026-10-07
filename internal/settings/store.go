package settings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

var (
	ErrConflict = errors.New("配置已被其他页面或文件编辑修改，请重新读取后再保存")
	ErrRead     = errors.New("无法读取配置，请检查配置文件格式和读取权限")
	ErrWrite    = errors.New("保存失败，请检查配置目录的写入权限；Docker 需挂载可写目录，不能仅挂载单个文件。当前运行配置未改变")
	ErrConfig   = errors.New("配置校验失败，请检查现有配置和自定义提示词文件")
)

type Invalid struct{ Message string }

func (e *Invalid) Error() string { return e.Message }

type Values map[string]any
type Resolve func([]byte) (configured Values, effective Values, err error)

// Prepare must not change live state. Commit is infallible and performs no I/O;
// Abort releases prepared resources if persistence fails. Neither may call Store.
type Prepared struct {
	Commit func()
	Abort  func()
}
type Prepare func(effective Values) (Prepared, error)
type Snapshot struct {
	Fields          []Field           `json:"fields"`
	Values          Values            `json:"values"`
	Current         Values            `json:"current"`
	Defaults        Values            `json:"defaults"`
	Locked          map[string]string `json:"locked"`
	Revision        string            `json:"revision"`
	Pending         []string          `json:"pending"`
	RestartRequired bool              `json:"restart_required"`
	HotReload       bool              `json:"hot_reload"`
	AppliedVersion  uint64            `json:"applied_version"`
	// Labels 选项取值 → 界面文案（select 字段的中文显示名）。
	// 与 Options 分开：Options 是配置里能写的取值（协议），Labels 只是怎么显示。
	Labels   map[string]map[string]string `json:"labels,omitempty"`
	Timezone string                       `json:"timezone"`
}
type Store struct {
	mu                sync.Mutex
	path              string
	resolve           Resolve
	current, defaults Values
	locked            map[string]string
	prepare           Prepare
	appliedVersion    uint64
}

// SetApplier installs the runtime transaction during startup.
func (s *Store) SetApplier(prepare Prepare) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepare = prepare
}

func New(path string, current, defaults Values, locked map[string]string, resolve Resolve) *Store {
	locks := map[string]string{}
	for k, v := range locked {
		locks[k] = v
	}
	return &Store{path: path, current: clone(current), defaults: clone(defaults), locked: locks, resolve: resolve}
}
func clone(v Values) Values {
	raw, _ := json.Marshal(v)
	var out Values
	_ = json.Unmarshal(raw, &out)
	return out
}
func revision(raw []byte, exists bool) string {
	prefix := byte(0)
	if exists {
		prefix = 1
	}
	sum := sha256.Sum256(append([]byte{prefix}, raw...))
	return hex.EncodeToString(sum[:])
}
func (s *Store) read() ([]byte, bool, os.FileMode, error) {
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return []byte("{}"), false, 0600, nil
	}
	if err != nil {
		return nil, false, 0, ErrRead
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return nil, false, 0, ErrRead
	}
	raw, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, false, 0, ErrRead
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, false, 0, ErrRead
	}
	return raw, true, info.Mode().Perm(), nil
}
func same(field Field, a, b any) bool {
	if field.Kind == "duration" {
		as, aok := a.(string)
		bs, bok := b.(string)
		ad, ae := time.ParseDuration(as)
		bd, be := time.ParseDuration(bs)
		if aok && bok && ae == nil && be == nil {
			return ad == bd
		}
	}
	return reflect.DeepEqual(a, b)
}
func (s *Store) snapshot(raw []byte, exists bool) (Snapshot, error) {
	values, effective, err := s.resolve(raw)
	if err != nil {
		return Snapshot{}, ErrConfig
	}
	locks := map[string]string{}
	for k, v := range s.locked {
		locks[k] = v
		values[k] = s.current[k]
	}
	pending := []string{}
	fields := Catalog()
	for _, f := range fields {
		if !same(f, effective[f.Key], s.current[f.Key]) {
			pending = append(pending, f.Key)
		}
	}
	return Snapshot{Fields: fields, Values: values, Current: clone(s.current), Defaults: clone(s.defaults), Locked: locks, Labels: OptionLabels(), Revision: revision(raw, exists), Pending: pending, RestartRequired: len(pending) > 0 && s.prepare == nil, HotReload: s.prepare != nil, AppliedVersion: s.appliedVersion, Timezone: time.Now().Format("MST -07:00")}, nil
}
func (s *Store) Read() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, exists, _, err := s.read()
	if err != nil {
		return Snapshot{}, err
	}
	return s.snapshot(raw, exists)
}
func invalid(f Field, detail string) error { return &Invalid{f.Label + "：" + detail} }
func validate(f Field, raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalid(f, "不能为 null")
	}
	switch f.Kind {
	case "boolean":
		var v bool
		if json.Unmarshal(raw, &v) != nil {
			return invalid(f, "必须为布尔值")
		}
	case "integer", "number":
		var v float64
		if json.Unmarshal(raw, &v) != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < f.Min || v > f.Max || (f.Kind == "integer" && math.Trunc(v) != v) {
			return invalid(f, fmt.Sprintf("请输入 %g–%g 范围内的%s", f.Min, f.Max, map[bool]string{true: "整数", false: "数值"}[f.Kind == "integer"]))
		}
	case "duration":
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return invalid(f, "请输入时长，例如 30m、2h")
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 365*24*time.Hour {
			return invalid(f, "需为大于 0 且不超过 8760h 的时长，例如 30m、2h")
		}
	case "select":
		var v string
		if json.Unmarshal(raw, &v) == nil {
			for _, option := range f.Options {
				if v == option {
					return nil
				}
			}
		}
		return invalid(f, "请选择有效选项")
	case "hours":
		var hours []*int
		if json.Unmarshal(raw, &hours) != nil || len(hours) == 0 || len(hours) > 24 {
			return invalid(f, "请输入 1–24 个小时值，使用开关来禁用任务")
		}
		seen := map[int]bool{}
		for _, h := range hours {
			if h == nil || *h < 0 || *h > 23 || seen[*h] {
				return invalid(f, "小时需为 0–23 且不能重复")
			}
			seen[*h] = true
		}
	default:
		return invalid(f, "不支持此配置类型")
	}
	return nil
}

// Save only patches allowlisted leaves. Unrelated fields, secrets and exact
// numbers remain raw JSON. Prepared runtime changes publish only after persistence.
func (s *Store) Save(expected string, changes map[string]json.RawMessage) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, exists, mode, err := s.read()
	if err != nil {
		return Snapshot{}, err
	}
	if expected == "" || expected != revision(raw, exists) {
		return Snapshot{}, ErrConflict
	}
	if len(changes) == 0 {
		return Snapshot{}, &Invalid{"没有需要保存的更改"}
	}
	allowed := map[string]Field{}
	for _, f := range Catalog() {
		allowed[f.Key] = f
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return Snapshot{}, ErrRead
	}
	for key, value := range changes {
		f, ok := allowed[key]
		if !ok {
			return Snapshot{}, &Invalid{"包含不允许通过管理页面修改的配置项"}
		}
		if s.locked[key] != "" {
			return Snapshot{}, invalid(f, "由环境变量控制，请修改部署环境后重启")
		}
		if err := validate(f, value); err != nil {
			return Snapshot{}, err
		}
		parts := strings.Split(key, ".")
		group := map[string]json.RawMessage{}
		if old, ok := object[parts[0]]; ok {
			if json.Unmarshal(old, &group) != nil {
				return Snapshot{}, ErrRead
			}
			if group == nil {
				group = map[string]json.RawMessage{}
			}
		}
		group[parts[1]] = value
		object[parts[0]], err = json.Marshal(group)
		if err != nil {
			return Snapshot{}, ErrRead
		}
	}
	updated, err := json.MarshalIndent(object, "", "  ")
	if err != nil {
		return Snapshot{}, ErrRead
	}
	updated = append(updated, '\n')
	next, err := s.snapshot(updated, true)
	if err != nil {
		return Snapshot{}, err
	}
	// Validate limits together, including effective environment overrides.
	_, effective, err := s.resolve(updated)
	if err != nil {
		return Snapshot{}, ErrConfig
	}
	if s.prepare != nil {
		// Hot application includes other pending file edits, so validate the entire
		// effective allowlist, not only fields present in this PATCH.
		for _, f := range Catalog() {
			value, marshalErr := json.Marshal(effective[f.Key])
			if marshalErr != nil {
				return Snapshot{}, ErrConfig
			}
			if err := validate(f, value); err != nil {
				return Snapshot{}, err
			}
		}
	}
	for _, pair := range [][2]string{{"cooldown.soft_rate", "cooldown.soft_rate_max"}, {"pool.breaker_cooldown", "pool.breaker_cooldown_max"}} {
		if s.prepare == nil && changes[pair[0]] == nil && changes[pair[1]] == nil {
			continue
		}
		base, _ := time.ParseDuration(fmt.Sprint(effective[pair[0]]))
		max, _ := time.ParseDuration(fmt.Sprint(effective[pair[1]]))
		if max < base {
			return Snapshot{}, invalid(allowed[pair[1]], "不得小于基础时长")
		}
	}
	var prepared Prepared
	if s.prepare != nil {
		prepared, err = s.prepare(clone(effective))
		if err != nil {
			if prepared.Abort != nil {
				prepared.Abort()
			}
			return Snapshot{}, err
		}
	}
	if err = s.write(updated, mode, expected); err != nil {
		if prepared.Abort != nil {
			prepared.Abort()
		}
		return Snapshot{}, err
	}
	if s.prepare != nil {
		if prepared.Commit != nil {
			prepared.Commit()
		}
		s.current = clone(effective)
		s.appliedVersion++
		next.Current = clone(s.current)
		next.Pending = []string{}
		next.RestartRequired = false
		next.AppliedVersion = s.appliedVersion
	}
	return next, nil
}
func (s *Store) write(raw []byte, mode os.FileMode, expected string) error {
	// Do not replace a symlink itself and silently detach a deployment's config.
	if info, err := os.Lstat(s.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return ErrWrite
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".gateway-config-*")
	if err != nil {
		return ErrWrite
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrWrite
	}
	// Catch external edits made while validation/file preparation was in progress.
	latest, exists, _, err := s.read()
	if err != nil {
		return err
	}
	if revision(latest, exists) != expected {
		return ErrConflict
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return ErrWrite
	}
	return nil
}
