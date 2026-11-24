package collie

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProc(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, s := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinuxProbe(t *testing.T) {
	root := t.TempDir()
	// eth0 is physical, docker0 virtual (its link says so), lo by name.
	if err := os.MkdirAll(filepath.Join(root, "sys/class/net"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Symlink("../../devices/pci0000:00/net/eth0", filepath.Join(root, "sys/class/net/eth0"))
	os.Symlink("../../devices/virtual/net/docker0", filepath.Join(root, "sys/class/net/docker0"))
	netdev := func(eth, docker string) string {
		return "Inter-|   Receive |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes\n" +
			"    lo: 999 1 0 0 0 0 0 0 999 1 0 0 0 0 0 0\n" +
			"  eth0: " + eth + "\n" +
			"docker0: " + docker + "\n"
	}
	writeProc(t, root, map[string]string{
		"etc/os-release":            "NAME=\"Debian\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n",
		"proc/sys/kernel/osrelease": "6.1.0\n",
		"proc/cpuinfo":              "processor\t: 0\nmodel name\t: AMD EPYC 9654\n",
		"proc/stat":                 "cpu  100 0 100 700 100 0 0 0 0 0\nbtime 1700000000\n",
		"proc/loadavg":              "1.50 1.00 0.50 1/100 42\n",
		"proc/meminfo":              "MemTotal:       16000000 kB\nMemFree:  1000 kB\nMemAvailable:   12000000 kB\n",
		"proc/net/dev":              netdev("1000 0 0 0 0 0 0 0 5000 0 0 0 0 0 0 0", "7 0 0 0 0 0 0 0 7 0 0 0 0 0 0 0"),
		// root's own device, a bind of one of its directories, and a loop image
		"proc/self/mountinfo": "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n" +
			"23 22 8:1 /srv /mnt/srv rw - ext4 /dev/sda1 rw\n" +
			"24 22 7:0 / /snap/x ro - squashfs /dev/loop0 ro\n" +
			"25 22 0:5 / /proc rw - proc proc rw\n",
	})
	p := &linuxProbe{root: root}
	in := p.info()
	if in.OS != "Debian GNU/Linux 12 (bookworm)" || in.Kernel != "6.1.0" || in.CPU != "AMD EPYC 9654" || in.Boot != 1700000000 {
		t.Fatalf("info = %+v", in)
	}
	s := sysSample{T: 100}
	disks := p.sample(&s)
	if s.CPU != nil || s.RX != nil {
		t.Fatalf("first sample has rates: %+v", s)
	}
	if s.MemTotal != 16000000*1024 || s.MemUsed != 4000000*1024 || s.Load == nil || *s.Load != 1.5 {
		t.Fatalf("memory/load = %+v", s)
	}
	if len(disks) != 1 || disks[0].Mount != "/" || s.DiskTotal == 0 || s.DiskUsed != disks[0].Used {
		t.Fatalf("disks = %+v, sample %d/%d", disks, s.DiskUsed, s.DiskTotal)
	}

	writeProc(t, root, map[string]string{
		"proc/stat":    "cpu  200 0 200 750 150 0 0 0 0 0\n",
		"proc/net/dev": netdev("21000 0 0 0 0 0 0 0 6000 0 0 0 0 0 0 0", "9999 0 0 0 0 0 0 0 9999 0 0 0 0 0 0 0"),
	})
	s = sysSample{T: 110}
	p.sample(&s)
	// busy 200 of 300 (idle 50 and iowait 50 are idle)
	if s.CPU == nil || int(*s.CPU) != 66 {
		t.Fatalf("cpu = %v", s.CPU)
	}
	if s.RX == nil || *s.RX != 2000 || *s.TX != 100 {
		t.Fatalf("net = %v %v", s.RX, s.TX)
	}

	// A new interface brings its bytes since boot: no rate that time.
	os.Symlink("../../devices/pci0000:00/net/eth1", filepath.Join(root, "sys/class/net/eth1"))
	writeProc(t, root, map[string]string{"proc/net/dev": netdev("22000 0 0 0 0 0 0 0 7000 0 0 0 0 0 0 0", "0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0") +
		"  eth1: 9000000 0 0 0 0 0 0 0 9000000 0 0 0 0 0 0 0\n"})
	s = sysSample{T: 120}
	p.sample(&s)
	if s.RX != nil {
		t.Fatalf("rate across a new interface: %v", *s.RX)
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/mnt/my\040disk`); got != "/mnt/my disk" {
		t.Fatalf("got %q", got)
	}
}
