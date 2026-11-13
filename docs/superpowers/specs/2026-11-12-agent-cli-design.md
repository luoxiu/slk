# slk 的无界面命令（给 agent 和脚本用）

## 目标

agent-slack 2026-09 之后没再更新，每条命令要好几秒，发一条消息约 25 秒，也不能实时收消息，调用方只能定时轮询。
这个 fork 给 slk 加一组不起界面的子命令，读写都输出 JSON，命令名、参数和字段照 agent-slack 0.10.2 做，换过来基本只换程序名；
再加一个 agent-slack 没有的 `slk listen`，用 slk 已有的那条网页版 websocket 实时推事件，轮询降成断线后的对账。

设计取舍：

- 走用户自己的 Slack 登录态，不建 Slack app、不要 bot token。
- 做成 `slk` 的子命令，不另起一个程序：`cmd/slk` 里的 token 读取、选 workspace、按名字找频道这些函数能直接复用。
- agent 命令永远不碰钥匙串，只用 `slk --add-workspace` 存下的 token：常驻进程半夜弹一个钥匙串密码框会把它卡住。

## 不改上游

为了以后合 gammons/slk 的更新不冲突，上游已有的文件只改一处：`cmd/slk/main.go` 的 `switch os.Args[1]` 加一个 case，把下面这些命令词交给 `runAgentCommand(os.Args[1:])`。
其余全是新文件：`cmd/slk/agentcmd*.go`（`package main`），要给 `internal/slack` 加方法就新建 `internal/slack/agentcmd_*.go`。不碰 `internal/ui` 和界面相关的任何东西。

命令词：`message`、`channel`、`user`、`search`、`unreads`、`listen`、`auth`、`later`。

## 通用约定

- 成功：JSON 写到 stdout，退出码 0。`listen` 是每行一个 JSON（NDJSON），每行写完立刻 flush。
- 失败：一行人话写到 stderr，里面带 Slack 的错误码原文（`channel_not_found`、`already_reacted`、`message_not_found`），退出码 1。
  认证问题（没有 token、`invalid_auth`、`not_authed`、`token_revoked`）退出码 2，提示跑 `slk --add-workspace`。
  调用方只看退出码和 stderr 里的错误码字符串，不解析别的。
- 参数可以写在位置参数前面或后面（`message list C123 --limit 5` 和 `message list --limit 5 C123` 一样）。`--` 之后全是位置参数，用来传以 `-` 开头的正文或 emoji 名。
  Go 的 `flag` 碰到第一个位置参数就停，所以要有一个公共的解析函数，所有命令都用它。
- 不打印 token、cookie，日志和报错里也不打。
- `--workspace <slug|team id|名字>` 选 workspace，复用 `selectExportToken` 的规则，默认取环境变量 `SLK_WORKSPACE`。
  都没写时：目标是消息链接，按链接的域名选；再不行用 `config.toml` 里 `[general] default_workspace`（TUI 开场选哪个也看它）；只存了一个 workspace 时用那一个。
  从桌面版导入时会把登着的 workspace 全带进来；写 `default_workspace` 比在每个调用的地方各设环境变量可靠：从 Finder 打开的 app、launchd、hook 拿到的环境各不一样，配置文件只有一份。
- 安全模式：环境变量 `SLK_SAFE_MODE=1` 时，除了建草稿以外的写操作一律拒绝，stderr 写 `safe mode: ...`，退出码 1，在连 Slack 之前：send（含 `--attach`）、edit、delete、react、`channel mark`、`later` 的写类子命令、`message draft update`、`message draft delete`。
  读操作和 `message draft list`、`message draft create` 不受影响：起草本来就是安全模式要留给 agent 的那条路：起草不用问，其余写入先问。
  改、删草稿照写操作拦：草稿列表里也有用户自己写的，无人值守的 agent 可以加草稿，但不能改、删用户的。
  标记已读和 Later 虽然只有用户自己看得到，但会改掉他的未读和待办，自动跑的 agent 标错了就漏消息，所以照写操作拦。
  建议自动跑的会话里一直开着它，只在用户批过的那一条命令前面 `env -u SLK_SAFE_MODE`。
