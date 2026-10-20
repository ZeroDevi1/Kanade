//go:build !windows

package conductor

import (
	"os/exec"
	"syscall"
)

// hideWindow 在非 Windows 平台无需处理：子进程不会弹出控制台窗口。
func hideWindow(*exec.Cmd) {}

// prepareWorker 让 worker 自成进程组，Kill 时可连同其子进程一起结束。
func prepareWorker(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// adoptWorker 在 Unix 上无需额外处理：父进程退出后 worker 读到 stdin EOF 会自行退出。
func adoptWorker(*exec.Cmd) error { return nil }

// killWorker 结束 worker 所在的整个进程组。
func killWorker(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
