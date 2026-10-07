package scheduler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/taskqueue"
	"workbuddy2api/internal/upstream"
)

func TestMaintenanceBalancesAndErrors(test *testing.T) {
	var calls atomic.Int32
	stub := balanceStub(test, map[string]int64{"account": 123}, &calls)
	accountPool := creditWatchPool("account")
	worker := New(Config{Pool: accountPool, Upstream: &upstream.Client{HTTP: stub.Client(), BillingBaseCN: stub.URL}})
	var progress taskqueue.Progress
	if err := worker.RunMaintenance(context.Background(), "balance", func(value taskqueue.Progress) { progress = value }); err != nil {
		test.Fatal(err)
	}
	if progress.Total != 1 || progress.Succeeded != 1 || calls.Load() != 1 {
		test.Fatalf("progress=%+v calls=%d", progress, calls.Load())
	}
	status, _ := accountPool.Status("account")
	if !status.CreditsKnown || status.Credits != 123 {
		test.Fatal("balance not reconciled")
	}
	if err := worker.RunMaintenance(context.Background(), "growth", nil); err == nil || calls.Load() != 1 {
		test.Fatal("growth operation accepted")
	}
	worker.cfg.Upstream.HTTP = &http.Client{Transport: maintenanceTransport(func(request *http.Request) (*http.Response, error) { return nil, errors.New("network failed") })}
	if err := worker.RunMaintenance(context.Background(), "balance", func(value taskqueue.Progress) { progress = value }); err == nil || progress.Failed != 1 {
		test.Fatal("failure reported as success")
	}
}

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (transport maintenanceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestMaintenanceCancellationAndScheduledExclusion(test *testing.T) {
	started := make(chan struct{})
	stub := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		close(started)
		<-request.Context().Done()
	}))
	defer stub.Close()
	worker := New(Config{Pool: creditWatchPool("account"), Upstream: &upstream.Client{HTTP: stub.Client(), BillingBaseCN: stub.URL}})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- worker.RunMaintenance(ctx, "balance", nil) }()
	<-started
	waitingCtx, waitingCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer waitingCancel()
	if err := worker.lockOperations(waitingCtx); !errors.Is(err, context.DeadlineExceeded) {
		test.Fatal("scheduled operations not excluded")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			test.Fatalf("cancel err=%v", err)
		}
	case <-time.After(time.Second):
		test.Fatal("upstream cancellation not propagated")
	}
}

func TestMaintenanceRefreshPersistsAndSkipsDisabled(test *testing.T) {
	accountPool := creditWatchPool("active", "disabled")
	accountPool.SetManualDisabled("disabled", true)
	accountPool.AuthByUID("active").FilePath = filepath.Join(test.TempDir(), "active.json")
	var calls atomic.Int32
	client := &upstream.Client{HTTP: &http.Client{Transport: maintenanceTransport(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`))}, nil
	})}}
	worker := New(Config{Pool: accountPool, Upstream: client})
	var progress taskqueue.Progress
	if err := worker.RunMaintenance(context.Background(), "refresh_tokens", func(value taskqueue.Progress) { progress = value }); err != nil {
		test.Fatal(err)
	}
	if calls.Load() != 1 || progress.Succeeded != 1 || progress.Skipped != 1 {
		test.Fatalf("progress=%+v calls=%d", progress, calls.Load())
	}
	raw, err := os.ReadFile(accountPool.AuthByUID("active").FilePath)
	if err != nil || !strings.Contains(string(raw), "new-refresh") {
		test.Fatal("rotated token not persisted")
	}
}
