package collie

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
)

// Sending answers back (extensions/README.md §3.5, §4): each send gets a ref,
// is stored before it goes out, and is retried with the same ref only while
// the extension could not be reached; the extension makes a ref go out at
// most once. An answer carries the footer.

const outboxGiveUp = time.Hour

// runInfo is who answered, for the footer.
type runInfo struct {
	Agent, Model, Effort, Machine, Worker, Ctx string
}

var footerVarRe = regexp.MustCompile(`\{[a-z]+\}`)

// renderFooter fills the template; unknown {names} stay as written.
func renderFooter(tpl string, vars map[string]string) string {
	return footerVarRe.ReplaceAllStringFunc(tpl, func(k string) string {
		if v, ok := vars[strings.Trim(k, "{}")]; ok {
			return v
		}
		return k
	})
}

// footerPrefix is the template's fixed start, which marks Collie's own sends.
func footerPrefix(tpl string) string {
	if i := strings.IndexByte(tpl, '{'); i >= 0 {
		tpl = tpl[:i]
	}
	if len([]rune(strings.TrimSpace(tpl))) < 2 {
		return "" // too short to tell an answer from a message
	}
	return tpl
}

func elapsedText(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%d秒", s)
	case s < 3600:
		if s%60 == 0 {
			return fmt.Sprintf("%d分", s/60)
		}
		return fmt.Sprintf("%d分%d秒", s/60, s%60)
	}
	return fmt.Sprintf("%d小时%d分", s/3600, s%3600/60)
}

func newRef() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "o" + hex.EncodeToString(b), nil
}

// sendOut sends text to the conversation of thread. kind says what it is
// (answer, progress, result, push). It returns the stored item.
func (h *extHost) sendOut(thread, text, kind string, ri runInfo, trig string) (*OutboxItem, error) {
	return h.sendOutFiles(thread, text, kind, ri, trig, nil)
}

// sendFile is a picture or file to send: in the request (Data, base64), or
// uploaded ahead with `collie reply --file` (ID, ext_links.go).
type sendFile struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Data string `json:"data,omitempty"`
	ID   string `json:"id,omitempty"`
}

// sendOutFiles also sends pictures and files; they wait on the hub's disk
// with the send, so a retry has them. A file too big to go through the
// extension goes as a download link: in the text, before the footer, or as
// the result's link.
func (h *extHost) sendOutFiles(thread, text, kind string, ri runInfo, trig string, files []sendFile) (*OutboxItem, error) {
	_, convID, ok := channelOf(thread)
	if !ok {
		return nil, fmt.Errorf("%s is not an extension conversation", thread)
	}
	conv, ok := h.store.conv(convID)
	if !ok {
		return nil, fmt.Errorf("no conversation %s", convID)
	}
	ask := conv.askOf(topicOf(thread)) // a group topic answers its own request
	if text = strings.TrimSpace(text); text == "" {
		text = "（这一轮没有文字回复）"
	}
	vars := map[string]string{
		"agent": ri.Agent, "model": ri.Model, "effort": ri.Effort, "machine": ri.Machine, "worker": ri.Worker,
		"ctx": ri.Ctx, "time": time.Now().Format("15:04"), "by": ask.AskBy, "elapsed": "—",
	}
	for k, v := range vars {
		if v == "" {
			vars[k] = "默认"
			if k == "by" || k == "ctx" || k == "worker" || k == "machine" {
				vars[k] = "—"
			}
		}
	}
	if !ask.AskAt.IsZero() {
		vars["elapsed"] = elapsedText(time.Since(ask.AskAt))
	}
	ref, err := newRef()
	if err != nil {
		return nil, err
	}
	o := &OutboxItem{Ref: ref, Conv: convID, Kind: kind, Trigger: trig, State: outPending, At: time.Now()}
	if len(files) > 0 {
		if m := h.manifest(conv.Ext); m == nil || !m.has(capSend) {
			return nil, fmt.Errorf("%s 只能发文字", conv.Ext)
		}
		var links []string
		for i, f := range files {
			of, err := h.keepFile(o, i, f)
			if err != nil {
				if rerr := os.RemoveAll(filepath.Join(h.home, "ext-outbox", ref)); rerr != nil {
					log.Printf("ext: drop the files of an unsent %s: %v", ref, rerr)
				}
				return nil, fmt.Errorf("file %d: %v", i+1, err)
			}
			if of.Link != "" {
				links = append(links, of.Name+"："+of.Link)
			}
			o.Files = append(o.Files, of)
		}
		if len(links) > 0 && kind != extproto.KindResult {
			text += "\n\n文件太大，发下载链接（24 小时内有效）：\n" + strings.Join(links, "\n")
		}
	}
	full, footer := text, ""
	if m := h.manifest(conv.Ext); kind != extproto.KindProgress && (m == nil || !m.Contributes.Channel.NoFooter) {
		footer = renderFooter(h.footer(), vars)
		full += "\n\n" + footer
	}
	o.Text, o.Footer = full, footer
	if kind == extproto.KindAnswer || kind == extproto.KindProgress {
		o.ReplyTo = ask.AskMsg
	}
	h.store.putOut(o)
	h.store.addMsg(InboxMsg{ID: "out:" + o.Ref, Conv: convID, At: o.At, Mine: true, Text: full, Ref: o.Ref})
	if r := h.run(conv.Ext); r != nil {
		go h.trySend(r, o)
	}
	return o, nil
}

