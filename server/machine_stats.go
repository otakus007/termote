package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	machineHistoryMaxSamples = 1440
	machineRange1hSamples    = 60
	machineSampleInterval    = time.Minute
	machineFlushInterval     = 5 * time.Minute
	machineRetention         = 24 * time.Hour
	machineHistoryFile       = "machine-history.json"
	machineAlertsFile        = "machine-alerts.json"
	maxMachineDisks          = 4
)

var (
	machinePartRe          = regexp.MustCompile(`^\.(machine-history|machine-alerts)-[0-9a-f]{16}\.part$`)
	errInvalidMachineRange = &codedError{"invalid_range", "range must be 1h or 24h", http.StatusBadRequest}
)

// MachineHostInfo describes the local host running Termote.
type MachineHostInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	UptimeSec int64  `json:"uptimeSec"`
}

// MachineDiskStat describes fullness of a mounted physical volume.
type MachineDiskStat struct {
	Mount      string  `json:"mount"`
	UsedBytes  int64   `json:"usedBytes"`
	TotalBytes int64   `json:"totalBytes"`
	UsedPct    float64 `json:"usedPct"`
}

// MachineSample is one 1-minute telemetry point in the 24h ring buffer.
type MachineSample struct {
	TS            string            `json:"ts"`
	CPUPct        float64           `json:"cpuPct"`
	MemUsedBytes  int64             `json:"memUsedBytes"`
	MemTotalBytes int64             `json:"memTotalBytes"`
	MemPct        float64           `json:"memPct"`
	Load1         float64           `json:"load1"`
	Load5         float64           `json:"load5"`
	Load15        float64           `json:"load15"`
	RxBytesPerSec int64             `json:"rxBytesPerSec"`
	TxBytesPerSec int64             `json:"txBytesPerSec"`
	Disks         []MachineDiskStat `json:"disks"`
}

// MachineStatusResponse answers GET /api/mux/machine.
type MachineStatusResponse struct {
	Host     MachineHostInfo     `json:"host"`
	Current  MachineSample       `json:"current"`
	Samples  []MachineSample     `json:"samples"`
	Alerts   []MachineAlertRule  `json:"alerts"`
	Machines []MachineNodeStatus `json:"machines,omitempty"`
}

type rawMachineCounters struct {
	takenAt   time.Time
	cpuBusy   uint64
	cpuTotal  uint64
	rxBytes   uint64
	txBytes   uint64
	uptimeSec int64
	valid     bool
}

type machineHistoryDisk struct {
	Samples []MachineSample `json:"samples"`
}

// machineCollector samples host resources every minute, maintains a 1440-point
// ring buffer in memory, evaluates sustained high-load alert rules, and
// persists history and alert rules under <stateDir>/machine.
type machineCollector struct {
	dir      string
	now      func() time.Time
	sampleFn func(prev *rawMachineCounters, now time.Time) (MachineSample, rawMachineCounters, error)

	mu         sync.RWMutex
	host       MachineHostInfo
	prevRaw    rawMachineCounters
	samples    []MachineSample
	current    MachineSample
	hasCurrent bool
	alerts     []MachineAlertRule
	firing     map[string]bool
	push       *pushStore
	crew       *CrewManager
	emitPush   func(pushEvent) bool
	started    bool
	closed     bool
	stopCh     chan struct{}
	doneCh     chan struct{}
}

func newMachineCollector(dir string, push *pushStore) (*machineCollector, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", dir)
		}
		if err := checkPrivateDir(dir); err != nil {
			return nil, err
		}
		removeStateParts(dir, machinePartRe)
	}

	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "localhost"
	}

	c := &machineCollector{
		dir:      dir,
		now:      time.Now,
		sampleFn: readHostSample,
		host: MachineHostInfo{
			ID:   "local",
			Name: hostname,
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
			CPUs: max(1, runtime.NumCPU()),
		},
		samples: make([]MachineSample, 0, machineHistoryMaxSamples),
		alerts:  defaultMachineAlertRules(),
		firing:  map[string]bool{},
		push:    push,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	if dir != "" {
		if err := c.loadHistory(); err != nil {
			log.Printf("machine: load history: %v", err)
		}
		if err := c.loadAlerts(); err != nil {
			log.Printf("machine: load alerts: %v", err)
		}
	}

	c.CollectOnce()
	return c, nil
}