- 每个命令先 `client.Connect`（一次 `auth.test`），再做事。慢了再说，先量。
- 会话缓存：`auth.test` 的结果（team_id、team、user_id、user、url，不含 token 和 cookie）存在 `$XDG_CACHE_HOME/slk/agent-session/<team_id>.json`（0600），记着 token 文件的大小和修改时间，一致才用，命中就省掉 `auth.test` 那一趟；缺、坏、token 文件变了都回退到 `Connect` 并重写。命中后命令遇到认证错误（退出码 2）就删掉它。`auth test` 和 `listen` 不读缓存，照旧真调 `auth.test`。

### 目标（`<target>`）

所有带 `<target>` 的命令都认三种写法：

- 频道 ID：`C…`、`D…`、`G…`。
- `#频道名`：复用 `resolveExportChannel`。
- 消息链接：`https://<ws>.slack.com/archives/<C>/p<16 位>[?thread_ts=<ts>&cid=<C>]`，用 `slackurl.Parse` 解析，带出 channel、ts 和 thread_ts。
  `slackurl.Parse` 原样带出 `thread_ts`，所以 `parseAgentLink` 再核一次：不像 Slack 的 ts（和 `--thread-ts` 同一个校验 `checkAgentTS` 的格式）就当参数错，退出码 1，不发请求。

### 消息对象

`message list`、`message get`、`search messages`、`unreads`、`listen` 输出的消息都是这一个形状，由一个公共函数从 `slack.Message` 转出来：

```json
{
  "ts": "1793932809.791579",
  "thread_ts": "1793932000.000100",
  "reply_count": 3,
  "channel_id": "C123",
  "author": { "user_id": "U1", "bot_id": "B1", "username": "deploy-bot" },
  "content": "正文",
  "subtype": "bot_message",
  "edited": true,
  "files": [{ "id": "F1", "name": "a.png", "title": "a.png", "mimetype": "image/png" }],
  "reactions": [{ "name": "eyes", "users": ["U1"], "count": 1 }],
  "permalink": "https://example.slack.com/archives/C123/p1793932809791579?thread_ts=…&cid=C123"
}
```

- 空值的字段不输出（`omitempty`），但 `ts`、`author`、`content` 总在。
- `content` 照 agent-slack 0.10.2 的 `renderSlackMessageContent` 渲染（`cmd/slk/agentcmd_content.go`），从 agent-slack 换过来后读到的字一样：
  - 先取 blocks（section 的 text、fields 和带链接的按钮，actions 里带链接的按钮，context 的文字，image，rich_text 转成 mrkdwn；header、divider 不出字）和 attachments（转发的消息是一行 `*Forwarded from …*` 加 `> ` 引用的原文；其余是 pretext、带链接的 title、text、fields，都没有才用 fallback），两段之间空一行；都没有才用 `text`。
  - 再转成 markdown：`<url|字>` → `[字](url)`，`<url>` → `url`（只认 http、https），`<#C…|名>` → `#名`，`<@U…|名>` → `@名`，`<@U…>` → `@U…`，`<!here>` 这类 → `@here`，`&lt; &gt; &amp;` 还原，`:shortcode:` 换成 emoji（用 slk 的表；自定义的、不认识的原样留着）。rich_text 里的频道提及没有名字，留成 `<#C…>`，agent-slack 也这样。
  - slack-go 不解码 attachment 的 `is_share`、`message_blocks`、`files` 和嵌套 `attachments`：转发靠 `from_url` 是 Slack 消息链接认出来，引用的是 attachment 的 `text`，转发里带的文件不显示。
  - `referenced_users` 从渲染前的 `text`、blocks、attachments 里找 `<@U…>`。
- `--max-body-chars N` 按字符截断渲染后的 `content`，截了就在后面加 `\n…`（unreads 里是 `\n...`，都照 agent-slack），`-1` 不截，默认 4000。
- `reactions` 只在 `--include-reactions` 时输出；`permalink` 在 search 结果里总有，别处不出。
- `channel_id` 每条消息都有（agent-slack 在 message get 和读频道历史时带，读 thread 时去掉了；这里一律带上）。

## 命令

### `slk message list <target> [--thread-ts TS] [--oldest TS] [--latest TS] [--limit N] [--max-body-chars N] [--resolve-users] [--include-reactions]`

