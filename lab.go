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
	duration   time.Duration
	started    chan struct{}
	finished   chan struct{}
	cancelled  chan struct{}
	startOnce  sync.Once
	endOnce    sync.Once
	cancelOnce sync.Once
}

// NewSlowStore は観測可能な重い処理を作成します。
func NewSlowStore(duration time.Duration) *SlowStore {
	return &SlowStore{
		duration:  duration,
		started:   make(chan struct{}),
		finished:  make(chan struct{}),
		cancelled: make(chan struct{}),
	}
}

// Load はContextの完了または疑似DB待機の完了を待ちます。
func (s *SlowStore) Load(ctx context.Context, events *EventLog) error {
	s.startOnce.Do(func() { close(s.started) })
	events.Add("store: start ctx_err=%v", ctx.Err())

	select {
	case <-ctx.Done():
		s.cancelOnce.Do(func() { close(s.cancelled) })
		events.Add("store: ctx.Done ctx_err=%v", ctx.Err())
		return ctx.Err()
	case <-time.After(s.duration):
		s.endOnce.Do(func() { close(s.finished) })
		events.Add("store: completed ctx_err=%v", ctx.Err())
		return nil
	}
}

// Started は下位処理が開始したことを知らせます。
func (s *SlowStore) Started() <-chan struct{} { return s.started }

// Finished は下位処理が通常完了したことを知らせます。
func (s *SlowStore) Finished() <-chan struct{} { return s.finished }

// Cancelled は下位処理がctx.Doneを受け取ったことを知らせます。
func (s *SlowStore) Cancelled() <-chan struct{} { return s.cancelled }

// API はHTTPハンドラと下位処理を束ねます。
type API struct {
	store             *SlowStore
	events            *EventLog
	requestCancelled  chan struct{}
	requestCancelOnce sync.Once
}

// NewAPI は不具合を含むAPIを作成します。
func NewAPI(store *SlowStore, events *EventLog) *API {
	return &API{
		store:            store,
		events:           events,
		requestCancelled: make(chan struct{}),
	}
}

// ServeHTTP は意図的にrequest.Contextを下位へ渡さない不具合を含みます。
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestCtx := r.Context()
	a.events.Add("handler: start request_ctx_err=%v", requestCtx.Err())

	go func() {
		<-requestCtx.Done()
		a.requestCancelOnce.Do(func() { close(a.requestCancelled) })
		a.events.Add("handler: request ctx.Done request_ctx_err=%v", requestCtx.Err())
	}()

	// 不具合: 要求由来のContextを捨てるため、クライアントの中断や期限切れが下位へ届かない。
	err := a.store.Load(context.Background(), a.events)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("done\n"))
}

// RequestCancelled はHTTP要求のContextが完了したことを知らせます。
func (a *API) RequestCancelled() <-chan struct{} { return a.requestCancelled }

// Events は状態遷移ログを返します。
func (a *API) Events() []string { return a.events.Events() }
