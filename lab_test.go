package cancellationlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientCancellationStopsDownstreamWork(t *testing.T) {
	t.Parallel()

	events := &EventLog{}
	store := NewSlowStore(250 * time.Millisecond)
	api := NewAPI(store, events)
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		t.Logf("observed events:\n%s", strings.Join(api.Events(), "\n"))
	})

	clientCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(clientCtx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	requestResult := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		requestResult <- err
	}()

	waitFor(t, "下位処理の開始", store.Started(), 100*time.Millisecond)
	cancel()

	select {
	case err := <-requestResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client request error = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("クライアント側のキャンセル後もHTTP呼び出しが戻りません")
	}

	waitFor(t, "サーバーのrequest.Context().Done()", api.RequestCancelled(), 100*time.Millisecond)
	waitFor(t, "下位処理のctx.Done()", store.Cancelled(), 100*time.Millisecond)
	waitFor(t, "下位処理goroutineの終了", store.WorkerExited(), 100*time.Millisecond)
	if !errors.Is(store.ResultErr(), context.Canceled) {
		t.Fatalf("store result error = %v, want context.Canceled", store.ResultErr())
	}
}

func TestRequestTimeoutStopsDownstreamWork(t *testing.T) {
	t.Parallel()

	events := &EventLog{}
	store := NewSlowStore(250 * time.Millisecond)
	api := NewAPIWithTimeout(store, events, 40*time.Millisecond)
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		t.Logf("observed timeout events:\n%s", strings.Join(api.Events(), "\n"))
	})

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body = %q, want %d", resp.StatusCode, body, http.StatusGatewayTimeout)
	}

	waitFor(t, "下位処理の開始", store.Started(), 100*time.Millisecond)
	waitFor(t, "タイムアウトした下位処理のctx.Done()", store.Cancelled(), 100*time.Millisecond)
	waitFor(t, "タイムアウトした下位処理goroutineの終了", store.WorkerExited(), 100*time.Millisecond)
	if !errors.Is(store.ResultErr(), context.DeadlineExceeded) {
		t.Fatalf("store result error = %v, want context.DeadlineExceeded", store.ResultErr())
	}
	select {
	case <-store.Finished():
		t.Fatal("タイムアウト後に下位処理が通常完了しました")
	default:
	}
}

func waitFor(t *testing.T, name string, done <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("%s を %s 以内に観測できませんでした", name, timeout)
	}
}