func (c *machineCollector) setEmitPush(fn func(pushEvent) bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.emitPush = fn
	c.mu.Unlock()
}

// Start launches the background 1-minute sampling ticker and 5-minute
// persistence ticker until ctx is cancelled or Close is called.
func (c *machineCollector) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return
	}
	c.started = true
	stopCh := c.stopCh
	doneCh := c.doneCh
	c.mu.Unlock()

	go func() {
		defer close(doneCh)
		sampleTick := time.NewTicker(machineSampleInterval)
		flushTick := time.NewTicker(machineFlushInterval)
		defer sampleTick.Stop()
		defer flushTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-sampleTick.C:
				c.CollectOnce()
			case <-flushTick.C:
				if err := c.Flush(); err != nil {
					log.Printf("machine: flush history: %v", err)
				}
			}
		}
	}()
}

// Close stops the background loop and flushes the ring buffer to disk.
func (c *machineCollector) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	started := c.started
	stopCh := c.stopCh
	doneCh := c.doneCh
	c.mu.Unlock()

	close(stopCh)
	if started {
		<-doneCh
	}
	return c.Flush()
}

// CollectOnce takes a single host telemetry sample and updates the ring buffer
// and alert states.
func (c *machineCollector) CollectOnce() {
	if c == nil {
		return
	}
	now := c.now()
	c.mu.RLock()
	prev := c.prevRaw
	sampleFn := c.sampleFn
	c.mu.RUnlock()

	if sampleFn == nil {
		return
	}
	sample, nextRaw, err := sampleFn(&prev, now)
	if err != nil {
		log.Printf("machine: sample failed: %v", err)
		return
	}
	if sample.TS == "" {
		sample.TS = now.UTC().Format(time.RFC3339)
	}
	if sample.Disks == nil {
		sample.Disks = []MachineDiskStat{}
	}

	c.mu.Lock()
	c.prevRaw = nextRaw
	if nextRaw.uptimeSec > 0 {
		c.host.UptimeSec = nextRaw.uptimeSec
	}
	events := c.appendSampleLocked(sample, now)
	c.mu.Unlock()

	for _, ev := range events {
		c.emitMachineLoad(ev)
	}
}

// AppendSample appends a sample directly (used by tests and CollectOnce) and
// evaluates sustained high-load alert rules.
func (c *machineCollector) AppendSample(sample MachineSample) {
	if c == nil {
		return
	}
	now := c.now()
	if ts, err := time.Parse(time.RFC3339, sample.TS); err == nil {
		now = ts
	} else if sample.TS == "" {
		sample.TS = now.UTC().Format(time.RFC3339)
	}
	if sample.Disks == nil {
		sample.Disks = []MachineDiskStat{}
	}

	c.mu.Lock()
	events := c.appendSampleLocked(sample, now)
	c.mu.Unlock()

	for _, ev := range events {
		c.emitMachineLoad(ev)
	}
}

func (c *machineCollector) appendSampleLocked(sample MachineSample, now time.Time) []pushEvent {
	cutoff := now.Add(-machineRetention)
	kept := c.samples[:0]
	for _, s := range c.samples {
		if ts, err := time.Parse(time.RFC3339, s.TS); err == nil && ts.Before(cutoff) {
			continue
		}
		kept = append(kept, s)
	}
	c.samples = append(kept, cloneMachineSample(sample))
	if len(c.samples) > machineHistoryMaxSamples {
		excess := len(c.samples) - machineHistoryMaxSamples
		c.samples = slices.Clone(c.samples[excess:])
	}
	c.current = cloneMachineSample(sample)
	c.hasCurrent = true
	return c.evaluateAlertsLocked()
}

