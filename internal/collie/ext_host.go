package collie

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// The hub side of extensions (extensions/README.md): which are on and where
// they run, one connection to each (through the process side, ext_proc.go),
// and the calls Collie makes on them. What happens to their messages is in
// inbox.go.

const (
	extRedialWait    = 10 * time.Second
	extStatusEvery   = time.Minute
	extArchiveEvery  = 10 * time.Minute
	extArchiveRetry  = time.Second
	extInterestEvery = 30 * time.Second
	extCallWait      = 30 * time.Second
	extLoginWait     = 50 * time.Second // under the reverse proxy's 60 s: a slow first step is retried by the page
	extViewLogs      = 12
)

// ExtSetting is what you chose for one extension (kv "ext_settings").
type ExtSetting struct {
	ID      string         `json:"id"`
	Enabled bool           `json:"enabled"`
	Machine string         `json:"machine"`          // machine id, "" for the hub
	Chosen  bool           `json:"chosen,omitempty"` // you picked Machine (needed when the manifest says ask)
	Config  map[string]any `json:"config,omitempty"`
	// NoWrites refuses its actions that change something (readonly false).
	NoWrites bool `json:"no_writes,omitempty"`
}

type extHost struct {
	s     *Server
	sp    *Super
	home  string
	store *extStore

	cfgMu    sync.Mutex // one configure at a time: an old program stops before a new one starts
	marketMu sync.Mutex // the marketplaces' list and tokens
	sending  sync.Map   // refs being sent right now
	starting sync.Map   // conversation IDs with a dedicated worker launch in flight
	mu       sync.Mutex
	cat      map[string]*ExtManifest
	sets     map[string]ExtSetting
	runs     map[string]*extRun
	ctx      context.Context

	avatarBytes []byte
	avatarType  string
	avatarURL   string
}

// extRun is one enabled extension's connection.
type extRun struct {
	h      *extHost
	man    *ExtManifest
	set    ExtSetting
	cancel context.CancelFunc

	mu       sync.Mutex
	peer     *extproto.Peer
	proc     extProcState
	status   extproto.Status
	statusAt time.Time
	dialErr  string
	pull     chan struct{}
	pulling  sync.Mutex
}

func newExtHost(s *Server, sp *Super, home string) *extHost {
	h := &extHost{s: s, sp: sp, home: home, store: newExtStore(sp.db),
		cat: map[string]*ExtManifest{}, sets: map[string]ExtSetting{}, runs: map[string]*extRun{}}
	cat, err := extCatalog()
	if err != nil {
		log.Printf("ext: catalog: %v", err)
	}
	for _, m := range cat {
		h.cat[m.ID] = m
	}
	h.loadMarketInstalled()
	var sets []ExtSetting
	if raw := h.store.kv("ext_settings"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &sets); err != nil {
			log.Printf("ext: settings: %v", err)
		}
	}
	for _, x := range sets {
		h.sets[x.ID] = x
	}
	h.loadWechatAvatarLocked()
	return h
}

// Run starts the enabled extensions and keeps their housekeeping going.
func (h *extHost) Run(ctx context.Context) {
	h.mu.Lock()
	h.ctx = ctx
	for id, set := range h.sets {
		if set.Enabled && h.cat[id] != nil {
			h.startLocked(h.cat[id], set)
		}
	}
	h.mu.Unlock()
	h.refreshLocal(ctx)
	t := time.NewTicker(extLocalEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.housekeep()
			h.refreshLocal(ctx)
		}
	}
}

func (h *extHost) saveSetsLocked() {
	list := make([]ExtSetting, 0, len(h.sets))
	for _, x := range h.sets {
		list = append(list, x)
	}
	b, _ := json.Marshal(list)
	h.store.setKV("ext_settings", string(b))
}

func (h *extHost) setting(id string) ExtSetting {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settingLocked(id)
}

func (h *extHost) settingLocked(id string) ExtSetting {
	if x, ok := h.sets[id]; ok {
		return x
	}
	x := ExtSetting{ID: id}
	if m := h.cat[id]; m != nil && m.Defaults.Machine == machineRouter {
		x.Machine = h.sp.routerOn()
	}
	return h.pinLocked(x)
}

