package collie

import (
	"context"
	"encoding/base64"
	"net/http"
	"path/filepath"
	"slices"
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
	api("GET /api/ext/header-status", s.extHeaderStatus)
	api("POST /api/ext/settings", s.extSaveSettings)
	api("POST /api/ext/{id}", s.extConfigure)
	api("POST /api/ext/{id}/login", s.extLogin)
	api("POST /api/ext/{id}/logout", s.extLogout)
	api("GET /api/ext/{id}/icon", s.extIcon)
	api("GET /api/ext/{id}/avatar", s.extAvatar)
	api("POST /api/ext/{id}/avatar", s.extSetAvatar)
	api("DELETE /api/ext/{id}/avatar", s.extClearAvatar)
	api("GET /api/ext-actions", s.extActions)
	api("GET /api/ext-markets", s.extMarkets)
	api("POST /api/ext-markets", s.extAddMarket)
	api("POST /api/ext-markets/remove", s.extRemoveMarket)
	api("POST /api/ext-markets/install", s.extInstall)
	api("POST /api/ext/{id}/uninstall", s.extUninstall)
	// Under /api/super/ so a node's own device token may fetch packages.
	api("GET /api/super/ext-pkg/{id}/{version}/{path...}", s.extPackageFile)
	api("POST /api/ext/{id}/do", s.extDo)
	api("GET /api/inbox", s.inboxList)
	api("GET /api/inbox/conv", s.inboxConv)
	api("GET /api/inbox/media", s.inboxMedia)
	api("POST /api/inbox/conv", s.inboxSetConv)
	api("POST /api/inbox/read", s.inboxRead)
	api("POST /api/inbox/reply", s.inboxReply)
	api("POST /api/inbox/draft", s.inboxDraftStart)
	api("GET /api/inbox/draft", s.inboxDraftGet)
	api("GET /api/inbox/avatar", s.inboxAvatar)
}

func (s *Server) extList(w http.ResponseWriter, r *http.Request) {
	// What the 运行在 picker shows, as every computer picker does.
	type machine struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		OS     string   `json:"os,omitempty"`
		Agents []string `json:"agents"`
		Local  bool     `json:"local"`
		Online bool     `json:"online"`
	}
	var ms []machine
	for _, m := range s.herdr.Machines() {
		id := m.ID
		if m.Local {
			id = ""
		}
		ms = append(ms, machine{ID: id, Name: m.Name, OS: m.OS, Agents: m.Agents, Local: m.Local, Online: m.Online || m.Local})
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

func (s *Server) extHeaderStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fresh := q.Get("fresh") == "1" || (q.Has("fresh") && q.Get("fresh") == "")
	slots := s.ext.headerStatuses(r.Context(), strings.TrimSpace(q.Get("model")), fresh)
	writeJSON(w, 200, map[string]any{"slots": slots})
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
	if body.Trigger != nil {
		s.ext.pushSettings()
	}
	writeJSON(w, 200, map[string]any{"trigger": set.ExtTrigger, "footer": set.ExtFooter})
}