func (c *machineCollector) loadHistory() error {
	if c.dir == "" {
		return nil
	}
	path := filepath.Join(c.dir, machineHistoryFile)
	data, err := readStateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", machineHistoryFile, err)
	}

	var raw []MachineSample
	if err := json.Unmarshal(data, &raw); err != nil {
		var wrapped machineHistoryDisk
		if err2 := json.Unmarshal(data, &wrapped); err2 != nil {
			log.Printf("machine: %s is corrupt, starting empty: %v", machineHistoryFile, err)
			_ = os.Rename(path, path+".bad")
			return nil
		}
		raw = wrapped.Samples
	}

	cutoff := c.now().Add(-machineRetention)
	kept := make([]MachineSample, 0, min(len(raw), machineHistoryMaxSamples))
	for _, s := range raw {
		ts, err := time.Parse(time.RFC3339, s.TS)
		if err != nil || ts.Before(cutoff) {
			continue
		}
		if s.Disks == nil {
			s.Disks = []MachineDiskStat{}
		}
		kept = append(kept, s)
	}
	if len(kept) > machineHistoryMaxSamples {
		kept = kept[len(kept)-machineHistoryMaxSamples:]
	}
	c.samples = kept
	if len(kept) > 0 {
		c.current = cloneMachineSample(kept[len(kept)-1])
		c.hasCurrent = true
	}
	return nil
}

// Flush atomically writes the 24h sample ring buffer to machine-history.json.
func (c *machineCollector) Flush() error {
	if c == nil || c.dir == "" {
		return nil
	}
	c.mu.RLock()
	samples := make([]MachineSample, len(c.samples))
	for i, s := range c.samples {
		samples[i] = cloneMachineSample(s)
	}
	c.mu.RUnlock()

	data, err := json.Marshal(samples)
	if err != nil {
		return err
	}
	return writeStateFile("machine", c.dir, "machine-history", filepath.Join(c.dir, machineHistoryFile), data, false)
}

// Status returns a snapshot of host info, current telemetry, historical samples
// for rangeParam ("1h" or "24h"), and alert rules. When viewOnly is true, disk
// mount paths other than "/" are redacted to generic indices ("disk-1", ...).
func (c *machineCollector) Status(rangeParam string, viewOnly bool) MachineStatusResponse {
	if c == nil {
		return MachineStatusResponse{
			Host:    MachineHostInfo{ID: "local", OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: max(1, runtime.NumCPU())},
			Current: MachineSample{Disks: []MachineDiskStat{}},
			Samples: []MachineSample{},
			Alerts:  defaultMachineAlertRules(),
		}
	}
	limit := machineRange1hSamples
	if rangeParam == "24h" {
		limit = machineHistoryMaxSamples
	}

	c.mu.RLock()
	host := c.host
	current := cloneMachineSample(c.current)
	start := 0
	if len(c.samples) > limit {
		start = len(c.samples) - limit
	}
	samples := make([]MachineSample, len(c.samples)-start)
	for i, s := range c.samples[start:] {
		samples[i] = cloneMachineSample(s)
	}
	alerts := slices.Clone(c.alerts)
	c.mu.RUnlock()

	if viewOnly {
		current.Disks = redactMachineDisks(current.Disks)
		for i := range samples {
			samples[i].Disks = redactMachineDisks(samples[i].Disks)
		}
	}

	return MachineStatusResponse{
		Host:    host,
		Current: current,
		Samples: samples,
		Alerts:  alerts,
	}
}

func cloneMachineSample(s MachineSample) MachineSample {
	out := s
	if s.Disks != nil {
		out.Disks = slices.Clone(s.Disks)
	} else {
		out.Disks = []MachineDiskStat{}
	}
	return out
}

