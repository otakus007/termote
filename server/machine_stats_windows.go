//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	modKernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = modKernel32.NewProc("GetDiskFreeSpaceExW")
	procGetSystemTimes       = modKernel32.NewProc("GetSystemTimes")
	procGetTickCount64       = modKernel32.NewProc("GetTickCount64")
)

type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

type winFiletime struct {
	lowDateTime  uint32
	highDateTime uint32
}

func (ft winFiletime) uint64() uint64 {
	return (uint64(ft.highDateTime) << 32) | uint64(ft.lowDateTime)
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

	if r1, _, _ := procGetTickCount64.Call(); r1 > 0 {
		nextRaw.uptimeSec = int64(r1 / 1000)
	}

	var idleFT, kernelFT, userFT winFiletime
	if r1, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFT)),
		uintptr(unsafe.Pointer(&kernelFT)),
		uintptr(unsafe.Pointer(&userFT)),
	); r1 != 0 {
		idle := idleFT.uint64()
		total := kernelFT.uint64() + userFT.uint64()
		if total >= idle {
			busy := total - idle
			nextRaw.cpuBusy = busy
			nextRaw.cpuTotal = total
			if prev != nil && prev.valid && total > prev.cpuTotal && busy >= prev.cpuBusy {
				sample.CPUPct = clampPct(float64(busy-prev.cpuBusy) * 100.0 / float64(total-prev.cpuTotal))
			}
		}
	}

	var mem memoryStatusEx
	mem.dwLength = uint32(unsafe.Sizeof(mem))
	if r1, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&mem))); r1 != 0 && mem.ullTotalPhys > 0 {
		total := int64(mem.ullTotalPhys)
		avail := int64(mem.ullAvailPhys)
		used := max(int64(0), total-max(int64(0), min(avail, total)))
		sample.MemUsedBytes = used
		sample.MemTotalBytes = total
		sample.MemPct = clampPct(float64(used) * 100.0 / float64(total))
	}

	if rootPtr, err := syscall.UTF16PtrFromString(`C:\`); err == nil {
		var freeAvail, totalBytes, totalFree uint64
		if r1, _, _ := procGetDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(rootPtr)),
			uintptr(unsafe.Pointer(&freeAvail)),
			uintptr(unsafe.Pointer(&totalBytes)),
			uintptr(unsafe.Pointer(&totalFree)),
		); r1 != 0 && totalBytes > 0 {
			total := int64(totalBytes)
			avail := int64(freeAvail)
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
