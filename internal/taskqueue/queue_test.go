package taskqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func awaitTask(test *testing.T, queue *Queue, id, status string) Task {
	test.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, task := range queue.List() {
			if task.ID == id && task.Status == status {
				return task
			}
		}
		time.Sleep(time.Millisecond)
	}
	test.Fatalf("task %s never reached %s: %+v", id, status, queue.List())
	return Task{}
}

func TestQueueDeduplicateBoundCancelAndContinue(test *testing.T) {
	queue := New(context.Background(), 2, 3)
	test.Cleanup(queue.Close)
	started := make(chan struct{})
	first, _, err := queue.Enqueue("cn", "balance", func(ctx context.Context, report func(Progress)) error { close(started); <-ctx.Done(); return ctx.Err() })
	if err != nil {
		test.Fatal(err)
	}
	<-started
	duplicate, reused, err := queue.Enqueue("cn", "balance", func(context.Context, func(Progress)) error { test.Error("duplicate executed"); return nil })
	if err != nil || !reused || duplicate.ID != first.ID {
		test.Fatal("deduplication failed")
	}
	second, _, err := queue.Enqueue("workbuddy", "balance", func(context.Context, func(Progress)) error { test.Error("cancelled queued job executed"); return nil })
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := queue.Enqueue("cn", "tokens", func(context.Context, func(Progress)) error { return nil }); !errors.Is(err, ErrFull) {
		test.Fatalf("capacity err=%v", err)
	}
	queue.Cancel(second.ID)
	awaitTask(test, queue, second.ID, "cancelled")
	third, _, err := queue.Enqueue("workbuddy", "balance", func(ctx context.Context, report func(Progress)) error {
		report(Progress{Total: 1, Succeeded: 1})
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	queue.Cancel(first.ID)
	awaitTask(test, queue, first.ID, "cancelled")
	finished := awaitTask(test, queue, third.ID, "succeeded")
	if finished.Progress.Succeeded != 1 || finished.StartedAt == nil || finished.FinishedAt == nil {
		test.Fatalf("incomplete progress=%+v", finished)
	}
	queue.Close()
	if _, _, err := queue.Enqueue("cn", "balance", func(context.Context, func(Progress)) error { return nil }); !errors.Is(err, ErrClosed) {
		test.Fatal("closed queue accepted job")
	}
}

func TestQueueFailureRecoveryAndHistory(test *testing.T) {
	queue := New(context.Background(), 1, 2)
	defer queue.Close()
	for index := 0; index < 5; index++ {
		current, _, err := queue.Enqueue("cn", "balance", func(context.Context, func(Progress)) error { panic("secret that must not escape") })
		if err != nil {
			test.Fatal(err)
		}
		failed := awaitTask(test, queue, current.ID, "failed")
		if failed.Error != "operation failed; see progress counts" {
			test.Fatal("panic leaked")
		}
	}
	if len(queue.List()) != 2 {
		test.Fatal("history is unbounded")
	}
}

func TestQueueShutdownCancelsPending(test *testing.T) {
	queue := New(context.Background(), 2, 2)
	started := make(chan struct{})
	first, _, _ := queue.Enqueue("cn", "balance", func(ctx context.Context, report func(Progress)) error { close(started); <-ctx.Done(); return ctx.Err() })
	<-started
	second, _, _ := queue.Enqueue("cn", "tokens", func(context.Context, func(Progress)) error { test.Error("pending task ran on shutdown"); return nil })
	queue.Close()
	awaitTask(test, queue, first.ID, "cancelled")
	awaitTask(test, queue, second.ID, "cancelled")
}
