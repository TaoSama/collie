package collie

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// The hub keeps every computer's launcher aliases, read there through its
// node; a computer that couldn't be asked keeps its last list, marked stale.

// aliasFresh: how long a computer's list is used before it is read again.
const aliasFresh = time.Minute

// aliasAskTimeout covers a node's shell read plus the trip through the tunnel.
const aliasAskTimeout = aliasReadTimeout + 4*time.Second

type aliasSnap struct {
	rep    aliasReport
	synced time.Time // last good read; zero if none yet
	tried  time.Time
	err    string
}

type aliasBook struct {
	run sync.Mutex // one refresh at a time
	mu  sync.Mutex // by; never held across I/O
	by  map[string]*aliasSnap
}

func (b *aliasBook) get(machine string) (aliasSnap, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.by[machine]; s != nil {
		return *s, true
	}
	return aliasSnap{}, false
}

func (b *aliasBook) put(machine string, rep aliasReport, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.by == nil {
		b.by = map[string]*aliasSnap{}
	}
	s := b.by[machine]
	if s == nil {
		s = &aliasSnap{}
		b.by[machine] = s
	}
	s.tried = time.Now()
	if err != nil {
		s.err = err.Error()
		return
	}
	s.rep, s.synced, s.err = rep, s.tried, ""
}

// getAliases is the node side: this computer's launcher aliases.
func (s *Server) getAliases(w http.ResponseWriter, r *http.Request) {
	if !s.node {
		http.NotFound(w, r)
		return
	}
	rep, err := discoverAliases(r.Context(), s.home)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, rep)
}

// readAliases reads machine's aliases now and files them in the book.
func (s *Server) readAliases(ctx context.Context, machine string) (aliasReport, error) {
	var rep aliasReport
	var err error
	if machine == "" {
		rep, err = discoverAliases(ctx, s.home)
	} else {
		var client *http.Client
		if client, err = s.herdr.Client(machine); err == nil {
			ctx, cancel := context.WithTimeout(ctx, aliasAskTimeout)
			defer cancel()
			err = nodeJSON(ctx, client, "GET", "/api/aliases", nil, &rep)
			var se *nodeStatusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				err = errors.New("这台电脑上的 collie 太旧，读不了 alias")
			}
		}
	}
	s.aliases.put(machine, rep, err)
	return rep, err
}

// aliasOn finds the alias called name that starts herdrKind on machine,
// reading the computer again unless a recent read already has it.
func (s *Server) aliasOn(ctx context.Context, machine, name, herdrKind string) (*LauncherAlias, error) {
	find := func(rep aliasReport) *LauncherAlias {
		for i := range rep.Aliases {
			if rep.Aliases[i].Name == name {
				return &rep.Aliases[i]
			}
		}
		return nil
	}
	// A listed alias is used as listed: the shell resolves the name when it
	// runs. Only a missing one is worth a read, and not again right after a
	// read that failed.
	snap, ok := s.aliases.get(machine)
	a := find(snap.rep)
	if a == nil && (!ok || snap.err == "" || time.Since(snap.tried) > aliasFresh) {
		rep, err := s.readAliases(ctx, machine)
		switch {
		case err == nil:
			snap.rep, a = rep, find(rep)
		case snap.synced.IsZero():
			return nil, fmt.Errorf("读不到这台电脑的 alias：%w", err)
		}
	} else if a == nil && snap.err != "" && snap.synced.IsZero() {
		return nil, fmt.Errorf("读不到这台电脑的 alias：%s", snap.err)
	}
	if a == nil {
		return nil, fmt.Errorf("这台电脑的 %s 里没有启动 agent 的 alias %q", snap.rep.RC, name)
	}
	if a.Kind != herdrKind {
		return nil, fmt.Errorf("alias %s 启动的是 %s，不是 %s", name, a.Kind, herdrKind)
	}
	return a, nil
}

type aliasMachineView struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Shell    string          `json:"shell,omitempty"`
	RC       string          `json:"rc,omitempty"`
	SyncedAt *time.Time      `json:"synced_at,omitempty"`
	Stale    bool            `json:"stale"`
	Error    string          `json:"error,omitempty"`
	Aliases  []LauncherAlias `json:"aliases"`
}

// aliasView is one alias across computers: what the first one to report it
// says, and every computer that has one by that name.
type aliasView struct {
	LauncherAlias
	RC       string   `json:"rc"`
	Machines []string `json:"machines"`
}

// superAliases lists the launcher aliases of every computer.
func (s *Server) superAliases(w http.ResponseWriter, r *http.Request) {
	machines := s.herdr.Machines()
	// A page closed mid-read shouldn't leave every computer marked stale.
	s.refreshAliases(context.WithoutCancel(r.Context()), machines, r.URL.Query().Has("refresh"))
	writeJSON(w, 200, s.aliasViews(machines))
}

// refreshAliases reads, in parallel, every online computer whose list is old.
func (s *Server) refreshAliases(ctx context.Context, machines []Machine, force bool) {
	s.aliases.run.Lock()
	defer s.aliases.run.Unlock()
	var wg sync.WaitGroup
	for _, m := range machines {
		if !m.Local && !m.Online {
			continue
		}
		if snap, ok := s.aliases.get(m.ID); ok && !force && time.Since(snap.tried) < aliasFresh {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := s.readAliases(ctx, id); err != nil {
				log.Printf("aliases on %q: %v", id, err)
			}
		}(m.ID)
	}
	wg.Wait()
}

func (s *Server) aliasViews(machines []Machine) map[string]any {
	ms := []aliasMachineView{}
	all := []*aliasView{}
	byName := map[string]*aliasView{}
	for _, m := range machines {
		snap, ok := s.aliases.get(m.ID)
		if !ok {
			continue
		}
		v := aliasMachineView{ID: m.ID, Name: m.Name, Shell: snap.rep.Shell, RC: snap.rep.RC, Error: snap.err,
			Stale: snap.err != "" || (!m.Local && !m.Online), Aliases: snap.rep.Aliases}
		if !snap.synced.IsZero() {
			v.SyncedAt = &snap.synced
		}
		if v.Aliases == nil {
			v.Aliases = []LauncherAlias{}
		}
		ms = append(ms, v)
		for _, a := range v.Aliases {
			av := byName[a.Name]
			if av == nil {
				av = &aliasView{LauncherAlias: a, RC: v.RC}
				byName[a.Name] = av
				all = append(all, av)
			}
			if !slices.Contains(av.Machines, m.ID) {
				av.Machines = append(av.Machines, m.ID)
			}
		}
	}
	slices.SortStableFunc(all, func(a, b *aliasView) int { return strings.Compare(a.Name, b.Name) })
	return map[string]any{"machines": ms, "aliases": all}
}

// launchAlias: the alias a router or worker of herdrKind starts through on
// machine, or nil to start the agent itself. One that can't be used is said
// in the chat, and the agent starts plain.
func (sp *Super) launchAlias(ctx context.Context, machine, name, herdrKind, who string) *LauncherAlias {
	if name == "" {
		return nil
	}
	a, err := sp.s.aliasOn(ctx, machine, name, herdrKind)
	if err != nil {
		sp.mu.Lock()
		sp.addEntryLocked("event", mainThread, fmt.Sprintf("%s 没用 alias %s 启动：%v。直接启动 %s", who, name, err, herdrKind), who, "error")
		sp.mu.Unlock()
		return nil
	}
	return a
}