// pinLocked: a local extension runs where it was found.
func (h *extHost) pinLocked(x ExtSetting) ExtSetting {
	if m := h.cat[x.ID]; m != nil && m.Local {
		x.Machine, x.Chosen = m.LocalOn, true
	}
	return x
}

// configure turns an extension on or off, moves it, or changes its config.
// Changes to program settings restart it.
func (h *extHost) configure(id string, change func(*ExtSetting)) (ExtSetting, error) {
	return h.configureWithRestart(id, false, change)
}

func (h *extHost) configureWithRestart(id string, restart bool, change func(*ExtSetting)) (ExtSetting, error) {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.mu.Lock()
	m := h.cat[id]
	if m == nil {
		h.mu.Unlock()
		return ExtSetting{}, fmt.Errorf("no extension %q", id)
	}
	x := h.settingLocked(id)
	before := x
	change(&x)
	x = h.pinLocked(x)
	if x.Enabled && !before.Enabled && m.Defaults.Machine == machineAsk && !x.Chosen {
		h.mu.Unlock()
		return before, fmt.Errorf("先选它运行在哪台电脑：它只能在一台电脑上运行")
	}
	x.Config = m.configWithDefaults(x.Config)
	if m.Runtime.Builtin == "wechat" {
		x.Config[wechatConversationNameKey] = m.wechatConversationName(x.Config)
	}
	h.sets[id] = x
	h.saveSetsLocked()
	old := h.runs[id]
	if old != nil && (!x.Enabled || x.Machine != before.Machine || restart || !sameJSON(m.programConfig(x.Config), m.programConfig(before.Config))) {
		delete(h.runs, id)
	} else {
		old = nil
	}
	h.mu.Unlock()
	// The old program stops before a new one starts: two at once would
	// steal each other's messages.
	if old != nil {
		old.stop(x.Machine != before.Machine || !x.Enabled)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if x.Enabled && h.runs[id] == nil && h.ctx != nil {
		h.startLocked(m, x)
	}
	return x, nil
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (h *extHost) startLocked(m *ExtManifest, set ExtSetting) {
	if m.declarative() {
		return // nothing to run: on is all it takes
	}
	ctx, cancel := context.WithCancel(h.ctx)
	r := &extRun{h: h, man: m, set: set, cancel: cancel, pull: make(chan struct{}, 1)}
	r.proc = extProcState{State: extStarting}
	h.runs[m.ID] = r
	go r.loop(ctx)
}

// featureOn: that part of Collie is turned on in 接入 (off until it is).
func (h *extHost) featureOn(name string) bool {
	return h != nil && h.setting(name).Enabled
}

func (h *extHost) run(id string) *extRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[id]
}

func (h *extHost) manifest(id string) *ExtManifest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cat[id]
}

// dial reaches the process side on the extension's computer.
func (h *extHost) dial(ctx context.Context, machine string) (net.Conn, error) {
	if machine == "" {
		return localExtConn(h.home), nil
	}
	m, err := h.s.herdr.machine(machine)
	if err != nil {
		return nil, err
	}
	if m.ext == nil {
		return nil, errors.New("that computer is the hub's own")
	}
	return m.ext(ctx)
}

// stop ends the connection; with halt the program stops too (off, or moved).
func (r *extRun) stop(halt bool) {
	r.mu.Lock()
	peer := r.peer
	r.mu.Unlock()
	if halt && peer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := peer.Call(ctx, "$proc.stop", nil, nil); err != nil {
			log.Printf("ext %s: stop: %v", r.man.ID, err)
		}
		cancel()
	}
	r.cancel()
}

func (r *extRun) loop(ctx context.Context) {
	for ctx.Err() == nil {
		err := r.session(ctx)
		r.mu.Lock()
		r.peer = nil
		if err != nil && ctx.Err() == nil {
			r.dialErr = err.Error()
		}
		r.mu.Unlock()
		if sleepCtx(ctx, extRedialWait) != nil {
			return
		}
	}
}

