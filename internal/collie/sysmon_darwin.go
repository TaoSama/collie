package collie

import (
	"bufio"
	"context"
	"io"
	"log"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Built without cgo, collie can't ask Mach for CPU and memory, and starting
// top or vm_stat every sample costs more than the reading. So iostat and
// vm_stat run alongside, each printing a line per sysEvery, and the latest
// line is what a sample takes; one that exits starts again, a minute later
// after the first time. A reading neither renewed lately is left out, as when
// one stalls. Both go when collie does, however it ends: their next line
// finds the pipe closed. Network and disk are read each sample, the network
// by one nettop snapshot: kept running, nettop has to write through a
// terminal (script's) to write each snapshot as it comes, burns most of a
// core doing it, and outlives a collie killed with SIGHUP ignored.

const (
	darwinRestart = time.Minute
	nettopTimeout = 5 * time.Second
)

type darwinProbe struct {
	ctx      context.Context
	memTotal uint64
	net      counterPair // only sample touches it: no mu

	mu        sync.Mutex
	cpu, load *float64
	cpuAt     time.Time
	cpuPrimed bool // past iostat's first line, which is since boot
	memUsed   uint64
	memAt     time.Time
	vm        vmstatReader
}

// A resident program's output: line takes each line, reset its end.
type residentReader interface {
	line(string)
	reset()
}

func newSysProbe(ctx context.Context) sysProbe {
	go endStaleNettops()
	p := &darwinProbe{ctx: ctx, vm: vmstatReader{page: 4096}}
	p.vm.p = p
	p.memTotal, _ = strconv.ParseUint(sysctl("hw.memsize"), 10, 64)
	every := strconv.Itoa(int(sysEvery / time.Second))
	go resident(ctx, "iostat", []string{"-n", "0", "-w", every}, (*iostatReader)(p))
	go resident(ctx, "vm_stat", []string{every}, &p.vm)
	return p
}

// fresh: a reading renewed within two of its program's intervals.
func fresh(at time.Time) bool { return time.Since(at) < 2*sysEvery }

func sysctl(name string) string {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var bootRe = regexp.MustCompile(`sec = (\d+)`)

func (p *darwinProbe) info() sysInfo {
	in := sysInfo{Cores: runtime.NumCPU(), Kernel: "Darwin " + sysctl("kern.osrelease"), CPU: sysctl("machdep.cpu.brand_string"), Model: sysctl("hw.model")}
	name, _ := exec.Command("sw_vers", "-productName").Output()
	ver, _ := exec.Command("sw_vers", "-productVersion").Output()
	in.OS = strings.TrimSpace(strings.TrimSpace(string(name)) + " " + strings.TrimSpace(string(ver)))
	if m := bootRe.FindStringSubmatch(sysctl("kern.boottime")); m != nil {
		in.Boot, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return in
}

// resident runs name with args until ctx ends, handing each line to r; the
// reading it fed is dropped when it exits.
func resident(ctx context.Context, name string, args []string, r residentReader) {
	for first := true; ctx.Err() == nil; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(darwinRestart):
			}
		}
		cmd := exec.CommandContext(ctx, name, args...)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			log.Printf("sysmon: %s: %v", name, err)
			continue
		}
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			r.line(sc.Text())
		}
		io.Copy(io.Discard, out)
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			log.Printf("sysmon: %s exited: %v", name, err)
		}
		r.reset()
	}
}

// iostatReader reads " us sy id   1m   5m   15m" lines.
type iostatReader darwinProbe

func (r *iostatReader) line(line string) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return
	}
	idle, err1 := strconv.ParseFloat(f[2], 64)
	load, err2 := strconv.ParseFloat(f[3], 64)
	if err1 != nil || err2 != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cpuPrimed {
		r.cpu = ptr(100 - idle)
	}
	r.load, r.cpuAt, r.cpuPrimed = ptr(load), time.Now(), true
}

func (r *iostatReader) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cpu, r.load, r.cpuPrimed = nil, nil, false
}

// vmstatReader: used = anonymous − purgeable + wired + compressor, as
// Activity Monitor's Memory Used. The page counts are levels, so every line
// counts.
type vmstatReader struct {
	p    *darwinProbe
	page uint64
	cols []string
}

var pageRe = regexp.MustCompile(`page size of (\d+) bytes`)

