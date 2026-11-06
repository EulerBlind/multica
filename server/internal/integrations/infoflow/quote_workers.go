package infoflow

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// defaultQuoteWorkers bounds how many conversations are processed at once.
// DEC7 (N81 R66): the quoted-message lookup used to run inside the WebSocket read
// loop, so two back-to-back events accumulated their lookups (ack1=2.80s,
// ack2=5.60s). Delivery now runs here instead, and the read loop only enqueues.
const defaultQuoteWorkers = 4

// quoteWorkers is a bounded pool with a per-conversation FIFO. Two workers never
// process the same chat at once, so messages of one conversation keep their
// arrival order even though delivery is asynchronous.
type quoteWorkers struct {
	ctx    context.Context
	logger *slog.Logger
	notify chan struct{}

	mu     sync.Mutex
	order  []string
	queues map[string][]func()
	active map[string]bool
	closed bool
}

func newQuoteWorkers(ctx context.Context, size int, logger *slog.Logger) *quoteWorkers {
	if size <= 0 {
		size = defaultQuoteWorkers
	}
	if logger == nil {
		logger = slog.Default()
	}
	workers := &quoteWorkers{
		ctx:    ctx,
		logger: logger,
		notify: make(chan struct{}, 1),
		queues: make(map[string][]func()),
		active: make(map[string]bool),
	}
	for i := 0; i < size; i++ {
		go workers.loop()
	}
	return workers
}

// enqueue schedules one delivery under its conversation key. It never blocks the
// caller: the queue lives in memory and one token wakes an idle worker.
func (w *quoteWorkers) enqueue(chatID string, task func()) {
	if w == nil || task == nil {
		return
	}
	if w.ctx.Err() != nil {
		return
	}
	key := strings.TrimSpace(chatID)
	if key == "" {
		// A payload without a chat id still has to be delivered; a shared queue
		// keeps its relative order with other such payloads.
		key = "_"
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if _, ok := w.queues[key]; !ok {
		w.order = append(w.order, key)
	}
	w.queues[key] = append(w.queues[key], task)
	w.mu.Unlock()

	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// stop prevents further work from being queued. In-flight tasks finish; queued
// tasks are dropped with the connection they belong to.
func (w *quoteWorkers) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

func (w *quoteWorkers) loop() {
	for {
		key, tasks, ok := w.take()
		if ok {
			for _, task := range tasks {
				task()
			}
			w.finish(key)
			continue
		}
		select {
		case <-w.ctx.Done():
			w.stop()
			return
		case <-w.notify:
		}
	}
}

// take claims the oldest conversation that has pending work and is not already
// being processed. The whole pending batch is returned so one worker keeps the
// conversation's order without re-queueing between messages.
func (w *quoteWorkers) take() (string, []func(), bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := 0; i < len(w.order); i++ {
		key := w.order[i]
		tasks := w.queues[key]
		if len(tasks) == 0 {
			delete(w.queues, key)
			w.order = append(w.order[:i], w.order[i+1:]...)
			i--
			continue
		}
		if w.active[key] {
			continue
		}
		w.active[key] = true
		w.order = append(w.order[:i], w.order[i+1:]...)
		delete(w.queues, key)
		return key, tasks, true
	}
	return "", nil, false
}

// finish releases the conversation. Work queued while it was active is appended
// behind the other conversations so a busy chat cannot starve the rest.
func (w *quoteWorkers) finish(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queues[key]) == 0 {
		delete(w.queues, key)
		delete(w.active, key)
		return
	}
	w.active[key] = false
	if !w.queuedLocked(key) {
		w.order = append(w.order, key)
	}
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// queuedLocked reports whether the key is already waiting in the schedule.
func (w *quoteWorkers) queuedLocked(key string) bool {
	for _, queued := range w.order {
		if queued == key {
			return true
		}
	}
	return false
}
