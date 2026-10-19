//go:build linux

package main

import (
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

const maxProcReadBytes = 64 * 1024

func readProcFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxProcReadBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readHostSample(prev *rawMachineCounters, now time.Time) (MachineSample, rawMachineCounters, error) {
	if now.IsZero() {
		now = time.Now()
	}
	sample := MachineSample{
		TS:    now.UTC().Format(time.RFC3339),
		Disks: []MachineDiskStat{},
	}
	nextRaw := rawMachineCounters{
		takenAt: now,
		valid:   true,
	}

	if statText, err := readProcFile("/proc/stat"); err == nil {
		if busy, total, ok := parseProcStatCPU(statText); ok {
			nextRaw.cpuBusy = busy
			nextRaw.cpuTotal = total
			if prev != nil && prev.valid && total > prev.cpuTotal && busy >= prev.cpuBusy {
				deltaBusy := busy - prev.cpuBusy
				deltaTotal := total - prev.cpuTotal
				sample.CPUPct = clampPct(float64(deltaBusy) * 100.0 / float64(deltaTotal))
			}
		}
	}

	if memText, err := readProcFile("/proc/meminfo"); err == nil {
		if used, total, pct, ok := parseProcMeminfo(memText); ok {
			sample.MemUsedBytes = used
			sample.MemTotalBytes = total
			sample.MemPct = pct
		}
	}

	if loadText, err := readProcFile("/proc/loadavg"); err == nil {
		if l1, l5, l15, ok := parseProcLoadavg(loadText); ok {
			sample.Load1 = l1
			sample.Load5 = l5
			sample.Load15 = l15
		}
	}

	if upText, err := readProcFile("/proc/uptime"); err == nil {
		if up, ok := parseProcUptime(upText); ok {
			nextRaw.uptimeSec = up
		}
	}

	if netText, err := readProcFile("/proc/net/dev"); err == nil {
		rx, tx := parseProcNetDev(netText)
		nextRaw.rxBytes = rx
		nextRaw.txBytes = tx
		if prev != nil && prev.valid && !prev.takenAt.IsZero() {
			elapsed := now.Sub(prev.takenAt).Seconds()
			if elapsed > 0 {
				if rx >= prev.rxBytes {
					sample.RxBytesPerSec = int64(float64(rx-prev.rxBytes) / elapsed)
				}
				if tx >= prev.txBytes {
					sample.TxBytesPerSec = int64(float64(tx-prev.txBytes) / elapsed)
				}
			}
		}
	}

	sample.Disks = readLinuxDisks()
	return sample, nextRaw, nil
}

func readLinuxDisks() []MachineDiskStat {
	mountsText, err := readProcFile("/proc/mounts")
	if err != nil {
		return statSingleMount("/")
	}
	candidates := parseProcMounts(mountsText)
	if len(candidates) == 0 {
		return statSingleMount("/")
	}

	seenFsid := map[syscall.Fsid]bool{}
	var zeroFsid syscall.Fsid
	var disks []MachineDiskStat
	for _, m := range candidates {
		var st syscall.Statfs_t
		if err := syscall.Statfs(m.mountPoint, &st); err != nil {
			continue
		}
		if st.Blocks == 0 || st.Bsize <= 0 {
			continue
		}
		if st.Fsid != zeroFsid {
			if seenFsid[st.Fsid] {
				continue
			}
			seenFsid[st.Fsid] = true
		}
		total := int64(st.Blocks) * int64(st.Bsize)
		avail := int64(st.Bavail) * int64(st.Bsize)
		if total <= 0 {
			continue
		}
		if avail < 0 {
			avail = 0
		}
		if avail > total {
			avail = total
		}
		used := total - avail
		pct := clampPct(float64(used) * 100.0 / float64(total))
		disks = append(disks, MachineDiskStat{
			Mount:      m.mountPoint,
			UsedBytes:  used,
			TotalBytes: total,
			UsedPct:    pct,
		})
	}

	if len(disks) == 0 {
		return statSingleMount("/")
	}

	slices.SortFunc(disks, func(a, b MachineDiskStat) int {
		if a.UsedPct > b.UsedPct {
			return -1
		}
		if a.UsedPct < b.UsedPct {
			return 1
		}
		return strings.Compare(a.Mount, b.Mount)
	})
	if len(disks) > maxMachineDisks {
		disks = disks[:maxMachineDisks]
	}
	return disks
}

func statSingleMount(mount string) []MachineDiskStat {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil || st.Blocks == 0 || st.Bsize <= 0 {
		return []MachineDiskStat{}
	}
	total := int64(st.Blocks) * int64(st.Bsize)
	avail := int64(st.Bavail) * int64(st.Bsize)
	if total <= 0 {
		return []MachineDiskStat{}
	}
	used := max(int64(0), total-max(int64(0), min(avail, total)))
	return []MachineDiskStat{{
		Mount:      mount,
		UsedBytes:  used,
		TotalBytes: total,
		UsedPct:    clampPct(float64(used) * 100.0 / float64(total)),
	}}
}
