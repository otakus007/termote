//go:build darwin

package main

import (
	"syscall"
	"time"
)

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

	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil && st.Blocks > 0 && st.Bsize > 0 {
		total := int64(st.Blocks) * int64(st.Bsize)
		avail := int64(st.Bavail) * int64(st.Bsize)
		if total > 0 {
			used := max(int64(0), total-max(int64(0), min(avail, total)))
			sample.Disks = []MachineDiskStat{{
				Mount:      "/",
				UsedBytes:  used,
				TotalBytes: total,
				UsedPct:    clampPct(float64(used) * 100.0 / float64(total)),
			}}
		}
	}

	return sample, nextRaw, nil
}
