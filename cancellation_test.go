package cancellationlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func logObservation(t *testing.T, worker *Worker) {
	t.Helper()
	t.Logf("\n--- cancellation observation ---\n%s\n-------------------------------", worker.Recorder.Format())
}

func TestClientCancellationStopsWorker(t *testing.T) {
	worker := NewWorker(250 * time.Millisecond)
	t.Cleanup(func() { logObservation(t, worker) })
	handler := NewHandler(worker)

	clientCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/reports", nil).WithContext(clientCtx)

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	select {
	case <-worker.Started:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("重い処理が開始されませんでした")
	}

	cancel()

	select {
	case err := <-handler.RequestCanceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request.Context().Err() = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("HTTPリクエストのキャンセルを観測できませんでした")
	}

	select {
	case <-handlerDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ハンドラが終了しませんでした")
	}

	select {
	case <-worker.Finished:
		if err := <-worker.Result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Worker.Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("クライアントをキャンセルしたのに、重い処理のgoroutineが終了しませんでした")
	}
}

func TestTimeoutStopsWorker(t *testing.T) {
	worker := NewWorker(250 * time.Millisecond)
	t.Cleanup(func() { logObservation(t, worker) })
	handler := NewHandler(worker)

	clientCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/reports", nil).WithContext(clientCtx)

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	select {
	case <-worker.Started:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("重い処理が開始されませんでした")
	}

	select {
	case err := <-handler.RequestCanceled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request.Context().Err() = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("HTTPリクエストのタイムアウトを観測できませんでした")
	}

	select {
	case <-handlerDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ハンドラが終了しませんでした")
	}

	select {
	case <-worker.Finished:
		if err := <-worker.Result; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Worker.Run() error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("タイムアウトしたのに、重い処理のgoroutineが終了しませんでした")
	}
}
