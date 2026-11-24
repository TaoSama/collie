package collie

import (
	"bufio"
	"context"
	"io"
	"log"
	"os/exec"
	"regexp"
	"runtime"
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
// after the first time; nettop the same, for the network. A reading none of
// them renewed lately is left out, as when one stalls. Disk is read each
// sample.

const darwinRestart = time.Minute

type darwinProbe struct {
	memTotal uint64

	mu        sync.Mutex
	cpu, load *float64
	cpuAt     time.Time
	cpuPrimed bool // past iostat's first line, which is since boot
	memUsed   uint64
	memAt     time.Time
	rx, tx    *float64
	netAt     time.Time
	vm        vmstatReader
	net       nettopReader
}

// A resident program's output: line takes each line, reset its end.
type residentReader interface {
	line(string)
	reset()
}

func newSysProbe(ctx context.Context) sysProbe {
	p := &darwinProbe{vm: vmstatReader{page: 4096}, net: nettopReader{every: sysEvery}}
	p.vm.p, p.net.p = p, p
	p.memTotal, _ = strconv.ParseUint(sysctl("hw.memsize"), 10, 64)
	every := strconv.Itoa(int(sysEvery / time.Second))
	go resident(ctx, "iostat", []string{"-n", "0", "-w", every}, (*iostatReader)(p))
	go resident(ctx, "vm_stat", []string{every}, &p.vm)
	// Into a pipe nettop writes in blocks, three snapshots at a time; on a
	// terminal (script's) it writes each as it comes. Its lines then end in
	// "\r", after the last comma, where nothing is read.
	go resident(ctx, "script", []string{"-q", "/dev/null", "nettop", "-P", "-n", "-L", "0", "-s", every, "-m", "route", "-J", "bytes_in,bytes_out"}, &p.net)
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
	p.mu.Lock()
	if fresh(p.cpuAt) {
		s.CPU, s.Load = p.cpu, p.load
	}
	if p.memUsed > 0 && p.memTotal > 0 && fresh(p.memAt) {
		s.MemUsed, s.MemTotal = min(p.memUsed, p.memTotal), p.memTotal
	}
	if fresh(p.netAt) {
		s.RX, s.TX = p.rx, p.tx
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

// nettopReader sums each snapshot of `nettop -m route`: the bytes every
// route through a physical interface (en*: Wi-Fi, Ethernet, Thunderbolt; not
// utun VPNs or bridges) carried. Interface counters can't do: a Wi-Fi
// interface doing channel I/O counts no input at all. A snapshot is whole
// when the next one's header comes; they are every apart. The terminal
// echoes script's end of input (^D) before the first header, so a header is
// found anywhere in its line.
type nettopReader struct {
	p      *darwinProbe
	every  time.Duration
	rx, tx uint64
	inSnap bool
	clock  time.Time // advances every per snapshot
	sums   counterPair
}

func (r *nettopReader) line(line string) {
	if strings.Contains(line, ",bytes_in,") {
		if r.inSnap {
			r.clock = r.clock.Add(r.every)
			rx, tx := r.sums.next(r.clock, r.rx, r.tx, "")
			r.p.mu.Lock()
			r.p.rx, r.p.tx, r.p.netAt = rx, tx, time.Now()
			r.p.mu.Unlock()
		}
		r.rx, r.tx, r.inSnap = 0, 0, true
		return
	}
	f := strings.Split(line, ",")
	if len(f) < 3 || !physicalRoute.MatchString(f[0]) {
		return
	}
	rx, err1 := strconv.ParseUint(f[1], 10, 64)
	tx, err2 := strconv.ParseUint(f[2], 10, 64)
	if err1 == nil && err2 == nil {
		r.rx, r.tx = r.rx+rx, r.tx+tx
	}
}

func (r *nettopReader) reset() {
	r.inSnap = false
	r.sums.reset()
	r.p.mu.Lock()
	r.p.rx, r.p.tx = nil, nil
	r.p.mu.Unlock()
}

var physicalRoute = regexp.MustCompile(`-> en\d+(?:\s|$)`)
