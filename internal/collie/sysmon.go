package collie

import (
	"context"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Every collie process (the hub, each node) samples its own computer every
// sysEvery and keeps the last sysKeep in memory: CPU, load, memory, network,
// disk and the tokens its agents ran. The devices page draws them as cards;
// nothing is stored, so a restart starts the curves again.
//
// A rate needs two readings: a sample without one leaves the field out
// rather than saying 0. Network counts physical interfaces only (virtual ones
// are local or count the same bytes again), and disk sums the local block
// devices, since a data disk is often not mounted at /.

const (
	sysEvery = 10 * time.Second
	sysKeep  = time.Hour
	// verEvery: how often the agents' and herdr's versions are read again.
	verEvery   = time.Hour
	verTimeout = 5 * time.Second
)

type sysSample struct {
	T         int64    `json:"t"` // unix seconds
	CPU       *float64 `json:"cpu,omitempty"`
	Load      *float64 `json:"load,omitempty"`
	MemUsed   uint64   `json:"mem_used,omitempty"`
	MemTotal  uint64   `json:"mem_total,omitempty"`
	RX        *float64 `json:"rx,omitempty"` // bytes per second
	TX        *float64 `json:"tx,omitempty"`
	DiskUsed  uint64   `json:"disk_used,omitempty"`
	DiskTotal uint64   `json:"disk_total,omitempty"`
}

type sysDisk struct {
	Mount string `json:"mount"`
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

type sysInfo struct {
	OS     string `json:"os,omitempty"`
	Kernel string `json:"kernel,omitempty"`
	Model  string `json:"model,omitempty"`
	CPU    string `json:"cpu,omitempty"`
	Cores  int    `json:"cores,omitempty"`
	Boot   int64  `json:"boot,omitempty"` // unix seconds
}

type sysReport struct {
	System   sysInfo           `json:"system"`
	Samples  []sysSample       `json:"samples"`
	Disks    []sysDisk         `json:"disks"`
	Tokens   tokenReport       `json:"tokens"`
	Versions map[string]string `json:"versions"` // claude, codex, agy, herdr → version
	Every    int               `json:"every"`    // seconds between samples
}

// sysProbe reads one platform's counters; sample fills what it can.
type sysProbe interface {
	info() sysInfo
	sample(s *sysSample) []sysDisk
}

type sysMonitor struct {
	home  string
	probe sysProbe
	meter *tokenMeter

	mu       sync.Mutex
	sysinfo  sysInfo
	ring     []sysSample
	disks    []sysDisk
	versions map[string]string
}

// startSysmon samples until ctx ends, on whole sysEvery boundaries so every
// computer's points share their times.
func startSysmon(ctx context.Context, home string) *sysMonitor {
	m := &sysMonitor{home: home, probe: newSysProbe(ctx), meter: newTokenMeter(home)}
	m.sysinfo = m.probe.info()
	go m.readVersions(ctx)
	go m.run(ctx)
	return m
}

func (m *sysMonitor) run(ctx context.Context) {
	for {
		now := time.Now()
		next := now.Truncate(sysEvery).Add(sysEvery)
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(now)):
		}
		m.tick(next)
	}
}

func (m *sysMonitor) tick(at time.Time) {
	s := sysSample{T: at.Unix()}
	disks := m.probe.sample(&s)
	m.meter.scan(at)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disks = disks
	m.ring = append(m.ring, s)
	if keep := int(sysKeep / sysEvery); len(m.ring) > keep {
		m.ring = append(m.ring[:0], m.ring[len(m.ring)-keep:]...)
	}
}

// report holds the samples after since (unix seconds; 0 for all).
func (m *sysMonitor) report(since int64) sysReport {
	m.mu.Lock()
	r := sysReport{System: m.sysinfo, Disks: m.disks, Versions: m.versions, Every: int(sysEvery / time.Second)}
	for _, s := range m.ring {
		if s.T > since {
			r.Samples = append(r.Samples, s)
		}
	}
	m.mu.Unlock()
	r.Tokens = m.meter.report(time.Now())
	if r.Samples == nil {
		r.Samples = []sysSample{}
	}
	return r
}

func (m *sysMonitor) readVersions(ctx context.Context) {
	for {
		v := map[string]string{}
		for _, name := range append(append([]string{}, agentKinds...), "herdr") {
			if bin := findBin(name, m.home); bin != "" {
				if ver := binVersion(ctx, bin); ver != "" {
					v[name] = ver
				}
			}
		}
		m.mu.Lock()
		m.versions = v
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(verEvery):
		}
	}
}

var versionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

// binVersion: the first version number `bin --version` prints.
func binVersion(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, verTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		log.Printf("sysmon: %s --version: %v", bin, err)
		return ""
	}
	return versionRe.FindString(string(out))
}

// getSysmon is this computer's report; the hub routes ?machine= to a node.
func (s *Server) getSysmon(w http.ResponseWriter, r *http.Request) {
	if s.sysmon == nil {
		writeJSON(w, 503, map[string]any{"error": "这台电脑没在采样"})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	writeJSON(w, 200, s.sysmon.report(since))
}

// ---------------------------------------------------------------- parsing helpers

// keyValues splits "key<sep>value" lines into a map, both trimmed.
func keyValues(text, sep string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(l, sep)
		if ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return out
}

func ptr(v float64) *float64 { return &v }

// counterPair turns two running byte counters (received, sent) into rates
// between readings. A reading whose set of sources (interfaces, routes)
// changed, or whose counters went back, starts again instead of giving a
// rate: a newcomer brings its bytes since boot.
type counterPair struct {
	at   time.Time
	a, b uint64
	set  string
	ok   bool
}

func (c *counterPair) next(at time.Time, a, b uint64, set string) (ra, rb *float64) {
	if d := at.Sub(c.at); c.ok && set == c.set && a >= c.a && b >= c.b && d > 0 {
		ra, rb = ptr(float64(a-c.a)/d.Seconds()), ptr(float64(b-c.b)/d.Seconds())
	}
	*c = counterPair{at: at, a: a, b: b, set: set, ok: true}
	return ra, rb
}

func (c *counterPair) reset() { c.ok = false }
