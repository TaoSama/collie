package collie

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type linuxProbe struct {
	root string // "/" outside tests

	cpuPrimed       bool
	cpuBusy, cpuAll uint64
	net             counterPair
	virtual         map[string]bool // interface → software, as /sys says
	disksAt         time.Time
	diskList        []sysDisk
}

// diskEvery: disks fill slowly; their mounts are read again this often.
const diskEvery = time.Minute

func newSysProbe(context.Context) sysProbe { return &linuxProbe{root: "/"} }

func (p *linuxProbe) read(path string) string {
	b, _ := os.ReadFile(filepath.Join(p.root, path))
	return string(b)
}

func (p *linuxProbe) info() sysInfo {
	in := sysInfo{Cores: runtime.NumCPU()}
	osr := p.read("etc/os-release")
	if osr == "" {
		osr = p.read("usr/lib/os-release")
	}
	in.OS = keyValues(osr, "=")["PRETTY_NAME"]
	in.Kernel = strings.TrimSpace(p.read("proc/sys/kernel/osrelease"))
	cpu := keyValues(p.read("proc/cpuinfo"), ":")
	in.CPU = cpu["model name"]
	if in.CPU == "" {
		in.CPU = cpu["Hardware"]
	}
	vendor := strings.TrimSpace(p.read("sys/class/dmi/id/sys_vendor"))
	product := strings.TrimSpace(p.read("sys/class/dmi/id/product_name"))
	in.Model = strings.TrimSpace(vendor + " " + product)
	for _, l := range strings.Split(p.read("proc/stat"), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			in.Boot, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return in
}

func (p *linuxProbe) sample(s *sysSample) []sysDisk {
	now := time.Unix(s.T, 0)
	p.sampleCPU(s)
	if f := strings.Fields(p.read("proc/loadavg")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			s.Load = ptr(v)
		}
	}
	p.sampleMem(s)
	p.sampleNet(s, now)
	if now.Sub(p.disksAt) >= diskEvery {
		p.disksAt, p.diskList = now, p.disks()
	}
	disks := p.diskList
	for _, d := range disks {
		s.DiskUsed += d.Used
		s.DiskTotal += d.Total
	}
	return disks
}

// sampleCPU: busy = all − idle − iowait (waiting on I/O is idle, as top's id + wa).
func (p *linuxProbe) sampleCPU(s *sysSample) {
	line, _, _ := strings.Cut(p.read("proc/stat"), "\n")
	f := strings.Fields(line)
	if len(f) < 6 || f[0] != "cpu" {
		return
	}
	var all, idle uint64
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		if i >= 8 { // guest time is already in user
			break
		}
		all += n
		if i == 3 || i == 4 {
			idle += n
		}
	}
	busy := all - idle
	if p.cpuPrimed && all > p.cpuAll && busy >= p.cpuBusy {
		s.CPU = ptr(float64(busy-p.cpuBusy) * 100 / float64(all-p.cpuAll))
	}
	p.cpuPrimed, p.cpuBusy, p.cpuAll = true, busy, all
}

// sampleMem: used = MemTotal − MemAvailable, as free's "available".
func (p *linuxProbe) sampleMem(s *sysSample) {
	kb := func(v string) uint64 {
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		return n * 1024
	}
	m := keyValues(p.read("proc/meminfo"), ":")
	total := kb(m["MemTotal"])
	avail := kb(m["MemAvailable"])
	if _, ok := m["MemAvailable"]; !ok {
		avail = kb(m["MemFree"]) + kb(m["Buffers"]) + kb(m["Cached"])
	}
	if total > 0 && avail <= total {
		s.MemTotal, s.MemUsed = total, total-avail
	}
}

// virtualNet: software interfaces, whose bytes are local or counted again on
// a physical one. The kernel files them under /sys/devices/virtual/net.
func (p *linuxProbe) virtualNet(name string) bool {
	if v, ok := p.virtual[name]; ok {
		return v
	}
	if link, err := os.Readlink(filepath.Join(p.root, "sys/class/net", name)); err == nil {
		if p.virtual == nil {
			p.virtual = map[string]bool{}
		}
		p.virtual[name] = strings.Contains(link, "/virtual/")
		return p.virtual[name]
	}
	for _, pre := range []string{"lo", "docker", "br-", "veth", "virbr", "tailscale", "tun", "tap", "wg", "zt", "cni", "flannel", "cali"} {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

func (p *linuxProbe) sampleNet(s *sysSample, now time.Time) {
	var rx, tx uint64
	var names []string
	for _, l := range strings.Split(p.read("proc/net/dev"), "\n") {
		name, rest, ok := strings.Cut(l, ":")
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if !ok || len(f) < 9 || p.virtualNet(name) {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx, tx = rx+r, tx+t
		names = append(names, name)
	}
	slices.Sort(names)
	s.RX, s.TX = p.net.next(now, rx, tx, strings.Join(names, ","))
}

// disks sums the local block devices, each once: data disks often mount
// elsewhere than / (devbox's /data00). A bind of one directory is the same
// device again, and loop or read-only images aren't storage.
func (p *linuxProbe) disks() []sysDisk {
	var out []sysDisk
	seen := map[string]bool{}
	for _, l := range strings.Split(p.read("proc/self/mountinfo"), "\n") {
		pre, post, ok := strings.Cut(l, " - ")
		f, g := strings.Fields(pre), strings.Fields(post)
		if !ok || len(f) < 5 || len(g) < 2 {
			continue
		}
		dev, root, mount, fstype, src := f[2], f[3], unescapeMount(f[4]), g[0], g[1]
		if !strings.HasPrefix(src, "/dev/") || strings.HasPrefix(src, "/dev/loop") || fstype == "squashfs" || fstype == "iso9660" {
			continue
		}
		if seen[dev] || (root != "/" && mount != "/" && fstype != "btrfs") {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(filepath.Join(p.root, mount), &st); err != nil || st.Blocks == 0 {
			continue
		}
		seen[dev] = true
		bs := uint64(st.Bsize)
		out = append(out, sysDisk{Mount: mount, Used: (st.Blocks - st.Bfree) * bs, Total: st.Blocks * bs})
	}
	return out
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