- 有 `--thread-ts`，或 target 是消息链接：读那条消息所在的整个 thread（`conversations.replies`，ts 用链接里的 thread_ts，没有就用消息自己的 ts）。翻页读完，最多 1000 条。
- 否则读频道历史（`conversations.history`），`--oldest`、`--latest` 照 Slack 默认不含边界，`--limit` 默认 25。
- 按 ts 从旧到新排。
- 输出 `{"messages": [...]}`。`--resolve-users` 时多一个 `"referenced_users": {"U1": {"id","name","real_name"}}`，覆盖作者和正文里 `<@U…>` 提到的人。
- thread 不存在：stderr 带 `thread_not_found`，退出码 1。

### `slk message get <target> [--ts TS] [--thread-ts TS] [--max-body-chars N]`

- 取一条消息。target 是链接时 ts 从链接来。有 thread_ts 就从 thread 里找。
- 输出 `{"message": {...}}`。找不到：stderr 带 `message_not_found`，退出码 1。

### `slk message send <target> <text> [--thread-ts TS]`

- 发消息；target 是消息链接时回到那条消息的 thread 里。`--thread-ts` 不像 ts 就是参数错，不发请求。
- 用 slk 的 `SendMessage` / `SendReply`，正文格式和 slk 界面里发的一样。
- 输出 `{"ok": true, "channel_id": "C123", "ts": "…", "permalink": "…"}`，permalink 用 `slackurl.Build` 拼，不多调一次 API。

### `slk message react add|remove <target> <emoji> [--ts TS]`

- 输出 `{"ok": true}`。重复加同一个：Slack 返回 `already_reacted`，原样进 stderr，退出码 1（调用方可以当成功）。

### `slk message edit <target> <text> [--ts TS]`、`slk message delete <target> [--ts TS]`

- 输出 `{"ok": true, "channel_id": "…", "ts": "…"}`。

### `slk unreads [--max-messages N] [--max-body-chars N]`

照 agent-slack 的 unreads 做：

- 一次 `client.counts`（带 `thread_count_by_channel`）列出有未读的频道、群聊、私信。每个会话同时问 `conversations.info`（私信再问一次 `users.info`）和 `conversations.history`（`last_read` 之后最近 `--max-messages` 条，默认 10；没有 `last_read` 就不读消息）。
- 所有会话一起取，同一时刻最多 16 个请求在路上（agent-slack 一次全发，未读多时一下几百个请求，撞 Slack 的限流，也不像浏览器）。限流等过去重试；会话没了、进不去（`channel_not_found` 一类）跳过并在 stderr 写一行；别的错误整条命令失败。
- 读回的消息去掉系统消息：`channel_join`、`channel_leave`、`channel_topic`、`channel_purpose`、`channel_name`、`channel_archive`、`channel_unarchive`，`group_` 开头的同样七种。先按条数取再去掉，所以可能少于 N 条。
- 输出 `{"channels": [{"channel_id","channel_name","channel_type","unread_count","mention_count","messages":[...]}], "threads": {"has_unreads": true, "mention_count": N}}`。
  - `channel_type` 是 `dm`、`mpim`、`channel`、`group` 之一（agent-slack 把私有频道也叫 `channel`）。
  - 私信的 `channel_name`：会话自己有名字就用，没有就用对方的 `display_name`，再没有用 `real_name`，再没有用 `name`；对方查不到写对方的 ID。
  - `unread_count`：Slack 给了 `unread_count_display` 就用它，其次 `unread_count`；都没给（通常如此）就是读回的、去掉系统消息后的条数，Slack 说还有更多时至少 2；没读消息时是 1。
  - 按 `mention_count` 从大到小排，一样多再按 `unread_count`，再一样保持 `client.counts` 的顺序（频道、群聊、私信）。
  - `threads` 只在 Slack 说有 thread 未读时出现。只有 thread 里有新回复的会话不单列，要靠 `search messages is:thread` 和 `listen` 补。

### `slk search messages <query> [--after YYYY-MM-DD] [--before YYYY-MM-DD] [--user U] [--limit N]`

- `--after`、`--before` 拼成 `after:`、`before:`，`--user U` 拼成 `from:<@U>`。按时间倒序（`sort=timestamp`）。`--limit` 默认 20。
- 输出 `{"messages": [...]}`，每条带 `channel_id` 和 Slack 给的 `permalink` 原文：thread 里的回复链接里带 `?thread_ts=`，靠它能找到 thread 的根。

### `slk channel list [--limit N] [--cursor C]`

- 一页 `users.conversations`，类型是公开频道、私有频道、群聊、私信，不含已归档的。`--limit` 默认 200。
- 输出 `{"channels": [{"id","name","is_channel","is_group","is_im","is_mpim","is_private","user"}], "next_cursor": ""}`。几个 `is_*` 一律输出布尔值，不省略。`user` 只有私信有，是对方的 ID。

