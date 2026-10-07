// login.go — WorkBuddy OAuth 登录 CLI（设备授权流程，按 realm 选上游产品线）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login url  [-realm cn|ai] → 取 state + authUrl（state 落 /tmp/wb2api-login-state-<realm>.json），
//	                            stdout 打印授权 URL
//	login poll [-realm cn|ai] → 轮询一次，成功打印完整 token+account JSON（含 realm）
//
// 无 PKCE（workbuddy 设备流由服务端签发 state）。核心 HTTP 逻辑在 internal/oauth，
// 与控制台（/admin）共用同一份实现，避免两处漂移。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"workbuddy2api/internal/oauth"
	"workbuddy2api/internal/realm"
)

// stateFileFor realm 之间互不串号（并行登录两个产品线时各自读写自己的 state）。
func stateFileFor(rn string) string {
	if rn == realm.CN {
		return "/tmp/wb2api-login-state.json" // 保持既有路径，兼容旧脚本/文档
	}
	return "/tmp/wb2api-login-state-" + rn + ".json"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|poll> [-realm cn|ai]")
	}
	sub := os.Args[1]
	fs := flag.NewFlagSet("login "+sub, flag.ContinueOnError)
	realmFlag := fs.String("realm", "", "上游产品线：cn（CodeBuddy CN，默认） / ai（WorkBuddy AI）")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fatal("parse flags: %v", err)
	}
	rn := realm.Normalize(*realmFlag)
	prof := realm.Defaults()[rn]
	stateFile := stateFileFor(rn)

	switch sub {
	case "url":
		sess, err := oauth.Start(prof)
		if err != nil {
			fatal("%v", err)
		}
		raw, _ := json.Marshal(map[string]string{"state": sess.State, "realm": rn})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(sess.AuthURL)

	case "poll":
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			fatal("read state: %v (先跑 login url)", err)
		}
		var st struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &st); err != nil || st.State == "" {
			fatal("parse state: %v", err)
		}
		b, err := oauth.Poll(prof, st.State)
		if err != nil {
			if err == oauth.ErrPending {
				fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
			}
			fatal("%v", err)
		}
		out := map[string]any{
			"access_token":  b.AccessToken,
			"refresh_token": b.RefreshToken,
			"expires_in":    b.ExpiresIn,
			"domain":        b.Domain,
			"uid":           b.UID,
			"enterprise_id": b.EnterpriseID,
			"nickname":      b.Nickname,
			"realm":         rn,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q (want url|poll)", sub)
	}
}