// session is one connection to the process side, until it drops.
func (r *extRun) session(ctx context.Context) error {
	conn, err := r.h.dial(ctx, r.set.Machine)
	if err != nil {
		return err
	}
	defer conn.Close()
	cfg := map[string]any{}
	for k, v := range r.set.Config {
		cfg[k] = v
	}
	for k, v := range r.h.configSecrets(r.man.ID) {
		cfg[k] = v // to the program, never into the hub's database
	}
	hdr, _ := json.Marshal(extStartHeader{ID: r.man.ID, Manifest: r.man, Config: cfg, HostVersion: version})
	if _, err := conn.Write(append(hdr, '\n')); err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	peer := extproto.NewPeer(bufio.NewReaderSize(conn, 64<<10), conn, "h", r.handle, r.event)
	r.mu.Lock()
	r.peer, r.dialErr = peer, ""
	r.mu.Unlock()
	go func() { <-sctx.Done(); conn.Close() }()
	go r.housekeep(sctx, peer)
	err = peer.Serve(sctx)
	return err
}

// housekeep: status now and then, the archive when told and as a fallback,
// interest and read state for extensions that want them, and sends still pending.
func (r *extRun) housekeep(ctx context.Context, peer *extproto.Peer) {
	status := time.NewTicker(extStatusEvery)
	archive := time.NewTicker(extArchiveEvery)
	interest := time.NewTicker(extInterestEvery)
	read := time.NewTicker(extReadEvery)
	defer read.Stop()
	firstRead := time.After(extReadFirst)
	defer status.Stop()
	defer archive.Stop()
	defer interest.Stop()
	var retryArchive <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-status.C:
			r.refreshStatus(ctx)
			r.h.retrySends(r)
		case <-archive.C:
			r.signalPull()
		case <-retryArchive:
			retryArchive = nil
			r.signalPull()
		case <-interest.C:
			r.sendInterest(ctx)
		case <-firstRead:
			r.syncRead(ctx)
		case <-read.C:
			r.syncRead(ctx)
		case <-r.pull:
			if r.man.has(capArchive) && r.running() {
				if r.pullArchive(ctx) {
					retryArchive = time.After(extArchiveRetry)
				} else {
					retryArchive = nil
				}
			}
		}
	}
}

func (r *extRun) running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.proc.State == extRunning
}

func (r *extRun) signalPull() {
	select {
	case r.pull <- struct{}{}:
	default:
	}
}

func (r *extRun) call(ctx context.Context, method string, params, out any) error {
	r.mu.Lock()
	peer := r.peer
	r.mu.Unlock()
	if peer == nil {
		return extproto.Errorf(extproto.ErrHubUnavailable, "not connected to %s", r.man.ID)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, extCallWait)
		defer cancel()
	}
	return peer.Call(ctx, method, params, out)
}

func (r *extRun) contribution() extproto.Contribution {
	return extproto.Contribution{Contribution: r.man.Contributes.Connector.ID}
}

func (r *extRun) refreshStatus(ctx context.Context) {
	if !r.running() {
		return
	}
	var st extproto.Status
	if err := r.call(ctx, extproto.MStatus, r.contribution(), &st); err != nil {
		st = extproto.Status{State: extproto.StateError, Error: err.Error()}
	}
	r.mu.Lock()
	r.status, r.statusAt = st, time.Now()
	r.mu.Unlock()
}

// sendSettings: the trigger word, to an extension that marks requests.
func (r *extRun) sendSettings(ctx context.Context) {
	if !r.man.has(capWorking) || !r.running() {
		return
	}
	args := extproto.ChannelSettings{Contribution: r.man.Contributes.Connector.ID, Trigger: r.h.trigger(), Ignored: r.h.requestsIgnored(r.man.ID)}
	if err := r.call(ctx, extproto.MChannelSettings, args, nil); err != nil {
		log.Printf("ext %s: settings: %v", r.man.ID, err)
	}
}

// pushSettings sends every running extension the settings it follows.
func (h *extHost) pushSettings() {
	h.mu.Lock()
	runs := make([]*extRun, 0, len(h.runs))
	for _, r := range h.runs {
		runs = append(runs, r)
	}
	h.mu.Unlock()
	for _, r := range runs {
		go r.sendSettings(context.Background())
	}
}

func (r *extRun) sendInterest(ctx context.Context) {
	if !r.man.has(capInterest) || !r.running() {
		return
	}
	chats := r.h.interestChats(r.man.ID)
	if err := r.call(ctx, extproto.MChannelInterest, extproto.Interest{Contribution: r.man.Contributes.Connector.ID, Chats: chats}, nil); err != nil {
		log.Printf("ext %s: interest: %v", r.man.ID, err)
	}
}

