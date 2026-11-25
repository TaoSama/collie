package extproto

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func pair(t *testing.T, hostH, progH Handler, onEv EventHandler) (*Peer, *Peer) {
	t.Helper()
	ar, aw := io.Pipe()
	br, bw := io.Pipe()
	host := NewPeer(br, aw, "h", hostH, nil)
	prog := NewPeer(ar, bw, "p", progH, onEv)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); aw.Close(); bw.Close() })
	go host.Serve(ctx)
	go prog.Serve(ctx)
	return host, prog
}

func TestCallBothWays(t *testing.T) {
	host, prog := pair(t,
		func(ctx context.Context, m string, p json.RawMessage) (any, error) {
			return map[string]string{"from": "host"}, nil
		},
		func(ctx context.Context, m string, p json.RawMessage) (any, error) {
			if m == "fail" {
				return nil, Errorf(ErrCannotSend, "no")
			}
			return map[string]string{"m": m}, nil
		}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out map[string]string
	if err := host.Call(ctx, "ping", nil, &out); err != nil || out["m"] != "ping" {
		t.Fatalf("host→prog: %v %v", out, err)
	}
	if err := prog.Call(ctx, "x", nil, &out); err != nil || out["from"] != "host" {
		t.Fatalf("prog→host: %v %v", out, err)
	}
	if err := host.Call(ctx, "fail", nil, nil); !IsCode(err, ErrCannotSend) {
		t.Fatalf("want cannot_send, got %v", err)
	}
}

func TestEventsAndClose(t *testing.T) {
	got := make(chan string, 1)
	host, prog := pair(t, nil, nil, nil)
	_ = prog
	host2 := host
	// Events go to the handler of the side that receives them.
	ar, aw := io.Pipe()
	p := NewPeer(ar, io.Discard, "p", nil, func(e string, _ json.RawMessage) { got <- e })
	go p.Serve(context.Background())
	aw.Write([]byte("not json\n{\"event\":\"connector.changed\",\"params\":{}}\n"))
	select {
	case e := <-got:
		if e != EChanged {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	aw.Close()
	<-p.Done()
	if err := p.Call(context.Background(), "x", nil, nil); !IsCode(err, ErrClosed) {
		t.Fatalf("want closed, got %v", err)
	}
	_ = host2
}
