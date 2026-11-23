package collie

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Collie (the router) and its dreams can run on a computer that joined the
// hub instead of on the hub itself: Settings.RouterMachine. The hub's
// ~/.collie/router stays the copy everything here reads and writes (ROUTER.md,
// memory/, the memory page, dreams); routerSync mirrors it both ways with the
// same folder on that computer, where the agent works in it.

// routerSyncEvery is how often the hub pulls what Collie wrote on its computer.
const routerSyncEvery = 10 * time.Second

// maxRouterFile: bigger files stay where they are; the folder holds notes, not data.
const maxRouterFile = 1 << 20

// routerFile is one file of the router folder, as it travels between hub and node.
type routerFile struct {
	Data []byte    `json:"data,omitempty"`
	Mod  time.Time `json:"mod"`
	Big  bool      `json:"big,omitempty"` // over maxRouterFile: listed, not sent
}

type routerFiles struct {
	Dir   string                `json:"dir"` // the folder's absolute path on that computer
	Files map[string]routerFile `json:"files"`
}

type routerChanges struct {
	Put map[string]routerFile `json:"put,omitempty"`
	Del []string              `json:"del,omitempty"`
}

// routerSync is the hub's side of the mirror. base is what both copies held
// after the last sync, so a file changed or removed on one side wins over the
// other's old copy; it is kept per computer in the kv table across restarts.
type routerSync struct {
	run sync.Mutex // held for a whole sync, network included: one at a time

	mu      sync.Mutex // the fields below; never held across I/O
	machine string
	dir     string // the folder on that computer, once a sync has told us
	base    map[string]string
	last    time.Time // last attempt
}

// routerMachineLocked: where Collie is set to run, "" for the hub. It takes
// effect at the next launch; until then routerOn says where it runs.
func (sp *Super) routerMachineLocked() string { return sp.st.Settings.RouterMachine }

// routerOn: the computer the router runs on (or last started on), "" for the hub.
func (sp *Super) routerOn() string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.routerOnLocked()
}

// machineBin is the collie binary to run on m.
func (sp *Super) machineBin(m *Machine) string {
	switch {
	case m.Local:
		return sp.bin
	case m.Bin != "":
		return m.Bin
	}
	return "collie" // a node too old to say: whatever is on its PATH
}

// routerTarget: the folder Collie works in and the collie binary it runs, on
// machine. The folder of a computer not synced with yet is "".
func (sp *Super) routerTarget(machine string) (dir, bin string) {
	if machine == "" {
		return sp.routerDir(), sp.bin
	}
	sp.rsync.mu.Lock()
	if sp.rsync.machine == machine {
		dir = sp.rsync.dir
	}
	sp.rsync.mu.Unlock()
	bin = "collie"
	if m, err := sp.s.herdr.machine(machine); err == nil {
		bin = sp.machineBin(m)
	}
	return dir, bin
}

func (sp *Super) routerCwd(machine string) string {
	dir, _ := sp.routerTarget(machine)
	return dir
}

// syncRouter brings the hub's router folder and the one on Collie's computer
// level, waiting for a sync already under way. Nothing to do on the hub.
func (sp *Super) syncRouter(ctx context.Context) error {
	machine := sp.routerOn()
	if machine == "" {
		return nil
	}
	sp.rsync.run.Lock()
	defer sp.rsync.run.Unlock()
	return sp.syncRouterWith(ctx, machine)
}

// syncRouterDue runs from the loop: a pull every routerSyncEvery, skipped
// while one is still going so a slow tunnel never holds the loop up.
func (sp *Super) syncRouterDue() {
	if sp.s == nil || sp.s.herdr == nil {
		return
	}
	machine := sp.routerOn()
	if machine == "" {
		return
	}
	rs := &sp.rsync
	rs.mu.Lock()
	due := time.Since(rs.last) >= routerSyncEvery
	if due {
		rs.last = time.Now()
	}
	rs.mu.Unlock()
	if !due || !rs.run.TryLock() {
		return
	}
	go func() {
		defer rs.run.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		known := sp.routerCwd(machine) != ""
		if err := sp.syncRouterWith(ctx, machine); err != nil {
			log.Printf("router sync with %s: %v", machine, err)
			return
		}
		if !known { // now ROUTER.md can carry that computer's paths
			if err := sp.writeRouterFiles(); err != nil {
				log.Printf("router files: %v", err)
			}
		}
	}()
}

