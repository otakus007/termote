package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// The push watcher sends a Web Push when a pane's agent becomes blocked or
// ends a turn, so a device hears of it with the PWA closed. The rule is the
// PWA's (pwa/src/utils/agent-notify.ts); both run the cases in
// testdata/agent-transitions.json.
const (
	pushWatchInterval = 5 * time.Second
	// pushQueueLen events wait for the sender; past it the oldest goes.
	pushQueueLen = 64
	// pushParallel requests to push services at a time.
	pushParallel = 4
	// pushSendTimeout bounds one request to a push service.
	pushSendTimeout = 5 * time.Second
	// pushDefaultBackoff keeps a push service that timed out, or answered
	// 429 without a Retry-After, from being asked again for a while.
	pushDefaultBackoff = time.Minute
	// pushLogEvery limits repeated log lines per push service.
	pushLogEvery = 10 * time.Second
)

// pushEvent is what a push carries: ids only, never names or screen text.
// The service worker reads the names itself from the snapshot.
type pushEvent struct {
	GroupID string `json:"groupId"`
	TabID   string `json:"tabId"`
	PaneID  string `json:"paneId"`
	Kind    string `json:"kind"`
}

type pendingTransition struct {
	kind           string
	since          time.Time
	heldByPresence bool
	event          pushEvent
}

// pushSendFunc sends one push (sendPush through the hardened client).
type pushSendFunc func(ctx context.Context, sub pushSub, payload []byte, topic string) (status int, retryAfter time.Duration, err error)

type pushWatcher struct {
	peek  func(ctx context.Context) (Snapshot, error)
	store *pushStore
	// activity is told every snapshot the tick reads (may be nil).
	activity *activityTracker
	send     pushSendFunc
	now      func() time.Time
	queue    chan pushEvent

	// prev and pending are owned by the tick loop.
	prev    map[string]string
	pending map[string]pendingTransition

	// mu guards lastSent, paneMeta, and the sender's per-host state.
	mu       sync.Mutex
	lastSent map[string]time.Time      // "<key>:<kind>" → last enqueued time
	paneMeta map[string]paneNotifyMeta // paneID → latest group/tab/agent names
	backoff  map[string]time.Time      // push service host → not asked before
	logged   map[string]time.Time      // host (or "*") → last log line
}

func newPushWatcher(peek func(context.Context) (Snapshot, error), store *pushStore, send pushSendFunc) *pushWatcher {
	w := &pushWatcher{
		peek:     peek,
		store:    store,
		send:     send,
		now:      time.Now,
		queue:    make(chan pushEvent, pushQueueLen),
		prev:     map[string]string{},
		pending:  map[string]pendingTransition{},
		lastSent: map[string]time.Time{},
		paneMeta: map[string]paneNotifyMeta{},
		backoff:  map[string]time.Time{},
		logged:   map[string]time.Time{},
	}
	if store != nil {
		store.setEmitExternal(w.EmitExternal)
	}
	return w
}

// startPushWatcher runs the watcher until ctx is done; nothing without a
// store. Its reads feed activity too.
func startPushWatcher(ctx context.Context, m Mux, store *pushStore, activity *activityTracker) *pushWatcher {
	if store == nil {
		return nil
	}
	client := pushHTTPClient()
	w := newPushWatcher(
		func(ctx context.Context) (Snapshot, error) { return peekSnapshot(ctx, m) },
		store,
		func(ctx context.Context, sub pushSub, payload []byte, topic string) (int, time.Duration, error) {
			return sendPush(ctx, client, store, sub, payload, topic)
		},
	)
	w.activity = activity
	activity.setEmitPush(w.EmitExternal)
	go w.run(ctx)
	go w.deliver(ctx)
	return w
}

// pushStatuses are the statuses that count as an observation; anything
// else (none, "unknown") keeps the last known one.
var pushStatuses = map[string]bool{"blocked": true, "working": true, "done": true, "idle": true}

// pushTransitionKind is the event a status change raises, if any. A first
// sighting raises nothing.
func pushTransitionKind(prev, next string) string {
	switch {
	case prev == "":
		return ""
	case next == "blocked" && prev != "blocked":
		return "blocked"
	case prev == "working" && (next == "done" || next == "idle"):
		return "done"
	}
	return ""
}

// foldPaneStatus is the status to remember for a pane whose agent is a,
// given the last known one: "" forgets the pane. A pane listed without an
// agent is forgotten, so the next agent there is a first sighting; an agent
// with no status (none, "unknown") keeps the last known one. The push watcher
// and the activity tracker both fold this way.
func foldPaneStatus(last string, a *AgentInfo) string {
	if a == nil {
		return ""
	}
	if pushStatuses[a.Status] {
		return a.Status
	}
	return last
}

