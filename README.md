# WeClaw · 定制版

把微信消息接入 AI Agent 的桥接服务。基于 [fastclaw-ai/weclaw](https://github.com/fastclaw-ai/weclaw) 定制，当前版本 **1.3.3**。

**这是个人定制 fork，不是上游官方版本。**
- 升级只用 `weclaw upgrade`：它已改为从本仓库的最新 Release 下载，不会切回官方版。不要执行上游一键安装脚本，也不要手动下载官方二进制覆盖。
- 预编译 Release 提供 `linux-amd64`、`windows-amd64`；其他平台按下面的源码构建。
- 仓库里的 `service/weclaw.service` 是上游旧版（缺 `-f`），systemd 请用本文示例。

## 这版改了什么

| 方向 | 能力 |
| --- | --- |
| 联网搜索 | 回答前自动判断，需要就多检索词搜索、读网页正文、提炼后作答；`/s` 手动搜索；默认 DuckDuckGo，`/se` 切 Tavily / Brave / 自建 SearXNG / 合并；`/se on\|off` 随时开关智能搜索并持久化 |
| 消息理解 | 引用消息、连续消息合并、重启后待处理消息恢复；视频用 ffmpeg 抽关键帧交给模型 |
| 模型切换 | `/mod` 查看/切换 HTTP 模型，`/mod-t` 实测可用性，持久化；`/gm` 切 Gemini |
| 生图 / 生视频 | `/图`、`/视频`，免费 CogView-3-Flash / CogVideoX-Flash，多档尺寸 |
| 语音 | `/语音`、`/音色`，edge-tts，8 个中文音色 |
| 长期记忆 | 边聊边记称呼、偏好、近况；`/记住`、`/记忆`、`/忘记`，每用户 200 条 |
| 自然语言提醒 | “一分钟后提醒我喝水”“每天下午 5 点提醒我写日报”，到点由 Agent 生成 |
| 阅读与文件 | 发链接读网页；发 txt/Markdown/代码/Word/Excel 可分析；不支持 PDF |
| 导出 | `/word`、`/excel`，零依赖生成 DOCX/XLSX |
| 代码工作区 | `/ws`、`/code`、`/files`、`/cat`、`/write`、`/run`、`/diff`、`/pack`、`/send`，按用户隔离 |
| 服务器命令 | 开启后助手可在本机执行 shell：查看 docker 容器、服务状态、磁盘、日志，也能按指令运维；危险命令拦截，仅限白名单用户 |
| Windows 双击启动 | 双击 exe 直接运行，窗口里给出 Key 申请地址并引导粘贴，首次配置不用手动改 JSON |
| 中止 | `/stop` 可打断回复、`/code`、`/run`、生图、生视频 |

## 快速开始

要求：Linux/macOS/Windows，Go 1.25+（源码构建），一个微信 iLink 机器人账号，至少一个 AI Agent（HTTP 接口或 ACP/CLI）。可选 `git`、`edge-tts`、`ffmpeg`（视频理解、语音气泡依赖 ffmpeg）。

### 用发行版（Linux）

```bash
tar -xzf weclaw-1.3.3-linux-amd64.tar.gz
sudo install -m 0755 weclaw /usr/local/bin/weclaw
weclaw version    # 应显示 1.3.3
weclaw login      # 微信扫码
weclaw start -f   # 前台试跑
```

### 用发行版（Windows）

解压出 `weclaw.exe`，**双击即可运行**：

- 窗口会先列出免费 Key 的申请地址（Gemini / 智谱 / Tavily / Brave）；还没有配置时，按提示把 Key 粘贴进去就会自动写入 `config.json`，不用手动改 JSON。
- 首次运行会显示微信扫码；之后双击直接连上。关闭窗口 = 停止机器人，要长期挂着就最小化。
- 启动失败时窗口会停住等回车，方便看报错。

也可以继续用命令行（在 cmd 或 PowerShell 里）：

```bat
weclaw.exe version
weclaw.exe login
weclaw.exe start -f
```

### 从源码构建

```bash
git clone https://github.com/KonoLLo/weclaw.git
cd weclaw
go build -trimpath -ldflags "-s -w -X github.com/fastclaw-ai/weclaw/cmd.Version=1.3.3" -o weclaw .
sudo install -m 0755 weclaw /usr/local/bin/weclaw
```

> 只写 `go build -o weclaw .` 时版本显示 `dev`，功能不受影响。

### 进程命令与日志

```bash
weclaw status    # 后台状态
weclaw start     # 后台启动
weclaw stop      # 停止
```

日志：`~/.weclaw/weclaw.log`。首次 `start` 若未登录会先进入扫码。

### systemd（开机自启）

必须带 `-f`，否则进程自己 daemon 化，systemd 会误判退出。

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

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now weclaw
journalctl -u weclaw -f
```

### Docker（可选）

```bash
docker build -t weclaw-fork .
docker run -it --rm -v ~/.weclaw:/root/.weclaw weclaw-fork login
docker run --rm -v ~/.weclaw:/root/.weclaw weclaw-fork start -f
```

镜像只含 WeClaw 本体；用 ACP/CLI Agent 需自行把 Agent 二进制加进镜像。HTTP 模式不需要。

## 命令速查

### 基本

| 命令 | 说明 |
| --- | --- |
| 直接发消息 | 发给默认 Agent |
| `/new`、`/clear` | 新会话 |
| `/stop` | 中止当前长任务 |
| `/info` | 当前 Agent 信息 |
| `/help` | 完整中文帮助 |
| `/cwd /路径` | 切换 Agent 工作目录 |
| `/cc 问题` / `@a @b 消息` | 发给指定 Agent / 多 Agent |

Agent 切换：`/claude`、`/codex`、`/hermes` 按名称；别名 `/cc` `/cx` `/hm` `/cs` `/km` `/gm` `/oc` `/ocd` `/pi` `/cp` `/dr` `/if` `/kr` `/qw`。也可在 `agents.<名字>.aliases` 自定义。

### 代码工作区

| 命令 | 说明 |
| --- | --- |
| `/ws [list]` | 查看/当前工作区 |
| `/ws new <名称>`、`/ws use <名称>` | 新建、切换 |
| `/ws import <GitHub 链接>` | 导入公开仓库；或先发 zip 再 `/ws import` |
| `/code <任务>` | 在工作区写/改代码 |
| `/files [目录]`、`/cat <路径>`、`/write <路径> <内容>` | 浏览、查看、写入 |
| `/run <命令>` | 执行命令（默认关闭） |
| `/diff`、`/pack`、`/send <路径>` | 查看改动、打包回传、回传单文件 |

### 定时与提醒

| 命令 | 说明 |
| --- | --- |
| `/cron list` | 查看任务 |
| `/cron add "名称" every:24h "任务"` | 固定间隔 |
| `/cron add "名称" "*/30 * * * *" "任务"` | cron |
| `/cron add "名称" at:2026-04-01T09:00:00 "任务"` | 一次性 |
| `/cron enable/disable/delete <id>` | 启停、删除 |
| `一分钟后提醒我喝水` | 自然语言一次性 |
| `每天下午5点提醒我写日报` | 自然语言重复 |

### 记忆、阅读与文件

| 命令 | 说明 |
| --- | --- |
| `/记住 <内容>`、`/记忆 [页码]`、`/忘记 <编号\|关键词>` | 记忆管理 |
| `/读 <链接>`、直接发链接 | 读网页并总结 |
| 直接发文件 | 分析 txt/Markdown/代码/Word/Excel |
| `/word`、`/excel`、`/导出 word\|excel` | 导出 DOCX/XLSX |

### 图片、视频、语音

| 命令 | 说明 |
| --- | --- |
| `/图 <描述>`、`画一只戴帽子的橘猫` | 免费生图 |
| `/视频 <描述>` | 免费生视频 |
| `/语音 <文字>`、`/音色`、`/语音 云希 你好` | 语音合成 |

### 搜索与模型

| 命令 | 说明 |
| --- | --- |
| `/s <关键词>` | 联网搜索 |
| `/se`、`/se <编号\|名称>` | 查看、切换搜索源 |
| `/se on`、`/se off` | 开启/关闭智能搜索（回复前自动联网），回传当前状态 |
| `/se time day\|week\|month\|year\|off` | 限制搜索结果发布时间（仅 SearXNG） |
| `/mod`、`/mod <编号\|名称>` | 查看、切换 HTTP 模型 |
| `/mod-t` | 实测可用模型（数十秒） |
| `/gm`、`/glm` | 切 Gemini / 切回智谱 |

## 功能要点

### Agent 接入

ACP（长驻进程、最快）优先于 CLI（每条起新进程）；HTTP 为 OpenAI 兼容 Chat Completions 接口。`command`、`args`、`cwd`、`env`、`model`、`system_prompt` 均在 `~/.weclaw/config.json` 配置。

### 联网搜索

- 普通聊天先做一次轻量判断，需要外部事实（新闻、价格、天气、赛事、政策等）才搜索：生成 1-3 个检索词 → 去重合并结果 → 并发读前几页正文 → 提炼成 ≤700 字资料注入作答。闲聊、创作不触发。
- `/se on` / `/se off` 即时开关并回传“智能搜索已开启/已关闭”；状态写回 `config.json` 的 `search.disable_auto`，重启后保持。`/se` 列表里也会显示当前状态。
- 自动联网最多读 6 页、`/s` 最多 4 页，每页约 2400 字；Tavily 直接返回服务端抓好的正文（不受反爬影响），其他源由本机抓取。只抓公网 http/https，内网和 PDF/图片跳过；抓不到就退回搜索片段。
- 自建 SearXNG：WeClaw 侧不计次，适合替代有月度额度的商业搜索 API；实际能否稳定搜到中文结果取决于实例配置的上游引擎和网络。配置见下方“自建 SearXNG”。
- `/se time day|week|month|year|off` 限制发布时间，默认不限，选择随 `~/.weclaw/search.json` 持久化。SearXNG 只有天/周/月/年四档，没有“三天”；且不支持时间过滤的引擎会被整家跳过（例如 `brave`、`bing`）。默认不限是为了让 `合并` 真正合并两路结果。
- `合并` 源：同时查自建 SearXNG 和内置 DuckDuckGo，按链接去重后交替排列；一路失败自动用另一路。开了时间过滤时只查 SearXNG（DuckDuckGo 没法按时间过滤，混进来会带回旧内容）。
- 联网问题通常多等十几秒，最长约 30 秒封顶；闲聊速度不变。
- 结果只给中文摘要（`/s` ≈400 字、自动 ≈700 字），不拼 URL（微信机器人文本里链接不可点）。
- 搜索源选择持久化在 `~/.weclaw/search.json`；`search.provider` 只是首次默认。智能搜索开关在 `config.json` 的 `search.disable_auto`。

### 自建 SearXNG

```json
"search": {
  "provider": "duckduckgo",
  "searxng_url": "https://search.example.com",
  "searxng_auth": ""
}
```

- 实例必须开启 JSON：`settings.yml` 的 `search.formats` 包含 `json`，改完重启。
- `searxng_url` 填反代后的根地址；挂在子路径（如 `https://example.com/searxng/`）就连子路径一起填。WeClaw 请求的是 `<地址>/search?q=…&format=json`。
- 反代要保留 `/search` 路径和查询参数，不要缓存搜索结果。
- 反代有口令时填 `searxng_auth`：`Bearer xxx` 或 `Basic base64(用户名:密码)`；只填 token 会自动按 Bearer 发送。
- 建议在 SearXNG 里启用 `google cse`、`360search`、`brave`、`bing`、`duckduckgo`；机房 IP 上 `baidu`、`sogou`、`quark` 通常会被验证码拦截，不是配置问题。
- 配好后 `/se` 会多 `SearXNG` 和 `合并`（SearXNG + DuckDuckGo）两项，选择会持久化。
- 时间范围（`/se time`）由 SearXNG 的 `time_range` 参数实现，只对支持该参数的引擎生效，不支持的引擎会被跳过（例如 `bing`）；`google cse`、`360search`、`duckduckgo` 支持。

### Gemini 免费模型

`~/.weclaw/config.json` 增加：

```json
"gemini": {
  "type": "http",
  "endpoint": "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
  "api_key": "REPLACE_WITH_GEMINI_API_KEY",
  "model": "gemini-3.5-flash-lite",
  "models": ["gemini-4-hydrogen", "gemini-3.8-flash", "gemini-3.6-flash", "gemini-3.5-flash-lite", "gemini-3.1-flash-lite"],
  "max_history": 20,
  "timeout_seconds": 300
}
```

`/gm` 切过去，`/mod` 选型号。免费层有速率限制、高峰可能 503，内容会被 Google 用于改进产品。

### 生图与生视频

依赖智谱免费模型：图片 `cogview-3-flash`（7 档尺寸，消息里写分辨率会自动吸附，150 秒超时）；视频 `cogvideox-flash`（8 档尺寸，默认 1920x1080、无声，可配 `quality`/`with_audio`，1-2 分钟出片，审核失败错误码 `1301` 需换描述）。

Key 优先级：`image.api_key` → 与接口同域名的 HTTP Agent Key（GLM）→ 默认 Agent Key → `ZHIPU_API_KEY`。**默认 Agent 是 Gemini 时 Google Key 调智谱会报 `1000`**，同域名规则会自动用 GLM 的 Key；一个智谱 Key 都没有时请显式配 `image.api_key`。

### 语音

依赖 `edge-tts`，单次 ≤300 字；有 ffmpeg 时回可点开的视频气泡，否则回 mp3。iLink 不支持绿色语音条。音色：晓晓（默认）、晓伊、云希、云扬、云健、云夏、小北（东北）、小妮（陕西）。

### 长期记忆

自动记忆从你的原话提取稳定信息（称呼、偏好、风格、近况），`/记忆` 里标 `[自动]`；手动用 `/记住`（≤300 字）。每用户 200 条（自动最多 160，满了先淘汰最旧自动条目）。密码、密钥、证件号、住址等不保存；仅 HTTP 助手支持；异步执行不阻塞回复。存储 `~/.weclaw/memory.json`。

### 提醒与定时

到点用该用户会话触发 Agent 生成提醒，Agent 不可用才回退模板。单条 ≤200 字；复杂规则（每月几号、周末）用 `/cron`。任务持久化在 `~/.weclaw/cron/jobs.json`。

### 阅读与导出

网页：只支持公网 http/https，最多 6MB / 正文 12000 字，20 秒抓取。文件：文本 ≤256KiB、支持 `.docx`/`.xlsx`，**不支持 PDF 和旧版 `.doc`/`.xls`**。导出：DOCX 支持标题/列表/Markdown 表格，XLSX 支持 `|` 或 Tab 分列、≤5000 行 60 列，单次 ≤60000 字。

### 代码工作区

每用户 `~/.weclaw/workspaces/<用户>-<hash>/`，默认 `main`。路径只允许工作区内相对路径，拒绝 `..`、绝对路径、软链逃逸；`/run` 默认关闭、开启后同进程权限执行（**不是沙箱**）。zip 导入拒绝路径穿越/软链/特殊文件，不覆盖已有工作区；GitHub 导入仅公开仓库，私有仓库可设 `WECLAW_GITHUB_TOKEN`。

### 服务器命令（server_shell）

开启后，助手可以通过函数调用在**运行机器人的那台机器**上执行 shell 命令，结果回到聊天里。适合“看看现在跑着哪些 docker 容器”“磁盘还剩多少”“weclaw 服务重启一下”“看下最近的日志”。

```json
"server_shell": { "enabled": true, "timeout_seconds": 30, "max_output_kb": 32 }
```

前提：`allowed_users` **必须非空**（只放自己的微信 ID）；空列表时即使 `enabled` 为 true 也不会启用，启动日志会写明原因。只建议在个人服务器上开，不要把接了 shell 的机器人借给别人用。

- 一次只执行一条命令，单条超时 `timeout_seconds`（默认 30 秒）；输出超过 `max_output_kb`（默认 32KB）保留头尾。
- 黑名单拦截整机级破坏：清根目录、`mkfs`、`dd` 写块设备、关机/重启、fork bomb、删用户组。服务重启、看日志、查容器不受影响。
- 执行的是 `sh -c`（Windows 是 `cmd /C`），权限就是 weclaw 进程的权限（systemd 下通常是 root）。**不是沙箱**，也不做逐条确认；不想让它动的服务，别把 key/权限交给它，或者保持关闭。

### 主动推送与 API

默认监听 `127.0.0.1:18011`，**无鉴权**：

```bash
curl -X POST http://127.0.0.1:18011/api/send -H "Content-Type: application/json" \
  -d '{"to":"wxid_xxx@im.wechat","text":"你好"}'
# media_url 字段可发图片/视频/文件 URL；GET /health 健康检查
weclaw send --to "wxid_xxx@im.wechat" --text "你好"
```

改监听地址：`--api-addr` 或 `WECLAW_API_ADDR`。

## 配置参考

`~/.weclaw/config.json`，示例均为占位符：

```json
{
  "default_agent": "openclaw",
  "api_addr": "127.0.0.1:18011",
  "allowed_users": ["wxid_xxx@im.wechat"],
  "agents": {
    "openclaw": {
      "type": "http",
      "endpoint": "https://api.example.com/v1/chat/completions",
      "api_key": "sk-REPLACE_ME",
      "model": "your-model",
      "models": ["your-model", "your-model-2"],
      "timeout_seconds": 300
    }
  },
  "search": { "provider": "duckduckgo", "api_key": "", "brave_api_key": "", "searxng_url": "", "searxng_auth": "" },
  "image": { "model": "cogview-3-flash", "size": "1024x1024", "api_key": "" },
  "video": { "model": "cogvideox-flash", "size": "1920x1080", "quality": "speed", "with_audio": false, "api_key": "" },
  "server_shell": { "enabled": false, "timeout_seconds": 30, "max_output_kb": 32 },
  "workspace": { "root": "/root/.weclaw/workspaces", "agent": "qwen", "allow_shell": false, "timeout_seconds": 600 },
  "inbound": { "debounce_ms": 800, "max_batch": 8 },
  "channel": { "bot_agent": "WeClaw/2.4.4", "error_notices": false }
}
```

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `allowed_users` | 空 | 白名单，**空 = 不限制**，务必填自己的微信 ID |
| `search.provider` | `duckduckgo` | 首次默认；`/se` 选择优先 |
| `search.api_key` | 空 | Tavily Key，也支持 `TAVILY_API_KEY` |
| `search.brave_api_key` | 空 | Brave Key，也支持 `BRAVE_API_KEY` |
| `search.searxng_url` | 空 | 自建 SearXNG 的反代地址，也支持 `SEARXNG_URL` |
| `search.searxng_auth` | 空 | 可选访问口令，也支持 `SEARXNG_AUTH`；`Bearer …` / `Basic …` 或只填 token |
| `search.disable_auto` | `false` | `true` 关闭回复前自动联网 |
| `workspace.allow_shell` | `false` | 是否启用 `/run` |
| `server_shell.enabled` | `false` | 是否允许助手执行服务器命令；需 `allowed_users` 非空 |
| `server_shell.timeout_seconds` / `max_output_kb` | `30` / `32` | 单条命令超时与输出上限 |
| `inbound.debounce_ms` / `max_batch` | `800` / `8` | 连续消息合并窗口与条数 |
| `channel.bot_agent` | `WeClaw/2.4.4` | 渠道身份，ASCII，≤256 字节 |

环境变量：`WECLAW_DEFAULT_AGENT`、`WECLAW_API_ADDR`、`WECLAW_SAVE_DIR`、`WECLAW_GITHUB_TOKEN`、`TAVILY_API_KEY`、`BRAVE_API_KEY`、`SEARXNG_URL`、`SEARXNG_AUTH`、`ZHIPU_API_KEY`。

## API Key 申请

除聊天助手外都可先不配：搜索默认 DuckDuckGo（免 Key），语音 edge-tts（免 Key）。

| 用途 | 配置 | 申请地址 | 免费情况 |
| --- | --- | --- | --- |
| 聊天 Gemini | `agents.<名>.api_key` | <https://aistudio.google.com/apikey> | Flash 系列有免费额度 |
| 聊天/生图/生视频 智谱 | 对应 `api_key` | <https://open.bigmodel.cn> | GLM、CogView-3-Flash、CogVideoX-Flash 免费 |
| 搜索 Tavily | `search.api_key` | <https://tavily.com> | 约 1000 次/月 |
| 搜索 Brave | `search.brave_api_key` | <https://api.search.brave.com> | 约 2000 次/月 |
| 搜索 自建 SearXNG | `search.searxng_url` | 自己部署并反代 | WeClaw 侧不计次，取决于实例与上游引擎 |
| 私有仓库导入 | `WECLAW_GITHUB_TOKEN` | <https://github.com/settings/tokens> | 免费 |

一个智谱 Key 同时用于聊天、生图、生视频；Gemini Key 不能调智谱。Key 只放 `config.json`（建议 `chmod 600`）或环境变量，**不要提交到仓库**。

## 安全边界

只给自己的机器人用，别当多租户系统：

1. 务必配 `allowed_users`；空列表不拦任何人。
2. HTTP API 无鉴权，默认只听 `127.0.0.1`；改 `0.0.0.0` 前先加反代和鉴权。
3. `/run`、`server_shell` 和 Agent 的 `--dangerously-skip-permissions` 都不是沙箱，同进程权限执行。
4. 导入的仓库不可信：会拒绝路径穿越/软链，但代码没做审计，别对陌生仓库开 `/run`。
5. `server_shell` 只给可信的人开；`allowed_users` 为空时不会启用，但也不要依赖空列表“默认安全”。
6. `config.json` 含 Key，保持 `0600`，不进 Git、不发聊天。

## 数据目录

| 路径 | 内容 |
| --- | --- |
| `~/.weclaw/config.json` | 主配置 |
| `~/.weclaw/weclaw.log` | 日志 |
| `~/.weclaw/accounts/` | 账号、游标、引用缓存、待处理消息 |
| `~/.weclaw/workspaces/` | 代码工作区 |
| `~/.weclaw/cron/jobs.json` | 定时任务 |
| `~/.weclaw/memory.json` | 长期记忆 |
| `~/.weclaw/search.json`、`models.json`、`sessions.json` | 搜索源、模型、CLI 会话 |

备份至少：`config.json`、`accounts/`、`cron/`、`memory.json`、`workspaces/`。

## 已知限制

- 不支持 PDF（不读也不导出）；`.doc`/`.xls` 读不了，请另存为新格式。
- 视频理解靠抽帧（最多 6 张），**听不到声音**；没装 ffmpeg 时会如实说没看清。
- iLink 不支持原生语音条，只能视频气泡或 mp3。
- `/mod` 只对 HTTP 助手有效。
- 会话、记忆、工作区按微信用户区分，不按机器人账号隔离；没有知识库（记忆是轻量条目，非向量检索）。
- 网页抓取可能被反爬/登录拦截，退回搜索片段；正文可能带少量导航文字。
- 免费服务会波动：搜索、生图、生视频、TTS 都可能限流或停止免费。
- 上游一键安装脚本/官方二进制会把本 fork 覆盖，升级只用 `weclaw upgrade`。

## 升级

```bash
weclaw upgrade
```

- 从本仓库最新 Release（<https://github.com/KonoLLo/weclaw/releases/latest>）下载对应平台的二进制并原地替换；会先比较版本号，本地比 Release 新时不动。
- Linux + systemd：检测到 `weclaw` 服务在跑会自动 `systemctl restart weclaw`；前台或其他方式启动的，升级后自己重启。
- Windows：在 cmd 里运行 `weclaw.exe upgrade`，完成后关闭机器人窗口、重新双击即可。
- 只认这两个资产名：`weclaw_linux_amd64`、`weclaw_windows_amd64.exe`；其他平台按下面的源码构建。

```bash
git clone https://github.com/KonoLLo/weclaw.git
cd weclaw
go build -trimpath -ldflags "-s -w -X github.com/fastclaw-ai/weclaw/cmd.Version=1.3.3" -o weclaw .
sudo install -m 0755 weclaw /usr/local/bin/weclaw
```

合并上游新代码时保留本 fork 提交历史、单独解冲突，不要用官方二进制覆盖。

## 上游与许可

- 上游：[fastclaw-ai/weclaw](https://github.com/fastclaw-ai/weclaw) · 本 fork：[KonoLLo/weclaw](https://github.com/KonoLLo/weclaw) · 许可：[MIT](LICENSE)
- 参考 [@tencent-weixin/openclaw-weixin](https://npmx.dev/package/@tencent-weixin/openclaw-weixin) 实现，仅限个人学习与自用。