### `slk user get <U…|邮箱|@账号名>`

- 用户 ID（`U` 或 `W` 开头、后面至少 8 位大写字母或数字，真实 ID 都至少 9 位）直接 `users.info`；`WANGLI` 这种全大写的账号名不算 ID。
  邮箱和账号名（带不带 `@` 都行）在 `users.list` 里找（每页 500 条翻到底）：邮箱比 `profile.email`；账号名先比 `name`（不分大小写，找到就停），没有再比 `display_name`，只有一个才用，几个重名就报错列出 ID。找到后再 `users.info`，输出和按 ID 查一样。
  翻 `users.list` 遇到限流等过去重试那一页（和别的列表命令一样）；Slack 给了一个已经翻过的 cursor 就停下报错，不当成「找不到」。
  不用 `users.lookupByEmail`：2026-11-13 实测浏览器会话的 token 调它回 `not_allowed_token_type`。
- 输出 `{"id","name","real_name","display_name","email","is_bot","deleted","is_restricted","is_ultra_restricted","is_stranger","tz","image_72"}`。布尔值不省略。找不到：stderr 带 `user_not_found`。

### 草稿：`slk message draft list|create|update|delete`

Slack 原生草稿，会出现在用户自己 Slack 客户端的草稿列表和对应输入框里，只有他自己看得到。**`list` 和 `create` 不受安全模式拦，`update` 和 `delete` 受拦**（见通用约定）。走 Slack 网页版的 `drafts.*` 接口。

- `draft list [--limit N] [--all]`：`drafts.list`，默认只要没发、没删的（`is_active=true`），`--all` 全要。`--limit` 默认 20。
  输出 `{"drafts": [{"id","text","destinations":[{"channel_id","thread_ts","broadcast"}],"last_updated_ts","date_created","date_scheduled","file_ids"}]}`，
  空字段省略。`text` 从 rich_text block 渲染成 markdown（用 `blockkit.RichTextToMarkdown`，和界面里编辑已发消息同一个函数）。
- `draft create <target> <text> [--thread-ts TS] [--broadcast]`：`drafts.create`。target 是消息链接时草稿挂在那条消息的 thread 上。
  正文用 `mrkdwn.Convert` 转成 rich_text block（和 `message send` 一样）。请求体：`blocks`、`destinations=[{channel_id, thread_ts?, broadcast?}]`、`client_msg_id`（新 UUID）、`file_ids=[]`、**`is_from_composer=false`**。
  2026-11-13 看过 Slack 桌面版建的 100 条草稿（只看结构），频道里的、thread 里的全是 `is_from_composer=false`，destination 是 `{channel_id, thread_ts, broadcast:false}`；
  agent-slack 传的是 `true`，它建的 thread 草稿落在频道输入框里，这里不照它。
  `--broadcast` 只在有 thread 时有意义，没有 thread 时报错。输出 `{"ok": true, "draft": {...同 list...}}`。
- `update` 和 `delete` 按 ID 找草稿：先在 `drafts.list is_active=true` 里找（翻页最多 20 页），找不到再翻全部草稿，只为分清「已经发了或删了」和「没有这条」。
  只翻全部的话，发过的草稿攒多了，20 页翻满也到不了一条还活着的草稿。全部里找到的条目要是没发没删，照样用。
- `draft update <draft-id> <text>`：`drafts.update`，只换正文，destination 和 `file_ids` 照旧（带上找到的那条的 destinations 和 `client_last_updated_ts`），`client_msg_id` 每次新生成（照 agent-slack）。
  destinations 只回传 `channel_id`、`thread_ts`、`broadcast`：私信草稿 `drafts.list` 里还带 `user_ids`，两样一起传 `drafts.update` 会拒（`both_user_ids_and_channel_id_provided_in_destination`，2026-11-13）。
  不传 `is_from_composer`：2026-11-13 实测，slk 建的一个 thread 草稿和一个私信草稿各 update 一次，`drafts.list` 读回来都还是 `false`，不带不会被重置。
  找不到、已经发了或删了：stderr 带 `draft_not_found`，退出码 1。定时草稿（`date_scheduled>0`）拒绝改：`drafts.update` 不带定时，改了会把定时冲掉。输出同 create。
