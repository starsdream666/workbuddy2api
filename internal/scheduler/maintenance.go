package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"workbuddy2api/internal/taskqueue"
	"workbuddy2api/internal/upstream"
)

func (scheduler *Scheduler) lockOperations(ctx context.Context) error {
	scheduler.operationsOnce.Do(func() { scheduler.operations = make(chan struct{}, 1) })
	select {
	case scheduler.operations <- struct{}{}:
		if err := ctx.Err(); err != nil {
			scheduler.unlockOperations()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (scheduler *Scheduler) unlockOperations() { <-scheduler.operations }

func (scheduler *Scheduler) RunMaintenance(ctx context.Context, kind string, report func(taskqueue.Progress)) error {
	if kind != "balance" && kind != "refresh_tokens" {
		return errors.New("unsupported maintenance operation")
	}
	if scheduler.cfg.Pool == nil || scheduler.CurrentConfig().Upstream == nil {
		return errors.New("maintenance dependencies unavailable")
	}
	if err := scheduler.lockOperations(ctx); err != nil {
		return err
	}
	defer scheduler.unlockOperations()
	accounts := scheduler.cfg.Pool.List()
	progress := taskqueue.Progress{Total: len(accounts)}
	if report != nil {
		report(progress)
	}
	for index, status := range accounts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if index > 0 {
			timer := time.NewTimer(300 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		account := scheduler.cfg.Pool.AuthByUID(status.UID)
		current, found := scheduler.cfg.Pool.Status(status.UID)
		if !found || current.Stopped() || account == nil || (kind == "balance" && account.AccessTokenValue() == "") || (kind == "refresh_tokens" && account.RefreshTokenValue() == "") {
			progress.Skipped++
		} else {
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			var err error
			if kind == "balance" {
				var remain int64
				remain, err = scheduler.CurrentConfig().Upstream.UserResourceContext(callCtx, account)
				if err == nil {
					scheduler.cfg.Pool.ReconcileCredits(status.UID, remain)
				}
			} else {
				err = scheduler.CurrentConfig().Upstream.RefreshTokenContext(callCtx, account)
				if err == nil {
					scheduler.cfg.Pool.ClearSessionDead(status.UID)
					err = account.SaveAtomic()
				} else if callCtx.Err() == nil {
					var upstreamErr *upstream.Error
					if errors.As(err, &upstreamErr) && upstreamErr.Kind == upstream.ErrSessionDead {
						scheduler.cfg.Pool.NoteSessionDead(status.UID)
					}
				}
			}
			cancel()
			if err != nil {
				progress.Failed++
			} else {
				progress.Succeeded++
			}
		}
		if report != nil {
			report(progress)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if progress.Failed > 0 {
		return fmt.Errorf("%d accounts failed maintenance", progress.Failed)
	}
	return ctx.Err()
}
