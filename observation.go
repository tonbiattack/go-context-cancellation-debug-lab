package cancellationlab

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Event はキャンセル伝播を追跡するための観測値です。
type Event struct {
	At         time.Time
	Name       string
	ContextErr error
	DoneIsNil  bool
	Goroutines int
}

// Recorder は並行処理から安全に観測値を収集します。
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// NewRecorder は空の観測レコーダーを作成します。
func NewRecorder() *Recorder {
	return &Recorder{}
}

// Record はコンテキストとgoroutine数を記録します。
func (r *Recorder) Record(name string, ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, Event{
		At:         time.Now(),
		Name:       name,
		ContextErr: ctx.Err(),
		DoneIsNil:  ctx.Done() == nil,
		Goroutines: runtime.NumGoroutine(),
	})
}

// Snapshot は記録済みイベントのコピーを返します。
func (r *Recorder) Snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	events := make([]Event, len(r.events))
	copy(events, r.events)
	return events
}

// Format はテストログへ出力する読みやすい観測結果を返します。
func (r *Recorder) Format() string {
	events := r.Snapshot()
	if len(events) == 0 {
		return "観測イベントはありません"
	}

	base := events[0].At
	lines := make([]string, 0, len(events))
	for _, event := range events {
		lines = append(lines, fmt.Sprintf(
			"+%3dms %-43s ctx.Err=%-26v Doneがnil=%-5t goroutines=%d",
			event.At.Sub(base).Milliseconds(),
			event.Name,
			event.ContextErr,
			event.DoneIsNil,
			event.Goroutines,
		))
	}
	return strings.Join(lines, "\n")
}