// syncRouterNow: the hub changed the folder; the next tick sends it.
func (sp *Super) syncRouterNow() {
	sp.rsync.mu.Lock()
	sp.rsync.last = time.Time{}
	sp.rsync.mu.Unlock()
}

// syncRouterWith does one sync; the caller holds rsync.run. A hub write that
// lands while it runs can lose to the node's copy of the same file, which
// only matters for a file both sides write, and none do.
func (sp *Super) syncRouterWith(ctx context.Context, machine string) error {
	rs := &sp.rsync
	rs.mu.Lock()
	if rs.machine != machine {
		rs.machine, rs.dir, rs.base = machine, "", nil
	}
	base := rs.base
	rs.mu.Unlock()
	if base == nil {
		base = sp.loadSyncBase(machine)
	}
	client, err := sp.s.herdr.Client(machine)
	if err != nil {
		return err
	}
	var remote routerFiles
	if err := nodeJSON(ctx, client, "GET", "/api/router/files", nil, &remote); err != nil {
		return err
	}
	local, err := readRouterFiles(sp.routerDir())
	if err != nil {
		return err
	}
	push, pull, next := planRouterSync(local, remote.Files, base)
	if len(push.Put) > 0 || len(push.Del) > 0 {
		if err := nodeJSON(ctx, client, "POST", "/api/router/files", push, nil); err != nil {
			return err
		}
	}
	if err := applyRouterChanges(sp.routerDir(), pull); err != nil {
		return err
	}
	rs.mu.Lock()
	rs.dir, rs.base = remote.Dir, next
	rs.mu.Unlock()
	if base == nil || !maps.Equal(base, next) {
		sp.saveSyncBase(machine, next)
	}
	return nil
}

func (sp *Super) loadSyncBase(machine string) map[string]string {
	if sp.db == nil {
		return nil
	}
	var raw string
	err := sp.db.QueryRow(`SELECT v FROM kv WHERE k=?`, "router_sync:"+machine).Scan(&raw)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("router sync base: %v", err)
		}
		return nil
	}
	var base map[string]string
	if err := json.Unmarshal([]byte(raw), &base); err != nil {
		log.Printf("router sync base: %v", err)
		return nil
	}
	return base
}

func (sp *Super) saveSyncBase(machine string, base map[string]string) {
	if sp.db == nil {
		return
	}
	b, err := json.Marshal(base)
	if err == nil {
		_, err = sp.db.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, "router_sync:"+machine, string(b))
	}
	if err != nil {
		log.Printf("router sync base: %v", err)
	}
}

// planRouterSync is the three-way merge of the two copies against base. With
// no base (a computer never synced with) the hub's copy is the truth: the
// other side may hold files Collie long since moved or removed.
// It returns what to send to the node, what to apply here, and the new base.
func planRouterSync(local, remote map[string]routerFile, base map[string]string) (push, pull routerChanges, next map[string]string) {
	push.Put, pull.Put, next = map[string]routerFile{}, map[string]routerFile{}, map[string]string{}
	seen := map[string]bool{}
	for _, m := range []map[string]routerFile{local, remote} {
		for p := range m {
			seen[p] = true
		}
	}
	for p := range base {
		seen[p] = true
	}
	for p := range seen {
		l, lok := local[p]
		r, rok := remote[p]
		if l.Big || r.Big { // too big to mirror: leave both copies alone
			if b, ok := base[p]; ok {
				next[p] = b
			}
			continue
		}
		lh, rh := hashOf(l, lok), hashOf(r, rok)
		b, known := base[p]
		switch {
		case lh == rh:
		case base == nil, known && rh == b: // only the hub changed it
			push.take(p, l, lok)
			rh = lh
		case known && lh == b: // only the node changed it
			pull.take(p, r, rok)
			lh = rh
		case !rok || (lok && !l.Mod.Before(r.Mod)): // both did, or new on both: the newer (or only) copy
			push.take(p, l, lok)
			rh = lh
		default:
			pull.take(p, r, rok)
			lh = rh
		}
		if lh != "" {
			next[p] = lh
		}
	}
	return push, pull, next
}

