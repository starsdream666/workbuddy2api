// Package access owns administrator passwords, sessions and issued gateway keys.
// It is separate from internal/auth, which stores upstream OAuth credentials.
package access

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"workbuddy2api/internal/realm"
)

var (
	ErrCredentials = errors.New("账号或密码错误")
	ErrInvalidKey  = errors.New("访问密钥无效、已停用或已过期")
	ErrRateLimit   = errors.New("已超过该密钥的每分钟请求上限")
	ErrNotFound    = errors.New("密钥不存在或已吊销")
)

type Policy struct {
	Name           string     `json:"name"`
	Channels       []string   `json:"channels"`
	DefaultChannel string     `json:"default_channel"`
	ExpiresAt      *time.Time `json:"expires_at"`
	RPM            int        `json:"rpm"`
	Enabled        bool       `json:"enabled"`
}

type Key struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
	Policy
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func (k Key) Allows(channel string) bool {
	for _, value := range k.Channels {
		if value == realm.Normalize(channel) {
			return true
		}
	}
	return false
}

type storedKey struct {
	Key
	Hash string `json:"hash"`
}
type diskState struct {
	Version      int         `json:"version"`
	Username     string      `json:"username"`
	PasswordHash string      `json:"password_hash"`
	SetupHash    string      `json:"setup_hash,omitempty"`
	Keys         []storedKey `json:"keys"`
}
type LegacyKey struct {
	Token, Name, DefaultChannel string
	Channels                    []string
}
type Session struct {
	Username  string
	CSRF      string
	ExpiresAt time.Time
	Done      <-chan struct{}
}
type sessionEntry struct {
	Session
	done chan struct{}
}
type rateWindow struct {
	minute int64
	count  int
}
type Store struct {
	mu       sync.Mutex
	path     string
	data     diskState
	lock     *os.File
	sessions map[string]sessionEntry
	rates    map[string]rateWindow
}

func randomSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// Open imports legacy keys only when the store is first created. A revoked key
// cannot be revived by leaving its old api_key setting in config.json.
func Open(path string, legacy []LegacyKey) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("security.store_file 不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := lockStore(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("认证数据已被其他进程使用或无法锁定: %w", err)
	}
	s := &Store{path: path, lock: lock, sessions: map[string]sessionEntry{}, rates: map[string]rateWindow{}}
	raw, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(raw, &s.data) != nil || s.data.Version != 1 || (s.data.PasswordHash == "" && s.data.SetupHash == "") {
			s.Close()
			return nil, errors.New("认证数据无效；请恢复备份，不会自动重置")
		}
		if s.data.PasswordHash != "" {
			if _, err := bcrypt.Cost([]byte(s.data.PasswordHash)); err != nil {
				s.Close()
				return nil, errors.New("管理员密码数据无效")
			}
		}
		return s, nil
	}
	if !os.IsNotExist(err) {
		s.Close()
		return nil, err
	}
	setup, err := randomSecret("setup-")
	if err != nil {
		s.Close()
		return nil, err
	}
	s.data = diskState{Version: 1, SetupHash: digest(setup), Keys: []storedKey{}}
	seen := map[string]int{}
	for _, old := range legacy {
		if old.Token == "" {
			continue
		}
		hash := digest(old.Token)
		if index, exists := seen[hash]; exists {
			previous := s.data.Keys[index].Policy
			for _, channel := range old.Channels {
				present := false
				for _, existing := range previous.Channels {
					if existing == channel {
						present = true
					}
				}
				if !present {
					previous.Channels = append(previous.Channels, channel)
				}
			}
			merged, mergeErr := normalizePolicy(previous)
			if mergeErr != nil {
				s.Close()
				return nil, mergeErr
			}
			s.data.Keys[index].Policy = merged
			continue
		}
		seen[hash] = len(s.data.Keys)
		p, err := normalizePolicy(Policy{Name: old.Name, Channels: old.Channels, DefaultChannel: old.DefaultChannel, Enabled: true})
		if err != nil {
			s.Close()
			return nil, err
		}
		key, err := makeStoredKey(old.Token, p)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.data.Keys = append(s.data.Keys, key)
	}
	// Write the local setup proof before the database. A crash cannot leave an
	// uninitialized database without a way for its owner to complete setup.
	if err = atomicWrite(s.SetupFile(), []byte(setup+"\n")); err == nil {
		err = s.saveLocked(s.data)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) SetupFile() string { return s.path + ".setup-token" }
func (s *Store) Ready() bool       { s.mu.Lock(); defer s.mu.Unlock(); return s.data.PasswordHash != "" }
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearSessionsLocked()
	if s.lock != nil {
		err := s.lock.Close()
		s.lock = nil
		return err
	}
	return nil
}