- `draft delete <draft-id>`：`drafts.delete`，带 `client_last_updated_ts`（从 list 找）。输出 `{"ok": true, "id": "…"}`。
  找不到、已经发了或删了：同 update，`draft_not_found`。定时草稿一律拒绝删：那是一条定好要发的消息，删了等于取消发送，报错让他在 Slack 里处理。
- `client_last_updated_ts` 补成小数点后 7 位（agent-slack 的 `padDraftTs`：Slack 不收位数少的，`1700000000.123` 要发成 `1700000000.1230000`）。

### Later：`slk later list|save|complete|archive|reopen|remove|remind`

Slack 的「稍后」（Later）。走网页版的 `saved.*` 接口。`list` 是读；其余子命令改他的待办，受安全模式拦。

- `later list [--state in_progress|archived|completed|all] [--limit N] [--max-body-chars N] [--counts-only]`：`saved.list` 翻页（每页 50），只要 `item_type=message`，按 state 过滤，取前 `--limit`（默认 20）条。
  每条再取会话名（同 unreads 的规则）和那条消息本身：**thread 里的回复也要取得到**（走 `ReadThreadMessage` 那条路，agent-slack 只查 `conversations.history`，存的是 thread 回复时取不到）。取不到的消息就不带 `message` 字段，不让整条命令失败。
  输出 `{"counts": {"in_progress","archived","completed","total"}, "items": [{"channel_id","channel_name","ts","state","date_saved","date_completed","date_due","message": {...消息对象...}}]}`。
  没有 `next_cursor`：命令一直翻页到凑够 `--limit` 条，Slack 最后一页的 cursor 接不上输出停下的地方。
  `--counts-only` 只给 `counts`。
- `later save <target> [--ts TS]`：`saved.add`（`item_id=频道`、`item_type=message`、`ts`）。
- `later complete|archive|reopen <target> [--ts TS]`：`saved.update` 的 `mark` 分别是 `completed`、`archived`；reopen 照 agent-slack 0.10.2 先发 `uncompleted` 再发 `unarchived`，有一个被接受就算成功。
  `saved.update` 用 multipart 发：agent-slack PR #76 记的是用 form 发时 Slack 回 ok、却悄悄忽略 `mark` 和 `date_due`。正因为错了也回 ok，只有在 Slack 的 Later 页签里看结果才算验过。
- `later remove <target> [--ts TS]`：`saved.delete`。
- `later remind <target> [--ts TS] (--in 30m|2h|3d | --at <RFC3339 或 Unix 秒>)`：`saved.update` 带 `date_due`。两个都不给或都给报错。`--in` 只认整数加 m、h、d（agent-slack 认的 tomorrow、星期几没做）。
- 写类子命令输出 `{"ok": true, "channel_id", "ts"}`，remind 多一个 `"date_due"`。

### 标记已读：`slk channel mark <target> [--ts TS] [--thread-ts TS]`

受安全模式拦：只改他自己的已读位置，但标错了他会漏看消息。

- 没有 thread：`conversations.mark` 到 `--ts`（target 是链接时用链接的 ts）；都没给就标到这个会话最新一条（`conversations.history limit=1`）。
- 有 thread（`--thread-ts`，或链接带 `thread_ts`）：`subscriptions.thread.mark`（slk 的 `MarkThread`），ts 规则同上，「最新一条」取 thread 的最后一条回复。
- 给了 ts（`--ts` 或链接）就先核对那条消息在不在，再标：没有 thread 时用 `conversations.history`（`oldest=latest=ts`、`inclusive`、`limit=1`），有 thread 时用 `ReadThreadMessage`。
  取不到：stderr 带 `message_not_found`，退出码 1，不标。标会话时那条要是 thread 里的回复（`thread_ts` 不等于 ts，又不是同时发到频道的 `thread_broadcast`），拒绝，提示改用 `--thread-ts`（或带 `thread_ts` 的链接）：那是 thread 的已读位置，不是会话的。
- 输出 `{"ok": true, "channel_id", "ts"}`，thread 时多 `"thread_ts"`。

### 上传：`slk message send <target> [text] --attach <path> [--attach <path> …]`

