//go:build windows

package conductor

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows 进程托管：所有 worker 加入同一个设置了 KILL_ON_JOB_CLOSE 的 Job Object。
// Kanade 进程无论正常退出还是崩溃，系统关闭最后一个 Job 句柄时都会结束其中全部进程，
// 避免占用显存的 Python 进程残留。直接调用 kernel32，不引入 golang.org/x/sys。

const (
	createNoWindow               = 0x08000000
	jobObjectExtendedLimitInfo   = 9
	jobObjectLimitKillOnJobClose = 0x2000
	processSetQuota              = 0x0100
	processTerminate             = 0x0001
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

// jobObjectBasicLimitInformation 与 jobObjectExtendedLimitInformation 对应 Win32 的
// JOBOBJECT_BASIC_LIMIT_INFORMATION / JOBOBJECT_EXTENDED_LIMIT_INFORMATION，字段顺序与对齐须保持一致。
type jobObjectBasicLimitInformation struct {
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

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	jobOnce   sync.Once
	jobHandle syscall.Handle
	jobErr    error
)

// workerJob 返回进程级共享的 Job Object，首次调用时创建；句柄有意不关闭，随进程退出释放。
func workerJob() (syscall.Handle, error) {
	jobOnce.Do(func() {
		h, _, err := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			jobErr = fmt.Errorf("CreateJobObject: %w", err)
			return
		}
		var info jobObjectExtendedLimitInformation
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		r, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInfo,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		if r == 0 {
			syscall.CloseHandle(syscall.Handle(h))
			jobErr = fmt.Errorf("SetInformationJobObject: %w", err)
			return
		}
		jobHandle = syscall.Handle(h)
	})
	return jobHandle, jobErr
}

// hideWindow 阻止 GUI 进程启动的控制台程序（uv、python）弹出黑色控制台窗口。
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

func prepareWorker(cmd *exec.Cmd) { hideWindow(cmd) }

// adoptWorker 把已启动的 worker 加入 Job Object。
//
// 限制：进程在 Start 与加入之间若已派生子进程，那些子进程不在 Job 中；Python worker 启动初期不会派生，
// 可接受。彻底消除需以 CREATE_SUSPENDED 启动，os/exec 不支持。
func adoptWorker(cmd *exec.Cmd) error {
	job, err := workerJob()
	if err != nil {
		return err
	}
	ph, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(ph)
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(ph)); r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

func killWorker(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
