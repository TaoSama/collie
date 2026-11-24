package collie

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRouteBytes(t *testing.T) {
	rx, tx := routeBytes(strings.Join([]string{
		",bytes_in,bytes_out,",
		"default -> en0 -> 100.72.232.1,5000,7000,",
		"100.72.232/21 -> en0,1000,1000,",
		"default -> utun4,999999,999999,",
		"1/8 -> utun4,5,5,",
		"3.68.81.44 -> en10,1,2,",
	}, "\n"))
	if rx != 6001 || tx != 8002 {
		t.Fatalf("rx %d tx %d", rx, tx)
	}
}

func TestStaleNettops(t *testing.T) {
	ps := map[int]*proc{}
	for _, l := range []string{
		"32165 1 nettop -P -n -L 0 -s 10 -m route -J bytes_in",                // left by a killed collie
		"401 1 nettop -P -n -L 0 -s 10 -m route -J bytes_in,bytes_out",        // the same, uncut
		"36535 36530 nettop -P -n -L 0 -s 10 -m route -J bytes_in",            // a running collie's
		"402 1 nettop -P -L 0 -s 10",                                          // someone else's
		"403 1 nettop -P -n -L 1 -m route -J bytes_in",                        // this collie's snapshot
		"404 1 /usr/bin/script -q /dev/null nettop -P -n -L 0 -s 10 -m route", // not nettop
	} {
		f := strings.Fields(l)
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		ps[pid] = &proc{pid: pid, ppid: ppid, args: f[2:]}
	}
	if got := staleNettops(ps); !slices.Equal(got, []int{401, 32165}) {
		t.Fatalf("stale %v", got)
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

// A collie killed outright, with SIGHUP ignored as nohup leaves it, leaves
// none of its sampling programs behind: they go when their pipe does, within
// one of their intervals.
func TestSysmonKilledLeavesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a sampling interval")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSysmonHelper$")
	cmd.Env = append(os.Environ(), "COLLIE_SYSMON_HELPER=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !bufio.NewScanner(out).Scan() {
		cmd.Process.Kill()
		t.Fatal("the helper didn't start sampling")
	}
	kids := descendants(t, cmd.Process.Pid)
	if len(kids) < 2 {
		t.Fatalf("sampling programs %v, want iostat and vm_stat", kids)
	}
	cmd.Process.Kill()
	cmd.Wait()
	deadline := time.Now().Add(sysEvery + 5*time.Second)
	for {
		var left []string
		for pid, args := range kids {
			if alive(pid) {
				left = append(left, strconv.Itoa(pid)+" "+args)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for pid := range kids {
				syscall.Kill(pid, syscall.SIGKILL)
			}
			t.Fatalf("left behind: %v", left)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestSysmonHelper is the collie that TestSysmonKilledLeavesNothing kills.
func TestSysmonHelper(t *testing.T) {
	if os.Getenv("COLLIE_SYSMON_HELPER") == "" {
		t.Skip("run by TestSysmonKilledLeavesNothing")
	}
	signal.Ignore(syscall.SIGHUP)
	p := newSysProbe(context.Background())
	p.sample(&sysSample{})
	time.Sleep(time.Second) // the residents are started
	fmt.Println("sampling")
	select {}
}

// descendants: pid's children, theirs and so on, with their command lines.
func descendants(t *testing.T, pid int) map[int]string {
	ps, err := listProcs()
	if err != nil {
		t.Fatal(err)
	}
	found := map[int]string{}
	for todo := []int{pid}; len(todo) > 0; todo = todo[1:] {
		for _, p := range ps {
			if p.ppid == todo[0] {
				found[p.pid] = strings.Join(p.args, " ")
				todo = append(todo, p.pid)
			}
		}
	}
	return found
}