func (s *Server) extConfigure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  *bool          `json:"enabled"`
		Machine  *string        `json:"machine"`
		Config   map[string]any `json:"config"`
		NoWrites *bool          `json:"no_writes"`
		// ClearSecrets: x-secret fields to forget (an empty value keeps one).
		ClearSecrets []string `json:"clear_secrets"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	secretsChanged := false
	if m := s.ext.manifest(id); m != nil && (body.Config != nil || body.ClearSecrets != nil) {
		var err error
		secretsChanged, err = s.ext.setConfigSecrets(m, body.Config, body.ClearSecrets)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
	}
	set, err := s.ext.configureWithRestart(id, secretsChanged, func(x *ExtSetting) {
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
	if id == featureWorkbench && body.Enabled != nil {
		s.workbenchToggled()
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
	// Hidden: left out of the list (folded in the chat app, or a rule of 接入).
	Hidden bool `json:"hidden,omitempty"`
	// Avatar: where the page gets the chat's picture, through the hub.
	Avatar string `json:"avatar,omitempty"`
	// SendAs: "self" (sends as the signed-in human, e.g. Feishu), "bot" (sends
	// only as the bound bot, e.g. WeChat), or "none" when sending is unsupported.
	SendAs    string `json:"send_as,omitempty"`
	SendMedia string `json:"send_media,omitempty"` // "all" | "none"
	NoFooter  bool   `json:"no_footer,omitempty"`
}

func (s *Server) inboxList(w http.ResponseWriter, r *http.Request) {
	convs := s.ext.store.list(r.URL.Query().Get("ext"))
	out := make([]convView, 0, len(convs))
	hides := map[string][]string{}
	readBack := map[string]bool{} // extensions that mark what you read here read in the app too
	for _, c := range convs {
		sendAs, sendMedia, noFooter := s.ext.convSendCaps(c)
		v := convView{InboxConv: s.ext.displayConversation(c), Label: c.Ext, Avatar: s.ext.convAvatarPath(c), SendAs: sendAs, SendMedia: sendMedia, NoFooter: noFooter}
		if m := s.ext.manifest(c.Ext); m != nil {
			v.Label = extLabel(m)
			if _, ok := hides[c.Ext]; !ok {
				cfg := s.ext.setting(c.Ext).Config
				hides[c.Ext] = m.hides(cfg)
				if m.readBack(cfg) {
					readBack[c.Ext] = true
				}
			}
		}
		v.Hidden = hiddenInInbox(c, hides[c.Ext])
		if c.On {
			v.Out = s.ext.store.lastOut(c.ID)
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"convs": out, "trigger": s.ext.trigger(), "read_back": readBack})
}

// hiddenInInbox: folded in the chat app, or caught by a rule switched on.
// Only the list leaves it out: your own trigger word works there as anywhere.
func hiddenInInbox(c InboxConv, rules []string) bool {
	if c.Folded {
		return true
	}
	for _, r := range rules {
		if r == hideMuted && c.Muted {
			return true
		}
		if text, ok := strings.CutPrefix(r, hideNamePrefix); ok && text != "" && strings.Contains(c.Name, text) {
			return true
		}
		if tag, ok := strings.CutPrefix(r, hideTagPrefix); ok && tag != "" && slices.Contains(c.Tags, tag) {
			return true
		}
	}
	return false
}

const (
	hideMuted      = "muted"
	hideNamePrefix = "name:"
	hideTagPrefix  = "tag:"
)

func (s *Server) inboxConv(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	c, ok := s.ext.store.conv(id)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no conversation " + id})
		return
	}
	outs := map[string]*OutboxItem{}
	for _, o := range s.ext.store.outs(`conv = ?`, id) {
		outs[o.Ref] = o
	}
	msgs := s.ext.store.msgs(id, time.Time{})
	if !c.On {
		// A closed conversation shows its source window plus any outbox sends
		// made from Collie, never saved incoming request context.
		var sent []InboxMsg
		for _, m := range msgs {
			if m.Ref != "" {
				sent = append(sent, m)
			}
		}
		msgs = sent
	}
	msgs, unavailable := s.inboxRecent(r.Context(), &c, msgs, outs)
	for i := range msgs {
		s.inboxMediaURLs(c.ID, &msgs[i])
		msgs[i].Avatar = s.senderAvatarPath(c.ID, msgs[i].Avatar)
	}
	writeJSON(w, 200, map[string]any{"conv": s.convView(c), "msgs": msgs, "outbox": outs, "history_unavailable": unavailable})
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
		if body.On != nil && *body.On {
			c.Handed = time.Now() // what came before isn't context for the first request
		}
	})
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "no conversation " + body.ID})
		return
	}
	if body.Seen || body.On != nil {
		s.ext.readConvs([]string{c.ID})
		c, _ = s.ext.store.conv(c.ID)
	}
	if body.On != nil && !*body.On {
		s.ext.store.dropMsgs(c.ID) // a closed chat keeps only its preview (§7)
	}
	writeJSON(w, 200, map[string]any{"conv": s.convView(c)})
}

// convView is one conversation as the page gets it: its label and its picture through the hub.
func (s *Server) convView(c InboxConv) convView {
	sendAs, sendMedia, noFooter := s.ext.convSendCaps(c)
	v := convView{InboxConv: s.ext.displayConversation(c), Label: c.Ext, Avatar: s.ext.convAvatarPath(c), SendAs: sendAs, SendMedia: sendMedia, NoFooter: noFooter}
	if m := s.ext.manifest(c.Ext); m != nil {
		v.Label = extLabel(m)
	}
	return v
}

// inboxRead marks conversations read (收件箱's 全部已读): in Collie, and in
// the chat app where read-back is on.
func (s *Server) inboxRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &body) {
		return
	}
	writeJSON(w, 200, map[string]any{"read": s.ext.readConvs(body.IDs)})
}

// marketView is a marketplace with what is installed from it.
type marketView struct {
	ExtMarket
	HasToken  bool              `json:"has_token"`
	Installed map[string]string `json:"installed"` // id → installed version
}

func (s *Server) marketViews(ms []ExtMarket) []marketView {
	toks := s.ext.marketTokens()
	out := []marketView{}
	for _, m := range ms {
		v := marketView{ExtMarket: m, HasToken: toks[m.Repo] != "", Installed: map[string]string{}}
		for _, e := range m.Index {
			if x := s.ext.manifest(e.ID); x != nil && x.Market != nil && x.Market.Repo == m.Repo {
				v.Installed[e.ID] = x.Version
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) extMarkets(w http.ResponseWriter, r *http.Request) {
	ms := s.ext.markets()
	if r.URL.Query().Has("fresh") {
		ms = s.ext.refreshMarkets(r.Context())
	}
	writeJSON(w, 200, map[string]any{"markets": s.marketViews(ms)})
}

func (s *Server) extAddMarket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo  string `json:"repo"`
		Ref   string `json:"ref"`
		Token string `json:"token"` // kept on the hub only, never sent back
	}
	if !decode(w, r, &body) {
		return
	}
	if _, err := s.ext.addMarket(r.Context(), strings.TrimSpace(body.Repo), strings.TrimSpace(body.Ref), strings.TrimSpace(body.Token)); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error(), "markets": s.marketViews(s.ext.markets())})
		return
	}
	writeJSON(w, 200, map[string]any{"markets": s.marketViews(s.ext.markets())})
}

func (s *Server) extRemoveMarket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo string `json:"repo"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.ext.removeMarket(body.Repo)
	writeJSON(w, 200, map[string]any{"markets": s.marketViews(s.ext.markets())})
}

func (s *Server) extInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo string `json:"repo"`
		ID   string `json:"id"`
	}
	if !decode(w, r, &body) {
		return
	}
	m, err := s.ext.installFromMarket(r.Context(), body.Repo, body.ID)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"view": s.ext.view(m.ID)})
}

func (s *Server) extUninstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Purge bool `json:"purge"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.ext.uninstall(r.Context(), r.PathValue("id"), body.Purge); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// extPackageFile serves one file of a marketplace package from the hub's
// copy, only files the package lists.
func (s *Server) extPackageFile(w http.ResponseWriter, r *http.Request) {
	m := s.ext.manifest(r.PathValue("id"))
	rel := r.PathValue("path")
	if m == nil || m.Market == nil || m.Version != r.PathValue("version") || m.Market.Files[rel] == "" {
		writeJSON(w, 404, map[string]any{"error": "no such package file"})
		return
	}
	http.ServeFile(w, r, filepath.Join(marketPackagesDir(s.ext.home), m.ID, m.Version, filepath.FromSlash(rel)))
}
