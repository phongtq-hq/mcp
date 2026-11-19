//go:build unix

package claudecli

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup đặt process con vào nhóm riêng và, khi lượt bị huỷ hoặc quá
// thời hạn, giết cả nhóm. Nếu không, process phụ do CLI sinh ra vẫn sống, tiếp tục
// tiêu hạn mức của tài khoản sau khi lượt đã bị huỷ.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
