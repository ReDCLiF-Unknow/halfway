package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// hub fans "something changed" signals out to the browsers that are connected
// to an event stream. It carries no data: a signal just tells the page to
// fetch itself again, so what each viewer sees is always decided by the
// normal access checks.
//
// Streams are keyed by who is watching: a change to a poll signals everyone
// on it, which covers the poll's page, their sidebars and their lists.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	// closing is closed when the server shuts down, which ends every stream.
	closing   chan struct{}
	closeOnce sync.Once
}

type subscriber struct {
	user int64
	ch   chan struct{} // buffered(1): bursts of changes coalesce into one signal
}

// maxStreams is how many live-update connections one person may hold open at
// once. Each is a goroutine and a connection for as long as it lasts, so
// without a cap one token could open thousands. This is more tabs and devices
// than anybody uses; a page over the limit still works, it just stops updating
// by itself until another tab is closed.
const maxStreams = 16

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}, closing: make(chan struct{})} }

// close ends every open stream. Streams never finish by themselves, so a
// graceful shutdown would otherwise wait for them until it gave up.
func (h *hub) close() { h.closeOnce.Do(func() { close(h.closing) }) }

// subscribe registers a stream for user, or returns nil if they already have
// maxStreams open.
func (h *hub) subscribe(user int64) *subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	open := 0
	for s := range h.subs {
		if s.user == user {
			open++
		}
	}
	if open >= maxStreams {
		return nil
	}
	s := &subscriber{user: user, ch: make(chan struct{}, 1)}
	h.subs[s] = struct{}{}
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// publish signals these people. It never blocks.
func (h *hub) publish(users []int64) {
	want := make(map[int64]bool, len(users))
	for _, u := range users {
		want[u] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if want[s.user] {
			select {
			case s.ch <- struct{}{}:
			default: // a signal is already pending
			}
		}
	}
}

// stream sends change signals for user to one browser as server-sent events.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, user int64) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub := s.hub.subscribe(user)
	if sub == nil {
		// A browser's EventSource gives up for good on an error status rather
		// than retrying, so this does not turn into a reconnect storm.
		http.Error(w, "too many open live-update connections", http.StatusTooManyRequests)
		return
	}
	defer s.hub.unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // don't let a reverse proxy buffer the stream

	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	ping := time.NewTicker(25 * time.Second) // keeps idle connections open
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.closing:
			return
		case <-sub.ch:
			fmt.Fprint(w, "event: changed\ndata: {}\n\n")
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		fl.Flush()
	}
}
