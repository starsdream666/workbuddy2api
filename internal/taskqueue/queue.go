package taskqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var ErrFull = errors.New("task queue is full")
var ErrClosed = errors.New("task queue is closed")

type Progress struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type Task struct {
	ID         string     `json:"id"`
	Realm      string     `json:"realm"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Progress   Progress   `json:"progress"`
	Error      string     `json:"error,omitempty"`
}

type Runner func(context.Context, func(Progress)) error

type job struct {
	task   Task
	run    Runner
	ctx    context.Context
	cancel context.CancelFunc
}

type Queue struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	wake              chan struct{}
	done              chan struct{}
	jobs              []*job
	pending           []*job
	capacity, history int
}

func New(parent context.Context, capacity, history int) *Queue {
	if capacity <= 0 {
		capacity = 64
	}
	if history < capacity {
		history = capacity
	}
	ctx, cancel := context.WithCancel(parent)
	queue := &Queue{ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), capacity: capacity, history: history}
	go queue.work()
	return queue
}

func active(status string) bool {
	return status == "queued" || status == "running" || status == "cancelling"
}

func (queue *Queue) Enqueue(scope, kind string, run Runner) (Task, bool, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.ctx.Err() != nil {
		return Task{}, false, ErrClosed
	}
	if run == nil || scope == "" || kind == "" {
		return Task{}, false, errors.New("invalid task")
	}
	count := 0
	for _, current := range queue.jobs {
		if !active(current.task.Status) {
			continue
		}
		if current.task.Realm == scope && current.task.Kind == kind {
			return current.task, true, nil
		}
		count++
	}
	if count >= queue.capacity {
		return Task{}, false, ErrFull
	}
	if len(queue.jobs) >= queue.history {
		for index, current := range queue.jobs {
			if !active(current.task.Status) {
				queue.jobs = append(queue.jobs[:index], queue.jobs[index+1:]...)
				break
			}
		}
	}
	var identifier [12]byte
	if _, err := rand.Read(identifier[:]); err != nil {
		return Task{}, false, errors.New("cannot generate task ID")
	}
	ctx, cancel := context.WithCancel(queue.ctx)
	current := &job{task: Task{ID: hex.EncodeToString(identifier[:]), Realm: scope, Kind: kind, Status: "queued", CreatedAt: time.Now()}, ctx: ctx, cancel: cancel, run: run}
	queue.jobs = append(queue.jobs, current)
	queue.pending = append(queue.pending, current)
	select {
	case queue.wake <- struct{}{}:
	default:
	}
	return current.task, false, nil
}

func (queue *Queue) List() []Task {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	result := make([]Task, 0, len(queue.jobs))
	for index := len(queue.jobs) - 1; index >= 0; index-- {
		result = append(result, queue.jobs[index].task)
	}
	return result
}

func (queue *Queue) Cancel(id string) (Task, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for _, current := range queue.jobs {
		if current.task.ID != id {
			continue
		}
		if active(current.task.Status) {
			current.cancel()
			if current.task.Status == "queued" {
				queue.finish(current, "cancelled", "")
				for index, pending := range queue.pending {
					if pending == current {
						queue.pending = append(queue.pending[:index], queue.pending[index+1:]...)
						break
					}
				}
			} else {
				current.task.Status = "cancelling"
			}
		}
		return current.task, true
	}
	return Task{}, false
}

func (queue *Queue) Close() { queue.cancel(); <-queue.done }

func (queue *Queue) finish(current *job, status, message string) {
	now := time.Now()
	current.task.Status, current.task.Error, current.task.FinishedAt = status, message, &now
	current.cancel()
	current.run = nil
}

func (queue *Queue) work() {
	defer close(queue.done)
	defer func() {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		for _, current := range queue.pending {
			queue.finish(current, "cancelled", "")
		}
		queue.pending = nil
	}()
	for {
		select {
		case <-queue.ctx.Done():
			return
		case <-queue.wake:
		}
		for {
			queue.mu.Lock()
			if queue.ctx.Err() != nil || len(queue.pending) == 0 {
				queue.mu.Unlock()
				break
			}
			current := queue.pending[0]
			queue.pending = queue.pending[1:]
			now := time.Now()
			current.task.Status, current.task.StartedAt = "running", &now
			queue.mu.Unlock()
			ctx, cancel := context.WithTimeout(current.ctx, 30*time.Minute)
			err := invoke(ctx, current.run, func(progress Progress) {
				queue.mu.Lock()
				current.task.Progress = progress
				queue.mu.Unlock()
			})
			queue.mu.Lock()
			switch {
			case current.ctx.Err() != nil:
				queue.finish(current, "cancelled", "")
			case ctx.Err() != nil:
				queue.finish(current, "failed", "task deadline exceeded")
			case err != nil:
				queue.finish(current, "failed", "operation failed; see progress counts")
			default:
				queue.finish(current, "succeeded", "")
			}
			queue.mu.Unlock()
			cancel()
		}
	}
}

func invoke(ctx context.Context, run Runner, report func(Progress)) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("task panicked")
		}
	}()
	return run(ctx, report)
}
