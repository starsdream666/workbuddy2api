// importauth.go — 凭证导入：把各种来源的 WorkBuddy 凭证转成网关 auth 文件。
//
// 支持三种输入格式（**自动识别**，无需指定）：
//
//  1. wb-switch 导出（多账号数组）
//     [{"uid":"…","access_token":"…","refresh_token":"…","expiresAt":1820885293646,
//     "domain":"www.workbuddy.ai","region":"intl","nickname":"…","auth_raw":{…}}, …]
//     —— auth_raw 缺字段时回退到 auth_raw.auth 里取；realm 由 domain/region 推断。
//
//  2. 桌面端凭证文件（单账号；也是 1 里 auth_raw 的形态）
//     {"account":{"uid":"…"},"auth":{"accessToken":"…","expiresAt":<ms>,"domain":"…"},"accounts":[…]}
//     —— 默认路径 %LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop-ai.info
//
//  3. 网关自有格式（幂等重导）：{"realm":"ai","auth":{…},"account":{…}}
//
// 输出：auths/<prefix>-<uid>.json（嵌套形 + realm，与 internal/auth 读取格式一致）。
// 默认**导入全部**账号；用 -uid 只导指定那个。
//
// 用法：
//
//	importauth                                  # 自动探测桌面端凭证，导入全部账号
//	importauth -source wb-switch-accounts.json  # 导 wb-switch 导出文件（可多账号）
//	importauth -list                            # 只列出将导入的账号
//	importauth -dry-run                         # 不落盘，只打印摘要
//	importauth -verify=false                    # 跳过上游可用性校验
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

// desktopAuthFilename 桌面端凭证文件名（= product.json authentication.id + ".info"）。
const desktopAuthFilename = "workbuddy-desktop-ai.info"

// candidateSources 自动探测的凭证文件位置（按优先级）。
func candidateSources() []string {
	var out []string
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		out = append(out, filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth", desktopAuthFilename))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			// Windows 之外的等价位置（桌面端只发 Windows，留作兜底/自建布局）
			filepath.Join(home, ".local", "share", "CodeBuddyExtension", "Data", "Public", "auth", desktopAuthFilename),
			filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension", "Data", "Public", "auth", desktopAuthFilename),
		)
	}
	return out
}

// credential 归一后的一条凭证（多格式解析的公共出口）。
type credential struct {
	UID          string
	Nickname     string
	EnterpriseID string
	AccessToken  string
	RefreshToken string
	Domain       string
	Realm        string
	ExpiresAt    int64 // 磁盘原值（可能是毫秒），落盘前统一归一
}

// authBlock 各格式共有的 auth 段。
type authBlock struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
}

// accountBlock 各格式共有的 account 段。
type accountBlock struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterpriseId"`
	LastLogin    bool   `json:"lastLogin"`
}

// wbSwitchEntry wb-switch 导出的单条账号（顶层为下划线命名，另有 auth_raw 原始块）。
type wbSwitchEntry struct {
	UID          string          `json:"uid"`
	Nickname     string          `json:"nickname"`
	EnterpriseID string          `json:"enterpriseId"`
	Domain       string          `json:"domain"`
	Region       string          `json:"region"`
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresAt    int64           `json:"expiresAt"`
	AuthRaw      json.RawMessage `json:"auth_raw"`
	ProfileRaw   json.RawMessage `json:"profile_raw"`
}

// nestedDoc 桌面端 / 网关自有格式（对象形态）。
type nestedDoc struct {
	Realm   string       `json:"realm"`
	Account accountBlock `json:"account"`
	Auth    authBlock    `json:"auth"`
	// 桌面端可能带 accounts 列表（本文件只取 account；列表仅用于提示）
	Accounts []accountBlock `json:"accounts"`
}

// inferRealm 由 domain / region 推断产品线；无法判断时用 fallbackRealm。
//
// 注意 codebuddy.ai 属于**国际线**（与 www.workbuddy.ai 同后端、同账号空间、同账单），
// 只有 codebuddy.cn / copilot.tencent.com 才是 CN 线。凭证按来源线归档，
// 因此从 CodeBuddy IDE（codebuddy.ai）导出的凭证会落到 workbuddy 池，codebuddy 路线直接复用。
func inferRealm(domain, region, fallbackRealm string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	switch {
	case strings.Contains(d, "workbuddy.ai"), strings.Contains(d, "codebuddy.ai"):
		return realm.WB
	case strings.Contains(d, "codebuddy.cn"), strings.Contains(d, "copilot.tencent.com"):
		return realm.CN
	}
	if strings.EqualFold(strings.TrimSpace(region), "intl") {
		return realm.WB
	}
	return realm.Normalize(fallbackRealm)
}

