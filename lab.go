package cancellationlab

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// EventLog は、リクエストと下位処理の状態をテストから観測するための記録器です。
type EventLog struct {
	mu     sync.Mutex
	events []string
}

// Add はイベントを記録します。
func (l *EventLog) Add(format string, args ...any) {
	event := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
	log.Printf("%s", event)
}

// Events は記録済みのイベントをコピーして返します。
func (l *EventLog) Events() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// SlowStore はDBや外部APIの待機を模した、Contextを受け取る下位処理です。
type SlowStore struct {
	duration       time.Duration
	started        chan struct{}
	finished       chan struct{}
	cancelled      chan struct{}
	workerExited   chan struct{}
	startOnce      sync.Once
	finishOnce     sync.Once
	cancelOnce     sync.Once
	workerExitOnce sync.Once
	resultMu       sync.Mutex
	resultErr      error
}

// NewSlowStore は観測可能な重い処理を作成します。
func NewSlowStore(duration time.Duration) *SlowStore {
	return &SlowStore{
		duration:     duration,
		started:      make(chan struct{}),
		finished:     make(chan struct{}),
		cancelled:    make(chan struct{}),
		workerExited: make(chan struct{}),
	}
}

// Load は別goroutineで疑似DB待機を実行し、Contextの完了または通常完了を返します。
func (s *SlowStore) Load(ctx context.Context, events *EventLog) error {
	s.startOnce.Do(func() { close(s.started) })
	events.Add("store: start ctx_err=%v", ctx.Err())

	result := make(chan error, 1)
	go func() {
		defer s.workerExitOnce.Do(func() { close(s.workerExited) })

		timer := time.NewTimer(s.duration)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			err := ctx.Err()
			s.cancelOnce.Do(func() { close(s.cancelled) })
			s.setResult(err)
			events.Add("store: ctx.Done ctx_err=%v", err)
			result <- err
		case <-timer.C:
			s.finishOnce.Do(func() { close(s.finished) })
			s.setResult(nil)
			events.Add("store: completed ctx_err=%v", ctx.Err())
			result <- nil
		}
	}()

	return <-result
}

func (s *SlowStore) setResult(err error) {
	s.resultMu.Lock()
	s.resultErr = err
	s.resultMu.Unlock()
}

// Started は下位処理が開始したことを知らせます。
func (s *SlowStore) Started() <-chan struct{} { return s.started }

// Finished は下位処理が通常完了したことを知らせます。
func (s *SlowStore) Finished() <-chan struct{} { return s.finished }

// Cancelled は下位処理がctx.Doneを受け取ったことを知らせます。
func (s *SlowStore) Cancelled() <-chan struct{} { return s.cancelled }

// WorkerExited は下位処理goroutineが終了したことを知らせます。
func (s *SlowStore) WorkerExited() <-chan struct{} { return s.workerExited }

// ResultErr は下位処理が最後に返したエラーを返します。
func (s *SlowStore) ResultErr() error {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	return s.resultErr
}

// API はHTTPハンドラと下位処理を束ねます。
type API struct {
	store             *SlowStore
	events            *EventLog
	workTimeout       time.Duration
	requestCancelled  chan struct{}
	requestCancelOnce sync.Once
}

// NewAPI は要求のContextをそのまま下位へ渡すAPIを作成します。
func NewAPI(store *SlowStore, events *EventLog) *API {
	return NewAPIWithTimeout(store, events, 0)
}

// NewAPIWithTimeout は要求の子Contextへ処理時間上限を設定するAPIを作成します。
func NewAPIWithTimeout(store *SlowStore, events *EventLog, workTimeout time.Duration) *API {
	return &API{
		store:            store,
		events:           events,
		workTimeout:      workTimeout,
		requestCancelled: make(chan struct{}),
	}
}

// ServeHTTP はrequest.Contextを下位処理まで伝播します。
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestCtx := r.Context()
	a.events.Add("handler: start request_ctx_err=%v", requestCtx.Err())

	go func() {
		<-requestCtx.Done()
		a.requestCancelOnce.Do(func() { close(a.requestCancelled) })
		a.events.Add("handler: request ctx.Done request_ctx_err=%v", requestCtx.Err())
	}()

	workCtx := requestCtx
	cancel := func() {}
	if a.workTimeout > 0 {
		workCtx, cancel = context.WithTimeout(requestCtx, a.workTimeout)
	}
	defer cancel()

	if err := a.store.Load(workCtx, a.events); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			http.Error(w, "processing timeout", http.StatusGatewayTimeout)
		case errors.Is(err, context.Canceled):
			// クライアントはすでに応答を待っていないため、書き込みを試みません。
		default:
			http.Error(w, "store failed", http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("done\n"))
}

// RequestCancelled はHTTP要求のContextが完了したことを知らせます。
func (a *API) RequestCancelled() <-chan struct{} { return a.requestCancelled }

// Events は状態遷移ログを返します。
func (a *API) Events() []string { return a.events.Events() }
