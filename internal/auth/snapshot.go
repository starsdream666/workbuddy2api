package auth

import (
	"context"
	"fmt"
)

func (account *Auth) Snapshot() *Auth {
	if account == nil {
		return &Auth{}
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return &Auth{
		AccessToken:  account.AccessToken,
		RefreshToken: account.RefreshToken,
		ExpiresAt:    account.ExpiresAt,
		Domain:       account.Domain,
		UID:          account.UID,
		EnterpriseID: account.EnterpriseID,
		Nickname:     account.Nickname,
		Realm:        account.Realm,
		FilePath:     account.FilePath,
		deleted:      account.deleted,
	}
}

func (account *Auth) AccessTokenValue() string {
	return account.Snapshot().AccessToken
}

func (account *Auth) RefreshTokenValue() string {
	return account.Snapshot().RefreshToken
}

func (account *Auth) Refresh(ctx context.Context, update func(*Auth) error) error {
	account.refreshOnce.Do(func() { account.refreshGate = make(chan struct{}, 1) })
	select {
	case account.refreshGate <- struct{}{}:
		defer func() { <-account.refreshGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot := account.Snapshot()
	if snapshot.deleted {
		return fmt.Errorf("refresh refused: credential deleted")
	}
	previousAccess, previousRefresh := snapshot.AccessToken, snapshot.RefreshToken
	if err := update(snapshot); err != nil {
		return err
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.deleted {
		return fmt.Errorf("refresh refused: credential deleted")
	}
	if account.AccessToken != previousAccess || account.RefreshToken != previousRefresh {
		return fmt.Errorf("credential changed during token refresh")
	}
	account.AccessToken = snapshot.AccessToken
	account.RefreshToken = snapshot.RefreshToken
	account.ExpiresAt = snapshot.ExpiresAt
	account.Domain = snapshot.Domain
	return nil
}

func ValidUID(uid string) bool {
	if len(uid) == 0 || len(uid) > 64 {
		return false
	}
	for _, character := range uid {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
