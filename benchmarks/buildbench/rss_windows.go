//go:build windows

package buildbench

import (
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObjectW         = modkernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = modkernel32.NewProc("AssignProcessToJobObject")
	procQueryInformationJobObj   = modkernel32.NewProc("QueryInformationJobObject")
)

// jobObjectExtendedLimitInformation class index, and the process rights
// needed to move a running process into a job. syscall does not export
// PROCESS_SET_QUOTA.
const (
	jobObjectExtendedLimitInformation = 9

	processTerminate        = 0x0001
	processSetQuota         = 0x0100
	processQueryInformation = 0x0400
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// watchPeakRSS puts the started process into a job object and reports the
// job's peak committed memory in KB once it finishes. Measuring the job
// rather than the process is what makes the number meaningful here: "go run"
// does its real work in child compiler processes, which inherit the job.
//
// The process is assigned immediately after Start, so a grandchild spawned in
// the first instants could in principle escape accounting; in practice the go
// tool has not forked anything that early.
func watchPeakRSS(c *exec.Cmd) func() uint64 {
	if c.Process == nil {
		return func() uint64 { return 0 }
	}
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return func() uint64 { return 0 }
	}
	h, err := syscall.OpenProcess(
		processSetQuota|processTerminate|processQueryInformation,
		false, uint32(c.Process.Pid))
	if err != nil {
		syscall.CloseHandle(syscall.Handle(job))
		return func() uint64 { return 0 }
	}
	ok, _, _ := procAssignProcessToJobObject.Call(job, uintptr(h))
	syscall.CloseHandle(h)
	if ok == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return func() uint64 { return 0 }
	}

	return func() uint64 {
		defer syscall.CloseHandle(syscall.Handle(job))
		var info jobExtendedLimitInformation
		r, _, _ := procQueryInformationJobObj.Call(
			job,
			uintptr(jobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
			0,
		)
		if r == 0 {
			return 0
		}
		return uint64(info.PeakJobMemoryUsed) / 1024
	}
}
