package wechat

import "context"

// maxActiveChats is how many chats are processed at once.
const maxActiveChats = 8

// InboundRunnerOptions wire the runner to the gateway.
type InboundRunnerOptions struct {
	Inbox   *FileInboundInbox
	Process func(InboundMessage) error
	// Steer hands a message to a request already running in its chat;
	// true means it needs no processing of its own.
	Steer   func(InboundMessage) (bool, error)
	Covered func(InboundMessage) error
	OnError func(error)
}

// InboundRunner serializes admission per chat; a running request never
// blocks receiving that chat's next message. A chat whose processing
// failed retries only its first message until it succeeds.
type InboundRunner struct {
	o          InboundRunnerOptions
	loop       *loop
	admissions map[string]chan struct{}
	roots      map[string]chan struct{}
	handling   map[string]bool
	blocked    map[string]bool
	stopped    bool
	running    int
	idle       chan struct{}
}

func NewInboundRunner(l *loop, o InboundRunnerOptions) *InboundRunner {
	return &InboundRunner{o: o, loop: l, admissions: map[string]chan struct{}{}, roots: map[string]chan struct{}{},
		handling: map[string]bool{}, blocked: map[string]bool{}}
}

// HasPending: chat has a request running or a message waiting.
func (r *InboundRunner) HasPending(chatID string) bool {
	if _, ok := r.roots[chatID]; ok {
		return true
	}
	messages, err := r.o.Inbox.List()
	if err != nil {
		return true
	}
	for _, m := range messages {
		if m.ChatID == chatID {
			return true
		}
	}
	return false
}

// Schedule starts work for every pending message it may start now.
func (r *InboundRunner) Schedule() {
	if r.stopped {
		return
	}
	messages, err := r.o.Inbox.List()
	if err != nil {
		r.o.OnError(err)
		return
	}
	first := map[string]string{}
	for _, m := range messages {
		if _, ok := first[m.ChatID]; !ok {
			first[m.ChatID] = m.DeliveryKey
		}
	}
	for _, m := range messages {
		if r.handling[m.DeliveryKey] {
			continue
		}
		if r.blocked[m.ChatID] && first[m.ChatID] != m.DeliveryKey {
			continue
		}
		chats := map[string]bool{}
		for c := range r.admissions {
			chats[c] = true
		}
		for c := range r.roots {
			chats[c] = true
		}
		if !chats[m.ChatID] && len(chats) >= maxActiveChats {
			continue
		}
		r.handling[m.DeliveryKey] = true
		prior := r.admissions[m.ChatID]
		mine := make(chan struct{})
		r.admissions[m.ChatID] = mine
		r.track(1)
		go r.loop.do(func() { r.admit(m, prior, mine, first[m.ChatID]) })
	}
}

func (r *InboundRunner) admit(m InboundMessage, prior, mine chan struct{}, firstKey string) {
	started := false
	defer func() {
		if !started {
			delete(r.handling, m.DeliveryKey)
		}
		if r.admissions[m.ChatID] == mine {
			delete(r.admissions, m.ChatID)
		}
		close(mine)
		r.track(-1)
	}()
	if prior != nil {
		r.loop.wait(context.Background(), prior)
	}
	if r.stopped {
		return
	}
	if root, ok := r.roots[m.ChatID]; ok {
		steered, err := r.o.Steer(m)
		if err != nil {
			r.o.OnError(err)
			return
		}
		if steered {
			if err := r.o.Covered(m); err != nil {
				r.o.OnError(err)
				return
			}
			if err := r.o.Inbox.Remove(m.DeliveryKey); err != nil {
				r.o.OnError(err)
			}
			return
		}
		r.loop.wait(context.Background(), root)
		if r.blocked[m.ChatID] {
			return
		}
	}
	if r.stopped || r.blocked[m.ChatID] && firstKey != m.DeliveryKey {
		return
	}
	started = true
	root := make(chan struct{})
	r.roots[m.ChatID] = root
	r.track(1)
	go r.loop.do(func() {
		defer func() {
			delete(r.handling, m.DeliveryKey)
			if r.roots[m.ChatID] == root {
				delete(r.roots, m.ChatID)
			}
			close(root)
			r.track(-1)
		}()
		if err := r.o.Process(m); err != nil {
			r.blocked[m.ChatID] = true
			r.o.OnError(err)
			return
		}
		if err := r.o.Inbox.Remove(m.DeliveryKey); err != nil {
			r.blocked[m.ChatID] = true
			r.o.OnError(err)
			return
		}
		delete(r.blocked, m.ChatID)
	})
}

func (r *InboundRunner) track(delta int) {
	r.running += delta
	if r.running == 0 && r.idle != nil {
		close(r.idle)
		r.idle = nil
	}
}

func (r *InboundRunner) Halt() { r.stopped = true }

// Stop halts and waits for running work.
func (r *InboundRunner) Stop() {
	r.Halt()
	for r.running > 0 {
		if r.idle == nil {
			r.idle = make(chan struct{})
		}
		r.loop.wait(context.Background(), r.idle)
	}
}
