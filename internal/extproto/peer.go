// Package extproto is the wire protocol between Collie and its extensions:
// one JSON frame a line over the extension's stdin and stdout. Both sides
// use the same Peer: either one may call the other and wait for an answer,
// or send an event that has none. See extensions/README.md.
package extproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Protocol is the version both sides speak; initialize checks it.
const Protocol = 1

// MaxFrame bounds one line: a message with its pictures fits in it.
const MaxFrame = 64 << 20

// maxConcurrent is how many requests of the other side run at once.
const maxConcurrent = 16

// Frame is one line. A request has ID and Method, its answer the same ID and
// Result or Error, an event Event and no ID.
type Frame struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
}

// Error is an answer that failed. Message is for people and never carries a
// secret or a message body; Code is for programs (see the Err* codes).
type Error struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

func (e *Error) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

// Errorf makes an *Error with a code.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err is an *Error with that code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Handler answers one request of the other side. A returned *Error goes back
// as it is; any other error goes back as ErrInternal with its text.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// EventHandler takes one event of the other side.
type EventHandler func(event string, params json.RawMessage)

// Peer is one end of the connection.
type Peer struct {
	w       io.Writer
	r       io.Reader
	prefix  string
	handle  Handler
	onEvent EventHandler

	wmu   sync.Mutex
	mu    sync.Mutex
	next  int
	calls map[string]chan Frame
	done  chan struct{}
	err   error
	sem   chan struct{}
}

// NewPeer reads frames from r and writes to w. prefix starts the ids of its
// own requests ("h" for the host, "p" for a program), so the two sides'
// ids never meet. handle or onEvent may be nil.
func NewPeer(r io.Reader, w io.Writer, prefix string, handle Handler, onEvent EventHandler) *Peer {
	return &Peer{r: r, w: w, prefix: prefix, handle: handle, onEvent: onEvent,
		calls: map[string]chan Frame{}, done: make(chan struct{}), sem: make(chan struct{}, maxConcurrent)}
}

// Serve reads until the other side closes or ctx ends. Calls still waiting
// fail with ErrClosed.
func (p *Peer) Serve(ctx context.Context) error {
	sc := bufio.NewScanner(p.r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxFrame)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Frame
		if json.Unmarshal(line, &f) != nil {
			continue // a broken line is dropped, the stream goes on
		}
		switch {
		case f.Method != "":
			wg.Add(1)
			p.sem <- struct{}{}
			go func(f Frame) {
				defer func() { <-p.sem; wg.Done() }()
				p.answer(ctx, f)
			}(f)
		case f.Event != "":
			if p.onEvent != nil {
				p.onEvent(f.Event, f.Params)
			}
		case f.ID != "":
			p.mu.Lock()
			ch := p.calls[f.ID]
			delete(p.calls, f.ID)
			p.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	p.mu.Lock()
	p.err = err
	for id, ch := range p.calls {
		close(ch)
		delete(p.calls, id)
	}
	p.mu.Unlock()
	close(p.done)
	wg.Wait()
	return err
}

// Done is closed when Serve has returned.
func (p *Peer) Done() <-chan struct{} { return p.done }

func (p *Peer) answer(ctx context.Context, f Frame) {
	out := Frame{ID: f.ID}
	if p.handle == nil {
		out.Error = Errorf(ErrUnknownMethod, "unknown method %s", f.Method)
	} else if res, err := p.handle(ctx, f.Method, f.Params); err != nil {
		var e *Error
		if errors.As(err, &e) {
			out.Error = e
		} else {
			out.Error = &Error{Code: ErrInternal, Message: err.Error()}
		}
	} else {
		if res == nil {
			res = struct{}{}
		}
		b, err := json.Marshal(res)
		if err != nil {
			out.Error = &Error{Code: ErrInternal, Message: "answer is not JSON"}
		} else {
			out.Result = b
		}
	}
	if f.ID != "" {
		p.write(out)
	}
}

func (p *Peer) write(f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.w.Write(b)
	return err
}

// Call asks the other side and waits for its answer, decoded into out (which
// may be nil). It gives up when ctx ends.
func (p *Peer) Call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(orEmpty(params))
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return Errorf(ErrClosed, "connection closed")
	}
	p.next++
	id := p.prefix + strconv.Itoa(p.next)
	ch := make(chan Frame, 1)
	p.calls[id] = ch
	p.mu.Unlock()
	if err := p.write(Frame{ID: id, Method: method, Params: raw}); err != nil {
		p.forget(id)
		return Errorf(ErrClosed, "write: %v", err)
	}
	select {
	case f, ok := <-ch:
		if !ok {
			return Errorf(ErrClosed, "connection closed")
		}
		if f.Error != nil {
			return f.Error
		}
		if out != nil && len(f.Result) > 0 {
			return json.Unmarshal(f.Result, out)
		}
		return nil
	case <-ctx.Done():
		p.forget(id)
		return Errorf(ErrTimeout, "%s: %v", method, ctx.Err())
	}
}

func (p *Peer) forget(id string) {
	p.mu.Lock()
	delete(p.calls, id)
	p.mu.Unlock()
}

// Notify sends an event.
func (p *Peer) Notify(event string, params any) error {
	raw, err := json.Marshal(orEmpty(params))
	if err != nil {
		return err
	}
	return p.write(Frame{Event: event, Params: raw})
}

// Send writes a frame as it is: for relays that pass frames through.
func (p *Peer) Send(f Frame) error { return p.write(f) }

func orEmpty(v any) any {
	if v == nil {
		return struct{}{}
	}
	return v
}