// keepFile puts file i of send o into the send's folder on the hub, and
// gives one too big to go through the extension its link.
func (h *extHost) keepFile(o *OutboxItem, i int, f sendFile) (OutFile, error) {
	dir := filepath.Join(h.home, "ext-outbox", o.Ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return OutFile{}, err
	}
	of := OutFile{Path: filepath.Join(dir, fmt.Sprintf("%d-%s", i, uploadName(f.Name))), Name: f.Name, Mime: f.Mime}
	var size int64
	if f.ID != "" {
		src, ok := h.stagedPath(f.ID)
		if !ok {
			return OutFile{}, fmt.Errorf("bad upload id")
		}
		if err := os.Rename(src, of.Path); err != nil {
			return OutFile{}, fmt.Errorf("the upload is gone (it waits %s): %v", stagedKeep, err)
		}
		fi, err := os.Stat(of.Path)
		if err != nil {
			return OutFile{}, err
		}
		size = fi.Size()
	} else {
		b, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			return OutFile{}, err
		}
		if err := os.WriteFile(of.Path, b, 0o600); err != nil {
			return OutFile{}, err
		}
		size = int64(len(b))
	}
	if size > extFileInlineMost {
		link, err := h.fileLink(o.Ref, i, o.At)
		if err != nil {
			return OutFile{}, err
		}
		of.Link = link
	}
	return of, nil
}

// trySend makes one attempt; false when the extension could not be reached
// (the send stays pending). Anything the extension answered is final. One
// ref is tried by one goroutine at a time, and a result only lands while
// the send is still pending: a delivery event that came first wins.
func (h *extHost) trySend(r *extRun, o *OutboxItem) bool {
	if !r.running() {
		return false
	}
	if _, busy := h.sending.LoadOrStore(o.Ref, true); busy {
		return true
	}
	defer h.sending.Delete(o.Ref)
	conv, ok := h.store.conv(o.Conv)
	if !ok {
		return true
	}
	ctx := context.Background()
	var err error
	var res extproto.SendResult
	o.Tries++
	if r.man.has(capSend) {
		send := extproto.Send{Contribution: r.man.Contributes.Connector.ID, Ref: o.Ref, Chat: conv.Chat, Text: o.Text, Footer: o.Footer, Kind: o.Kind, ReplyTo: o.ReplyTo}
		var links []string
		for _, f := range o.Files {
			if f.Link != "" { // too big: its link is in the text, or the result's
				links = append(links, f.Link)
				continue
			}
			b, rerr := os.ReadFile(f.Path)
			if rerr != nil {
				o.State, o.Reason, o.DoneAt = extproto.DeliveryFailed, "附件不见了："+f.Name, time.Now()
				h.store.putOutIf(o, outPending)
				return true
			}
			blob := extproto.Blob{Name: f.Name, Mime: f.Mime, Data: base64.StdEncoding.EncodeToString(b)}
			if strings.HasPrefix(f.Mime, "image/") {
				send.Images = append(send.Images, blob)
			} else {
				send.Files = append(send.Files, blob)
			}
		}
		if len(links) > 0 && o.Kind == extproto.KindResult {
			send.Link = strings.Join(links, "\n")
		}
		err = r.call(ctx, extproto.MChannelSend, send, &res)
	} else {
		var out extproto.ActionResult
		args := map[string]any{"chat": conv.Chat, "text": o.Text}
		if o.ReplyTo != "" {
			args["reply_to"] = o.ReplyTo
		}
		err = r.call(ctx, extproto.MActionsDo, extproto.ActionsDo{Contribution: r.man.Contributes.Connector.ID,
			Name: r.man.Contributes.Connector.SendAction, Args: args, RequestID: o.Ref}, &out)
		if err == nil {
			res.MessageID = sentIDOf(out.Text)
		}
	}
	switch {
	case err == nil && res.Deferred:
		o.State, o.Reason = extproto.DeliveryDeferred, res.Reason
	case err == nil:
		o.State, o.MessageID, o.DoneAt = extproto.DeliverySent, res.MessageID, time.Now()
	case unreachable(err):
		log.Printf("ext %s: send %s: %v", r.man.ID, o.Ref, err)
		return false
	case extproto.IsCode(err, extproto.ErrUncertain):
		o.State, o.Reason, o.DoneAt = extproto.DeliveryUncertain, err.Error(), time.Now()
	default:
		o.State, o.Reason, o.DoneAt = extproto.DeliveryFailed, err.Error(), time.Now()
	}
	h.store.putOutIf(o, outPending)
	return true
}

