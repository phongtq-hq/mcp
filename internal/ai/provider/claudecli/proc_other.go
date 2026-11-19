//go:build !unix

package claudecli

import "os/exec"

func isolateProcessGroup(*exec.Cmd) {}
