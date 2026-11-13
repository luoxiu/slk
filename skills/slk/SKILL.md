---
name: slk
description: 用 slk 命令行读写 Slack：打开消息链接、读 thread 和频道历史、看未读、搜消息、查人（ID、邮箱、账号名）、列会话、下载附件、看 Later，发消息、回 thread、传文件、加 reaction、改和删自己的消息、建原生草稿、标记已读，用 `slk listen` 实时收消息事件。走用户自己的 Slack 登录态，不建 Slack app、不要 bot token。Use when the user shares a slack.com link or asks to read, search, draft, reply to or watch Slack messages from the command line.
---

# slk：给 agent 用的 Slack 命令行

slk 是 [gammons/slk](https://github.com/gammons/slk)（一个 Slack 终端客户端）的 fork：[luoxiu/slk](https://github.com/luoxiu/slk)。
这个 fork 在原来的界面之外加了一组不起界面的子命令，读写都输出 JSON，另外有一个 `slk listen` 通过 Slack 网页版的 websocket 实时推事件，不用轮询。

它用的是用户本人的 Slack 登录态（从 Slack 桌面版导入的浏览器会话），所以：

- 不用建 Slack app，不用找管理员装 bot，用户能看到的频道和私信它都能读。
- **发出去的消息、reaction 都是用户本人发的**，别人看到的就是他本人。写操作前一定要用户这一次明确同意，见「写」一节。
- 它走的是 Slack 网页版的内部协议，不是官方开放的 API。slk 自己的说明是「may violate Slack's TOS. Use at your own risk」，要不要用由用户决定。

## 安装（每台电脑一次）

1. 装程序。**要装这个 fork，不要 `brew install slk`**：Homebrew 和 gammons 发布的是原版，没有下面这些命令。

   ```bash
   # 用 mise（推荐，升级用 mise up）
   mise use -g github:luoxiu/slk@latest
   # 或者从 https://github.com/luoxiu/slk/releases 下载对应平台的包，把里面的 slk 放进 PATH
   ```

   用浏览器下载的包，macOS 会给程序打上隔离标记、拒绝运行（程序没有经过 Apple 公证），解开后跑一次 `xattr -d com.apple.quarantine <路径>/slk`。用 mise 装的没有这个问题。

   装完 `slk --version` 应该带 `-agent` 字样，比如 `slk 0.34.0-agent.1`。
   报 `mise ERROR No version is set for shim: slk`，是装了但没设成全局版本（比如之前只用 `mise exec` 跑过），再跑一次上面的 `mise use -g`。

2. 登录。这一步要用户自己在终端里跑，agent 不要替他跑：它是交互式的，macOS 会弹一次钥匙串要他的电脑密码。

   ```bash
   slk --add-workspace            # 从 Slack 桌面版导入（桌面版要先登录着）
   slk --add-workspace --browser  # 没装桌面版时，从浏览器的会话导入
   ```

   导入之后 token 存在 `~/.local/share/slk/tokens/`，之后所有命令都只读这里，不再碰钥匙串。不要读、打印或拷贝这个目录里的文件。

3. 登了不止一个 workspace 时，定一个默认的，否则每条命令都要带 `--workspace`：

   ```bash
   slk --list-workspaces          # 看各 workspace 的 SLUG
   ```

   在 `~/.config/slk/config.toml` 里加（这个文件 slk 自己也会写，只加这两行，别的别动）：

   ```toml
   [general]
   default_workspace = "your-workspace"
   ```

4. 验证：

   ```bash
   slk auth test
   # {"ok":true,"team_id":"T…","team":"example","user_id":"U…","user":"…","url":"https://example.slack.com/"}
   ```

   退出码 2 就是没登录或登录失效，请用户重跑第 2 步。

## 通用约定

- 成功：JSON 写到 stdout，退出码 0。
- 失败：stderr 一行英文，里面带 Slack 的错误码原文（`channel_not_found`、`message_not_found`、`already_reacted`、`not_in_channel`），退出码 1。
  没登录、token 失效退出码 2。判断成败只看退出码和 stderr 里的错误码，不要解析别的。
- 参数写在位置参数前后都行：`slk message list C123 --limit 5` 和 `slk message list --limit 5 C123` 一样。
  `--` 之后全当位置参数，正文以 `-` 开头时用：`slk message send C123 -- "-1 这个方案不行"`。
- 选 workspace：`--workspace <slug|team id|名字|域名>` > 环境变量 `SLK_WORKSPACE` > 消息链接的域名 > config 的 `default_workspace` > 只登了一个就用那个。
- 被 Slack 限流时命令会等限流过去再重试，所以偶尔会慢几秒，不用自己重试。
- 不会打印 token 和 cookie，报错里也不会。

### `<target>` 三种写法

所有带 `<target>` 的命令都认：

- 频道 ID：`C…`（频道）、`D…`（私信）、`G…`（私有频道、群聊）。
- `#频道名`：`#team-btp-dev`。会多一次列会话的请求，知道 ID 就用 ID。
- 消息链接：`https://example.slack.com/archives/C123/p1793932809791579`，也可以带 `?thread_ts=…&cid=…`。链接本身就指明了是哪条消息、在哪个 thread 里。

用户丢来一个 Slack 链接，直接把链接当 target 用，不用自己拆。

### 消息对象

读消息的命令（`message list`、`message get`、`search messages`、`unreads`、`listen`）返回的每条消息都是这个形状，空的字段不出现：

```json
{
  "ts": "1793932809.791579",
  "thread_ts": "1793932000.000100",
  "reply_count": 3,
  "channel_id": "C123",
  "author": { "user_id": "U1", "bot_id": "B1", "username": "deploy-bot" },
  "content": "渲染好的正文（markdown）",
  "subtype": "bot_message",
  "edited": true,
  "files": [{ "id": "F1", "name": "a.png", "title": "a.png", "mimetype": "image/png" }],
  "reactions": [{ "name": "eyes", "users": ["U1"], "count": 1 }],
  "permalink": "https://example.slack.com/archives/C123/p1793932809791579"
}
```

- `content` 已经渲染过：链接是 `[字](url)`，频道是 `#名字`，`:emoji:` 换成了字符，blocks 和转发的消息也展开了。
  **提到的人是 `@U…` 这种 ID**，要名字就加 `--resolve-users`（`message list` 有）或者 `slk user get <U…>`。
  发消息的人也只有 ID：`author.username` 只有少数机器人消息带，大多数机器人和真人一样只有 `user_id`（机器人另带 `bot_id`），名字同样从 `referenced_users` 或 `user get` 拿。
- 正文默认截到 4000 字，截了末尾有 `…`。`--max-body-chars -1` 不截，`--max-body-chars 300` 只要个摘要。
- `thread_ts` 和 `ts` 一样的是 thread 的根；不一样的是 thread 里的回复。
- `reactions` 只在 `--include-reactions` 时有；`permalink` 只在搜索结果里有。

## 读

```bash
# 一条消息的整个 thread（链接指向 thread 里任何一条都行），从旧到新，最多 1000 条
slk message list "https://example.slack.com/archives/C123/p1793932809791579"
slk message list C123 --thread-ts 1793932000.000100 --resolve-users

# 频道或私信最近的消息，默认 25 条，从旧到新
slk message list C123 --limit 50
slk message list C123 --oldest 1793900000.000000 --latest 1793990000.000000

# 只要一条
slk message get "https://example.slack.com/archives/C123/p1793932809791579"
slk message get C123 --ts 1793932809.791579 --max-body-chars -1

# 有未读的会话，每个带最近几条未读消息
slk unreads --max-messages 3 --max-body-chars 600

# 搜索，Slack 搜索语法照用（from:、in:、is:thread、has:link…），按时间倒序，默认 20 条
slk search messages "release train" --after 2026-11-01 --limit 30
slk search messages "<@U012ABCDE>" --after 2026-11-10     # 点名某人的
slk search messages "is:thread" --user U012ABCDE          # 某人在 thread 里发的

# 他在的会话（频道、私有频道、群聊、私信），一页最多 999 个，用 next_cursor 翻页
slk channel list --limit 999
slk channel list --limit 999 --cursor "<上一页的 next_cursor>"

# 一个人是谁：ID、邮箱、账号名（带不带 @ 都行）都可以；按邮箱和账号名查要翻整个通讯录，要几秒
slk user get U012ABCDE
slk user get ann.lee
slk user get someone@example.com

# 连附件一起读：每个文件下载到本地，文件对象上多一个 path
slk message get "<消息链接>" --download-files

# 用户 Slack 里「稍后」（Later）的待办，默认只要进行中的
slk later list --limit 20
slk later list --state all --counts-only

# 用户自己的未发草稿
slk message draft list
```

输出：

- `message list`：`{"messages": [...]}`；带 `--resolve-users` 时多一个 `"referenced_users": {"U1": {"id","name","real_name"}}`。
- `message get`：`{"message": {...}}`。
- `unreads`：`{"channels": [{"channel_id","channel_name","channel_type","unread_count","mention_count","messages":[...]}], "threads": {"has_unreads": true, "mention_count": 1}}`。
  `channel_type` 是 `dm`、`mpim`、`channel`、`group`（私有频道）之一，按 `mention_count` 从多到少排。只在 thread 里有新回复的会话不单列，`threads` 告诉你有没有。
- `search messages`：`{"messages": [...]}`，每条带 `permalink`，thread 里的回复的链接带 `?thread_ts=`。
  `--after`、`--before` 照 Slack 搜索的 `after:`、`before:`，都不含当天：要今天和昨天的，`--after` 写前天的日期。
- `channel list`：`{"channels": [{"id","name","is_channel","is_group","is_im","is_mpim","is_private","user"}], "next_cursor": "…"}`。私信的 `user` 是对方的 ID，没有下一页时 `next_cursor` 是空字符串。
- `user get`：`{"id","name","real_name","display_name","email","is_bot","deleted","is_restricted","is_ultra_restricted","is_stranger","tz","image_72"}`。
  `is_restricted`、`is_ultra_restricted` 是访客，`is_stranger` 是 Slack Connect 的外部人。账号名先比 `name`，比不上再比显示名，显示名重名时报错并列出几个 ID。
- `--download-files`（`message get`、`message list` 都有）：文件下到 `~/.cache/slk/downloads/<文件 ID>/<文件名>`，文件对象多 `path`（绝对路径）和 `size`；某个没下成写 `download_error`，命令照样成功。
  只下 `https://files.slack.com` 上的文件，超过 200 MB 的不下，这两种都写 `download_error`，让用户在 Slack 里看。
  同一个文件下过就直接用，不重下。图片、PDF、日志拿到 `path` 之后用读文件的工具看。
- `later list`：`{"counts": {"in_progress","archived","completed","total"}, "items": [{"channel_id","channel_name","ts","state","date_saved","date_completed","date_due","message": {...}}]}`。
  `--state` 是 `in_progress`（默认）、`archived`、`completed`、`all`。消息取不到（删了、没权限）时那一项没有 `message`。
- `message draft list`：`{"drafts": [{"id","text","destinations":[{"channel_id","thread_ts","broadcast"}],"last_updated_ts","date_created","date_scheduled","file_ids"}]}`，`--all` 连发过的、删过的一起列。

找频道里最近一个有回复的 thread：`slk message list <频道> --limit 50`，结果是从旧到新，从后往前找第一条 `reply_count` 大于 0 的，它的 `ts` 就是 thread 根，再 `slk message list <频道> --thread-ts <ts>` 读整个 thread。

找某个人的私信频道：`slk channel list` 里 `is_im` 为真、`user` 是那个人 ID 的那一项，没在第一页就翻页。

## 写

**发消息、传文件、加 reaction、改、删都是以用户本人的身份做的，对外可见，发出去收不回来。** 每一条都要用户这一次明确说「发」，起草给他看不算同意，上次同意过也不算。
标记已读、Later 的增删改别人看不到，但会改掉用户自己的未读和待办，标错了他就漏看消息，同样先问。新建草稿只有他自己看得到，起草不用问（见下一节）；改、删草稿要先问，那些草稿里可能有他自己写的。

建议默认开着安全模式，除了列草稿和建草稿以外的写操作都会被直接拒绝（退出码 1，stderr 是 `safe mode: …`），改草稿、删草稿也拒绝：

```bash
export SLK_SAFE_MODE=1
```

用户批准了某一条，只给那一条去掉：

```bash
env -u SLK_SAFE_MODE slk message send C123 "好的，我下午看"
```

```bash
# 发到频道或私信
slk message send C123 "正文"
# 回 thread：用那条消息的链接当 target，或者带 --thread-ts <根的 ts>
slk message send "https://example.slack.com/archives/C123/p1793932809791579" "收到"
slk message send C123 "收到" --thread-ts 1793932000.000100
# reaction，emoji 名带不带冒号都行
slk message react add "<消息链接>" white_check_mark
slk message react remove C123 eyes --ts 1793932809.791579
# 只能改、删自己发的
slk message edit C123 "改过的正文" --ts 1793932809.791579
slk message delete C123 --ts 1793932809.791579
# 传文件：--attach 可以写多次，正文可省；每个文件各成一条消息，正文挂在最后一个上
slk message send C123 "日志在这" --attach ./error.log --attach ./screen.png
# 标记已读：不给 ts 就标到最新一条；链接指向 thread 时标的是那个 thread
# 给了 ts 会先核对那条消息在不在，不在就退出码 1、stderr 带 message_not_found；thread 里的回复要用 --thread-ts 或带 thread_ts 的链接标
slk channel mark C123
slk channel mark "<thread 里某条消息的链接>"
# Later：存、完成、归档、恢复、删、提醒
slk later save "<消息链接>"
slk later complete "<消息链接>"
slk later remind "<消息链接>" --in 2h
slk later remove "<消息链接>"
```

- `message send` 返回 `{"ok":true,"channel_id":"C123","ts":"…","permalink":"…"}`。发完把 `permalink` 给用户，他点一下就能看到。
- 正文按 markdown 写，和在 slk 界面里打字一样，发出去会转成 Slack 的格式：`**粗体**`、`_斜体_`、`` `代码` ``、`[文字](https://url)`、`- ` 列表、换行。
  注意 `*一个星号*` 在 markdown 里是斜体，不是 Slack 习惯的粗体。
  中文里加粗、斜体的收尾标记前面是标点、后面紧跟着字时转不成格式，星号会原样发出去：`**背景：**正文` 不行，`**背景：** 正文`（后面空一格）可以。
  Slack 原生的写法也认：@ 人写 `<@U…>`（光写名字不会通知到他），频道写 `<#C…|名字>`，`<!here>`，`<https://url|文字>`。
- 重复加同一个 reaction 会退出码 1、stderr 带 `already_reacted`，意思是已经有了，当成功处理。
- `react`、`edit`、`delete` 返回 `{"ok":true}` 或 `{"ok":true,"channel_id":"…","ts":"…"}`。
- 带 `--attach` 的 send 返回 `{"ok":true,"channel_id","files":[{"id","name","title","permalink"}]}`，没有 `ts`（文件是 Slack 异步并进消息的）。
  发之前会先检查所有文件都在、能读、不是空的、不超过 1 GB，一个不行就一个都不传。
  传失败时 stderr 会列出已经发出去的文件；说某个文件「may already have been posted」时，它可能已经在会话里了，先 `message list` 看一眼再决定要不要重传，别直接重试。
- `channel mark` 返回 `{"ok":true,"channel_id","ts"}`，标 thread 时多 `thread_ts`；`later` 的写类返回 `{"ok":true,"channel_id","ts"}`，`remind` 多 `date_due`。

## 草稿

建在用户自己 Slack 里的原生草稿：出现在他的草稿列表和对应的输入框里，他检查完自己点发送。只有他看得到，`create` 和 `list` 安全模式不拦，起草不用先问。
`update` 和 `delete` 安全模式会拦（他的草稿列表里也有他自己写的），要改、删先问他，他同意了再去掉安全模式跑那一条。

```bash
slk message draft create C123 "正文"
# 回 thread 的草稿：用 thread 里某条消息的链接当 target，或者带 --thread-ts
slk message draft create "<thread 里某条消息的链接>" "正文"
slk message draft update <草稿 id> "改过的正文"
slk message draft delete <草稿 id>
```

- 正文写法和 `message send` 一样（markdown）。`create`、`update` 返回 `{"ok":true,"draft":{...}}`，草稿 id 在 `draft.id`。
- `update` 只换正文，草稿挂在哪不变。已经发了、删了的草稿报 `draft_not_found`。
- 定时发送的草稿不让改，也不让删（删了等于取消发送），让用户自己在 Slack 里处理。
- 告诉用户草稿建好了，顺便把正文贴给他看：他不一定马上切到 Slack 去找。

## 实时收消息：`slk listen`

```bash
slk listen
```

常驻进程，连上 Slack 网页版用的那条 websocket，收到事件就往 stdout 写一行 JSON（NDJSON，每行立刻 flush）。新消息一般 0.3 秒左右就到（实测中位数 0.28 秒），不用轮询。

它不会自己退出。只想听一会儿看看，自己负责停掉它：

```bash
slk listen > /tmp/slk-events.ndjson & pid=$!
sleep 60; kill $pid          # SIGTERM，退出码 0
```

事件（都带 `type` 和 `at`，`at` 是本机收到的时间）：

- `{"type":"hello","team_id","user_id"}`：连上了，包括每次重连。
- `{"type":"disconnected","error"}`：一次连接结束（断线、没连上都算），之后会自己重连，退避从 1 秒涨到 30 秒。
- `{"type":"message", …消息对象…, "changed": false, "mentions_me": true}`：新消息；`changed` 为真是有人改了一条消息。`mentions_me` 是这条点了当前用户的名。
- `{"type":"message_deleted","channel_id","ts"}`
- `{"type":"reaction_added"|"reaction_removed","channel_id","ts","user_id","emoji"}`
- `{"type":"channel_marked","channel_id","ts"}`、`{"type":"thread_marked","channel_id","thread_ts","ts"}`：用户在别的客户端读到了这里。

要点：

- **不过滤。** 用户所在的所有频道、私信、群聊的消息都会来，包括他没参与的 thread 里的回复，也包括他自己发的。要什么自己挑：
  私信看 `channel_id` 是不是 `D` 开头，点名看 `mentions_me`，他自己发的看 `author.user_id` 是不是 `hello` 里的 `user_id`。
- **断线期间的消息不会补发。** 看到 `disconnected` 之后的那个 `hello`，用 `search messages` 或 `message list --oldest <断线前最后一条的 ts>` 补读一次。
  不要在 `disconnected` 那一刻补，那时多半还断着网。第一次启动也一样：先起 `listen`，等到第一个 `hello` 再补读启动之前漏掉的，这样补读和监听之间没有空档。
- 退出码：0 是收到 SIGINT、SIGTERM；2 是 token 失效，要用户重新 `slk --add-workspace`；1 是 stdout 写不进去这类本地问题。网络问题不退出，一直重连。
  要长期跑就交给 launchd、systemd 这类会自动拉起的东西。
- 消息里没有会话类型（私有频道和群聊都是 `G` 开头），要区分就用 `slk channel list` 的 `is_mpim`，结果可以缓存。

## 没有的功能

canvas、定时发送、workflow 这个 fork 没做。读 canvas、定时发送可以用 Claude 的 Slack 连接器（Slack MCP）。

## 从 agent-slack 换过来

命令名、参数、JSON 字段是照 agent-slack 0.10.2 做的，常用的那部分基本只换程序名：

| agent-slack | slk |
| --- | --- |
| `agent-slack message list/get/send/edit/delete` | 同名，参数一样 |
| `agent-slack message react add/remove` | 同名 |
| `agent-slack unreads`、`search messages`、`channel list`、`user get` | 同名 |
| `agent-slack auth whoami` / `auth test` | `slk auth test` |
| `AGENT_SLACK_SAFE_MODE=1` | `SLK_SAFE_MODE=1`（slk 是直接拒绝，不会改道成草稿；改、删草稿也拒绝） |
| 轮询 | `slk listen` |
| `agent-slack message draft *`、`later *`、`channel mark` | 同名 |
| `message send --attach` | 同名 |
| `message get/list` 自动下载附件 | 加 `--download-files` 才下 |
| `canvas`、定时发送、workflow | 没有，见上一节 |

几处不一样：

- `content` 里的 `:emoji:` 认得更全（agent-slack 认不出的 `:large_green_circle:` 这类会换成字符）。
- 每条消息都带 `channel_id`，读 thread 时也带。
- 私有频道在 `unreads` 里的 `channel_type` 是 `group`，agent-slack 写的是 `channel`。
- `user get` 多给 `is_restricted`、`is_ultra_restricted`、`is_stranger`。
- 搜索结果里没有 `files`。
- `search messages` 没有 `--channel`：在查询里写 `in:<#C…>` 或 `in:#频道名`（私信也行，`in:<#D…>`）。agent-slack 的 `--channel` 碰到私信其实不过滤。
- `message list` 没有 `--with-reaction`、`--without-reaction`：加 `--include-reactions` 自己筛。