func redactMachineDisks(disks []MachineDiskStat) []MachineDiskStat {
	if len(disks) == 0 {
		return []MachineDiskStat{}
	}
	out := make([]MachineDiskStat, len(disks))
	idx := 1
	for i, d := range disks {
		out[i] = d
		if d.Mount == "/" || d.Mount == "" {
			out[i].Mount = "/"
			continue
		}
		out[i].Mount = fmt.Sprintf("disk-%d", idx)
		idx++
	}
	return out
}

func hasWriteRole(r *http.Request) bool {
	return !isViewOnly(r)
}

func viewHidesMachineMounts(r *http.Request) bool {
	return !hasWriteRole(r)
}

func registerMachineRoutes(mux *http.ServeMux, collector *machineCollector, allowed hostAllowlist) {
	mux.HandleFunc("/api/mux/machine", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if msg := crossSiteRejection(allowed, r); msg != "" {
			jsonError(w, msg, http.StatusForbidden)
			return
		}
		rangeParam := strings.TrimSpace(r.URL.Query().Get("range"))
		if rangeParam == "" {
			rangeParam = "1h"
		}
		if rangeParam != "1h" && rangeParam != "24h" {
			jsonErrorCode(w, errInvalidMachineRange.code, errInvalidMachineRange.msg, errInvalidMachineRange.status)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		resp := collector.Status(rangeParam, viewHidesMachineMounts(r))
		if collector != nil && collector.crew != nil {
			resp = collector.crew.AugmentMachineStatus(r, resp)
		}
		jsonOK(w, resp)
	})

	mux.HandleFunc("/api/mux/machine/alerts", func(w http.ResponseWriter, r *http.Request) {
		collector.handleAlerts(w, r)
	})
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return math.Round(v*10) / 10
}

// Linux /proc text parsers kept in machine_stats.go so unit tests and vet run
// across all target OSes.

func parseProcStatCPU(content string) (busy, total uint64, ok bool) {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, false
		}
		vals := make([]uint64, 0, len(fields)-1)
		for _, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			vals = append(vals, v)
		}
		// fields: user(0) nice(1) system(2) idle(3) iowait(4) irq(5) softirq(6) steal(7)
		user := vals[0]
		nice := vals[1]
		system := vals[2]
		idle := vals[3]
		var iowait, irq, softirq, steal uint64
		if len(vals) > 4 {
			iowait = vals[4]
		}
		if len(vals) > 5 {
			irq = vals[5]
		}
		if len(vals) > 6 {
			softirq = vals[6]
		}
		if len(vals) > 7 {
			steal = vals[7]
		}
		busy = user + nice + system + irq + softirq + steal
		total = busy + idle + iowait
		return busy, total, total > 0
	}
	return 0, 0, false
}

func parseProcMeminfo(content string) (usedBytes, totalBytes int64, usedPct float64, ok bool) {
	var memTotalKB, memAvailKB, memFreeKB, buffersKB, cachedKB, sreclaimableKB int64
	var hasTotal, hasAvail bool

	for line := range strings.SplitSeq(content, "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		val, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || val < 0 {
			continue
		}
		switch key {
		case "MemTotal":
			memTotalKB = val
			hasTotal = true
		case "MemAvailable":
			memAvailKB = val
			hasAvail = true
		case "MemFree":
			memFreeKB = val
		case "Buffers":
			buffersKB = val
		case "Cached":
			cachedKB = val
		case "SReclaimable":
			sreclaimableKB = val
		}
	}
	if !hasTotal || memTotalKB <= 0 {
		return 0, 0, 0, false
	}
	availKB := memAvailKB
	if !hasAvail {
		availKB = memFreeKB + buffersKB + cachedKB + sreclaimableKB
	}
	if availKB < 0 {
		availKB = 0
	}
	if availKB > memTotalKB {
		availKB = memTotalKB
	}
	totalBytes = memTotalKB * 1024
	usedBytes = (memTotalKB - availKB) * 1024
	usedPct = clampPct(float64(usedBytes) * 100.0 / float64(totalBytes))
	return usedBytes, totalBytes, usedPct, true
}

