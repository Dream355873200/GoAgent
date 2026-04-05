//go:build !windows

package tui

import (
	"os"
	"syscall"
)

// interruptSignals 返回 Unix 平台的中断信号。
func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
