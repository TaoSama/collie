package collie

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/TaoSama/collie/internal/extproto"
	"github.com/skip2/go-qrcode"
)

// The 接入 page's extensions and the 收件箱 page's conversations.

func (s *Server) extRoutes(mux *http.ServeMux) {
	api := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.ext == nil {
				writeJSON(w, 404, map[string]any{"error": "extensions run on the hub"})
				return
			}
			h(w, r)
		})))
	}
	api("GET /api/ext", s.extList)
	api("POST /api/ext/settings", s.extSaveSettings)
	api("POST /api/ext/{id}", s.extConfigure)
	api("POST /api/ext/{id}/login", s.extLogin)
	api("POST /api/ext/{id}/logout", s.extLogout)
	api("GET /api/ext-actions", s.extActions)
	api("POST /api/ext/{id}/do", s.extDo)
	api("GET /api/inbox", s.inboxList)
	api("GET /api/inbox/conv", s.inboxConv)
	api("POST /api/inbox/conv", s.inboxSetConv)
}

func (s *Server) extList(w http.ResponseWriter, r *http.Request) {
	type machine struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Online bool   `json:"online"`
	}
	var ms []machine
	for _, m := range s.herdr.Machines() {
		id := m.ID
		if m.Local {
			id = ""
		}
		ms = append(ms, machine{ID: id, Name: m.Name, Online: m.Online || m.Local})
	}
	s.sup.mu.Lock()
	set := s.sup.st.Settings
	s.sup.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"extensions": s.ext.views(), "machines": ms,
		"trigger": set.ExtTrigger, "footer": set.ExtFooter,
		"default_trigger": defaultTrigger, "default_footer": defaultFooter,
	})
}

func (s *Server) extSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Trigger *string `json:"trigger"`
		Footer  *string `json:"footer"`
	}
	if !decode(w, r, &body) {
		return
	}
	sp := s.sup
	sp.mu.Lock()
	if body.Trigger != nil {
		t := strings.TrimSpace(*body.Trigger)
		if t != "" && triggerWord(t) != t {
			sp.mu.Unlock()
			writeJSON(w, 400, map[string]any{"error": "触发词不能有空格，最长 16 个字"})
			return
		}
		sp.st.Settings.ExtTrigger = t
	}
	if body.Footer != nil {
		sp.st.Settings.ExtFooter = strings.TrimSpace(clip(*body.Footer, 200))
	}
	set := sp.st.Settings
	sp.changed()
	sp.mu.Unlock()
	writeJSON(w, 200, map[string]any{"trigger": set.ExtTrigger, "footer": set.ExtFooter})
}

func (s *Server) extConfigure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  *bool          `json:"enabled"`
		Machine  *string        `json:"machine"`
		Config   map[string]any `json:"config"`
		NoWrites *bool          `json:"no_writes"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	set, err := s.ext.configure(id, func(x *ExtSetting) {
		if body.Enabled != nil {
			x.Enabled = *body.Enabled
		}
		if body.Machine != nil {
			x.Machine, x.Chosen = s.machineByName(*body.Machine), true
		}
		if body.NoWrites != nil {
			x.NoWrites = *body.NoWrites
		}
		if body.Config != nil {
			x.Config = body.Config
		}
	})
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"setting": set, "view": s.ext.view(id)})
}

// extLogin takes one step of the sign-in: a QR code to scan, until done.
func (s *Server) extLogin(w http.ResponseWriter, r *http.Request) {
	// Secret: a key you pasted (kind "secret"). It goes to the extension,
	// which keeps it with connector.secrets; Collie never logs or stores it.
	var body struct {
		Step   string `json:"step"`
		Secret string `json:"secret"`
	}
	if !decode(w, r, &body) {
		return
	}
	run := s.ext.run(r.PathValue("id"))
	if run == nil || !run.running() {
		writeJSON(w, 409, map[string]any{"error": "扩展还没在运行：先打开它，等状态变成运行中"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), extLoginWait)
	defer cancel()
	var step extproto.LoginStep
	args := map[string]any{"contribution": run.man.Contributes.Connector.ID, "step": body.Step}
	if body.Secret != "" {
		args["secret"] = body.Secret
	}
	if err := run.call(ctx, extproto.MLoginStep, args, &step); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"step": step}
	if step.Kind == "qr" && step.URL != "" {
		if png, err := qrcode.Encode(step.URL, qrcode.Medium, 320); err == nil {
			res["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		}
	}
	if step.Done {
		go run.refreshStatus(context.Background())
	}
	writeJSON(w, 200, res)
}

func (s *Server) extLogout(w http.ResponseWriter, r *http.Request) {
	run := s.ext.run(r.PathValue("id"))
	if run == nil || !run.running() {
		writeJSON(w, 409, map[string]any{"error": "扩展没在运行"})
		return
	}
	if err := run.call(r.Context(), extproto.MLogout, run.contribution(), nil); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	go run.refreshStatus(context.Background())
	writeJSON(w, 200, map[string]any{"ok": true})
}

type convView struct {
	InboxConv
	Label string `json:"label"`
	// The last send's state, when it isn't plainly sent.
	Out *OutboxItem `json:"out,omitempty"`
}

func (s *Server) inboxList(w http.ResponseWriter, r *http.Request) {
	convs := s.ext.store.list(r.URL.Query().Get("ext"))
	out := make([]convView, 0, len(convs))
	for _, c := range convs {
		v := convView{InboxConv: c, Label: c.Ext}
		if m := s.ext.manifest(c.Ext); m != nil {
			v.Label = extLabel(m)
		}
		if c.On {
			v.Out = s.ext.store.lastOut(c.ID)
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"convs": out, "trigger": s.ext.trigger()})
}

func (s *Server) inboxConv(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	c, ok := s.ext.store.conv(id)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no conversation " + id})
		return
	}
	outs := map[string]*OutboxItem{}
	for _, o := range s.ext.store.outs(id) {
		outs[o.Ref] = o
	}
	writeJSON(w, 200, map[string]any{"conv": c, "msgs": s.ext.store.msgs(id, time.Time{}), "outbox": outs})
}

func (s *Server) inboxSetConv(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string  `json:"id"`
		On      *bool   `json:"on"`
		Anyone  *bool   `json:"anyone"`
		Handler *string `json:"handler"`
		Seen    bool    `json:"seen"`
	}
	if !decode(w, r, &body) {
		return
	}
	c, ok := s.ext.store.update(body.ID, func(c *InboxConv) {
		if body.On != nil {
			c.On = *body.On
		}
		if body.Anyone != nil {
			c.Anyone = *body.Anyone
		}
		if body.Handler != nil && (*body.Handler == handlerCollie || *body.Handler == handlerAgent) {
			c.Handler = *body.Handler
		}
		if body.Seen || body.On != nil {
			c.Unread = 0
		}
		if body.On != nil && *body.On {
			c.Handed = time.Now() // what came before isn't context for the first request
		}
	})
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no conversation " + body.ID})
		return
	}
	if body.On != nil && !*body.On {
		s.ext.store.dropMsgs(c.ID) // a closed chat keeps only its preview (§7)
	}
	writeJSON(w, 200, map[string]any{"conv": c})
}
