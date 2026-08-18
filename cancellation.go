// Package cancellationlab は、HTTPリクエストのキャンセル伝播を検証する最小サンプルです。
package cancellationlab

import (
	"context"
	"net/http"
	"time"
)

// Worker はDBアクセスなどの重い下位処理を模擬します。
type Worker struct {
	Duration time.Duration
	Started  chan struct{}
	Finished chan struct{}
	Result   chan error
}

// NewWorker はテストで観測できるWorkerを作成します。
func NewWorker(duration time.Duration) *Worker {
	return &Worker{
		Duration: duration,
		Started:  make(chan struct{}),
		Finished: make(chan struct{}),
		Result:   make(chan error, 1),
	}
}

// Run は重い処理を実行します。
// この初期実装はctxを受け取るにもかかわらず、キャンセルを確認しません。
func (w *Worker) Run(ctx context.Context) error {
	close(w.Started)
	defer close(w.Finished)

	time.Sleep(w.Duration)
	w.Result <- nil
	return nil
}

// Handler は重い処理を起動するHTTP APIです。
type Handler struct {
	Worker          *Worker
	RequestCanceled chan error
}

// NewHandler はHTTPハンドラを作成します。
func NewHandler(worker *Worker) *Handler {
	return &Handler{
		Worker:          worker,
		RequestCanceled: make(chan error, 1),
	}
}

// ServeHTTP はクライアントの切断を待ちます。
// 不具合は、下位処理にrequest.Context()ではなくcontext.Background()を渡す点です。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	go func() {
		_ = h.Worker.Run(context.Background())
	}()

	<-h.Worker.Started
	<-r.Context().Done()
	h.RequestCanceled <- r.Context().Err()
}
