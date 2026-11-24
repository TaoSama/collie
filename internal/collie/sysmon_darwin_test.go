package collie

import (
	"testing"
	"time"
)

func TestNettopLines(t *testing.T) {
	p := &darwinProbe{}
	n := &nettopReader{p: p, every: 10 * time.Second}
	read := n.line
	snap := func(in, out string) {
		read(",bytes_in,bytes_out,")
		read("default -> en0 -> 100.72.232.1," + in + "," + out + ",")
		read("100.72.232/21 -> en0,1000,1000,")
		read("default -> utun4,999999,999999,")
		read("1/8 -> utun4,5,5,")
	}
	read("^D\b\b,bytes_in,bytes_out,\r") // the first, after the terminal's echo
	read("default -> en0 -> 100.72.232.1,4000,6000,\r")
	snap("5000", "7000") // ends the first: no rate from one snapshot
	if p.rx != nil {
		t.Fatalf("a rate from one snapshot: %v", *p.rx)
	}
	snap("6000", "9000") // ends the second, (6000 − 4000) / 10 s: the first wasn't lost
	if p.rx == nil || *p.rx != 200 {
		t.Fatalf("rx %v", p.rx)
	}
	snap("7000", "9500")
	if p.rx == nil || p.tx == nil || *p.rx != 100 || *p.tx != 200 {
		t.Fatalf("rx %v tx %v", p.rx, p.tx)
	}
	n.reset()
	if p.rx != nil {
		t.Fatal("kept a rate after nettop ended")
	}
}

func TestDarwinReadings(t *testing.T) {
	p := &darwinProbe{memTotal: 48 << 30}
	io := (*iostatReader)(p)
	for _, l := range []string{"      cpu    load average", " us sy id   1m   5m   15m", " 13  7 79  7.02 6.23 5.52", " 22  9 69  6.50 6.23 5.52"} {
		io.line(l)
	}
	if p.cpu == nil || *p.cpu != 31 || *p.load != 6.5 {
		t.Fatalf("cpu %v load %v", p.cpu, p.load)
	}
	vr := &vmstatReader{p: p, page: 4096}
	vm := vr.line
	vm("Mach Virtual Memory Statistics: (page size of 16384 bytes)")
	vm("    free   active   specul inactive throttle    wired  prgable   faults     copy    0fill reactive   purged file-backed anonymous cmprssed cmprssor  dcomprs   comprs  pageins  pageout  swapins swapouts")
	vm("  178206  1147537   166965   989633        0   319004    22729   34553M 1631999K   14485M 3000652K 1022530K      864644   1439491  2530224   301367 3755802K 3948422K  951081K 61916474 13320591 20949833")
	if want := uint64(1439491-22729+319004+301367) * 16384; p.memUsed != want {
		t.Fatalf("mem used %d, want %d", p.memUsed, want)
	}
	// iostat ended: its readings go, the next first line is since boot again.
	io.reset()
	io.line(" 50 10 40  1.00 1.00 1.00")
	if p.cpu != nil {
		t.Fatalf("cpu from a since-boot line: %v", *p.cpu)
	}
}
