package collie

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// An external agent started through a launcher alias (ccx: its own gateway,
// model, config directory) has to be resumed through that alias too: a bare
// `claude --resume` misses what the alias set, or the conversation itself when
// the alias points the agent at another config directory. The process shows
// what the alias put in: the words right after the program, and the
// variables it set. The alias that put in the most of what it sets is the one.

var errShortProcArgs = errors.New("process arguments too short")

// procLaunch is how a process was started.
type procLaunch struct {
	args []string // what follows the program
	env  map[string]string
}

func addEnv(env map[string]string, kv string) map[string]string {
	k, v, ok := strings.Cut(kv, "=")
	if !ok || k == "" {
		return env
	}
	if env == nil {
		env = map[string]string{}
	}
	if _, seen := env[k]; !seen { // the first one is what getenv returns
		env[k] = v
	}
	return env
}

// readLaunch reads pid's command line and environment; nil if it can't.
// (macOS keeps its own programs' environments to themselves, not agents'.)
func readLaunch(pid int) *procLaunch {
	argv, env, err := procArgs(pid)
	if err != nil || len(argv) == 0 {
		return nil
	}
	return &procLaunch{args: argv[progIndex(argv)+1:], env: env}
}

// shellOnly: a word the shell rewrites when it runs the alias, so the process
// shows something else in its place. Glob characters are left to match as
// written ("opus[1m]"): an alias's words seldom name files.
func shellOnly(w string) bool { return strings.ContainsAny(w, "$`") }

// expandTilde expands a leading ~/ the way the shell does in an alias.
func expandTilde(w, home string) string {
	if w == "~" || strings.HasPrefix(w, "~/") {
		return home + w[1:]
	}
	return w
}

// aliasFit: how much of what a sets l shows (0: nothing to tell it from a
// bare start), and false when l shows something else.
func aliasFit(a *LauncherAlias, l *procLaunch, home string) (int, bool) {
	raw := a.launchArgs()
	if len(raw) > len(l.args) {
		return 0, false
	}
	score := 0
	for i, w := range raw {
		if w = expandTilde(w, home); shellOnly(w) {
			continue
		}
		if l.args[i] != w {
			return 0, false
		}
		score++
	}
	for k, v := range a.rawEnv {
		got, ok := l.env[k]
		if !ok {
			return 0, false
		}
		if v = expandTilde(v, home); shellOnly(v) {
			continue
		}
		if got != v {
			return 0, false
		}
		score++
	}
	return score, true
}

// sameLaunch: a and b start the agent the same way.
func sameLaunch(a, b *LauncherAlias) bool {
	return slices.Equal(a.launchArgs(), b.launchArgs()) && maps.Equal(a.rawEnv, b.rawEnv)
}

// launchedBy picks, from this computer's aliases, the one that started a
// herdrKind agent launched as l, typed into a shell around started. choices
// lists the aliases it can't tell apart when they start it differently: the
// person picks then. nil: none, or none that differs from a bare start.
func launchedBy(aliases []LauncherAlias, herdrKind string, l *procLaunch, started time.Time, home string) (a *LauncherAlias, choices []string) {
	if l == nil {
		return nil, nil
	}
	var best []*LauncherAlias
	top := 1
	for i := range aliases {
		c := &aliases[i]
		if c.Kind != herdrKind {
			continue
		}
		score, ok := aliasFit(c, l, home)
		switch {
		case !ok || score < top:
		case score > top:
			top, best = score, []*LauncherAlias{c}
		default:
			best = append(best, c)
		}
	}
	if len(best) <= 1 {
		if len(best) == 1 {
			return best[0], nil
		}
		return nil, nil
	}
	names := make([]string, len(best))
	same := true
	for i, c := range best {
		names[i] = c.Name
		same = same && sameLaunch(best[0], c)
	}
	// The one typed as it started; the same launch under several names may
	// also go by the one typed most, as it makes no difference.
	near, most := typedName(home, names, started)
	if typed := cmp.Or(near, most); typed != "" && (near != "" || same) {
		return best[slices.Index(names, typed)], nil
	}
	if same {
		return best[0], nil
	}
	return best[0], names
}

// historyTail: how much of a shell history file is read, from its end.
const historyTail = 256 << 10

// typedWindow: how far a history entry's time may be from the process start.
const typedWindow = 10 * time.Second

