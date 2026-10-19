package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMachineProcParsers(t *testing.T) {
	statText := `cpu  1000 100 500 8000 200 50 50 100 0 0
cpu0 500 50 250 4000 100 25 25 50 0 0
`
	busy, total, ok := parseProcStatCPU(statText)
	if !ok {
		t.Fatal("expected parseProcStatCPU to succeed")
	}
	// busy = 1000 + 100 + 500 + 50 + 50 + 100 = 1800
	// total = 1800 + 8000 + 200 = 10000
	if busy != 1800 || total != 10000 {
		t.Fatalf("unexpected cpu counters: busy=%d total=%d", busy, total)
	}

	memText := `MemTotal:       16000000 kB
MemFree:         1000000 kB
MemAvailable:   10000000 kB
Buffers:          500000 kB
Cached:          8000000 kB
`
	usedBytes, totalBytes, usedPct, ok := parseProcMeminfo(memText)
	if !ok {
		t.Fatal("expected parseProcMeminfo to succeed")
	}
	if totalBytes != 16000000*1024 {
		t.Fatalf("unexpected totalBytes: %d", totalBytes)
	}
	// MemTotal - MemAvailable = 6000000 kB (37.5%)
	if usedBytes != 6000000*1024 || usedPct != 37.5 {
		t.Fatalf("unexpected memory: used=%d pct=%.1f", usedBytes, usedPct)
	}

	l1, l5, l15, ok := parseProcLoadavg("2.15 1.80 1.45 3/842 19234\n")
	if !ok || l1 != 2.15 || l5 != 1.80 || l15 != 1.45 {
		t.Fatalf("unexpected loadavg: %v %v %v (ok=%v)", l1, l5, l15, ok)
	}

	up, ok := parseProcUptime("86400.75 123456.00\n")
	if !ok || up != 86400 {
		t.Fatalf("unexpected uptime: %d (ok=%v)", up, ok)
	}

	netText := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999999    100    0    0    0     0          0         0  9999999    100    0    0    0     0       0          0
veth1a: 5555555    100    0    0    0     0          0         0  5555555    100    0    0    0     0       0          0
docker0: 444444    100    0    0    0     0          0         0  4444444    100    0    0    0     0       0          0
tailscale0: 3333   100    0    0    0     0          0         0     3333    100    0    0    0     0       0          0
  eth0: 1000000    200    0    0    0     0          0         0   250000    150    0    0    0     0       0          0
wlan0:   500000    100    0    0    0     0          0         0   150000     80    0    0    0     0       0          0
`
	rx, tx := parseProcNetDev(netText)
	if rx != 1500000 || tx != 400000 {
		t.Fatalf("unexpected net counters (virtual interfaces should be skipped): rx=%d tx=%d", rx, tx)
	}

	mountsText := `proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
tmpfs /run tmpfs rw,nosuid,nodev,size=3276800k,nr_inodes=819200,mode=755 0 0
/dev/nvme0n1p2 / ext4 rw,relatime 0 0
/dev/nvme0n1p2 /var/snap/firefox/common/host-hunspell ext4 ro,noexec,noatime 0 0
/dev/nvme1n1p1 /home/alice/work\040drive xfs rw,relatime 0 0
server:/export /mnt/nfs nfs4 rw,relatime 0 0
overlay /var/lib/docker/overlay2/abc/merged overlay rw 0 0
`
	mounts := parseProcMounts(mountsText)
	if len(mounts) != 2 {
		t.Fatalf("expected 2 physical mounts after deduplication and filtering, got %d: %+v", len(mounts), mounts)
	}
	if mounts[0].mountPoint != "/" || mounts[1].mountPoint != "/home/alice/work drive" {
		t.Fatalf("unexpected mounts: %+v", mounts)
	}
}

func TestMachineRingBufferRolloverAndPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "machine")
	c, err := newMachineCollector(dir, nil)
	if err != nil {
		t.Fatalf("newMachineCollector: %v", err)
	}
	base := time.Now().UTC().Add(-1500 * time.Minute).Truncate(time.Minute)
	c.mu.Lock()
	c.samples = nil
	c.mu.Unlock()

	for i := range 1500 {
		ts := base.Add(time.Duration(i) * time.Minute)
		c.now = func() time.Time { return ts }
		c.AppendSample(MachineSample{
			TS:     ts.Format(time.RFC3339),
			CPUPct: float64(i % 100),
			MemPct: 45.0,
			Disks: []MachineDiskStat{
				{Mount: "/", UsedBytes: 100, TotalBytes: 200, UsedPct: 50.0},
			},
		})
	}

	status24h := c.Status("24h", false)
	if len(status24h.Samples) != machineHistoryMaxSamples {
		t.Fatalf("expected %d samples in 24h ring buffer, got %d", machineHistoryMaxSamples, len(status24h.Samples))
	}
	expectedFirstTS := base.Add(60 * time.Minute).Format(time.RFC3339)
	if status24h.Samples[0].TS != expectedFirstTS {
		t.Fatalf("expected oldest sample after rollover to be %s, got %s", expectedFirstTS, status24h.Samples[0].TS)
	}

	status1h := c.Status("1h", false)
	if len(status1h.Samples) != machineRange1hSamples {
		t.Fatalf("expected %d samples for 1h range, got %d", machineRange1hSamples, len(status1h.Samples))
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	histPath := filepath.Join(dir, machineHistoryFile)
	fi, err := os.Stat(histPath)
	if err != nil {
		t.Fatalf("stat %s: %v", histPath, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions on %s, got %o", histPath, fi.Mode().Perm())
	}

	// Reload from disk and verify samples are retained.
	c2, err := newMachineCollector(dir, nil)
	if err != nil {
		t.Fatalf("reload newMachineCollector: %v", err)
	}
	reloaded := c2.Status("24h", false)
	if len(reloaded.Samples) < 1400 {
		t.Fatalf("expected reloaded history to keep recent 24h samples, got %d", len(reloaded.Samples))
	}
}

func TestMachineViewRoleMountRedactionAndEndpoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "machine")
	c, err := newMachineCollector(dir, nil)
	if err != nil {
		t.Fatalf("newMachineCollector: %v", err)
	}
	c.mu.Lock()
	c.samples = nil
	c.mu.Unlock()
	now := time.Now().UTC().Truncate(time.Minute)
	c.AppendSample(MachineSample{
		TS:     now.Format(time.RFC3339),
		CPUPct: 42.5,
		MemPct: 61.2,
		Disks: []MachineDiskStat{
			{Mount: "/", UsedBytes: 100, TotalBytes: 200, UsedPct: 50.0},
			{Mount: "/home/tuannh/secret-project", UsedBytes: 150, TotalBytes: 200, UsedPct: 75.0},
			{Mount: "/Users/alice/private-mount", UsedBytes: 180, TotalBytes: 200, UsedPct: 90.0},
		},
	})

	mux := http.NewServeMux()
	allowed := parseAllowedHosts("", false)
	registerMachineRoutes(mux, c, allowed)

	// Full role sees actual mount paths.
	reqFull := httptest.NewRequest(http.MethodGet, "http://localhost/api/mux/machine?range=1h", nil)
	reqFull = reqFull.WithContext(withAuth(reqFull.Context(), authInfo{Kind: authSession, Role: roleFull}))
	recFull := httptest.NewRecorder()
	mux.ServeHTTP(recFull, reqFull)
	if recFull.Code != http.StatusOK {
		t.Fatalf("expected 200 for full role, got %d: %s", recFull.Code, recFull.Body.String())
	}
	var respFull MachineStatusResponse
	if err := json.Unmarshal(recFull.Body.Bytes(), &respFull); err != nil {
		t.Fatalf("unmarshal full response: %v", err)
	}
	if len(respFull.Current.Disks) != 3 || respFull.Current.Disks[1].Mount != "/home/tuannh/secret-project" {
		t.Fatalf("expected full role to see custom mount path, got %+v", respFull.Current.Disks)
	}

	// View role has non-root mounts redacted to disk-1, disk-2.
	reqView := httptest.NewRequest(http.MethodGet, "http://localhost/api/mux/machine?range=1h", nil)
	reqView = reqView.WithContext(withAuth(reqView.Context(), authInfo{Kind: authDevice, Role: roleView}))
	recView := httptest.NewRecorder()
	mux.ServeHTTP(recView, reqView)
	if recView.Code != http.StatusOK {
		t.Fatalf("expected 200 for view role, got %d", recView.Code)
	}
	var respView MachineStatusResponse
	if err := json.Unmarshal(recView.Body.Bytes(), &respView); err != nil {
		t.Fatalf("unmarshal view response: %v", err)
	}
	if len(respView.Current.Disks) != 3 {
		t.Fatalf("expected 3 disks in view response, got %d", len(respView.Current.Disks))
	}
	if respView.Current.Disks[0].Mount != "/" ||
		respView.Current.Disks[1].Mount != "disk-1" ||
		respView.Current.Disks[2].Mount != "disk-2" {
		t.Fatalf("expected view-role mounts [/, disk-1, disk-2], got %+v", respView.Current.Disks)
	}
	for _, s := range respView.Samples {
		for _, d := range s.Disks {
			if d.Mount != "/" && d.Mount != "disk-1" && d.Mount != "disk-2" {
				t.Fatalf("view-role sample leaked mount path %q", fmt.Sprintf("%s", d.Mount))
			}
		}
	}
}