func (c *routerChanges) take(p string, f routerFile, ok bool) {
	if ok {
		c.Put[p] = f
	} else {
		c.Del = append(c.Del, p)
	}
}

func hashOf(f routerFile, ok bool) string {
	if !ok {
		return ""
	}
	h := sha256.Sum256(f.Data)
	return hex.EncodeToString(h[:])
}

// readRouterFiles lists the folder: regular files only, by slash path.
func readRouterFiles(dir string) (map[string]routerFile, error) {
	out := map[string]routerFile{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if fi.Size() > maxRouterFile {
			out[filepath.ToSlash(rel)] = routerFile{Mod: fi.ModTime(), Big: true}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = routerFile{Data: b, Mod: fi.ModTime()}
		return nil
	})
	return out, err
}

// applyRouterChanges writes and removes files inside dir, keeping their times.
func applyRouterChanges(dir string, c routerChanges) error {
	for p, f := range c.Put {
		full, err := routerPath(dir, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(full, f.Data, 0o600); err != nil {
			return err
		}
		if !f.Mod.IsZero() {
			if err := os.Chtimes(full, f.Mod, f.Mod); err != nil {
				return err
			}
		}
	}
	for _, p := range c.Del {
		full, err := routerPath(dir, p)
		if err != nil {
			return err
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// routerPath resolves a synced path inside dir and refuses anything outside it.
func routerPath(dir, rel string) (string, error) {
	clean := path.Clean("/" + rel)[1:]
	if clean == "" || clean != rel || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("bad router path %q", rel)
	}
	return filepath.Join(dir, filepath.FromSlash(clean)), nil
}

// nodeJSON calls a node's own API through the tunnel.
func nodeJSON(ctx context.Context, client *http.Client, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://node"+p, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// An older node has no such route: 404.
		return fmt.Errorf("node %s %s: %s %s", method, p, resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// routerContext is checkHandover's reading of Collie's context, wherever it runs.
func (sp *Super) routerContext(ctx context.Context, machine, kind, session, pane string, window int) ctxUsage {
	cwd := sp.routerCwd(machine)
	if machine == "" {
		return sp.s.contextFor(kind, session, pane, cwd, window)
	}
	client, err := sp.s.herdr.Client(machine)
	if err != nil {
		return ctxUsage{}
	}
	local, _ := split(pane)
	q := url.Values{"kind": {kind}, "session": {session}, "pane": {local}, "cwd": {cwd}, "window": {fmt.Sprint(window)}}
	var u ctxUsage
	if err := nodeJSON(ctx, client, "GET", "/api/context?"+q.Encode(), nil, &u); err != nil {
		log.Printf("router context: %v", err)
	}
	return u
}

// remoteMessages reads an ended session of Collie's on another computer, for
// the dream's input.
func (sp *Super) remoteMessages(ctx context.Context, e *Ended) []*Message {
	client, err := sp.s.herdr.Client(e.Machine)
	if err != nil {
		return nil
	}
	q := url.Values{"kind": {e.Kind}, "session": {e.Session}, "cwd": {e.Cwd}}
	var out struct {
		Messages []*Message `json:"messages"`
	}
	if err := nodeJSON(ctx, client, "GET", "/api/transcript?"+q.Encode(), nil, &out); err != nil {
		log.Printf("dream input from %s: %v", e.Machine, err)
		return nil
	}
	return out.Messages
}