// typedName looks in the shell history for which of names was typed: near,
// the one entered around started; most, the one typed most. Times need
// timestamps in the history (bash's #<epoch> lines, zsh's extended format),
// and a shell that writes each command as it runs; most write theirs when
// they exit.
func typedName(home string, names []string, started time.Time) (near, most string) {
	nearOff := typedWindow + 1
	count := map[string]int{}
	for _, f := range []string{".bash_history", ".zsh_history"} {
		for _, h := range historyEntries(filepath.Join(home, f)) {
			if !slices.ContainsFunc(names, func(n string) bool { return strings.Contains(h.line, n) }) {
				continue
			}
			words, err := shellSplit(h.line)
			if err != nil {
				continue
			}
			for _, seg := range segments(words) {
				i := commandAt(seg, func(string) {})
				if i < 0 || !slices.Contains(names, seg[i]) {
					continue
				}
				count[seg[i]]++
				if off := h.at.Sub(started).Abs(); !h.at.IsZero() && off <= typedWindow && off < nearOff {
					near, nearOff = seg[i], off
				}
			}
		}
	}
	for _, n := range names {
		if count[n] > count[most] {
			most = n
		}
	}
	return near, most
}

type historyEntry struct {
	at   time.Time
	line string
}

// historyEntries reads the entries at the end of a history file, with their
// times where it has them.
func historyEntries(path string) []historyEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	cut := false
	if fi, err := f.Stat(); err == nil && fi.Size() > historyTail {
		if _, err := f.Seek(-historyTail, io.SeekEnd); err != nil {
			return nil
		}
		cut = true
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	if i := bytes.IndexByte(b, '\n'); cut && i >= 0 {
		b = b[i+1:] // the rest of a line cut in two
	}
	var out []historyEntry
	var at time.Time
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "#"): // bash: #<epoch>, then the command
			if n, err := strconv.ParseInt(l[1:], 10, 64); err == nil {
				at = time.Unix(n, 0)
				continue
			}
			at = time.Time{}
		case strings.HasPrefix(l, ": "): // zsh: : <epoch>:<seconds>;<command>
			meta, cmd, ok := strings.Cut(l[2:], ";")
			ts, _, _ := strings.Cut(meta, ":")
			if n, err := strconv.ParseInt(ts, 10, 64); ok && err == nil {
				out = append(out, historyEntry{time.Unix(n, 0), cmd})
			}
			at = time.Time{}
		default:
			out = append(out, historyEntry{at, l})
			at = time.Time{}
		}
	}
	return out
}

// localAliases: this computer's launcher aliases as last read. wait reads
// them now when that read is old; otherwise an old list is read again behind
// the scenes and used as it is meanwhile.
func (s *Server) localAliases(ctx context.Context, wait bool) []LauncherAlias {
	snap, ok := s.aliases.get("")
	if ok && time.Since(snap.tried) < aliasFresh {
		return snap.rep.Aliases
	}
	if wait {
		rep, err := s.readAliases(ctx, "")
		if err == nil {
			return rep.Aliases
		}
		log.Printf("aliases: %v", err)
		return snap.rep.Aliases
	}
	if s.aliases.reading.CompareAndSwap(false, true) {
		go func() {
			defer s.aliases.reading.Store(false)
			if _, err := s.readAliases(context.Background(), ""); err != nil {
				log.Printf("aliases: %v", err)
			}
		}()
	}
	return snap.rep.Aliases
}

// guessAlias: the alias e was started through, from aliases ("": none).
func guessAlias(e *External, aliases []LauncherAlias, home string) (name string, choices []string) {
	if a, choices := launchedBy(aliases, kinds[e.Kind], e.launch, e.Started, home); a != nil {
		return a.Name, choices
	}
	return "", nil
}

// launchMemo is what a running process was started with and the alias
// guessed from it: neither changes while it runs, and a guess may read the
// shell history. Guarded by externalCache's lock.
type launchMemo struct {
	started   time.Time
	launch    *procLaunch
	aliasesAt time.Time // the alias list the guess was made from
	alias     string
	choices   []string
}

var launchMemos = map[int]*launchMemo{}

// memoFor: pid's memo, new when the pid is another process now.
func memoFor(pid int, started time.Time) *launchMemo {
	m := launchMemos[pid]
	if m == nil || m.started.Sub(started).Abs() > 2*time.Second { // ps counts whole seconds
		m = &launchMemo{started: started, launch: readLaunch(pid)}
		launchMemos[pid] = m
	}
	return m
}

// forgetLaunches drops the memos of processes that are gone.
func forgetLaunches(ps map[int]*proc) {
	for pid := range launchMemos {
		if ps[pid] == nil {
			delete(launchMemos, pid)
		}
	}
}

// localAliasesAt is localAliases without waiting, and when that list was read.
func (s *Server) localAliasesAt() ([]LauncherAlias, time.Time) {
	aliases := s.localAliases(context.Background(), false)
	snap, _ := s.aliases.get("")
	return aliases, snap.tried
}
