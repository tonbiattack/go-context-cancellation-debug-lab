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
	Recorder *Recorder
}

// NewWorker はテストで観測できるWorkerを作成します。
func NewWorker(duration time.Duration) *Worker {
	return &Worker{
		Duration: duration,
		Started:  make(chan struct{}),
		Finished: make(chan struct{}),
		Result:   make(chan error, 1),
		Recorder: NewRecorder(),
	}
}

// Run は重い処理を実行します。
// DBや外部APIの待機に相当する処理とctx.Done()を同じselectで待ちます。
func (w *Worker) Run(ctx context.Context) error {
	w.Recorder.Record("worker: goroutine started", ctx)
	close(w.Started)
	defer close(w.Finished)

	w.Recorder.Record("worker: simulated DB call started", ctx)
	timer := time.NewTimer(w.Duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		err := ctx.Err()
		w.Recorder.Record("worker: ctx.Done observed; goroutine exits", ctx)
		w.Result <- err
		return err
	case <-timer.C:
		w.Recorder.Record("worker: simulated DB call completed", ctx)
		w.Result <- nil
		return nil
	}
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
// request.Context()を下位処理へそのまま渡すため、HTTPのキャンセルが連鎖します。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestCtx := r.Context()
	h.Worker.Recorder.Record("http: handler entered", requestCtx)

	go func() {
		h.Worker.Recorder.Record("worker: launched with request.Context", requestCtx)
		_ = h.Worker.Run(requestCtx)
	}()

	<-h.Worker.Started
	<-requestCtx.Done()
	h.Worker.Recorder.Record("http: request context Done observed", requestCtx)
	h.RequestCanceled <- requestCtx.Err()
}
