package tui

import "os"

// interruptSignals 返回 Windows 平台的中断信号。
func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