// event: the process side's state, and the extension's own events.
func (r *extRun) event(name string, params json.RawMessage) {
	switch name {
	case "$proc.state":
		var st extProcState
		if json.Unmarshal(params, &st) != nil {
			return
		}
		r.mu.Lock()
		was := r.proc.State
		r.proc = st
		r.mu.Unlock()
		if st.State == extRunning && was != extRunning {
			go func() {
				r.refreshStatus(context.Background())
				r.signalPull()
				r.h.retrySends(r)
				r.sendInterest(context.Background())
				r.sendSettings(context.Background())
			}()
		}
	case extproto.EChanged:
		var c extproto.Changed
		json.Unmarshal(params, &c)
		if c.Scope == "status" || c.Scope == "" {
			go r.refreshStatus(context.Background())
		}
		if c.Scope == "outbox" {
			go r.h.retrySends(r)
		}
		if c.Scope != "status" && c.Scope != "header_status" && c.Scope != "outbox" {
			r.signalPull()
		}
	case extproto.EDelivery:
		var d extproto.Delivery
		if json.Unmarshal(params, &d) == nil {
			if err := r.h.delivered(r, d); err != nil {
				log.Printf("ext %s: delivery %s: %v", r.man.ID, d.Ref, err)
			}
		}
	}
}

// handle answers the extension's requests (secrets never get here: the
// process side keeps them on its computer).
func (r *extRun) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case extproto.MChannelDelivery:
		var d extproto.Delivery
		if err := json.Unmarshal(params, &d); err != nil {
			return nil, extproto.Errorf(extproto.ErrInvalidParams, "bad delivery")
		}
		return struct{}{}, r.h.delivered(r, d)
	case extproto.MChannelReceive:
		if !r.man.has(capReceive) {
			return struct{}{}, nil // it said it doesn't: Collie reads its archive instead
		}
		var in extproto.Receive
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, extproto.Errorf(extproto.ErrInvalidParams, "bad message")
		}
		if err := r.h.received(r, in.Message); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	}
	return nil, extproto.Errorf(extproto.ErrUnknownMethod, "%s", method)
}

// pullArchive reads every new message since the cursor. The first time
// (no cursor yet) it only moves the cursor to now: history isn't new.
// A storage failure asks housekeeping to retry before messages go stale.
func (r *extRun) pullArchive(ctx context.Context) bool {
	r.pulling.Lock()
	defer r.pulling.Unlock()
	key := "ext_cursor:" + r.man.ID
	cursor, err := r.h.store.readKV(key)
	if err != nil {
		log.Printf("ext %s: read archive cursor: %v", r.man.ID, err)
		return true
	}
	baseline := cursor == ""
	for i := 0; i < 50 && ctx.Err() == nil; i++ {
		var out extproto.ArchiveChanges
		args := map[string]any{"contribution": r.man.Contributes.Connector.ID, "cursor": cursor, "limit": 50}
		if err := r.call(ctx, extproto.MArchiveChanges, args, &out); err != nil {
			log.Printf("ext %s: archive: %v", r.man.ID, err)
			return false
		}
		for _, b := range out.Batches {
			if err := r.h.archived(r, b, baseline); err != nil {
				log.Printf("ext %s: store archive page: %v", r.man.ID, err)
				return true
			}
		}
		if out.Next != "" {
			if err := r.h.store.writeKV(key, out.Next); err != nil {
				log.Printf("ext %s: save archive cursor: %v", r.man.ID, err)
				return true
			}
			cursor = out.Next
		}
		if !out.More {
			break
		}
	}
	if baseline && cursor == "" {
		if err := r.h.store.writeKV(key, "0"); err != nil { // nothing yet: later messages are all new
			log.Printf("ext %s: save initial archive cursor: %v", r.man.ID, err)
			return true
		}
	}
	return false
}

// view is what the 接入 page shows of an extension.
type extView struct {
	Manifest *ExtManifest  `json:"manifest"`
	Setting  ExtSetting    `json:"setting"`
	Proc     *extProcState `json:"proc,omitempty"`
	// Secrets: which x-secret fields of its config are set (never their values).
	Secrets   []string         `json:"secrets,omitempty"`
	Status    *extproto.Status `json:"status,omitempty"`
	Problem   string           `json:"problem,omitempty"`
	AvatarURL string           `json:"avatar_url,omitempty"`
}