// parseCredentials 自动识别格式并归一为凭证列表。
func parseCredentials(raw []byte, fallbackRealm string) ([]credential, string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, "", fmt.Errorf("文件为空")
	}

	// ── 格式 1：顶层数组（wb-switch 导出）──
	if strings.HasPrefix(trimmed, "[") {
		var entries []wbSwitchEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, "", fmt.Errorf("解析 wb-switch 导出失败: %w", err)
		}
		out := make([]credential, 0, len(entries))
		for i, e := range entries {
			c := credential{
				UID: e.UID, Nickname: e.Nickname, EnterpriseID: e.EnterpriseID,
				AccessToken: e.AccessToken, RefreshToken: e.RefreshToken,
				Domain: e.Domain, ExpiresAt: e.ExpiresAt,
			}
			// 顶层字段缺失时回退到 auth_raw / profile_raw（wb-switch 两者都存）。
			if c.AccessToken == "" || c.RefreshToken == "" || c.ExpiresAt == 0 || c.Domain == "" {
				var inner struct {
					Auth    authBlock    `json:"auth"`
					Account accountBlock `json:"account"`
				}
				if len(e.AuthRaw) > 0 {
					_ = json.Unmarshal(e.AuthRaw, &inner)
				}
				if c.AccessToken == "" {
					c.AccessToken = inner.Auth.AccessToken
				}
				if c.RefreshToken == "" {
					c.RefreshToken = inner.Auth.RefreshToken
				}
				if c.ExpiresAt == 0 {
					c.ExpiresAt = inner.Auth.ExpiresAt
				}
				if c.Domain == "" {
					c.Domain = inner.Auth.Domain
				}
				if c.UID == "" {
					c.UID = inner.Account.UID
				}
				if c.Nickname == "" {
					c.Nickname = inner.Account.Nickname
				}
			}
			if len(e.ProfileRaw) > 0 && c.Nickname == "" {
				var prof accountBlock
				if err := json.Unmarshal(e.ProfileRaw, &prof); err == nil {
					c.Nickname = prof.Nickname
				}
			}
			if strings.TrimSpace(c.UID) == "" {
				fmt.Fprintf(os.Stderr, "importauth: 跳过第 %d 条（缺少 uid）\n", i+1)
				continue
			}
			if strings.TrimSpace(c.AccessToken) == "" {
				fmt.Fprintf(os.Stderr, "importauth: 跳过 %s（缺少 accessToken）\n", c.UID)
				continue
			}
			c.Realm = inferRealm(c.Domain, e.Region, fallbackRealm)
			out = append(out, c)
		}
		return out, "wb-switch 导出（数组）", nil
	}

	// ── 格式 2 / 3：对象（桌面端单账号 或 网关自有格式）──
	var doc nestedDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("解析凭证文件失败: %w", err)
	}
	rn := realm.Normalize(doc.Realm)
	if strings.TrimSpace(doc.Realm) == "" {
		rn = inferRealm(doc.Auth.Domain, "", fallbackRealm)
	}
	c := credential{
		UID: doc.Account.UID, Nickname: doc.Account.Nickname, EnterpriseID: doc.Account.EnterpriseID,
		AccessToken: doc.Auth.AccessToken, RefreshToken: doc.Auth.RefreshToken,
		Domain: doc.Auth.Domain, ExpiresAt: doc.Auth.ExpiresAt, Realm: rn,
	}
	// 桌面端格式此前用 accounts[lastLogin] 兜底 uid（account 段缺失的畸形文件）。
	if strings.TrimSpace(c.UID) == "" {
		for _, a := range doc.Accounts {
			if a.LastLogin {
				c.UID, c.Nickname, c.EnterpriseID = a.UID, a.Nickname, a.EnterpriseID
				break
			}
		}
	}
	if strings.TrimSpace(c.UID) == "" {
		return nil, "", fmt.Errorf("缺少 uid（account 段为空）")
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return nil, "", fmt.Errorf("缺少 accessToken（uid=%s）", c.UID)
	}
	label := "桌面端凭证文件（对象）"
	if strings.TrimSpace(doc.Realm) != "" {
		label = "网关 auth 文件（对象）"
	}
	return []credential{c}, label, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "importauth: "+format+"\n", args...)
	os.Exit(1)
}

// filePrefixFor realm 的 auth 文件名前缀（与 login.sh / 控制台落盘规则一致；
// override 为显式 -prefix）。默认规则见 auth.FilePrefixFor。
func filePrefixFor(rn, override string) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	return auth.FilePrefixFor(rn)
}

