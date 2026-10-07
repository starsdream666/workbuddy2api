package main

import (
	"errors"
	"log"
	"os"
	"strings"

	"workbuddy2api/internal/access"
	"workbuddy2api/internal/realm"
)

func openAccess(cfg *Config, reset bool) (*access.Store, error) {
	legacy := []access.LegacyKey{}
	if cfg.APIKey != "" {
		legacy = append(legacy, access.LegacyKey{Token: cfg.APIKey, Name: "迁移 · 原全局 Key", Channels: realm.All(), DefaultChannel: realm.Normalize(cfg.Upstream.Realm)})
	}
	for _, rn := range realm.All() {
		if token := cfg.Realms[rn].APIKey; token != "" {
			channels := []string{}
			for _, channel := range realm.All() {
				if realm.AuthRealmOf(channel) == realm.AuthRealmOf(rn) {
					channels = append(channels, channel)
				}
			}
			legacy = append(legacy, access.LegacyKey{Token: token, Name: "迁移 · " + rn + " Key", Channels: channels, DefaultChannel: rn})
		}
	}
	store, err := access.Open(cfg.Security.StoreFile, legacy)
	if err != nil {
		return nil, err
	}
	username, password := os.Getenv("WB2A_ADMIN_USERNAME"), os.Getenv("WB2A_ADMIN_PASSWORD")
	if reset {
		if username == "" || password == "" {
			store.Close()
			return nil, errors.New("重置管理员需同时设置 WB2A_ADMIN_USERNAME 和 WB2A_ADMIN_PASSWORD")
		}
		if err = store.ResetAdministrator(username, password); err != nil {
			store.Close()
			return nil, err
		}
		log.Print("管理员已重置，旧登录会话已失效；分发 Key 保持不变")
	} else if !store.Ready() {
		if username != "" || password != "" {
			proof, readErr := os.ReadFile(store.SetupFile())
			if readErr != nil {
				store.Close()
				return nil, readErr
			}
			err = store.Setup(strings.TrimSpace(string(proof)), username, password)
			if err != nil {
				store.Close()
				return nil, err
			}
		} else {
			log.Printf("请打开 /admin 初始化管理员。一次性初始化凭据保存在本机文件：%s（勿分享）", store.SetupFile())
		}
	}
	return store, nil
}
