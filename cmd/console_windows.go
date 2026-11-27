//go:build windows

package cmd

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
)

// setConsoleUTF8 switches the console to code page 65001 so Chinese output,
// the QR code blocks and error text render instead of mojibake.
func setConsoleUTF8() {
	procSetConsoleOutputCP.Call(65001)
	procSetConsoleCP.Call(65001)
}

// isDoubleClickLaunch is true when the exe was started with no arguments,
// which is what Explorer does on double-click.
func isDoubleClickLaunch() bool {
	return len(os.Args) == 1
}

// pauseBeforeExit keeps the console window open after a failure so the user
// can read the message; a double-click window would otherwise vanish.
func pauseBeforeExit() {
	fmt.Print("\n按回车键关闭窗口…")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