func main() {
	var (
		source     = flag.String("source", "", "凭证文件路径（默认自动探测桌面端位置；支持 wb-switch 导出）")
		outDir     = flag.String("out", "./auths", "auth 文件输出目录")
		uidFlag    = flag.String("uid", "", "只导入指定 uid（默认导入文件里的全部账号）")
		realmFlag  = flag.String("realm", realm.WB, "兜底产品线：无法从 domain/region 推断时使用")
		verify     = flag.Bool("verify", true, "导入后调用一次余额接口校验 token 可用性")
		listOnly   = flag.Bool("list", false, "只列出凭证文件中的账号，不落盘")
		dryRun     = flag.Bool("dry-run", false, "不写文件，仅打印将要写入的内容摘要")
		prefixFlag = flag.String("prefix", "", "auth 文件名前缀（默认按 realm：workbuddy / workbuddy-ai）")
	)
	flag.Parse()

	fallbackRealm := realm.Normalize(*realmFlag)

	path := strings.TrimSpace(*source)
	if path == "" {
		for _, c := range candidateSources() {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				path = c
				break
			}
		}
	}
	if path == "" {
		fatal("未找到凭证文件，请用 -source 指定（候选：%s）", strings.Join(candidateSources(), ", "))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("读凭证文件失败: %v", err)
	}
	creds, format, err := parseCredentials(raw, fallbackRealm)
	if err != nil {
		fatal("解析失败（%s）: %v", path, err)
	}
	if len(creds) == 0 {
		fatal("文件里没有可用账号（%s）", path)
	}

	fmt.Printf("来源     : %s\n", path)
	fmt.Printf("格式     : %s\n", format)
	fmt.Printf("账号数   : %d\n", len(creds))

	if *listOnly {
		for i, c := range creds {
			exp := "未知"
			if c.ExpiresAt > 0 {
				exp = time.Unix(auth.NormalizeExpiresAt(c.ExpiresAt), 0).Format("2006-01-02 15:04")
			}
			fmt.Printf("  %d. realm=%-2s %-24s uid=%s 过期=%s\n", i+1, c.Realm, c.Nickname, c.UID, exp)
		}
		return
	}

	// 选择要导入的账号（默认全部）。
	selected := creds
	if want := strings.TrimSpace(*uidFlag); want != "" {
		selected = nil
		for _, c := range creds {
			if c.UID == want {
				selected = append(selected, c)
			}
		}
		if len(selected) == 0 {
			fatal("文件里没有 uid=%s（用 -list 查看）", want)
		}
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil && !*dryRun {
		fatal("创建输出目录失败: %v", err)
	}

	var client *upstream.Client
	if *verify {
		client = upstream.New()
		client.HTTP.Timeout = 30 * time.Second
	}

	var okN, failN int
	for i, c := range selected {
		// 凭证按**来源线**归档：路线别名（codebuddy）与来源线共用同一份账号/额度，
		// 落盘统一记到来源线（realm 字段与文件名都用它），重扫后进的是同一个池。
		authRealm := realm.AuthRealmOf(c.Realm)
		prefix := filePrefixFor(authRealm, *prefixFlag)
		outFile := filepath.Join(*outDir, fmt.Sprintf("%s-%s.json", prefix, c.UID))
		expiresAt := auth.NormalizeExpiresAt(c.ExpiresAt)
		expiry := "未知"
		if expiresAt > 0 {
			expiry = time.Unix(expiresAt, 0).Format("2006-01-02 15:04:05")
		}
		fmt.Printf("\n[%d/%d] realm=%s  %s (%s)\n", i+1, len(selected), authRealm, c.Nickname, c.UID)
		fmt.Printf("        domain=%s  token 过期=%s\n", c.Domain, expiry)
		fmt.Printf("        输出=%s\n", outFile)

		a := &auth.Auth{
			AccessToken:  c.AccessToken,
			RefreshToken: c.RefreshToken,
			ExpiresAt:    expiresAt,
			Domain:       c.Domain,
			Realm:        authRealm,
			UID:          c.UID,
			EnterpriseID: c.EnterpriseID,
			Nickname:     c.Nickname,
			FilePath:     outFile,
		}

		if *verify {
			remain, err := client.UserResource(a)
			if err != nil {
				fmt.Printf("        校验: 失败（%v）\n", err)
				failN++
				continue // 校验不过就不落盘，避免把废凭证塞进池子
			}
			fmt.Printf("        校验: 通过，余额 %d credits\n", remain)
		}

		if *dryRun {
			fmt.Printf("        dry-run: 未写入\n")
			okN++
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			fmt.Printf("        写入失败: %v\n", err)
			failN++
			continue
		}
		okN++
	}

	fmt.Printf("\n完成：成功 %d，失败 %d", okN, failN)
	if *dryRun {
		fmt.Printf("（dry-run，未落盘）")
	} else {
		fmt.Printf("，输出目录 %s", *outDir)
	}
	fmt.Println()
	if failN > 0 {
		os.Exit(1)
	}
}