- `--attach` 可以写多次。有 `--attach` 时 text 可以省略，有 text 就作为 initial comment 挂在**最后一个**文件上（`UploadFile` 的注释说一次 share 里的文件 Slack 会并成一条消息，顺序上传的不行）。
- 用 slk 的 `UploadFile`（`files.getUploadURLExternal` → PUT → `files.completeUploadExternal`），回 thread 的规则和 send 一样。
- 先检查所有路径都是能读的普通文件，再开始传，一个不行整条命令不传。单个文件上限 1 GB（Slack 的上限），超了报错。
- 受安全模式拦（别人看得到）。
- 输出 `{"ok": true, "channel_id", "thread_ts"?, "files": [{"id","name","title","permalink"}]}`。拿不到消息的 ts（上传是异步并进消息的），所以没有 `ts`。
  permalink 是传完之后每个文件再调一次 `files.info` 拿的；这一步失败命令照样成功（文件已经发出去了，报失败会招来重发），只是那个文件没有 permalink，stderr 写一行。
- 每个文件单独成一条消息（`UploadFile` 一次只传一个），说明文字挂在最后一个上。说明文字和 send 的正文一样按 markdown 写，转成 mrkdwn 作 `initial_comment`。
- 空文件也拒绝（slack-go 不收 0 字节，不先拦就会一批传到一半才失败）。1 GB 按十进制算。传到一半失败：退出码 1，stderr 列出已经发出去的文件。
  `files.completeUploadExternal`（把文件发进会话的那一步）出错、又不是 Slack 明确拒绝（slack-go 的 `SlackErrorResponse`）时，比如 5xx、连接断了、回了 ok 却没带文件，
  Slack 可能已经发出去了：stderr 说「第 i 个文件可能已经发出去了，重试前先看一下会话」，不说「什么都没传」。之前的两步出错和 Slack 明确拒绝，那个文件没发出去。
  `UploadFile` 只在 slack-go 的错误文字里标出是哪一步，`TestAgentSendAttachCompleteFailureMayHavePosted` 钉住这两处文字，升级 slack-go 改了它会失败。

### 下载附件：`message get` 和 `message list` 加 `--download-files`

- 给了这个参数，就把输出里每条消息的每个文件下载到本地，文件对象上多一个 `"path"`（绝对路径）；某个下载失败就写 `"download_error"`，命令照样成功。
- 下载目录 `$XDG_CACHE_HOME/slk/downloads/<文件 ID>/<文件名>`（`~/.cache/slk/downloads/…`），文件名用 Slack 的 `name`（保留扩展名），没有才用 `title`，照 `filedl` 的 `sanitizeName` 清洗、不许带路径；文件 ID 只许大写字母和数字。
  同一个文件 ID 目录里已经有文件就直接用，不再下（agent 反复读同一个 thread 不会堆一堆副本）。先下进隐藏的临时目录，下完整个改名过去，缓存里不会有下了一半的文件。
  `filedl` 整个下载限时 60 秒、先读进内存，大文件可能超时，结果写进 `download_error`。
- 用 slk 的 `filedl.Downloader` 和 `slackhttp.AuthResolver`（带 cookie 认证、认得出 Slack 返回登录页的失败），URL 用 `url_private_download`，没有就 `url_private`。
- 只下 `https://files.slack.com` 上的 URL（scheme 是 https、host 正好是 `files.slack.com`）。文件对象的 URL 来自消息，应用发的文件 URL 可以指向任何地方；别的一律写 `download_error`，不发请求。
- 按 Slack 给的 `size` 设上限 200 MB（十进制），超了写 `download_error`（太大，请在 Slack 里下），不发请求：`filedl` 先把整个文件读进内存。
  这两条在查缓存之前，所以缓存里有没有，输出都一样。`size` 不准或没给时这条拦不住，`filedl` 照样整个读进内存。
- 文件对象（消息对象里的 `files`）为此多带 `"size"`；不给 `--download-files` 时形状不变。

### `slk auth test`

- 只调一次 `auth.test`，不碰钥匙串。输出 `{"ok": true, "team_id","team","user_id","user","url"}`。没有 token 或 token 失效时退出码 2。

### `slk listen`

常驻，连 slk 已有的那条网页版 websocket（`Client.StartWebSocket`），收到事件就往 stdout 写一行 JSON。断线自动重连，退避从 1 秒翻倍到 30 秒，一条连接稳定连满一分钟才把退避清零。
没有直接用 slk 的 `ConnectionManager`：它永不停止（token 失效也一直重试）、丢掉断线原因、每次握手成功就清零退避（服务器连上就挂断时会每秒重拨一次）。机器睡眠唤醒后主动断开重连，和界面一样。

