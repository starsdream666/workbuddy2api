// main.go workbuddy2api 入口：加载配置、按 realm 分池构建 pool、起调度器与 HTTP 服务。
//
// realm（上游产品线）是本项目的核心维度：账号凭证自带 realm（cn / ai），
// 池、调度、上游 base/指纹都按 realm 分派，绝不跨池回退
// （同一 token 在另一条线必被上游 401）。
//
// 控制台（/admin）复用同一套 pool 与 upstream：登录成功后落盘凭证并热加载账号池，
// 不必重启进程（Docker 部署下也就是不必 restart 容器）。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/eventbus"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/taskqueue"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usagelog"
)

// groupByRealm 按 auth 自带 realm 分组；未标注的归 defaultRealm（旧凭证零改动）。
// usageBalanceTimeout 使用日志的"调用后余额查询"专用超时。
// 这次查询发生在响应写完之后的 defer 里，只为算差值；迟到的观测对用户毫无价值
// （上游计费本来就可能延迟），故宁可快速失败记 null，也不要占住连接。
const usageBalanceTimeout = 20 * time.Second

// groupByRealm 按 auth 自带 realm 分组；未标注的归 defaultRealm（旧凭证零改动）。
func groupByRealm(auths []*auth.Auth, defaultRealm string) map[string][]*auth.Auth {
	groups := map[string][]*auth.Auth{}
	for _, a := range auths {
		rn := realm.Normalize(a.Realm)
		if strings.TrimSpace(a.Realm) == "" {
			rn = defaultRealm
		}
		a.Realm = rn // 归一写回内存；落盘发生在 token refresh（SaveAtomic）
		groups[rn] = append(groups[rn], a)
	}
	return groups
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	resetAdmin := flag.Bool("reset-admin", false, "reset administrator using WB2A_ADMIN_USERNAME/PASSWORD and exit")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	if *resetAdmin {
		accessStore, resetErr := openAccess(cfg, true)
		if resetErr != nil {
			log.Fatalf("认证重置失败: %v", resetErr)
		}
		accessStore.Close()
		return
	}
	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// ── realm 分组 + 池清单 ──
	defaultRealm := realm.Normalize(cfg.Upstream.Realm)
	groups := groupByRealm(auths, defaultRealm)
	// 池清单 = 默认 realm + 其他有账号的 realm（顺序稳定：cn 在前）。
	realmList := []string{defaultRealm}
	for _, rn := range realm.All() {
		if rn != defaultRealm && len(groups[rn]) > 0 {
			realmList = append(realmList, rn)
		}
	}
	// 默认路由校正：cfg 指定的默认 realm 一个账号都没有、而另一个 realm 有账号时，
	// 把默认路由让给它——否则"只有 ai 账号"的部署用全局 key 会稳定 503。
	if len(groups[defaultRealm]) == 0 {
		for _, rn := range realmList {
			if len(groups[rn]) > 0 {
				log.Printf("默认 realm %s 无账号 → 默认路由改指向 %s（可由 upstream.realm 显式指定）", defaultRealm, rn)
				defaultRealm = rn
				break
			}
		}
	}

	// Import the legacy global key using the same effective default route as
	// the old gateway, including its fallback when the configured pool is empty.
	accessCfg := *cfg
	accessCfg.Upstream.Realm = defaultRealm
	accessStore, err := openAccess(&accessCfg, false)
	if err != nil {
		log.Fatalf("认证初始化失败: %v", err)
	}
	defer accessStore.Close()

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	// ── 上游 client（realm 档案/指纹在池创建前就绪，便于按需建池时注册）──
	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	up.PromptCacheKey = cfg.Features.PromptCacheKey
	up.RepairToolHistory = cfg.Features.RepairToolHistory
	// 出站 UA 覆盖（issue #42）：全局非空才改写；空 = 按各 realm 指纹解析。
	up.UserAgent = cfg.Upstream.UserAgent
	up.RealmDefault = defaultRealm
	up.Profiles = map[string]realm.Profile{}
	up.RealmFingerprints = map[string]realm.Fingerprint{}
	up.RealmVersions = map[string]string{}
	up.RealmUserAgents = map[string]string{}

	// ── 每个 realm 一个独立池（状态文件按 realm 派生，互不干扰）──
	// 写入由 runtime 串行化，handler 通过不可变注册表快照读取新增的池。
	pools := map[string]*pool.Pool{}
	runtime := &runtimeController{current: cfg, up: up, pools: pools, schedulers: map[string]*scheduler.Scheduler{}}
	registerRealm := func(rn string) {
		up.Profiles[rn] = cfg.RealmProfile(rn)
		if fp := cfg.RealmFingerprint(rn); fp != "" {
			up.RealmFingerprints[rn] = fp
		}
		if v := cfg.RealmClientVersion(rn); v != "" {
			up.RealmVersions[rn] = v
		}
		if ua := cfg.RealmUserAgent(rn); ua != "" {
			up.RealmUserAgents[rn] = ua
		}
	}
	// Profiles are immutable once requests start; pre-register even empty realms.
	for _, rn := range realm.All() {
		registerRealm(rn)
	}
	// 控制台实时推送总线：池状态变化 → 广播 → SSE。
	//
	// 只建一条、由所有池共享：控制台的列表是"全部 realm"的聚合视图，
	// 任一池变化都该刷新整个页面。生命周期与进程一致（退出时 Close）。
	events := eventbus.New()
	defer events.Close()

	ensurePool := func(rn string) *pool.Pool {
		if p, ok := pools[rn]; ok {
			return p
		}
		// realm 改名（ai → workbuddy）的一次性状态迁移：新名文件不存在而历史文件存在时复制一份，
		// 避免积分 / 冷却 / 熔断计数凭空清零（见 Config.LegacyStateFileFor）。
		migrateLegacyStateFile(cfg, rn)
		p := pool.New(cfg.StateFileFor(rn))
		p.SetStore(store)
		p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
		// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
		p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
		p.SetMaxInFlight(cfg.Pool.MaxInFlight)
		p.SetCreditFloor(cfg.Pool.CreditFloor)
		p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
		p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
		// 额度冻结的兜底时长（到期即使巡检没确认恢复也放行一次，防永久冻结）。
		p.SetFreezeMax(cfg.Schedule.CreditFreezeMaxDur())
		// 选号策略：weighted（默认，打散热点）/ lowest_credits（集中打光单号提缓存命中）。
		p.SetSelectionMode(cfg.Pool.SelectionMode)
		if runtime.handler != nil {
			p.ApplyRuntime(poolOptions(runtime.current))
		}
		// 状态变化 → 控制台实时推送。注入后池内所有变更（额度/冷却/冻结/
		// 禁用/在途）都会即时广播；未接控制台时它只是几次非阻塞 channel 投递。
		p.SetChangeNotifier(events)
		pools[rn] = p
		runtime.publishPools()
		return p
	}
	for _, rn := range realmList {
		p := ensurePool(rn)
		p.SyncToDir(groups[rn]) // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）
		log.Printf("realm %s: %d 账号，base=%s，指纹=%s", rn, len(groups[rn]), cfg.RealmProfile(rn).ChatBase, cfg.RealmFingerprint(rn))
	}

	// ── 路线别名（route alias）：codebuddy 复用 ai 的账号池 ──
	//
	// 不搬迁凭证：别名线不建自己的池，直接把 pools[别名] 指向来源线的池对象，
	// 于是账号、积分、冷却、在途、禁用状态与来源线**共享同一份**（不会两套计数打架），
	// 只有出站 base / Origin / 归因按别名线档案解析（handler 用 upstream.Client.Route 钉住）。
	// 启用条件：realms.<别名> 或 upstream.realm_overrides.<别名> 显式出现（见 Config.RouteEnabled）。
	var routeRealms []string
	for _, rn := range realm.All() {
		if !cfg.RouteEnabled(rn) {
			continue
		}
		src := cfg.AuthRealmFor(rn)
		if src == rn {
			continue
		}
		if len(groups[src]) == 0 {
			log.Printf("路线 %s 已配置，但来源线 %s 当前没有账号 → 该路线暂时无号可用（可在控制台登录后自动生效）", rn, src)
		}
		ensurePool(src) // 来源池必须先存在（即使暂时为空）
		pools[rn] = pools[src]
		runtime.publishPools()
		routeRealms = append(routeRealms, rn)
		log.Printf("realm %s: 路线别名 → 复用 %s 的 %d 个账号，base=%s（凭证来源线 %s）",
			rn, src, len(groups[src]), cfg.RealmProfile(rn).ChatBase, src)
	}
	defer func() {
		// 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）。
		for _, p := range runtime.poolsSnapshot() {
			p.Flush()
		}
	}()
	defaultPool := pools[defaultRealm]

	// reloadAuths 重扫凭证目录并对齐各 realm 池（控制台登录成功 / 手动重扫时调用）。
	// 新出现的 realm 使用预注册的上游档案自动建池，因此"先只有 ai 账号、后来登录了 cn 账号"
	// 这类场景无需重启进程。
	reloadAuths := func() (map[string]int, error) {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		loaded, err := auth.LoadDir(cfg.AuthDir)
		if err != nil {
			return nil, err
		}
		g := groupByRealm(loaded, defaultRealm)
		counts := map[string]int{}
		// 先对已存在的池做增删对齐，再为全新的 realm 建池。
		// 路线别名（codebuddy）共享来源线的池：它有没有账号由来源线决定，
		// 不能按别名线自己的 g[rn]（恒为空）判断 —— 那会连带清掉来源线的账号。
		for rn := range pools {
			if realm.IsRouteAlias(rn) {
				continue
			}
			counts[rn] = len(g[rn])
		}
		for rn := range g {
			counts[rn] = len(g[rn])
		}
		for rn, subset := range g {
			ensurePool(rn).SyncToDir(subset)
		}
		for rn := range pools {
			if realm.IsRouteAlias(rn) {
				continue // 别名线的增删由来源线同步，避免误清共享池
			}
			if _, ok := g[rn]; !ok {
				pools[rn].SyncToDir(nil) // 该 realm 凭证被删光 → 池内清空
				counts[rn] = 0
			}
		}
		for rn, p := range pools {
			if !realm.IsRouteAlias(rn) {
				runtime.addScheduler(rn, p)
			}
		}
		return counts, nil
	}

	// 会话粘性路由（可配关闭）。可用性判定跨池聚合：uid 全局唯一，任一池可用即算可用。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	{
		available := func() []string {
			var out []string
			seen := map[*pool.Pool]bool{}
			for _, p := range runtime.poolsSnapshot() {
				if seen[p] {
					continue
				}
				seen[p] = true
				out = append(out, p.AvailableUIDs()...)
			}
			return out
		}
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  available,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	runtime.session = sessRouter
	sessCount := func() int {
		if runtime.handler == nil || runtime.handler.CurrentRuntime().Session != nil {
			return sessRouter.Count()
		}
		return 0
	}

	// ── 调度器：每 realm 一个（任务在各池内独立执行，能力探测见 scheduler）──
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if cfg.Schedule.CreditWatchEnabled {
		log.Printf("额度巡检已启用：每 %s 一轮，范围=%s（余额为 0 冻结，恢复自动解冻；兜底 %s）",
			cfg.Schedule.CreditWatchIntervalDur(), cfg.Schedule.CreditWatchScope, cfg.Schedule.CreditFreezeMaxDur())
	} else {
		log.Printf("额度巡检已禁用（schedule.credit_watch_enabled=false）：冻结仅由请求撞额度耗尽触发")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var tasks *taskqueue.Queue
	if cfg.Console.Enabled {
		tasks = taskqueue.New(ctx, 64, 200)
		defer tasks.Close()
	}

	runtime.ctx = ctx
	for rn, p := range pools {
		if !realm.IsRouteAlias(rn) {
			runtime.addScheduler(rn, p)
		}
	}

	realmKeys := map[string]string{}
	for rn, rc := range cfg.Realms {
		if strings.TrimSpace(rc.APIKey) != "" {
			realmKeys[realm.Normalize(rn)] = rc.APIKey
		}
	}

	// ── 使用日志：每请求一条（凭证 + 消耗积分 + token/缓存）──
	// 积分消耗靠"调用前余额 - 调用后余额"：上游不按请求返回消耗，
	// 只能在响应写完后查一次余额（用专用短超时，见 usageBalanceTimeout 注释）。
	usageLogger := usagelog.New(usagelog.Config{
		Enabled:    cfg.UsageLog.Enabled,
		File:       cfg.UsageLog.File,
		MemorySize: cfg.UsageLog.MemorySize,
		MaxSizeMB:  cfg.UsageLog.MaxSizeMB,
		MaxBackups: cfg.UsageLog.MaxBackups,
		BackupsSet: true, // 配置层已归一（默认 1），显式传值以覆盖包的零值默认
	})
	initialRuntime := runtimeFor(cfg, up, sessRouter)
	if cfg.UsageLog.Enabled {
		// 启动即打印磁盘占用上限，让"会不会爆炸"变成一眼可见的数字。
		si := usageLogger.SizeInfo()
		log.Printf("使用日志已启用：file=%s 内存=%d 条 单文件上限=%.0fMB 保留备份=%d 份 磁盘占用上限=%.0fMB 余额刷新=%v 单号校准间隔=%v（支持控制台清空）",
			cfg.UsageLog.File, cfg.UsageLog.MemorySize,
			float64(si.MaxBytes)/(1<<20), si.MaxBackups, float64(si.LimitBytes)/(1<<20),
			cfg.UsageLog.RefreshBalance, cfg.CalibrateDur)
	}

	settingsStore := newSettingsStore(*cfgPath, cfg)
	h := server.NewHandler(server.Config{
		Access:              accessStore,
		SecureCookie:        cfg.Security.SecureCookie,
		Settings:            settingsStore,
		Tasks:               tasks,
		MaintenanceSnapshot: runtime.maintenanceSnapshot,
		PoolsSnapshot:       runtime.poolsSnapshot,
		Pool:                defaultPool,
		Pools:               pools,
		RealmKeys:           realmKeys,
		ChannelPrefixes:     cfg.ModelPrefixes,
		Events:              events,
		Notify:              events, // 同一个总线：用量日志等非池路径的变化也从这里广播
		DefaultRealm:        defaultRealm,
		Upstream:            up,
		APIKey:              cfg.APIKey,
		Session:             initialRuntime.Session,
		StickyCount:         sessCount,
		RedisMode:           redisMode,
		SoftCooldown:        initialRuntime.SoftCooldown,
		PromptMode:          cfg.Prompt.Mode,
		PromptText:          cfg.PromptText,
		MaxBodyBytes:        initialRuntime.MaxBodyBytes,
		ConsoleEnabled:      cfg.Console.Enabled,
		UsageLog:            usageLogger,
		RefreshBalanceAfter: initialRuntime.RefreshBalanceAfter,
		CalibrateInterval:   initialRuntime.CalibrateInterval,
		ReloadAuths:         reloadAuths,
		AuthDir:             cfg.AuthDir,
	})
	runtime.handler, runtime.logger = h, usageLogger
	h.ApplyRuntime(initialRuntime)
	settingsStore.SetApplier(runtime.prepare)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		for _, p := range runtime.poolsSnapshot() {
			p.Flush() // 信号触发：先落盘再做优雅停机
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// 系统提示词：网关不内置任何提示词。这里只报「实际生效的注入方式」，
	// 避免"以为注入了 / 以为没注入"的排障黑洞。
	switch {
	case cfg.Prompt.Mode == "custom" && cfg.PromptText != "":
		log.Printf("系统提示词：custom → 用 %s 的内容（%d 字符）替换客户端 system/developer", cfg.Prompt.File, len([]rune(cfg.PromptText)))
	case cfg.Prompt.Mode == "custom":
		log.Printf("系统提示词：custom 但 prompt.file 为空 → 不注入任何提示词（等价 passthrough）")
	default:
		log.Printf("系统提示词：passthrough → 原样透传客户端请求体，网关不注入任何提示词")
	}
	log.Printf("workbuddy2api listening on %s (auth=managed, default_realm=%s, realms=%v, console=%v)",
		cfg.Listen, defaultRealm, realmList, cfg.Console.Enabled)
	if len(routeRealms) > 0 {
		log.Printf("启用路线别名：%v（复用来源线账号池，出站 base/归因按别名线档案）", routeRealms)
	}
	log.Printf("控制台：http://127.0.0.1%s/admin", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// migrateLegacyStateFile realm 改名后的历史状态文件迁移（幂等：新文件已存在就什么都不做）。
// 只复制不删除 —— 历史文件留着当备份，确认无误后可自行清理。
func migrateLegacyStateFile(cfg *Config, rn string) {
	legacy := cfg.LegacyStateFileFor(rn)
	if legacy == "" {
		return
	}
	cur := cfg.StateFileFor(rn)
	if cur == "" || cur == legacy {
		return
	}
	if _, err := os.Stat(cur); err == nil {
		return // 新文件已在，无需迁移
	}
	src, err := os.ReadFile(legacy)
	if err != nil {
		return // 没有历史文件（全新部署）
	}
	if err := os.WriteFile(cur, src, 0o600); err != nil {
		log.Printf("WARN: realm %s: 历史状态文件迁移失败（%v），本次从空状态启动", rn, err)
		return
	}
	log.Printf("realm %s: 已把历史状态文件 %s 迁移为 %s（realm 改名 %s → %s）", rn, legacy, cur, realm.AI, realm.WB)
}
