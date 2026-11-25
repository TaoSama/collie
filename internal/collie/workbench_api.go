package collie

import (
	"net/http"
	"time"
)

func (s *Server) wbRoutes(mux *http.ServeMux) {
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("GET /api/wb", s.wbGet)
	api("POST /api/wb", s.wbSave)
	api("POST /api/wb/token", s.wbToken)
	api("POST /api/wb/probe", s.wbProbeURL)
	api("GET /api/wb/ls", s.wbList)
	api("GET /api/wb/read/{id}", s.wbReadModule)
}

// wbModuleView is a module as the page sees it: no token, only whether one is set.
type wbModuleView struct {
	WBModule
	HasToken bool      `json:"has_token,omitempty"`
	SnapAt   time.Time `json:"snap_at,omitzero"`
	SnapErr  string    `json:"snap_err,omitempty"`
}

// views is the layout as the page and `collie wb ls` see it.
func (wb *workbench) views() ([]WBGroup, []wbModuleView, error) {
	wb.mu.Lock()
	secrets, err := wb.secrets()
	groups := append([]WBGroup{}, wb.cfg.Groups...)
	mods := make([]wbModuleView, 0, len(wb.cfg.Modules))
	for _, m := range wb.cfg.Modules {
		mods = append(mods, wbModuleView{WBModule: m, HasToken: secrets[m.ID] != "", SnapErr: wb.checked[m.ID].err})
	}
	wb.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	for i := range mods {
		if mods[i].Share == wbShareKeep {
			_, mods[i].SnapAt = wb.latestSnap(mods[i].ID)
		}
	}
	return groups, mods, nil
}

func (s *Server) wbGet(w http.ResponseWriter, r *http.Request) {
	groups, mods, err := s.sup.wb.views()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"groups": groups, "modules": mods})
}

func (s *Server) wbSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		wbConfig
		Purge []string `json:"purge"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.sup.wb.save(body.wbConfig, body.Purge); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s.wbGet(w, r)
}

func (s *Server) wbToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.sup.wb.setToken(body.ID, body.Token); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// wbProbeURL tells the add form whether a page can be embedded here. The
// frame's parent is the page that asks: its Origin.
func (s *Server) wbProbeURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !httpURL(body.URL) {
		writeJSON(w, 400, map[string]any{"error": "网址要以 http:// 或 https:// 开头"})
		return
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "https://" + r.Host
	}
	p, err := probeEmbed(r.Context(), s.sup.wb.client, body.URL, origin)
	if err != nil {
		writeJSON(w, 200, wbProbe{Embed: wbEmbedTab, Reason: "打不开：" + err.Error()})
		return
	}
	writeJSON(w, 200, p)
}

// wbList is `collie wb ls`: the modules shared with agents.
func (s *Server) wbList(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.wbCaller(r, r.URL.Query().Get("pane")); err != nil {
		writeJSON(w, 403, map[string]any{"error": err.Error()})
		return
	}
	type row struct {
		ID        string    `json:"id"`
		Title     string    `json:"title"`
		Share     string    `json:"share"`
		Sensitive bool      `json:"sensitive,omitempty"`
		SnapAt    time.Time `json:"snap_at,omitzero"`
	}
	_, mods, err := s.sup.wb.views()
	if err != nil {
		fail(w, err)
		return
	}
	out := []row{}
	for _, m := range mods {
		if m.Share != wbShareOff {
			out = append(out, row{ID: m.ID, Title: m.Title, Share: m.Share, Sensitive: m.Sensitive, SnapAt: m.SnapAt})
		}
	}
	writeJSON(w, 200, map[string]any{"modules": out})
}

// wbReadModule is `collie wb read ID` for agents, and the page's 「发给
// Collie」 (human=1): the human may send any module that has a feed.
func (s *Server) wbReadModule(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	who, external, err := s.wbCaller(r, q.Get("pane"))
	if err != nil {
		writeJSON(w, 403, map[string]any{"error": err.Error()})
		return
	}
	m, ok := s.sup.wb.module(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "没有这个模块：" + r.PathValue("id") + "（collie wb ls 看有哪些）"})
		return
	}
	if err := wbAllowed(m, who == wbHuman && q.Get("human") == "1", external); err != nil {
		writeJSON(w, 403, map[string]any{"error": err.Error()})
		return
	}
	head, body, err := s.sup.wb.read(r.Context(), m)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"id": m.ID, "title": m.Title, "text": head + body})
}
