package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:     "weclaw",
	Short:   "WeClaw · 定制版 — WeChat AI agent bridge",
	Long:    "WeClaw · 定制版：把微信消息接入 AI Agent 的桥接服务，含命令行控制、智能搜索开关与服务器 Shell 能力。",
	Version: Version,
	RunE:    runStart, // default command is start
}

// Execute runs the root command. A no-argument launch on Windows (double-click)
// gets the console welcome window and runs in the foreground instead of the
// background daemon, so the window stays open and shows the logs.
func Execute() {
	if isDoubleClickLaunch() {
		runConsoleMode()
		return
	}
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
