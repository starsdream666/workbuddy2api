// 额度巡检（credit watch）：定期查上游余额，把「余额为 0」的账号冻结、
// 「余额恢复」的账号解冻。
//
// 为什么需要它（而不是只靠请求撞 ErrHardCredit）：
//   - 被动路径只在账号**被选中并失败**后才冻结，那一次注定白跑；
//   - 解冻也需要它：冻结的账号已移出轮转，没有请求会再去问它的余额，
//     必须有人主动复查（签到/套餐周期刷新后余额恢复才有意义）。
//
// 与签到任务的分工：签到是"到点去领额度"，巡检是"持续确认额度状态"。
// 签到成功后也会走同一条 ReconcileCredits 解冻路径（见 pool.ReconcileCredits）。
package scheduler

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/logfmt"
)

// creditWatchAccountDelay 账号间限速：避免一次性对上游打出一串余额查询。
// 余额查询是只读轻接口，间隔取小值即可。测试可置 0。
var creditWatchAccountDelay = 300 * time.Millisecond

// creditWatchLoop 巡检循环：先立即全量巡检一次（让开局状态正确），再按间隔复查。
func (s *Scheduler) creditWatchLoop(ctx context.Context) {
	// Only process startup performs the eager full scan, never a settings update.
	if cfg := s.CurrentConfig(); s.cfg.CreditWatchEnabled && cfg.CreditWatchEnabled {
		if err := s.lockOperations(ctx); err != nil {
			return
		}
		if s.CurrentConfig().CreditWatchEnabled {
			s.runCreditWatch(true)
		}
		s.unlockOperations()
	}
	for {
		generation := s.runtime.Load()
		cfg := s.cfg
		if generation != nil {
			cfg = *generation
		}
		if !cfg.CreditWatchEnabled {
			select {
			case <-ctx.Done():
				return
			case <-s.creditWake:
				continue
			}
		}
		iv := cfg.CreditWatchInterval
		if iv <= 0 {
			iv = defaultCreditWatchInterval
		}
		t := time.NewTimer(iv)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.creditWake:
			t.Stop()
			continue
		case <-t.C:
			if err := s.lockOperations(ctx); err != nil {
				return
			}
			if sameCreditSchedule(cfg, s.CurrentConfig()) {
				s.runCreditWatch(false)
			}
			s.unlockOperations()
		}
	}
}

// RunCreditWatchNow 立即执行一轮额度巡检。
//
// forceAll=true 时无视配置范围强制全量（启动首轮用：开局就把余额为 0 的号冻结，
// 免得第一个请求撞上去）；否则按 CreditWatchScope 决定范围。
//
// 返回 (冻结数, 解冻数)，供测试断言。
func (s *Scheduler) RunCreditWatchNow(forceAll bool) (frozeN, unfrozeN int) {
	s.lockOperations(context.Background())
	defer s.unlockOperations()
	return s.runCreditWatch(forceAll)
}

func (s *Scheduler) runCreditWatch(forceAll bool) (frozeN, unfrozeN int) {
	pl := s.cfg.Pool
	if pl == nil {
		return 0, 0
	}
	var uids []string
	if forceAll || s.CurrentConfig().CreditWatchScope == "all" {
		for _, st := range pl.List() {
			if st.Stopped() {
				continue
			}
			uids = append(uids, st.UID)
		}
	} else {
		uids = pl.FrozenUIDs()
	}
	if len(uids) == 0 {
		return 0, 0
	}
	for i, uid := range uids {
		if i > 0 && creditWatchAccountDelay > 0 {
			time.Sleep(creditWatchAccountDelay)
		}
		a := pl.AuthByUID(uid)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		remain, err := s.CurrentConfig().Upstream.UserResource(a)
		if err != nil {
			// 查询失败不改状态：宁可保持现状，也不要因一次网络抖动把账号解冻。
			log.Printf("WARN: [scheduler] credit-watch %s: 余额查询失败: %v", logfmt.UID8(uid), err)
			continue
		}
		froze, unfroze := pl.ReconcileCredits(uid, remain)
		if froze {
			frozeN++
			log.Printf("credit-watch %s: 余额 %d → 冻结（等额度恢复后自动解冻）", logfmt.UID8(uid), remain)
		}
		if unfroze {
			unfrozeN++
			log.Printf("credit-watch %s: 余额 %d → 解冻", logfmt.UID8(uid), remain)
		}
	}
	if frozeN > 0 || unfrozeN > 0 {
		log.Printf("credit-watch: 巡检 %d 个账号 → 冻结 %d，解冻 %d", len(uids), frozeN, unfrozeN)
	}
	return frozeN, unfrozeN
}