// transitions returns the last known status of every pane in snap and the
// events since prev. A pane no longer listed, or listed without an agent, is
// forgotten (foldPaneStatus): a killed working agent and a new one's first
// idle raise no finished turn. Panes are known by their key (paneKey), so a
// tmux pane whose id a window move shifted is still the same pane, and never
// takes the status of the one that had the id before.
func transitions(prev map[string]string, snap Snapshot) (map[string]string, []pushEvent) {
	next := map[string]string{}
	var events []pushEvent
	for _, g := range snap.Groups {
		for _, t := range g.Tabs {
			for _, p := range t.Panes {
				key := paneKey(p)
				last := prev[key]
				status := foldPaneStatus(last, p.Agent)
				if status == "" {
					continue
				}
				next[key] = status
				if kind := pushTransitionKind(last, status); kind != "" {
					events = append(events, pushEvent{GroupID: g.ID, TabID: t.ID, PaneID: p.ID, Kind: kind})
				}
			}
		}
	}
	return next, events
}

func (w *pushWatcher) run(ctx context.Context) {
	tick := time.NewTicker(pushWatchInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.tick(ctx)
		}
	}
}

// tick reads the panes, settles transitions, applies presence/cooldown/mute/
// category filters, and queues the events. Without a target (Web Push or
// Telegram) it reads nothing, and forgets the statuses: they would be stale
// by the time one arrives. A failed read keeps them.
func (w *pushWatcher) tick(ctx context.Context) {
	if !w.store.hasTargets() {
		clear(w.prev)
		clear(w.pending)
		return
	}
	gen := w.activity.begin()
	pctx, cancel := context.WithTimeout(ctx, muxTimeout)
	snap, err := w.peek(pctx)
	cancel()
	if err != nil {
		w.logf("peek", "push: reading the panes: %v", err)
		return
	}
	w.activity.observe(gen, snap)

	now := w.now()
	cfg := w.store.notifyConfig()
	next := map[string]string{}
	var keys []string

	for _, g := range snap.Groups {
		for _, t := range g.Tabs {
			for _, p := range t.Panes {
				key := paneKey(p)
				last := w.prev[key]
				status := foldPaneStatus(last, p.Agent)
				if status == "" {
					delete(w.pending, key)
					continue
				}
				next[key] = status
				keys = append(keys, key)

				agentName := ""
				if p.Agent != nil {
					agentName = p.Agent.Name
				}
				w.setPaneMeta(p.ID, paneNotifyMeta{
					groupName: g.Name,
					tabName:   t.Name,
					agent:     agentName,
				})

				if pend, ok := w.pending[key]; ok {
					cancelled := (pend.kind == "blocked" && status != "blocked") ||
						(pend.kind == "done" && status != "done" && status != "idle")
					if cancelled {
						delete(w.pending, key)
					} else {
						pend.event.GroupID = g.ID
						pend.event.TabID = t.ID
						pend.event.PaneID = p.ID
						w.pending[key] = pend
					}
				}

				if kind := pushTransitionKind(last, status); kind != "" {
					w.pending[key] = pendingTransition{
						kind:  kind,
						since: now,
						event: pushEvent{
							GroupID: g.ID,
							TabID:   t.ID,
							PaneID:  p.ID,
							Kind:    kind,
						},
					}
				}
			}
		}
	}

	for key := range w.pending {
		if _, ok := next[key]; !ok {
			delete(w.pending, key)
		}
	}
	w.prev = next

	for _, key := range keys {
		pend, ok := w.pending[key]
		if !ok {
			continue
		}
		if !pend.heldByPresence && now.Sub(pend.since) < cfg.SettleDuration(pend.kind) {
			continue
		}
		if cfg.IsMuted(now) || !cfg.CategoryEnabled(pend.kind) {
			delete(w.pending, key)
			continue
		}
		if cfg.SkipWhileViewing && w.store.isViewing(pend.event.PaneID, now) {
			if pend.kind == "done" {
				delete(w.pending, key)
				continue
			}
			if pend.kind == "blocked" {
				pend.heldByPresence = true
				w.pending[key] = pend
				continue
			}
		}
		delete(w.pending, key)
		if !w.allowCooldown(key+":"+pend.kind, cfg.CooldownDuration(), now) {
			continue
		}
		w.enqueue(pend.event)
	}
}

func (w *pushWatcher) setPaneMeta(paneID string, meta paneNotifyMeta) {
	if paneID == "" {
		return
	}
	w.mu.Lock()
	w.paneMeta[paneID] = meta
	w.mu.Unlock()
}

