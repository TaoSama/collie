package collie

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// fakeExt is an extension for the tests, run by the test binary itself as
// `ext-fake` (TestMain). It reads the messages to "receive" from data/in.jsonl
// and writes what it sent to data/sent.jsonl. FAKE_MODE in its config picks
// archive or receive.
func init() { builtinExts["fake"] = fakeExtMain }

type fakeMsg struct {
	Chat, ChatName, ID, Sender, SenderName, Text, At string
	Mine                                             bool
}

func fakeExtMain(args []string) int {
	var (
		mu       sync.Mutex
		data     string
		mode     string
		signedIn bool
		peer     *extproto.Peer
		seen     = map[string]bool{}
		archive  []fakeMsg
		sent     = map[string]string{}
		n        int
	)
	appendSent := func(v any) {
		b, _ := json.Marshal(v)
		f, err := os.OpenFile(filepath.Join(data, "sent.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	poll := func() {
		for range time.Tick(50 * time.Millisecond) {
			f, err := os.Open(filepath.Join(data, "in.jsonl"))
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(f)
			var fresh []fakeMsg
			for sc.Scan() {
				var m fakeMsg
				if json.Unmarshal(sc.Bytes(), &m) == nil && m.ID != "" {
					mu.Lock()
					if !seen[m.ID] {
						seen[m.ID] = true
						fresh = append(fresh, m)
					}
					mu.Unlock()
				}
			}
			f.Close()
			for _, m := range fresh {
				if mode == "receive" {
					owner := m.Mine
					peer.Call(context.Background(), extproto.MChannelReceive, extproto.Receive{Message: extproto.Message{
						ID: m.ID, Text: m.Text, ChatID: m.Chat, Chat: m.ChatName, ChatKind: "dm", Sender: m.Sender, SenderName: m.SenderName, Owner: &owner, At: m.At}}, nil)
					continue
				}
				mu.Lock()
				archive = append(archive, m)
				mu.Unlock()
				peer.Notify(extproto.EChanged, extproto.Changed{Scope: "inbox"})
			}
		}
	}
	handle := func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case extproto.MInitialize:
			var p extproto.InitializeParams
			json.Unmarshal(raw, &p)
			data, _ = p.Extension.DataDir, 0
			mode, _ = p.Config["mode"].(string)
			go func() {
				var s extproto.Secrets
				if peer.Call(context.Background(), extproto.MSecretsLoad, nil, &s) == nil && s.Data != "" {
					mu.Lock()
					signedIn = true
					mu.Unlock()
				}
				poll()
			}()
			return extproto.InitializeResult{Protocol: extproto.Protocol, ID: p.Extension.ID, Version: p.Extension.Version}, nil
		case extproto.MPing:
			return struct{}{}, nil
		case extproto.MStatus:
			mu.Lock()
			defer mu.Unlock()
			if signedIn {
				return extproto.Status{State: extproto.StateOK, Account: "tester"}, nil
			}
			return extproto.Status{State: extproto.StateNotSignedIn}, nil
		case extproto.MLoginStep:
			var p struct{ Step, Secret string }
			json.Unmarshal(raw, &p)
			if p.Step == "" && mode == "actions" {
				return extproto.LoginStep{Step: "k1", Kind: extproto.LoginSecret, Label: "API Key", Hint: "在服务的设置页复制"}, nil
			}
			if p.Step == "" {
				return extproto.LoginStep{Step: "s1", Kind: "qr", URL: "fake-qr", Interval: 1}, nil
			}
			tok := "tok"
			if p.Secret != "" {
				tok = p.Secret
			}
			if err := peer.Call(ctx, extproto.MSecretsSave, extproto.Secrets{Data: base64.StdEncoding.EncodeToString([]byte(tok))}, nil); err != nil {
				return nil, err
			}
			mu.Lock()
			signedIn = true
			mu.Unlock()
			return extproto.LoginStep{Done: true, Account: "tester"}, nil
		case extproto.MArchiveChanges:
			var p struct{ Cursor string }
			json.Unmarshal(raw, &p)
			mu.Lock()
			defer mu.Unlock()
			var from int
			fmt.Sscan(p.Cursor, &from)
			byChat := map[string]*extproto.ArchiveBatch{}
			var order []string
			for _, m := range archive[min(from, len(archive)):] {
				b := byChat[m.Chat]
				if b == nil {
					b = &extproto.ArchiveBatch{Chat: extproto.ArchiveChat{ID: m.Chat, Name: m.ChatName, Kind: "group"}}
					byChat[m.Chat] = b
					order = append(order, m.Chat)
				}
				b.Messages = append(b.Messages, extproto.ArchiveMessage{ID: m.ID, SenderID: m.Sender, SenderName: m.SenderName, Mine: m.Mine, Type: "TEXT", Text: m.Text, At: m.At})
			}
			out := extproto.ArchiveChanges{Next: fmt.Sprint(len(archive))}
			for _, c := range order {
				out.Batches = append(out.Batches, *byChat[c])
			}
			return out, nil
		case extproto.MActionsDo:
			var p extproto.ActionsDo
			json.Unmarshal(raw, &p)
			switch p.Name {
			case "echo":
				return extproto.ActionResult{Text: fmt.Sprintf("%v×%v", p.Args["text"], p.Args["n"]),
					Files: []extproto.ActionFile{{Path: "/tmp/x.pdf", Mime: "application/pdf"}}, Items: []json.RawMessage{json.RawMessage(`{"id":1}`)}}, nil
			case "write":
				return extproto.ActionResult{Text: "written"}, nil
			case "sleep":
				time.Sleep(3 * time.Second)
				return extproto.ActionResult{Text: "late"}, nil
			}
			mu.Lock()
			defer mu.Unlock()
			if r, ok := sent[p.RequestID]; ok {
				return extproto.ActionResult{Text: r}, nil
			}
			n++
			id := fmt.Sprintf("m-%d", n)
			sent[p.RequestID] = "已发出（" + id + "）。"
			appendSent(map[string]any{"ref": p.RequestID, "chat": p.Args["chat"], "text": p.Args["text"], "reply_to": p.Args["reply_to"]})
			// Its own message comes back through the archive, as a real chat app's does.
			archive = append(archive, fakeMsg{Chat: fmt.Sprint(p.Args["chat"]), ID: id, Sender: "me", SenderName: "我", Mine: true, Text: fmt.Sprint(p.Args["text"]), At: time.Now().Format(time.RFC3339)})
			go peer.Notify(extproto.EChanged, extproto.Changed{Scope: "inbox"})
			return extproto.ActionResult{Text: sent[p.RequestID]}, nil
		case extproto.MChannelSend:
			var p extproto.Send
			json.Unmarshal(raw, &p)
			appendSent(map[string]any{"ref": p.Ref, "chat": p.Chat, "text": p.Text, "kind": p.Kind, "link": p.Link, "files": len(p.Images) + len(p.Files)})
			if strings.Contains(p.Text, "DEFER") {
				go func() {
					time.Sleep(100 * time.Millisecond)
					peer.Notify(extproto.EDelivery, extproto.Delivery{Ref: p.Ref, State: extproto.DeliverySent, MessageID: "late"})
				}()
				return extproto.SendResult{Deferred: true, Reason: "window_expired"}, nil
			}
			return extproto.SendResult{MessageID: "w-" + p.Ref}, nil
		case extproto.MLogout:
			peer.Call(ctx, extproto.MSecretsDelete, nil, nil)
			mu.Lock()
			signedIn = false
			mu.Unlock()
			return struct{}{}, nil
		}
		return nil, extproto.Errorf(extproto.ErrUnknownMethod, "%s", method)
	}
	peer = extproto.NewPeer(os.Stdin, os.Stdout, "p", handle, nil)
	peer.Serve(context.Background())
	return 0
}