func atomicWrite(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".access-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}
func (s *Store) saveLocked(next diskState) error {
	raw, err := json.MarshalIndent(next, "", "  ")
	if err == nil {
		err = atomicWrite(s.path, raw)
	}
	if err == nil {
		s.data = next
	}
	return err
}
func validatePassword(username, password string) error {
	if strings.TrimSpace(username) != username || username == "" || utf8.RuneCountInString(username) > 64 || strings.IndexFunc(username, unicode.IsControl) >= 0 {
		return errors.New("账号名称应为 1–64 个字符，不能含控制字符或首尾空格")
	}
	if len(password) < 8 || len(password) > 72 || strings.TrimSpace(password) == "" {
		return errors.New("密码应为 8–72 字节，可使用易记的长口令")
	}
	return nil
}
func (s *Store) Setup(proof, username, password string) error {
	if err := validatePassword(username, password); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.PasswordHash != "" || !equal(digest(proof), s.data.SetupHash) {
		return errors.New("初始化凭据无效，或管理员已初始化")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	next := s.data
	next.Username = username
	next.PasswordHash = string(hash)
	next.SetupHash = ""
	if err = s.saveLocked(next); err != nil {
		return err
	}
	_ = os.Remove(s.SetupFile())
	return nil
}
func (s *Store) Login(username, password string) (string, Session, error) {
	s.mu.Lock()
	name, hash := s.data.Username, s.data.PasswordHash
	s.mu.Unlock()
	// Always perform password verification for existing administrators, even
	// when the username is wrong, so login does not disclose valid usernames.
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || username != name {
		return "", Session{}, ErrCredentials
	}
	token, err := randomSecret("")
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := randomSecret("")
	if err != nil {
		return "", Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.PasswordHash != hash {
		return "", Session{}, ErrCredentials
	}
	s.pruneSessionsLocked()
	if len(s.sessions) >= 64 {
		return "", Session{}, errors.New("登录会话过多，请先退出其他设备")
	}
	done := make(chan struct{})
	sess := Session{Username: name, CSRF: csrf, ExpiresAt: time.Now().Add(12 * time.Hour), Done: done}
	s.sessions[digest(token)] = sessionEntry{sess, done}
	return token, sess, nil
}
func (s *Store) pruneSessionsLocked() {
	for hash, entry := range s.sessions {
		if !time.Now().Before(entry.ExpiresAt) {
			close(entry.done)
			delete(s.sessions, hash)
		}
	}
}
func (s *Store) Session(token string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneSessionsLocked()
	entry, ok := s.sessions[digest(token)]
	return entry.Session, ok
}
func (s *Store) Logout(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := digest(token)
	if entry, ok := s.sessions[hash]; ok {
		close(entry.done)
		delete(s.sessions, hash)
	}
}
func (s *Store) clearSessionsLocked() {
	for hash, entry := range s.sessions {
		close(entry.done)
		delete(s.sessions, hash)
	}
}
func (s *Store) ChangePassword(current, nextPassword string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bcrypt.CompareHashAndPassword([]byte(s.data.PasswordHash), []byte(current)) != nil {
		return ErrCredentials
	}
	return s.setPasswordLocked(s.data.Username, nextPassword)
}

// ResetAdministrator is only used by the explicit local recovery CLI flag.
func (s *Store) ResetAdministrator(username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setPasswordLocked(username, password)
}
func (s *Store) setPasswordLocked(username, password string) error {
	if err := validatePassword(username, password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	next := s.data
	next.Username = username
	next.PasswordHash = string(hash)
	next.SetupHash = ""
	if err = s.saveLocked(next); err != nil {
		return err
	}
	s.clearSessionsLocked()
	_ = os.Remove(s.SetupFile())
	return nil
}

func normalizePolicy(p Policy) (Policy, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || utf8.RuneCountInString(p.Name) > 80 {
		return p, errors.New("密钥名称应为 1–80 个字符")
	}
	if len(p.Channels) == 0 || len(p.Channels) > len(realm.All()) {
		return p, errors.New("请选择至少一个允许的渠道")
	}
	seen := map[string]bool{}
	channels := []string{}
	for _, rn := range p.Channels {
		if strings.TrimSpace(rn) == "" || !realm.Known(rn) {
			return p, errors.New("未知渠道")
		}
		rn = realm.Normalize(rn)
		if !seen[rn] {
			channels = append(channels, rn)
			seen[rn] = true
		}
	}
	if !realm.Known(p.DefaultChannel) || p.DefaultChannel == "" || !seen[realm.Normalize(p.DefaultChannel)] {
		return p, errors.New("默认渠道必须在允许的渠道中")
	}
	p.DefaultChannel = realm.Normalize(p.DefaultChannel)
	sort.Strings(channels)
	p.Channels = channels
	if p.RPM < 0 || p.RPM > 1000000 {
		return p, errors.New("每分钟请求上限应为 0–1000000，0 表示不限")
	}
	if p.ExpiresAt != nil {
		v := p.ExpiresAt.UTC()
		p.ExpiresAt = &v
	}
	return p, nil
}
func makeStoredKey(token string, p Policy) (storedKey, error) {
	id, err := randomSecret("")
	if err != nil {
		return storedKey{}, err
	}
	prefix := "已导入密钥"
	// Legacy values may share the prefix without having the generated length.
	// Keep them opaque to avoid slicing short tokens or exposing their entirety.
	if strings.HasPrefix(token, "sk-wb2a-") && len(token) == len("sk-wb2a-")+base64.RawURLEncoding.EncodedLen(32) {
		prefix = token[:12] + "…" + token[len(token)-4:]
	}
	return storedKey{Key: Key{ID: id[:22], Prefix: prefix, Policy: p, CreatedAt: time.Now().UTC()}, Hash: digest(token)}, nil
}
func cloneKey(k Key) Key {
	k.Channels = append([]string{}, k.Channels...)
	if k.ExpiresAt != nil {
		v := *k.ExpiresAt
		k.ExpiresAt = &v
	}
	if k.RevokedAt != nil {
		v := *k.RevokedAt
		k.RevokedAt = &v
	}
	return k
}
func (s *Store) List() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, 0, len(s.data.Keys))
	for _, key := range s.data.Keys {
		out = append(out, cloneKey(key.Key))
	}
	return out
}
func (s *Store) Create(p Policy) (Key, string, error) {
	if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now()) {
		return Key{}, "", errors.New("有效期必须晚于当前时间")
	}
	p, err := normalizePolicy(p)
	if err != nil {
		return Key{}, "", err
	}
	token, err := randomSecret("sk-wb2a-")
	if err != nil {
		return Key{}, "", err
	}
	key, err := makeStoredKey(token, p)
	if err != nil {
		return Key{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Keys) >= 10000 {
		return Key{}, "", errors.New("密钥数量已达到上限")
	}
	next := s.data
	next.Keys = append(append([]storedKey{}, s.data.Keys...), key)
	if err = s.saveLocked(next); err != nil {
		return Key{}, "", err
	}
	return cloneKey(key.Key), token, nil
}
func (s *Store) Update(id string, p Policy) (Key, error) {
	p, err := normalizePolicy(p)
	if err != nil {
		return Key{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	next.Keys = append([]storedKey{}, s.data.Keys...)
	for i, key := range next.Keys {
		if key.ID == id && key.RevokedAt == nil {
			if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now()) && (key.ExpiresAt == nil || !p.ExpiresAt.Equal(*key.ExpiresAt)) {
				return Key{}, errors.New("新的有效期必须晚于当前时间")
			}
			next.Keys[i].Policy = p
			if err = s.saveLocked(next); err != nil {
				return Key{}, err
			}
			return cloneKey(next.Keys[i].Key), nil
		}
	}
	return Key{}, ErrNotFound
}
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	next.Keys = append([]storedKey{}, s.data.Keys...)
	for i, key := range next.Keys {
		if key.ID == id {
			if key.RevokedAt != nil {
				return nil
			}
			now := time.Now().UTC()
			next.Keys[i].RevokedAt = &now
			next.Keys[i].Enabled = false
			if err := s.saveLocked(next); err != nil {
				return err
			}
			delete(s.rates, id)
			return nil
		}
	}
	return ErrNotFound
}
func (s *Store) lookupLocked(token string) (Key, error) {
	if token == "" || len(token) > 4096 {
		return Key{}, ErrInvalidKey
	}
	hash := digest(token)
	for _, key := range s.data.Keys {
		if equal(hash, key.Hash) {
			if key.RevokedAt != nil || !key.Enabled || (key.ExpiresAt != nil && !time.Now().Before(*key.ExpiresAt)) {
				return Key{}, ErrInvalidKey
			}
			return cloneKey(key.Key), nil
		}
	}
	return Key{}, ErrInvalidKey
}
func (s *Store) Lookup(token string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLocked(token)
}
func (s *Store) Admit(token string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, err := s.lookupLocked(token)
	if err != nil {
		return Key{}, err
	}
	if key.RPM > 0 {
		minute := time.Now().Unix() / 60
		window := s.rates[key.ID]
		if window.minute != minute {
			window = rateWindow{minute: minute}
		}
		if window.count >= key.RPM {
			return Key{}, ErrRateLimit
		}
		window.count++
		s.rates[key.ID] = window
	}
	return key, nil
}