怎么结束：

- 退出码 0：SIGINT、SIGTERM。关掉连接，最多等 2 秒，不再补一行 `disconnected`。
- 退出码 2：token 失效。任何一次没等到 Slack `hello` 的连接，都再跑一次 `auth.test`；返回 `invalid_auth`、`not_authed`、`token_revoked`、`token_expired`、`account_inactive` 就退出。
- 退出码 1：stdout 写不进去；启动时本地配置有问题；握手或第一次 `auth.test` 卡了 3 分钟还没回来（库里这一步没有超时，卡住的连接没法重试，只能退出让 launchd 重启）。
- 网络问题不退出，一直重试。

连上之后 90 秒还没等到 `hello`，就关掉这条连接，写一行 `disconnected`（`error` 是「no hello within 1m30s」），照常 `auth.test`、退避、重连。
库里 60 秒的读超时会被 ping 续上，一条一直有 ping、就是不发 `hello` 的连接否则永远挂着，`listen` 一声不吭。
90 秒比读超时长，所以完全没动静的连接还是先被读超时结束；比握手的 3 分钟短，因为这种连接关得掉、能重拨，不用退出。

`disconnected` 的 `error` 和 stderr 那一行写出去之前都过一遍脱敏：去掉 URL 的查询串（`?` 起到空格或引号为止），`xox[a-z]-…` 形状的 token 和 cookie 换成 `[redacted]`。
websocket 的 URL 在查询串里带着 token，gorilla/websocket 解析不了这个 URL 时会把它原样写进错误。

已知限制：握手被拒（HTTP 401/403）时 `StartWebSocket` 把响应丢了，看不到状态码，只能靠上面那次 `auth.test` 判断；`disconnected` 的 `error` 也只能写成固定的「connection lost」「connection closed before hello」，读错误被库吞了。要改得动 `internal/slack/client.go`，先不动。

给调用方：在 `disconnected` 之后的那个 `hello` 上补一次轮询，不要在 `disconnected` 那一刻补，那时多半还断着网，补了也白补。断着的时候 `disconnected` 最多每 30 秒出一行。

事件（每行一个，都带 `"type"` 和 `"at"`，`at` 是 RFC 3339 的本机收到时间）：

- `{"type":"hello","team_id","user_id"}`：每次连上（包括重连）。
- `{"type":"disconnected","error"}`：每一次连接尝试结束时一行，连上又断的、没拨通的、没等到 `hello` 就断的都算，所以它一定在下一个 `hello` 之前。调用方看到它，就知道这段时间可能漏了事件。
- `{"type":"message", ...消息对象..., "changed": false, "mentions_me": true}`：新消息和改过的消息（`changed` 为真）。`mentions_me` 用 `mention.InText`。`edited` 是消息对象里的字段，为假时不输出。
- `{"type":"message_deleted","channel_id","ts"}`
- `{"type":"reaction_added"|"reaction_removed","channel_id","ts","user_id","emoji"}`
- `{"type":"channel_marked","channel_id","ts"}`、`{"type":"thread_marked","channel_id","thread_ts","ts"}`：他在别的客户端读了。

打字、在线状态、自定义 emoji 这类不输出。不过滤频道，过滤交给调用方。

## 不做

canvas、workflow、定时发送、SQLite 缓存、edge 缓存。等真用到再加。

## 验证

- 单元测试：`package main` 白盒，用 `cmd/slk/bootstrap_adapters_test.go` 里的假 Slack（`newFakeSlack`、`newTestClient`），每个命令测输出形状和错误码；
  测安全模式拦写、参数前后都能写、`--`、三种 target；`listen` 拿真实帧喂 `dispatchWebSocketEvent` 测输出行，测断线时先出 `disconnected` 再出 `hello`。
- 真实 workspace（要他先跑一次 `slk --add-workspace`，会弹一次钥匙串）：
  - 同一组输入下，8 个只读命令的输出拿去和 agent-slack 逐字段对比；
  - 两边各跑 10 次取中位数，比耗时；
  - `listen` 挂着，对照轮询的结果，看新消息几秒到；
  - 发消息只往他自己的私信里发一条，他点头再发。
- 提交前：`go build ./...`、`go vet ./...`、`go test ./cmd/slk/... ./internal/slack/... -race`、`gofmt -l .` 为空。
