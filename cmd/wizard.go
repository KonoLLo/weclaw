package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/fastclaw-ai/weclaw/config"
)

const (
	geminiChatEndpoint = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	zhipuChatEndpoint  = "https://open.bigmodel.cn/api/paas/v4/chat/completions"
)

// runConsoleMode is the double-click entrypoint on Windows: print the key
// guide, walk a first-time user through pasting keys, then run in foreground
// so the console window itself is the process (closing it stops the bot).
func runConsoleMode() {
	setConsoleUTF8()
	printWelcome()

	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("读取配置失败：%v\n", err)
		pauseBeforeExit()
		return
	}
	if !hasChatAgent(cfg) {
		reader := bufio.NewReader(os.Stdin)
		if err := runKeyWizard(reader, cfg); err != nil {
			fmt.Printf("\n保存配置失败：%v\n", err)
			pauseBeforeExit()
			return
		}
	} else {
		printConfiguredAgents(cfg)
	}

	foregroundFlag = true
	if err := runStart(rootCmd, nil); err != nil {
		fmt.Printf("\n启动失败：%v\n", err)
		pauseBeforeExit()
	}
}

func printWelcome() {
	path, _ := config.ConfigPath()
	fmt.Println("============================================================")
	fmt.Printf("  WeClaw · 定制版  %s\n", rootCmd.Version)
	fmt.Println("============================================================")
	fmt.Printf("配置文件：%s\n", path)
	fmt.Println()
	fmt.Println("【申请免费 API Key（在浏览器打开下面的网址）】")
	fmt.Println("  聊天 Gemini      https://aistudio.google.com/apikey   Flash 系列有免费额度")
	fmt.Println("  聊天/生图/生视频  智谱 https://open.bigmodel.cn         GLM、CogView、CogVideoX 免费")
	fmt.Println("  联网搜索 Tavily   https://tavily.com                  可选，默认 DuckDuckGo 免 Key")
	fmt.Println("  联网搜索 Brave    https://api.search.brave.com         可选")
	fmt.Println()
	fmt.Println("申请后把 Key 直接粘贴到本窗口的提示处即可，会写进上面的 config.json。")
	fmt.Println("============================================================")
	fmt.Println()
}

func hasChatAgent(cfg *config.Config) bool {
	for _, a := range cfg.Agents {
		switch a.Type {
		case "http":
			if strings.TrimSpace(a.APIKey) != "" {
				return true
			}
		case "acp", "cli":
			if strings.TrimSpace(a.Command) != "" {
				return true
			}
		}
	}
	return false
}

func printConfiguredAgents(cfg *config.Config) {
	var parts []string
	for name, a := range cfg.Agents {
		switch a.Type {
		case "http":
			if a.APIKey != "" {
				parts = append(parts, name+"（Key 已保存）")
			} else {
				parts = append(parts, name)
			}
		default:
			parts = append(parts, name)
		}
	}
	if len(parts) > 0 {
		fmt.Printf("已配置 Agent：%s\n", strings.Join(parts, "、"))
	}
	if cfg.DefaultAgent != "" {
		fmt.Printf("默认 Agent：%s\n", cfg.DefaultAgent)
	}
	if cfg.ServerShell.Enabled {
		fmt.Println("服务器命令（server_shell）：已开启")
	} else {
		fmt.Println("服务器命令（server_shell）：未开启（在 config.json 里加 \"server_shell\": {\"enabled\": true} 可让机器人执行服务器命令）")
	}
	fmt.Println("如需更换 Key：用记事本打开上面的 config.json 修改 api_key，保存后重新双击本程序。")
	fmt.Println()
}

// runKeyWizard asks for the two free-tier keys and writes them as HTTP agents.
// Empty answers skip that provider; the bot still starts with whatever exists.
func runKeyWizard(reader *bufio.Reader, cfg *config.Config) error {
	fmt.Println("还没有可用的聊天 Agent，粘贴下面的 Key 就能用（直接回车跳过该项）。")
	geminiKey := promptLine(reader, "Gemini API Key：")
	zhipuKey := promptLine(reader, "智谱 API Key（聊天/生图/生视频，可选）：")

	if geminiKey == "" && zhipuKey == "" {
		fmt.Println("\n没有填写 Key，本次会用现有配置启动；随时重新双击本程序再来填写。")
		return nil
	}

	if geminiKey != "" {
		cfg.Agents["gemini"] = config.AgentConfig{
			Type:           "http",
			Endpoint:       geminiChatEndpoint,
			APIKey:         geminiKey,
			Model:          "gemini-3.5-flash-lite",
			Models:         []string{"gemini-4-hydrogen", "gemini-3.8-flash", "gemini-3.6-flash", "gemini-3.5-flash-lite", "gemini-3.1-flash-lite"},
			MaxHistory:     20,
			TimeoutSeconds: 300,
		}
	}
	if zhipuKey != "" {
		cfg.Agents["glm"] = config.AgentConfig{
			Type:           "http",
			Endpoint:       zhipuChatEndpoint,
			APIKey:         zhipuKey,
			Model:          "glm-4.5-flash",
			Models:         []string{"glm-4.5-flash", "glm-4-flash-250414", "glm-4.1v-thinking-flash"},
			MaxHistory:     20,
			TimeoutSeconds: 300,
		}
	}

	if !agentConfigured(cfg, cfg.DefaultAgent) {
		switch {
		case geminiKey != "":
			cfg.DefaultAgent = "gemini"
		case zhipuKey != "":
			cfg.DefaultAgent = "glm"
		}
	}

	if err := config.Save(cfg); err != nil {
		return err
	}
	path, _ := config.ConfigPath()
	fmt.Printf("\n已写入 %s\n", path)
	if geminiKey != "" {
		fmt.Println("  gemini：已保存（/gm 切换）")
	}
	if zhipuKey != "" {
		fmt.Println("  glm：已保存（/glm 切换，生图生视频同用这把 Key）")
	}
	fmt.Println()
	return nil
}

func agentConfigured(cfg *config.Config, name string) bool {
	if name == "" {
		return false
	}
	_, ok := cfg.Agents[name]
	return ok
}

func promptLine(reader *bufio.Reader, label string) string {
	fmt.Print(label)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return sanitizeKey(line)
}

func sanitizeKey(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'")
	return strings.TrimSpace(s)
}
