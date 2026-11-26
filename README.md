# WeClaw 微信 AI 桥 · 定制版

把微信消息接入 AI Agent 的桥接服务。基于 [fastclaw-ai/weclaw](https://github.com/fastclaw-ai/weclaw)（上游基线 `f956075`）定制，当前文档对应 **1.2.6**。

> **这是个人定制 fork，不是上游官方版本。**
>
> - 不要执行上游一键安装脚本：`curl ... fastclaw-ai/weclaw/install.sh | sh`
> - 不要运行 `weclaw update` / `weclaw upgrade`：它会从官方 Release 下载并覆盖当前二进制，本 fork 的改动会全部丢失
> - 本仓库目前没有预编译 Release，请按下面的“从源码构建”安装
> - 仓库自带的 `service/weclaw.service` 是上游旧版（`ExecStart` 缺少 `-f`），systemd 请使用本文给出的单元示例

简体中文是默认文档。上游的通用说明仍可参考，但功能、命令、配置和限制以本文为准。

## 这版改了什么

相对上游基线，本 fork 主要增加/强化了以下能力：

| 方向 | 本版能力 |
| --- | --- |
| 微信排版 | `/help`、Hermes 帮助、技能中心帮助全部中文化；按微信气泡规则逐行排版，不再把整份帮助压成几段 |
| 联网搜索 | `/s` 联网搜索，默认 DuckDuckGo；`/se` 在 DuckDuckGo / Tavily 之间切换，选择重启后保留；回复只给中文摘要，不带不可点击的 URL |
| 模型切换 | `/mod` 查看/切换 HTTP 助手的模型，`/mod-t` 实测候选模型当前可用性；选择持久化；`/gm` 可切到 Gemini |
| 图片生成 | `/图` 或“画一只……”触发，免费 `cogview-3-flash`，支持 7 档尺寸并自动吸附 |
| 视频生成 | `/视频` 或“生成一段……”触发，免费 `cogvideox-flash`，支持 8 档尺寸，异步生成、可 `/stop` |
| 语音 | `/语音`、`/音色`，基于 `edge-tts`，8 个中文音色；有 ffmpeg 时发可点开播放的视频气泡，没有则回传 mp3 |
| 长期记忆 | `/记住`、`/记忆`、`/忘记`，每用户最多 200 条，自动注入对话 |
| 自然语言提醒 | “一分钟后提醒我喝水”“每天下午 5 点提醒我写日报”；到点像聊天一样由 Agent 生成提醒，而不是固定模板 |
| 阅读与文件 | `/读` 或直接发链接可读网页；直接发 txt/Markdown/代码/Word/Excel 文件可分析；**不支持 PDF** |
| 导出 | `/word`、`/excel`、`/导出 word|excel`，零第三方依赖生成 DOCX/XLSX；**不支持 PDF** |
| 代码工作区 | `/ws`、`/code`、`/files`、`/cat`、`/write`、`/run`、`/diff`、`/pack`、`/send`，按用户隔离 |
| 工作区导入 | 先发 zip 再 `/ws import`，或 `/ws import <公开 GitHub 链接>`；校验路径穿越、软链、特殊文件，不覆盖已有工作区 |
| 中止任务 | `/stop` 可中止普通回复、合并消息、`/code`、`/run`、生图、视频生成；ACP 会尽量发送 `session/cancel` |
| 消息理解 | 引用消息（含图片/文件/部分引用）、连续消息合并、重启后的待处理消息恢复 |
| 稳定性 | 媒体消息 `message_id` 数字/字符串兼容，修复 `/pack`、`/send` 已送达却误报失败；渠道身份、媒体下载超时等运维项可配 |

## 快速开始

### 1. 环境要求

- Linux 服务器（也支持 macOS / Windows，本文以 Linux 为例）
- Go **1.25+**
- 一个可用的微信 iLink 机器人账号
- 至少一个 AI Agent：ACP / CLI 可执行文件，或一个 OpenAI 兼容的 HTTP 接口
- 可选：`git`（导入 GitHub 仓库、工作区 diff）、`edge-tts`（语音）、`ffmpeg`（语音视频气泡）

### 2. 从源码构建

```bash
git clone https://github.com/KonoLLo/weclaw.git
cd weclaw

go build -trimpath \
  -ldflags "-s -w -X github.com/fastclaw-ai/weclaw/cmd.Version=1.2.6" \
  -o weclaw .

sudo install -m 0755 weclaw /usr/local/bin/weclaw
weclaw version
```

> 如果只执行 `go build -o weclaw .`，版本号会显示 `dev`，功能不受影响。

### 3. 登录微信

```bash
weclaw login
```

按提示用微信扫码。首次运行 `weclaw start` 时如果还没有账号，也会先进入扫码登录。

### 4. 前台试跑

```bash
weclaw start -f
```

确认日志里出现账号、Agent、搜索源、工作区初始化信息，然后在微信里给机器人发一条消息。

常用进程命令：

```bash
weclaw status    # 查看后台进程状态
weclaw stop      # 停止后台进程
weclaw start     # 后台启动
weclaw start -f  # 前台运行，调试或交给 systemd 时使用
```

日志默认写入 `~/.weclaw/weclaw.log`。

### 5. systemd（开机自启）

**注意：必须带 `-f`。** 不带 `-f` 时进程会自己再 daemon 化，systemd 会误判服务退出。

```ini
[Unit]
Description=WeClaw - WeChat AI Agent Bridge
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=/root
Environment=HOME=/root
ExecStart=/usr/local/bin/weclaw start -f
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

把 `User`、`WorkingDirectory`、`Environment=HOME` 改成实际运行用户后：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now weclaw
sudo systemctl status weclaw
journalctl -u weclaw -f
```

### 6. Docker（可选）

```bash
docker build -t weclaw-fork .

# 首次登录需要交互式终端
docker run -it --rm -v ~/.weclaw:/root/.weclaw weclaw-fork login

# 前台运行
docker run --rm -v ~/.weclaw:/root/.weclaw weclaw-fork start -f
```

默认镜像只包含 WeClaw 本体。使用 ACP / CLI Agent 时，需要把对应的 Agent 二进制和运行环境一起放进镜像或挂载进去；HTTP 模式无需额外二进制。

## 命令速查

### 基本

| 命令 | 说明 |
| --- | --- |
| `你好` | 发给默认 Agent |
| `/claude`、`/codex`、`/hermes` | 切换默认 Agent |
| `/cc 解释这段代码` | 通过别名发给指定 Agent |
| `@a @b 消息` | 同时发给多个 Agent |
| `/new`、`/clear` | 开始新会话 |
| `/cwd /路径` | 切换 Agent 工作目录 |
| `/stop` | 中止当前用户正在运行的长任务 |
| `/info` | 查看当前 Agent 信息 |
| `/help` | 查看完整中文帮助 |

内置别名：

| 别名 | Agent | 别名 | Agent |
| --- | --- | --- | --- |
| `/cc` | Claude | `/cx` | Codex |
| `/hm` | Hermes | `/cs` | Cursor |
| `/km` | Kimi | `/gm` | Gemini |
| `/oc` | OpenClaw | `/ocd` | OpenCode |
| `/pi` | Pi | `/cp` | Copilot |
| `/dr` | Droid | `/if` | iFlow |
| `/kr` | Kiro | `/qw` | Qwen |

也可以在 `agents.<名字>.aliases` 里自定义触发词。

### 代码工作区

| 命令 | 说明 |
| --- | --- |
| `/ws [list]` | 查看工作区和当前工作区 |
| `/ws new <名称>` | 新建并切换工作区 |
| `/ws use <名称>` | 切换工作区 |
| `/ws import <公开 GitHub 链接>` | 导入公开仓库 |
| 先发 zip，再发 `/ws import` | 导入压缩包 |
| `/code <任务>` | 在当前工作区写代码/改代码 |
| `/files [目录]` | 列出工作区文件 |
| `/cat <路径>` | 查看文件内容 |
| `/write <路径> <内容>` | 写入文件 |
| `/run <命令>` | 执行命令，默认关闭 |
| `/diff` | 查看工作区改动 |
| `/pack` | 打包工作区并回传 zip |
| `/send <路径>` | 回传工作区里的单个文件 |

### 定时与提醒

| 命令 | 说明 |
| --- | --- |
| `/cron list` | 查看任务 |
| `/cron add "名称" every:24h "任务"` | 固定间隔 |
| `/cron add "名称" "*/30 * * * *" "任务"` | 标准 5 字段 cron |
| `/cron add "名称" at:2026-04-01T09:00:00 "任务"` | 一次性任务 |
| `/cron enable <id>` / `disable <id>` | 启用/停用 |
| `/cron delete <id>` | 删除 |
| `一分钟后提醒我喝水` | 一次性自然语言提醒 |
| `每天下午5点提醒我写日报` | 重复自然语言提醒 |

### 记忆、阅读与文件

| 命令 | 说明 |
| --- | --- |
| `/记住 <内容>` | 保存长期记忆 |
| `/记忆 [页码]` | 查看已保存记忆（每页 10 条，最新优先） |
| `/忘记 <编号\|关键词>` | 删除一条记忆 |
| `/读 <链接>` | 读取网页并总结 |
| 直接发链接 | 自动识别为网页阅读 |
| 直接发文件 | 分析文本/代码/Word/Excel |
| `/word [内容]` | 导出 Word |
| `/excel [内容]` | 导出 Excel |
| `/导出 word\|excel [内容]` | 中文写法，效果同上 |

### 图片、视频、语音

| 命令 | 说明 |
| --- | --- |
| `/图 <描述>` | 生成图片，免费 |
| `画一只戴帽子的橘猫` | 自然语言触发生图 |
| `/视频 <描述>` | 生成视频，免费 |
| `/语音 <文字>` | 文字转语音 |
| `/音色` | 查看可用音色 |
| `/语音 云希 你好呀` | 指定音色 |

### 搜索与模型

| 命令 | 说明 |
| --- | --- |
| `/s <关键词>` | 联网搜索，默认 DuckDuckGo |
| `/se` | 查看搜索源及可用性 |
| `/se <编号\|名称>` | 切换搜索源 |
| `/gm` | 切到 Gemini（配置见下文） |
| `/mod` | 查看 HTTP 助手的候选模型 |
| `/mod <编号\|名称>` | 切换模型 |
| `/mod-t` | 实测当前可用模型，约需数十秒 |

## 功能说明

### AI Agent 接入

支持三种模式：

| 模式 | 工作方式 | 适用 |
| --- | --- | --- |
| ACP | 长驻子进程，stdio JSON-RPC，复用进程和会话，速度最快 | Claude、Codex、Hermes、Kimi、Gemini、Cursor、OpenCode、Qwen 等 |
| CLI | 每条消息启动新进程，可配合 `--resume` 恢复会话 | Claude CLI、Codex CLI 等 |
| HTTP | OpenAI 兼容的 Chat Completions 接口 | OpenClaw HTTP 回退、MiniMax、自建网关等 |

同时存在 ACP 和 CLI 时优先 ACP。Agent 的 `command`、`args`、`cwd`、`env`、`model`、`system_prompt` 都在 `~/.weclaw/config.json` 里配置。

### 联网搜索与模型切换

- **默认 DuckDuckGo**，不需要 API Key。
- Tavily 需要配置 `search.api_key` 或环境变量 `TAVILY_API_KEY`；没配时 `/se` 仍会列出，但会标注“不可用：没配 api_key”。
- `/se` 的选择写入 `~/.weclaw/search.json`，重启后保留；`search.provider` 只是首次启动的默认值。
- `/s` 的结果是给微信阅读的中文摘要，**故意不拼 URL**，因为微信机器人文本里裸链接不可点击。
- `/mod` 只对 HTTP 助手有效。ACP / CLI 助手请在它们自己的配置里换模型。
- `/mod-t` 会逐个探测候选模型，单个请求可能较慢；免费档可用性会分钟级波动。
- 模型选择写入 `~/.weclaw/models.json`，不修改 `config.json`。

### Gemini 免费模型

Gemini 支持 OpenAI 兼容的 Chat Completions 接口，可以直接作为 HTTP Agent 接入。以下五个型号在 Google AI Studio 免费层可用：

| 型号 | API 名称 | 说明 |
| --- | --- | --- |
| Gemini 4 Hydrogen | `gemini-4-hydrogen` | 最新主力模型，免费层高峰可能 503 |
| Gemini 3.8 Flash | `gemini-3.8-flash` | 长文档与多步编码，免费层高峰可能 503 |
| Gemini 3.6 Flash | `gemini-3.6-flash` | 上一代主力 Flash，免费层高峰可能 503 |
| Gemini 3.5 Flash-Lite | `gemini-3.5-flash-lite` | 默认推荐，额度和稳定性相对好 |
| Gemini 3.1 Flash-Lite | `gemini-3.1-flash-lite` | 最轻量，延迟低 |

在 `~/.weclaw/config.json` 的 `agents` 下增加：

```json
"gemini": {
  "type": "http",
  "endpoint": "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
  "api_key": "REPLACE_WITH_GEMINI_API_KEY",
  "model": "gemini-3.5-flash-lite",
  "models": [
    "gemini-4-hydrogen",
    "gemini-3.8-flash",
    "gemini-3.6-flash",
    "gemini-3.5-flash-lite",
    "gemini-3.1-flash-lite"
  ],
  "max_history": 20,
  "timeout_seconds": 300
}
```

用法：`/gm` 切到 Gemini，发 `/mod` 查看并选择型号，`/mod 4` 切到稳定的 3.5 Flash-Lite；发 `/glm` 可切回原来的智谱助手。免费层有速率限制，且免费层内容会被 Google 用于改进产品，具体以 AI Studio 当前条款为准。

### 免费生图与生视频

依赖智谱开放平台的免费模型。Key 优先级：

- 图片：`image.api_key` → 默认 HTTP Agent 的 `api_key` → 环境变量 `ZHIPU_API_KEY`
- 视频：`video.api_key` → `image.api_key` → 默认 HTTP Agent 的 `api_key` → 环境变量 `ZHIPU_API_KEY`

图片：

- 模型 `cogview-3-flash`，默认 `1024x1024`
- 支持尺寸：`1024x1024`、`768x1344`、`864x1152`、`1344x768`、`1152x864`、`1440x720`、`720x1440`
- 消息里带 `1440x720`、`1440×720`、`1440*720` 会自动吸附到最近的支持尺寸
- 超过 150 秒未完成会报错

视频：

- 模型 `cogvideox-flash`，默认 `1920x1080`
- 支持尺寸：`720x480`、`1024x1024`、`1280x960`、`960x1280`、`1920x1080`、`1080x1920`、`2048x1080`、`3840x2160`
- 默认 `quality=speed`，不带音频；可用 `video.quality=quality`、`video.with_audio=true`
- 默认超时 5 分钟，实际可能 1-2 分钟；遇到平台内容审核（错误码 `1301`）需要换描述再试

免费额度、限流和模型生命周期由平台决定，不保证永久免费。

### 语音

- 依赖 `edge-tts` 命令（Python），需要服务器能访问微软 TTS 服务
- 有 `ffmpeg` 时，把 mp3 封成微信可直接点开播放的视频气泡；没有 ffmpeg 时回传 mp3 文件
- iLink 协议不支持原生语音条，所以不会出现绿色语音气泡，这是协议层限制
- 单次最多 300 字，超时 60 秒

音色：

| 别名 | 音色 | 特点 |
| --- | --- | --- |
| 晓晓 | `zh-CN-XiaoxiaoNeural` | 女声·温暖（默认） |
| 晓伊 | `zh-CN-XiaoyiNeural` | 女声·活泼 |
| 云希 | `zh-CN-YunxiNeural` | 男声·阳光 |
| 云扬 | `zh-CN-YunyangNeural` | 男声·新闻 |
| 云健 | `zh-CN-YunjianNeural` | 男声·激情 |
| 云夏 | `zh-CN-YunxiaNeural` | 男声·可爱 |
| 小北 | `zh-CN-liaoning-XiaobeiNeural` | 女声·东北话 |
| 小妮 | `zh-CN-shaanxi-XiaoniNeural` | 女声·陕西话 |

`/语音` 不带文字时会显示用法。`/音色` 也能看到别名和完整名称。

### 长期记忆

- `/记住`：每用户最多 200 条，单条最多 300 字
- `/记忆 [页码]`：按编号列出，每页 10 条、最新优先
- `/忘记 <编号>` 或 `/忘记 <关键词>`：删除匹配项
- 每轮对话最多注入 2000 字记忆，避免占满上下文
- 存储：`~/.weclaw/memory.json`

### 自然语言提醒

支持两类：

- 一次性：“一分钟后提醒我喝水”“明天上午 9 点提醒我交报告”
- 重复：“每天下午 5 点提醒我写日报”“每周一上午 10 点提醒我开周会”

到点后不是发固定模板，而是用该用户的会话触发一次 Agent，让 Agent 像聊天一样把提醒说出来；Agent 不可用或返回空时，才回退到单句模板。

限制：

- 单条提醒内容最多 200 字，最长 10 年
- 支持“几分钟后/几小时后/明天/后天/每天/每周几”等常见表达
- 特定日期、每月几号、周末等复杂规则请用 `/cron`
- 一次性提醒触发后自动停用；取消用 `/cron list` 查 id 后 `/cron delete <id>`

### 网页与文件阅读

网页：

- `/读 <链接>` 或直接发链接
- 只支持公网 `http/https`，会拒绝 localhost、私有地址、`.local`、`.internal` 等
- 页面最多读取 6MB，正文最多 12000 字
- 抓取超时 20 秒，总结超时 90 秒
- 登录后才可见、强反爬的页面可能读不到

文件：

- 文本/代码最多读取 256KiB、60000 字
- 支持 `.docx`、`.xlsx` 正文提取
- **不支持 PDF**，也读不了旧版 `.doc`、`.xls`
- 需要分析 PDF 时，请先转成 `.docx`、`.txt` 或 Markdown 再发

### Word / Excel 导出

| 命令 | 说明 |
| --- | --- |
| `/word <内容>` | 生成 DOCX |
| `/excel <内容>` | 生成 XLSX |
| `/导出 word <内容>` | 中文写法 |
| `/导出 excel <内容>` | 中文写法 |
| 先发文件，再发 `/word` / `/excel` | 用刚上传的文件正文导出 |

- Word 支持标题、列表、Markdown 表格、中文字体
- Excel 支持 `|` 分列、Tab 分列、Markdown 表格；首行加粗，单 sheet
- 单次最多 60000 字；Excel 最多 5000 行、60 列
- 生成后直接作为微信文件发送
- **不支持导出 PDF**

### 代码工作区

每个用户在 `~/.weclaw/workspaces/<用户>-<hash>/` 下有独立目录，默认工作区名 `main`。首次 `/code` 或文件命令会自动创建。

- `/ws new`、`/ws use`、`/ws list` 管理；没有删除命令
- `/code` 使用 `workspace.agent` 指定的 ACP/CLI Agent；未配置时优先 `qwen`，再回退默认 Agent；都没有时 `/code` 关闭，但文件命令仍可用
- 每个工作区独立会话，`/cwd` 不会破坏工作区绑定
- 所有路径必须是工作区内的相对路径，绝对路径、`..`、软链逃逸都会被拒绝
- 输出默认 64KB、打包默认 50MB / 5000 文件、命令默认 10 分钟
- `/run` 默认关闭；开启后以机器人进程权限执行，**不是安全沙箱**

工作区导入：

- 先发 `.zip`，再发 `/ws import`
- 或 `/ws import https://github.com/owner/repo`、`/ws import github.com/owner/repo`
- 只支持 GitHub 公开链接；私有仓库可设置 `WECLAW_GITHUB_TOKEN`，token 通过 Git 环境变量传入，不进入命令行参数
- `git clone --depth 1 --single-branch`，5 分钟超时
- zip 拒绝绝对路径、`..`、Windows volume、符号链接、特殊文件、越界路径
- 最多 5000 个文件；解压后总大小受 `workspace.max_archive_mb` 限制（默认 50MB）
- 导入到临时目录，校验通过后原子改名；不会覆盖已有工作区，重名自动加后缀

### `/stop` 中止任务

`/stop` 不排在普通消息队列后面，而是走 monitor 的 urgent 旁路，能及时打断当前任务。

覆盖范围：

- 普通 AI 回复（含合并后的连续消息）
- 图片/视频分析
- `/code`、`/run`
- 生图、生视频

行为：

- 取消后不发送半截回答，也不报“空回复”
- 回复“已停止：任务名”
- 没有任务时回复“当前没有正在运行的任务。”
- ACP Agent 会尽量发送 `session/cancel`；如果 Agent 不响应，桥会放弃等待，不阻塞下一条消息

### 引用、连续消息与断点恢复

- **引用消息**：支持引用文字、图片、视频、文件；服务端只给 `svr_id` 的旧消息会回查本地缓存；部分引用按 MD5 校验，失败时退回整条引用
- **本地引用缓存**：`~/.weclaw/accounts/<bot>.quotes.json`，保留 30 天、每账号最多 1 万条、单条正文最多 1.6 万字
- **连续消息合并**：同一发送者的纯文本和图片在 800ms 窗口内合并，最多 8 条；斜杠命令、引用、文件、视频、未转写语音仍逐条处理
- **断点恢复**：游标、待处理消息、正在处理的批次、去重表写入 `~/.weclaw/accounts/<bot>.sync.json`
- 恢复语义是 **at-most-once**：重启后重放 pending；重启瞬间正在处理的批次不回放，避免重复副作用
- 去重表保留 24 小时 / 最多 1 万条，pending 上限 1000

### 主动推送与 HTTP API

`weclaw start` 默认监听 `127.0.0.1:18011`，**没有鉴权**，不要直接暴露到公网。

```bash
# 文本
curl -X POST http://127.0.0.1:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to":"wxid_xxx@im.wechat","text":"你好，来自 weclaw"}'

# 图片/视频/文件 URL
curl -X POST http://127.0.0.1:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to":"wxid_xxx@im.wechat","media_url":"https://example.com/photo.png"}'
```

健康检查：`GET /health`。

命令行也可以主动发：

```bash
weclaw send --to "wxid_xxx@im.wechat" --text "你好"
weclaw send --to "wxid_xxx@im.wechat" --media "https://example.com/photo.png"
```

修改监听地址：

```bash
weclaw start -f --api-addr 127.0.0.1:19000
# 或环境变量 WECLAW_API_ADDR
```

### 定时任务与心跳

`/cron` 任务持久化到 `~/.weclaw/cron/jobs.json`，重启不丢。调度格式：

| 格式 | 示例 |
| --- | --- |
| 固定间隔 | `every:5m`、`every:24h` |
| 标准 cron | `*/30 * * * *`、`0 9 * * 1-5` |
| 一次性 | `at:2026-04-01T09:00:00` |

心跳（Heartbeat）是上游已有能力：在 `~/.weclaw/HEARTBEAT.md` 写检查清单，按间隔让默认 Agent 巡检；回复 `HEARTBEAT_OK` 会被吞掉，有内容才推送。配置在 `heartbeat` 段：

```json
{
  "heartbeat": {
    "enabled": true,
    "interval": "30m",
    "active_hours": "09:00-18:00",
    "timezone": "Asia/Shanghai",
    "target_user": "wxid_xxx@im.wechat"
  }
}
```

## 配置参考

配置文件：`~/.weclaw/config.json`

以下示例只使用占位符，复制后请替换成自己的值，**不要把真实 Key 提交到仓库**。

```json
{
  "default_agent": "openclaw",
  "api_addr": "127.0.0.1:18011",
  "allowed_users": [
    "wxid_xxx@im.wechat"
  ],
  "agents": {
    "openclaw": {
      "type": "http",
      "endpoint": "https://api.example.com/v1/chat/completions",
      "api_key": "sk-REPLACE_ME",
      "model": "your-model",
      "models": [
        "your-model",
        "your-model-2"
      ],
      "timeout_seconds": 300
    },
    "qwen": {
      "type": "acp",
      "command": "/path/to/qwen-acp",
      "args": []
    }
  },
  "search": {
    "provider": "duckduckgo",
    "api_key": "",
    "max_results": 5
  },
  "image": {
    "model": "cogview-3-flash",
    "size": "1024x1024",
    "api_key": "REPLACE_ME"
  },
  "video": {
    "model": "cogvideox-flash",
    "size": "1920x1080",
    "quality": "speed",
    "with_audio": false,
    "api_key": "REPLACE_ME"
  },
  "workspace": {
    "disabled": false,
    "root": "/root/.weclaw/workspaces",
    "agent": "qwen",
    "allow_shell": false,
    "timeout_seconds": 600,
    "max_output_kb": 64,
    "max_archive_mb": 50
  },
  "inbound": {
    "debounce_ms": 800,
    "max_batch": 8
  },
  "channel": {
    "bot_agent": "WeClaw/1.2.6",
    "route_tag": 0,
    "error_notices": false,
    "media_download": {
      "timeout_ms": 120000,
      "idle_timeout_ms": 30000
    }
  }
}
```

字段说明：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `default_agent` | 自动检测 | 默认 Agent |
| `api_addr` | `127.0.0.1:18011` | HTTP API 监听地址 |
| `allowed_users` | 空 | 白名单；**空列表表示不限制**，建议填自己的微信 ID |
| `search.provider` | `duckduckgo` | 启动默认搜索源；已有的 `/se` 选择优先 |
| `search.api_key` | 空 | Tavily Key，也可用 `TAVILY_API_KEY` |
| `image.*` | CogView-3-Flash | 免费生图 |
| `video.*` | CogVideoX-Flash | 免费生视频 |
| `workspace.disabled` | `false` | 关闭后工作区命令全部不可用 |
| `workspace.root` | `~/.weclaw/workspaces` | 工作区根目录 |
| `workspace.agent` | 优先 `qwen` | 指定 `/code` 使用的 ACP/CLI Agent |
| `workspace.allow_shell` | `false` | 是否启用 `/run` |
| `workspace.timeout_seconds` | `600` | `/code`、`/run` 默认超时 |
| `workspace.max_output_kb` | `64` | 命令输出上限 |
| `workspace.max_archive_mb` | `50` | `/pack` 与导入的默认大小上限 |
| `inbound.debounce_ms` | `800` | 连续消息合并窗口 |
| `inbound.max_batch` | `8` | 单批最多合并条数 |
| `channel.bot_agent` | `WeClaw/2.4.4` | 渠道身份；ASCII，最长 256 字节 |
| `channel.error_notices` | `false` | 是否在微信里提示媒体下载失败等错误 |

环境变量：

| 变量 | 用途 |
| --- | --- |
| `WECLAW_DEFAULT_AGENT` | 覆盖默认 Agent |
| `WECLAW_API_ADDR` | 覆盖 API 监听地址 |
| `WECLAW_SAVE_DIR` | 覆盖媒体保存目录 |
| `WECLAW_GITHUB_TOKEN` | 工作区导入私有 GitHub 仓库 |
| `TAVILY_API_KEY` | Tavily 搜索 Key |
| `ZHIPU_API_KEY` | 生图/生视频 Key |

## 安全边界

这不是一个多租户安全系统，请按“只给自己的机器人用”来部署：

1. **务必配置 `allowed_users`**。空列表不拒绝任何人，只要能给机器人发消息就能驱动 Agent。
2. **HTTP API 没有鉴权**。默认只监听 `127.0.0.1`；改成 `0.0.0.0` 前请自己加反向代理和鉴权。
3. **`/run` 不是沙箱**。它和机器人进程同权限执行；只在你信任工作区内容时开启。
4. **`--dangerously-skip-permissions` 等参数会跳过 Agent 的安全确认**。微信场景下确实需要非交互，但请理解风险。
5. **导入的仓库不可信**。虽然会拒绝路径穿越、软链和特殊文件，但代码本身没有做安全审计；不要对不可信仓库开启 `/run`。
6. **配置文件含 API Key**。`~/.weclaw/config.json` 应保持 `0600` 权限，不要提交到 Git，也不要发到聊天里。
7. **工作区按用户分目录，但不是隔离沙箱**。同一进程、同一系统用户下运行，恶意代码仍可能访问进程可访问的资源。

## 数据目录

| 路径 | 内容 |
| --- | --- |
| `~/.weclaw/config.json` | 主配置 |
| `~/.weclaw/weclaw.log` | 日志 |
| `~/.weclaw/accounts/` | 账号、游标、引用缓存、待处理消息 |
| `~/.weclaw/workspaces/` | 代码工作区 |
| `~/.weclaw/cron/jobs.json` | 定时任务 |
| `~/.weclaw/memory.json` | 长期记忆 |
| `~/.weclaw/search.json` | 当前搜索源 |
| `~/.weclaw/models.json` | 当前 HTTP 模型 |
| `~/.weclaw/sessions.json` | CLI Agent 会话 |
| `~/.weclaw/HEARTBEAT.md` | 心跳检查清单（可选） |

备份时至少备份 `config.json`、`accounts/`、`cron/`、`memory.json`、`workspaces/`。

## 已知限制

- **不支持 PDF**：既不能读 PDF，也不能导出 PDF。
- **不支持旧版 Office**：`.doc`、`.xls` 读不了，请另存为 `.docx`、`.xlsx`。
- **iLink 不支持原生语音条**：语音以视频气泡或 mp3 文件回传。
- **`/mod` 只对 HTTP 助手有效**：ACP / CLI 的模型由它们自己管理。
- **没有多账号会话隔离**：可以登录多个机器人账号，但会话、记忆、工作区按微信用户区分，不按机器人账号再分开；同一用户跨机器人账号会共用状态。
- **没有知识库**：记忆是轻量的按用户条目，不是向量库。
- **免费服务会波动**：搜索、生图、生视频、TTS 都依赖外部服务，可能限流、排队、改模型或停止免费。
- **工作区不是沙箱**：见上面的安全边界。
- **没有预编译包**：本 fork 需要自己构建；`weclaw update` 会切回官方版。

## 更新与跟进上游

不要使用：

```bash
weclaw update      # 会下载官方 Release 覆盖本地二进制
weclaw upgrade     # 同上
```

本 fork 的更新方式：

```bash
git pull
go build -trimpath \
  -ldflags "-s -w -X github.com/fastclaw-ai/weclaw/cmd.Version=1.2.6" \
  -o weclaw .
sudo install -m 0755 weclaw /usr/local/bin/weclaw
sudo systemctl restart weclaw
```

如果要合并上游新代码，建议保留本 fork 的提交历史，单独解决冲突，不要直接把官方二进制覆盖上来。

## 上游与许可

- 上游项目：[fastclaw-ai/weclaw](https://github.com/fastclaw-ai/weclaw)
- 本 fork：[KonoLLo/weclaw](https://github.com/KonoLLo/weclaw)
- 许可证：[MIT](LICENSE)

本项目参考 [@tencent-weixin/openclaw-weixin](https://npmx.dev/package/@tencent-weixin/openclaw-weixin) 实现，仅限个人学习与自用。