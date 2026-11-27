//go:build !windows

package cmd

// The double-click console experience is Windows-only; other platforms keep
// the normal CLI behaviour.

func setConsoleUTF8() {}

func isDoubleClickLaunch() bool { return false }

func pauseBeforeExit() {}