func unreachable(err error) bool {
	for _, c := range []string{extproto.ErrClosed, extproto.ErrTimeout, extproto.ErrHubUnavailable, extproto.ErrNotRunning} {
		if extproto.IsCode(err, c) {
			return true
		}
	}
	return false
}

var sentIDRe = regexp.MustCompile(`[（(]([^）)]+)[）)]`)

// sentIDOf reads the message id out of an action's answer, "已发出（123）。".
func sentIDOf(text string) string {
	if m := sentIDRe.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// retrySends tries the extension's pending sends again, oldest first,
// until one finds it unreachable.
func (h *extHost) retrySends(r *extRun) {
	for _, o := range h.store.pendingOutsFor(r.man.ID) {
		if !h.trySend(r, o) {
			return
		}
	}
}

// delivered records what the extension says became of a send.
func (h *extHost) delivered(r *extRun, d extproto.Delivery) {
	o := h.store.out(d.Ref)
	if o == nil || !strings.HasPrefix(o.Conv, r.man.ID+":") {
		return
	}
	o.State, o.Reason = d.State, d.Reason
	if d.MessageID != "" {
		o.MessageID = d.MessageID
	}
	if d.State != extproto.DeliveryDeferred {
		o.DoneAt = time.Now()
	}
	h.store.putOutIf(o, outPending, extproto.DeliveryDeferred)
}

// interestChats: chats with a send still waiting or a timer set to send there.
func (h *extHost) interestChats(ext string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(conv string) {
		if c, ok := h.store.conv(conv); ok && c.Ext == ext && !seen[c.Chat] {
			seen[c.Chat] = true
			out = append(out, c.Chat)
		}
	}
	for _, c := range h.store.waitingConvs(outPending, extproto.DeliveryDeferred) {
		add(c)
	}
	h.sp.mu.Lock()
	var threads []string
	for _, t := range h.sp.st.Timers {
		if t.Enabled {
			threads = append(threads, t.Thread)
		}
	}
	h.sp.mu.Unlock()
	for _, th := range threads {
		if _, conv, ok := channelOf(th); ok {
			add(conv)
		}
	}
	return out
}

// homeConv is where `collie reply --to <ext>` goes: the bot's chat with you,
// or your chat with yourself.
func (h *extHost) homeConv(ext string) (InboxConv, bool) {
	for _, c := range h.store.list(ext) {
		if c.Owner || c.Kind == chatSelf {
			return c, true
		}
	}
	return InboxConv{}, false
}

// replyKind: what a reply into an extension conversation is.
func (h *extHost) replyKind(thread string, push, progress bool) string {
	switch {
	case progress:
		return extproto.KindProgress
	case push:
		return extproto.KindPush
	}
	h.sp.mu.Lock()
	defer h.sp.mu.Unlock()
	for i := len(h.sp.st.Triggers) - 1; i >= 0; i-- {
		if t := h.sp.st.Triggers[i]; t.Thread == thread {
			if t.Source == "timer" {
				return extproto.KindResult
			}
			break
		}
	}
	return extproto.KindAnswer
}

func agentLabel(kind string) string {
	if n := agentNames[kinds[kind]]; n != "" {
		return n
	}
	if n := agentNames[kind]; n != "" {
		return n
	}
	return kind
}

func (sp *Super) machineLabel(id string) string {
	if m, err := sp.s.herdr.machine(id); err == nil && m != nil {
		return m.Name
	}
	return id
}

// routerRunInfo is Collie itself, for the footer of its replies.
func (sp *Super) routerRunInfo() runInfo {
	sp.mu.Lock()
	ri := runInfo{Agent: agentLabel(sp.st.Settings.RouterKind), Worker: "Collie"}
	if run := sp.routerRunLocked(); run != nil {
		ri.Model, ri.Effort = modelLabel(run[0]), run[1]
	}
	if sp.routerUsage.Model != "" {
		ri.Model = modelLabel(sp.routerUsage.Model)
	}
	machine := sp.routerOnLocked()
	if sp.routerUsage.Window > 0 {
		ri.Ctx = fmt.Sprintf("%d%%", sp.routerUsage.Used*100/sp.routerUsage.Window)
	}
	sp.mu.Unlock()
	ri.Machine = sp.machineLabel(machine)
	return ri
}

const (
	runInfoTries = 4
	runInfoWait  = 2 * time.Second
)

// workerRunInfo is a worker, for the footer of its answers.
func (sp *Super) workerRunInfo(w *Worker) runInfo {
	ri := runInfo{Agent: agentLabel(w.Kind), Worker: w.Name, Model: modelLabel(w.Model), Effort: w.Effort}
	run := w.Run
	// What its latest reply used, from its own log: started through an
	// alias, it has no model of its own to go by.
	if local, machine := split(w.PaneID); w.PaneID != "" {
		// The cache may be older than its first reply: a stale read starts
		// a reload, so wait for that a few times before going without.
		for i := 0; i < runInfoTries; i++ {
			if ps, ok := sp.s.titlesFor(machine, context.Background())[local]; ok && ps.Ran != nil {
				run = ps.Ran
				break
			}
			time.Sleep(runInfoWait)
		}
	}
	if run != nil && run[0] != "" {
		ri.Model, ri.Effort = modelLabel(run[0]), run[1]
	}
	ri.Machine = sp.machineLabel(w.Machine)
	return ri
}

// modelLabel is a model's display name for the footer, by the same rules
// as the web app's modelName: claude-opus-5-5 → Opus 5.5, opus →
// Opus, gemini-4-neon-high → Gemini 4 Neon High, gpt-6.2-astra → GPT-6.2
// Astra; anything else stays as it is.
func modelLabel(id string) string {
	base := strings.TrimSuffix(strings.TrimSpace(id), "[1m]")
	if m := claudeModelRe.FindStringSubmatch(base); m != nil {
		v := m[2]
		if m[3] != "" {
			v += "." + m[3]
		}
		return capitalize(m[1]) + " " + v
	}
	if m := vendorModelRe.FindStringSubmatch(base); m != nil {
		var words []string
		for _, w := range strings.Split(m[2], "-") {
			if w != "" {
				words = append(words, capitalize(w))
			}
		}
		if m[1] == "gpt" {
			return "GPT-" + strings.Join(words, " ")
		}
		return "Gemini " + strings.Join(words, " ")
	}
	if bareAlias.MatchString(base) {
		return capitalize(base)
	}
	return id
}

var (
	claudeModelRe = regexp.MustCompile(`^claude-([a-z]+)-(\d+)(?:-(\d{1,2}))?(?:-\d{8})?$`)
	vendorModelRe = regexp.MustCompile(`^(gemini|gpt)-(\d[\w.-]*)$`)
)

func capitalize(w string) string {
	if w == "" {
		return w
	}
	return strings.ToUpper(w[:1]) + w[1:]
}