func extSortRank(m *ExtManifest) int {
	switch {
	case m == nil:
		return 3
	case m.Market != nil:
		return 1
	case m.Local:
		return 2
	default:
		return 0
	}
}

func (h *extHost) views() []extView {
	h.mu.Lock()
	ids := make([]string, 0, len(h.cat))
	for id := range h.cat {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		ri, rj := extSortRank(h.cat[ids[i]]), extSortRank(h.cat[ids[j]])
		if ri != rj {
			return ri < rj
		}
		return ids[i] < ids[j]
	})
	h.mu.Unlock()
	out := []extView{}
	for _, id := range ids {
		out = append(out, h.view(id))
	}
	return out
}

func (h *extHost) view(id string) extView {
	h.mu.Lock()
	v := extView{Manifest: h.cat[id], Setting: h.settingLocked(id)}
	if v.Manifest != nil && v.Manifest.Runtime.Builtin == wechatBuiltin {
		v.AvatarURL = h.avatarURL
	}
	r := h.runs[id]
	h.mu.Unlock()
	for k := range h.configSecrets(id) {
		v.Secrets = append(v.Secrets, k)
	}
	sort.Strings(v.Secrets)
	if r != nil {
		r.mu.Lock()
		p, st := r.proc, r.status
		if n := len(p.Logs); n > extViewLogs {
			p.Logs = p.Logs[n-extViewLogs:]
		}
		v.Proc = &p
		if !r.statusAt.IsZero() {
			v.Status = &st
		}
		v.Problem = r.dialErr
		r.mu.Unlock()
	}
	return v
}

// Call makes any call on a running extension, for features built on it.
func (h *extHost) Call(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	r := h.run(id)
	if r == nil {
		return nil, extproto.Errorf(extproto.ErrNotRunning, "extension %s is off", id)
	}
	var out json.RawMessage
	if err := r.call(ctx, method, params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

const extHeaderStatusWait = 8 * time.Second

// headerStatusSlot is one enabled header_status extension's current reading
// for the session header bar.
type headerStatusSlot struct {
	ID      string                 `json:"id"`
	Name    string                 `json:"name"`
	Label   string                 `json:"label,omitempty"`
	IconURL string                 `json:"icon_url,omitempty"`
	State   string                 `json:"state"` // ok | starting | error
	Error   string                 `json:"error,omitempty"`
	Status  *extproto.HeaderStatus `json:"status,omitempty"`
}

func (h *extHost) headerStatuses(ctx context.Context, model string, fresh bool) []headerStatusSlot {
	type target struct {
		man *ExtManifest
		run *extRun
	}
	h.mu.Lock()
	var ids []string
	for id, m := range h.cat {
		if m.has(capHeaderStatus) && h.settingLocked(id).Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	targets := make([]target, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, target{man: h.cat[id], run: h.runs[id]})
	}
	h.mu.Unlock()

	slots := make([]headerStatusSlot, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		slot := headerStatusSlot{
			ID:      t.man.ID,
			Name:    t.man.Name,
			Label:   extLabel(t.man),
			IconURL: t.man.IconURL,
			State:   "starting",
		}
		if t.run == nil {
			slots[i] = slot
			continue
		}
		t.run.mu.Lock()
		proc, dialErr := t.run.proc, t.run.dialErr
		t.run.mu.Unlock()
		if proc.State != extRunning {
			if dialErr != "" {
				slot.State, slot.Error = "error", dialErr
			} else if proc.State == extFailed {
				slot.State, slot.Error = "error", proc.Error
			}
			slots[i] = slot
			continue
		}
		wg.Add(1)
		go func(idx int, tgt target, base headerStatusSlot) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, extHeaderStatusWait)
			defer cancel()
			var hs extproto.HeaderStatus
			err := tgt.run.call(cctx, extproto.MHeaderStatus, extproto.HeaderStatusParams{
				Contribution: tgt.man.Contributes.Connector.ID,
				Model:        model,
				Fresh:        fresh,
			}, &hs)
			if err != nil {
				base.State = "error"
				base.Error = err.Error()
			} else {
				base.State = "ok"
				base.Status = &hs
			}
			slots[idx] = base
		}(i, t, slot)
	}
	wg.Wait()
	return slots
}
