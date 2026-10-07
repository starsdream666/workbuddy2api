package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSnapshotConcurrentRefresh(test *testing.T) {
	account := &Auth{UID: "test", AccessToken: "old", RefreshToken: "refresh", Domain: "old"}
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				if err := account.Refresh(context.Background(), func(snapshot *Auth) error {
					snapshot.AccessToken, snapshot.Domain = "new", "new"
					snapshot.ExpiresAt = time.Now().Add(time.Hour).Unix()
					return nil
				}); err != nil {
					test.Error(err)
				}
			}
		}()
	}
	for iteration := 0; iteration < 400; iteration++ {
		snapshot := account.Snapshot()
		if snapshot.AccessToken != snapshot.Domain {
			test.Fatal("inconsistent credential snapshot")
		}
		_ = account.NeedsRefresh(time.Minute)
		_ = account.AccessTokenValue()
		_ = account.RefreshTokenValue()
	}
	workers.Wait()
}

func TestRefreshWaitCanCancelAndDeleteWins(test *testing.T) {
	account := &Auth{UID: "test", AccessToken: "old", RefreshToken: "refresh", FilePath: filepath.Join(test.TempDir(), "workbuddy-test.json")}
	if err := account.SaveAtomic(); err != nil {
		test.Fatal(err)
	}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- account.Refresh(context.Background(), func(snapshot *Auth) error {
			close(started)
			<-release
			snapshot.AccessToken = "new"
			return nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := account.Refresh(ctx, func(*Auth) error { test.Error("canceled refresh ran"); return nil }); !errors.Is(err, context.Canceled) {
		test.Errorf("error = %v", err)
	}
	if err := account.DeleteFile(); err != nil {
		test.Fatal(err)
	}
	close(release)
	if err := <-finished; err == nil {
		test.Fatal("refresh after deletion succeeded")
	}
	if err := account.SaveAtomic(); err == nil {
		test.Fatal("deleted credential saved")
	}
	if _, err := os.Stat(account.FilePath); !os.IsNotExist(err) {
		test.Fatalf("credential resurrected: %v", err)
	}
}

func TestRefreshKeepsCompletedRotationOnCancel(test *testing.T) {
	account := &Auth{AccessToken: "old", RefreshToken: "old-refresh"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := account.Refresh(ctx, func(snapshot *Auth) error {
		snapshot.AccessToken, snapshot.RefreshToken = "new", "new-refresh"
		cancel()
		return nil
	})
	if err != nil || account.RefreshTokenValue() != "new-refresh" {
		test.Fatalf("completed token rotation lost: %v", err)
	}
}

func TestValidUIDAndAuthPath(test *testing.T) {
	for _, uid := range []string{"u-cn", "123_ABC", strings.Repeat("a", 64)} {
		if !ValidUID(uid) || AuthFileFor("auths", "cn", uid) == "" {
			test.Errorf("valid uid rejected: %q", uid)
		}
	}
	for _, uid := range []string{"", "../x", `..\x`, "a/b", "a:b", "用户", "a\x00", strings.Repeat("a", 65)} {
		if ValidUID(uid) || AuthFileFor("auths", "cn", uid) != "" {
			test.Errorf("invalid uid accepted: %q", uid)
		}
	}
}