func parseProcLoadavg(content string) (load1, load5, load15 float64, ok bool) {
	fields := strings.Fields(content)
	if len(fields) < 3 {
		return 0, 0, 0, false
	}
	l1, err1 := strconv.ParseFloat(fields[0], 64)
	l5, err5 := strconv.ParseFloat(fields[1], 64)
	l15, err15 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err5 != nil || err15 != nil {
		return 0, 0, 0, false
	}
	return math.Round(l1*100) / 100, math.Round(l5*100) / 100, math.Round(l15*100) / 100, true
}

func parseProcUptime(content string) (int64, bool) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return 0, false
	}
	up, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || up < 0 {
		return 0, false
	}
	return int64(up), true
}

func isIgnoredNetInterface(iface string) bool {
	iface = strings.TrimSpace(iface)
	if iface == "" || iface == "lo" {
		return true
	}
	for _, prefix := range []string{
		"veth", "docker", "br-", "utun", "tailscale", "wg", "tun", "tap",
		"virbr", "cni", "flannel", "cali", "dummy",
	} {
		if strings.HasPrefix(iface, prefix) {
			return true
		}
	}
	return false
}

func parseProcNetDev(content string) (rxBytes, txBytes uint64) {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if i < 2 {
			continue
		}
		iface, rest, ok := strings.Cut(line, ":")
		if !ok || isIgnoredNetInterface(iface) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 == nil {
			rxBytes += rx
		}
		if err2 == nil {
			txBytes += tx
		}
	}
	return rxBytes, txBytes
}

func isIgnoredFSType(fsType string) bool {
	fsType = strings.ToLower(strings.TrimSpace(fsType))
	if fsType == "" {
		return true
	}
	switch fsType {
	case "tmpfs", "devtmpfs", "squashfs", "overlay", "proc", "sysfs",
		"cifs", "autofs", "nsfs", "mqueue", "hugetlbfs", "debugfs",
		"tracefs", "securityfs", "pstore", "efivarfs", "binfmt_misc",
		"configfs", "devpts", "rpc_pipefs", "ramfs", "iso9660", "udf", "bpf":
		return true
	}
	for _, prefix := range []string{"cgroup", "nfs", "smb", "fuse"} {
		if strings.HasPrefix(fsType, prefix) {
			return true
		}
	}
	return false
}

type parsedMount struct {
	device     string
	mountPoint string
	fsType     string
}

func unescapeMountPath(p string) string {
	r := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return r.Replace(p)
}

func parseProcMounts(content string) []parsedMount {
	byDevice := map[string]parsedMount{}
	var order []string

	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		dev := fields[0]
		mnt := unescapeMountPath(fields[1])
		fsType := fields[2]
		if isIgnoredFSType(fsType) {
			continue
		}
		if !strings.HasPrefix(dev, "/") && mnt != "/" {
			continue
		}
		if mnt != "/" {
			skipMount := false
			for _, prefix := range []string{
				"/proc/", "/sys/", "/dev/", "/run/", "/snap/",
				"/var/lib/docker/", "/var/lib/containers/", "/var/lib/kubelet/",
			} {
				if strings.HasPrefix(mnt+"/", prefix) {
					skipMount = true
					break
				}
			}
			if skipMount {
				continue
			}
		}
		key := dev
		if key == "" {
			key = mnt
		}
		if existing, exists := byDevice[key]; exists {
			if len(mnt) < len(existing.mountPoint) {
				byDevice[key] = parsedMount{device: dev, mountPoint: mnt, fsType: fsType}
			}
			continue
		}
		byDevice[key] = parsedMount{device: dev, mountPoint: mnt, fsType: fsType}
		order = append(order, key)
	}

	out := make([]parsedMount, 0, len(order))
	for _, k := range order {
		out = append(out, byDevice[k])
	}
	return out
}