func (r *vmstatReader) line(line string) {
	if m := pageRe.FindStringSubmatch(line); m != nil {
		r.page, _ = strconv.ParseUint(m[1], 10, 64)
		return
	}
	f := strings.Fields(line)
	if len(f) > 0 && f[0] == "free" {
		r.cols = f
		return
	}
	if len(f) == 0 || len(f) != len(r.cols) {
		return
	}
	at := map[string]uint64{}
	for i, c := range r.cols {
		at[c] = vmCount(f[i])
	}
	used := at["anonymous"] + at["wired"] + at["cmprssor"]
	if at["prgable"] < used {
		used -= at["prgable"]
	}
	r.p.mu.Lock()
	r.p.memUsed, r.p.memAt = used*r.page, time.Now()
	r.p.mu.Unlock()
}

func (r *vmstatReader) reset() {
	r.p.mu.Lock()
	r.p.memUsed = 0
	r.p.mu.Unlock()
}

// vmCount reads vm_stat's numbers, which shorten with K and M.
func vmCount(s string) uint64 {
	mult := uint64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1000, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1000_000, strings.TrimSuffix(s, "M")
	}
	n, _ := strconv.ParseUint(s, 10, 64)
	return n * mult
}

func (p *darwinProbe) sample(s *sysSample) []sysDisk {
	s.RX, s.TX = p.network()
	p.mu.Lock()
	if fresh(p.cpuAt) {
		s.CPU, s.Load = p.cpu, p.load
	}
	if p.memUsed > 0 && p.memTotal > 0 && fresh(p.memAt) {
		s.MemUsed, s.MemTotal = min(p.memUsed, p.memTotal), p.memTotal
	}
	p.mu.Unlock()
	// The data volume shares its APFS container with the system: the
	// container's use is the disk's.
	var disks []sysDisk
	var st syscall.Statfs_t
	if err := syscall.Statfs("/System/Volumes/Data", &st); err == nil && st.Blocks > 0 {
		bs := uint64(st.Bsize)
		disks = append(disks, sysDisk{Mount: "/", Used: (st.Blocks - st.Bfree) * bs, Total: st.Blocks * bs})
		s.DiskUsed, s.DiskTotal = disks[0].Used, disks[0].Total
	}
	return disks
}

// network: the bytes per second since the last sample that every route
// through a physical interface (en*: Wi-Fi, Ethernet, Thunderbolt; not utun
// VPNs or bridges) carried, from `nettop -m route`'s counts. Interface
// counters can't do: a Wi-Fi interface doing channel I/O counts no input at
// all.
func (p *darwinProbe) network() (rx, tx *float64) {
	ctx, cancel := context.WithTimeout(p.ctx, nettopTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nettop", "-P", "-n", "-L", "1", "-m", "route", "-J", "bytes_in,bytes_out").Output()
	if err != nil {
		if p.ctx.Err() == nil {
			log.Printf("sysmon: nettop: %v", err)
		}
		p.net.reset()
		return nil, nil
	}
	in, sent := routeBytes(string(out))
	return p.net.next(time.Now(), in, sent, "")
}

// routeBytes sums the physical routes' "route,bytes_in,bytes_out," lines.
func routeBytes(out string) (rx, tx uint64) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ",")
		if len(f) < 3 || !physicalRoute.MatchString(f[0]) {
			continue
		}
		in, err1 := strconv.ParseUint(f[1], 10, 64)
		sent, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 == nil && err2 == nil {
			rx, tx = rx+in, tx+sent
		}
	}
	return rx, tx
}

var physicalRoute = regexp.MustCompile(`-> en\d+(?:\s|$)`)

// Collie used to keep nettop running under script. One whose collie was
// killed with SIGHUP ignored lived on with launchd as its parent, spinning on
// the closed terminal. Each start ends those left: launchd's nettop children
// with that command line (nettop cuts it at the -J list's comma). Drop this
// once no computer runs a collie from before 2026-11-25.
var staleNettop = regexp.MustCompile(`^nettop -P -n -L 0 -s \d+ -m route -J bytes_in(?:,bytes_out)?$`)

func endStaleNettops() {
	ps, err := listProcs()
	if err != nil {
		log.Printf("sysmon: ps: %v", err)
		return
	}
	for _, pid := range staleNettops(ps) {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			log.Printf("sysmon: ending stale nettop %d: %v", pid, err)
			continue
		}
		log.Printf("sysmon: ended stale nettop %d", pid)
	}
}

func staleNettops(ps map[int]*proc) []int {
	var pids []int
	for _, p := range ps {
		if p.ppid == 1 && staleNettop.MatchString(strings.Join(p.args, " ")) {
			pids = append(pids, p.pid)
		}
	}
	slices.Sort(pids)
	return pids
}