func (w *pushWatcher) metaFor(paneID string) paneNotifyMeta {
	if paneID == "" {
		return paneNotifyMeta{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paneMeta[paneID]
}

func (w *pushWatcher) allowCooldown(coolKey string, cd time.Duration, now time.Time) bool {
	if cd <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.lastSent[coolKey]; ok && now.Sub(last) < cd {
		return false
	}
	w.lastSent[coolKey] = now
	return true
}

// EmitExternal queues an external notification event (e.g. update, cache-cold,
// machine-load) after checking active targets, mute, category toggle, viewer
// presence, and cooldown.
func (w *pushWatcher) EmitExternal(ev pushEvent) bool {
	if w == nil || w.store == nil || !w.store.hasTargets() {
		return false
	}
	now := w.now()
	cfg := w.store.notifyConfig()
	if cfg.IsMuted(now) || !cfg.CategoryEnabled(ev.Kind) {
		return false
	}
	if ev.PaneID != "" && cfg.SkipWhileViewing && w.store.isViewing(ev.PaneID, now) {
		return false
	}
	coolKey := ev.PaneID + ":" + ev.Kind
	if !w.allowCooldown(coolKey, cfg.CooldownDuration(), now) {
		return false
	}
	w.enqueue(ev)
	return true
}

// enqueue never blocks the tick: a full queue loses its oldest event.
func (w *pushWatcher) enqueue(e pushEvent) {
	for {
		select {
		case w.queue <- e:
			return
		default:
		}
		select {
		case <-w.queue:
		default:
		}
	}
}

func (w *pushWatcher) deliver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-w.queue:
			w.deliverEvent(ctx, e)
		}
	}
}

type pushResult struct {
	host       string
	status     int
	retryAfter time.Duration
	err        error
}

// deliverEvent sends e to every subscription whose push service is not
// backed off, pushParallel at a time, and to Telegram when enabled.
func (w *pushWatcher) deliverEvent(ctx context.Context, e pushEvent) {
	payload, _ := json.Marshal(e)
	_, topicKey := w.store.keys()
	topic := pushTopic(topicKey, e.PaneID, e.Kind)
	now := w.now()
	w.mu.Lock()
	for h, until := range w.backoff {
		if !now.Before(until) {
			delete(w.backoff, h)
		}
	}
	for k, at := range w.logged {
		if now.Sub(at) >= pushLogEvery {
			delete(w.logged, k)
		}
	}
	w.mu.Unlock()

	var subs []pushSub
	for _, sub := range w.store.list() {
		if !w.backedOff(endpointHost(sub.Endpoint), now) {
			subs = append(subs, sub)
		}
	}
	results := make([]pushResult, len(subs))
	sem := make(chan struct{}, pushParallel)
	var wg sync.WaitGroup
	for i, sub := range subs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			sctx, cancel := context.WithTimeout(ctx, pushSendTimeout)
			defer cancel()
			status, retry, err := w.send(sctx, sub, payload, topic)
			results[i] = pushResult{endpointHost(sub.Endpoint), status, retry, err}
		}()
	}
	if w.store.telegramActive() {
		meta := w.metaFor(e.PaneID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.store.sendTelegramEvent(ctx, e, meta); err != nil {
				w.logf("telegram", "push to telegram failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	rejected := 0
	for i, r := range results {
		switch {
		case r.err != nil:
			if isTimeout(r.err) {
				w.backOff(r.host, pushDefaultBackoff)
			}
			w.logf(r.host, "push to %s failed: %v", r.host, r.err)
		case r.status >= 200 && r.status < 300:
		case r.status == http.StatusNotFound || r.status == http.StatusGone:
			if err := w.store.drop(subs[i].Endpoint); err != nil {
				log.Printf("push: dropping a subscription of %s: %v", r.host, err)
			}
		case r.status == http.StatusUnauthorized || r.status == http.StatusForbidden:
			rejected++
			w.logf(r.host, "push to %s rejected: %d", r.host, r.status)
		case r.status == http.StatusTooManyRequests:
			w.backOff(r.host, orDefault(r.retryAfter, pushDefaultBackoff))
			w.logf(r.host, "push to %s rate limited", r.host)
		default:
			w.logf(r.host, "push to %s answered %d", r.host, r.status)
		}
	}
	if rejected > 0 && rejected == len(results) {
		w.logf("*", "push rejected by every service: check the server clock")
	}
}

// orDefault is d, or def when d is zero.
func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (w *pushWatcher) backedOff(host string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return now.Before(w.backoff[host])
}

func (w *pushWatcher) backOff(host string, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.backoff[host] = w.now().Add(min(d, pushMaxRetryAfter))
}

// logf logs at most one line per key every pushLogEvery. Lines name the
// push service's host, never an endpoint's path.
func (w *pushWatcher) logf(key, format string, args ...any) {
	now := w.now()
	w.mu.Lock()
	last, seen := w.logged[key]
	if seen && now.Sub(last) < pushLogEvery {
		w.mu.Unlock()
		return
	}
	w.logged[key] = now
	w.mu.Unlock()
	log.Printf(format, args...)
}

// endpointHost is the host of a push endpoint (its path identifies the
// device, so it is never logged).
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "?"
	}
	return u.Hostname()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}
