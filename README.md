# 待办看板 (TodoBoard)

## 关于本项目

- **主包小学学历职业是杀猪师傅，完全不懂代码，本项目纯VIBE，主要目的是杀猪自动化，主包转行赛博包工头没看过一个字代码，不保证安全好用。**
- **README文件由DeepSeek生成，他满嘴黑话主包也没办法，管也管不动骂也骂不好，请大佬们找DeepSeek读然后解释给你们听。**
- **遇事请大佬们直接咨询DeepSeek，写issue应该没卵用，主包比较懒可能不太看😭😭😭。**

一个本地优先、**给人和 AI Agent 共用**的待办看板：Windows 桌面壳（Go + WebView2）+ 内嵌 HTTP 服务 + SQLite。
人和 Agent 读写的是同一份数据——你在界面上勾选完成，Agent 通过 API 或 MCP 看到的就是同一条；反之亦然。

## 功能

### 桌面版

- 深色界面，按「分区」组织任务；分区头可折叠、可拖动排序、可一键「⤓ 置于队尾」
- 任务状态三态：未完成 / 处理中 / 已完成（打勾控件：左键 = 完成，右键 = 处理中）
- 每个分区四种过滤：未完成 / 处理中 / 已完成 / 全部，选择会被记住
- 顶部概览：总数 / 未完成 / 处理中 / 已完成 / 已逾期，配 3/4 圆环完成率
- 任务详情：Markdown 渲染；编辑与新建都在独立窗口中完成
- 分区标题显示 `#<id>`（便于在 MCP / API 里指定分区）
- 分区管理：auth key 打码显示、查看、改名、改 key、删除分区（连同其任务，需二次确认）；每个分区独立的「完成推送」开关
- 托盘常驻：左键恢复窗口，右键菜单为「打开主界面 / 开机自启动 / 退出程序」；不显示任务栏图标
- 关闭按钮即退出程序；「—」最小化到托盘；单实例运行
- 窗口位置与尺寸自动记忆（含拔掉副屏后的屏外兜底），最小客户区 560×140
- 监听地址可配：默认仅本机，可加 IPv4 / IPv6 / 域名；要双栈就 `0.0.0.0` 与 `::` 两条都加

### 浏览器版

- 与桌面版同一服务、同一数据库，浏览器直接打开根路径即可使用（桌面壳内部走 `/app`）
- 多列瀑布流：`<768` 1 列 / `768–999` 2 列 / `1000–1199` 3 列 / `1200–1499` 4 列 / `≥1500` 5 列
- 任务可展开为卡片（Markdown 正文、截止、状态、操作按钮）
- 分区与任务都分页加载（默认 20 条/页），滚到底自动加载更多分区，栏内可「加载更多」任务
- 按设置的刷新间隔定时拉取（默认 5 秒，可在设置里改），任何来源的增删改在一个周期内可见

### 外部 API（给 AI 代理）

- Agent 可以完全通过 API 操作看板：列任务、新建、改标题/描述/截止、切换三态、删除，全程不需要人点界面
- 请求头 `X-Auth-Key` 认证，作用域严格限定在该 key 所属分区
- 支持任务查询 / 新建 / 修改 / 改状态 / 删除，截止时间可空
- 完整接口、字段与错误码见 [`skills/todo-board/SKILL.md`](skills/todo-board/SKILL.md)

### Bark 完成推送

- 任务被标记为「已完成」时推送到手机（Bark）
- 判定顺序：任务级「继承 / 开 / 关」优先，其次分区开关，最后受总开关约束
- 推送 5 秒超时，失败只记日志并在界面提示，**不影响任务状态变更**
- 不内置任何私有推送服务地址，需在设置页自行填写

### 分区排序

- `PUT /api/sections/order` 保存顺序，桌面与浏览器版都支持拖动排序
- 排序写入 `sections.sort_order` 并在增删分区后保持连续，变更通过 `/api/events` 广播（前端已改用定时轮询，该端点保留）

## 给 AI Agent 用的部分

看板从一开始就按「Agent 也要读写」来设计，目前提供三样东西，各管一段：

- **英文 Skill：`skills/todo-board/SKILL.md`** —— Agent 读写任务的入口文档，写明基址、`X-Auth-Key` 认证、
  各端点、状态取值（`open` / `doing` / `done`）、错误码与示例，Agent 照着调即可，不需要人教。
- **MCP：`mcp/todoboard-pager`（产物 `todoboard-pager.exe`）** —— 给 AionUI 这类 MCP 宿主的盯盘工具。
  启动时绑定「分区 + 该分区 KEY + 刷新间隔」，之后看板一有变化就在会话里发通知（新增任务逐条列标题，删除带
  `[删除]` 前缀）。共 4 个工具：`start_monitor` / `stop_monitor` / `list_tasks` / `set_status`；不写任何本地文件。
- **内置 HTTP API** —— `X-Auth-Key` 按分区隔离，Agent 可增删改查任务、改三态；还可以订阅 `/api/events`
  的 SSE 事件流做实时联动（前端默认已改用定时轮询，此端点供外部订阅）。分区之间的数据互不可见。

三者都不需要额外的服务端：桌面程序自己就是那个服务，Agent 连本机 `127.0.0.1:17321` 即可。
## 操作细节

不用记术语，照着做就行。

**窗口**

- 按住标题栏拖动可以移动窗口；鼠标移到窗口四边或四角 6 像素以内，光标会变成缩放箭头，按住拖动即可改大小。
- 窗口的位置和大小会自动记住，下次打开回到原处；屏幕分辨率变了也不会跑到看不见的地方。
- 窗口最小可以缩到客户区 560×140。
- 点「✕」是直接退出程序；点「—」是最小化到托盘（不退出）。托盘图标右键里有「开机自启动」。

**分区**

- 按住分区标题栏可以拖动排序；每个分区标题栏还有一个「⤓」把该分区一键挪到最后。
- 点分区标题栏可以折叠 / 展开该分区。
- 分区标题栏里分区名后面的 `#数字` 是这个分区的 ID（配 MCP 或调 API 时会用到）。
- 分区标题栏的「➕」就是新建任务（只有加号，没有文字）。

**任务**

- 打勾圆圈：**左键 = 标记完成，右键 = 标记处理中**；已经是处理中的再右键会回到未完成。已完成的行有「撤销」按钮。
- 点任务标题进入详情，描述按 Markdown 渲染；详情里可点「编辑描述」。
- 截止时间可以留空，不填就显示「未设截止」。
- 每条任务都可以删除（删除前会确认）。

**分区管理（标题栏的图层图标）**

- 可以新建分区、改分区名、改认证密钥（auth key）、删除分区。
- 删除分区会把该分区下的所有任务一起删掉，会先弹确认框并写明要删多少条任务。

**浏览器版**

- 浏览器打开 `http://127.0.0.1:17321/` 就能用（和桌面版同一份数据）。
- 分区按多列瀑布流排布：1280 宽四列、1600 宽五列，窄屏自动减列。
- 点任务行会展开成卡片；分区和任务都是滚到底自动加载更多；别人改了数据，页面会自动刷新。

**监听地址（设置页顶部那张卡片）**

- 只想本机用：勾上「仅本机（127.0.0.1）」，列表里就只剩 `127.0.0.1`。
- 想让手机/别的电脑访问：取消勾选，输入框填 `0.0.0.0`（只开 IPv4）或 `::`（只开 IPv6），点「➕ 添加」；**要双栈就两条都加**。填域名也行，会按解析结果绑。
- 点「💾 保存并生效」立刻生效，不用重启；每条地址后面会标「已生效」或「未生效」，失败原因会以提示条弹出（端口占用、域名解析失败等）。
- 全部地址都失败时会自动回滚到上一次能用的监听，界面不会失联。
- 出现黄色警示「已对局域网开放」表示当前有一条非环回地址——网页端没有鉴权，同网段的人都能读写，不用时把那条删掉。

**自动刷新间隔**

- 桌面「设置」或浏览器「设置」里都有「🔄 自动刷新间隔」，填 1–300 秒（默认 5），点保存即时生效，不用重启；两端读的是同一个服务端设置，改一处两边都变。
- **用户操作（新建 / 编辑 / 完成 / 删除等）完成后立即刷新**，轮询只用于接收其它来源（别的设备、Agent）的变化。
- **首次打开会立即加载一次**（不等第一个轮询周期）。

**完成推送（Bark）**

- 设置页填三项：服务器地址、设备 Key、图标 URL（可留空），填完可以先「发送测试推送」验证。
- 每个分区有一个「完成推送」开关（分区管理里），任务本身还可以单独选「继承分区 / 开 / 关」。

**MCP（给 AI 助手用）**

- 按上面「MCP」一节填好注册 JSON；启动时 Agent 需要向你要三样东西：分区（名称或 `#id`）、该分区的 KEY、刷新间隔（默认 60 秒，最小 1 秒）。
- 一共 4 个工具：开始盯盘、停止盯盘、列任务、改状态。
- 看板有变化会收到通知，格式是「`[通知] 看板有变化：共 N 条，新增 M 条`」，后面逐条列出新增任务标题；删除的任务会带「[删除]」前缀。
## 安装 / 运行

要求：Windows 10/11，且已安装 **Microsoft Edge WebView2 Runtime**（Win11 默认自带）。

直接双击 `TodoBoard.exe` 即可运行。命令行参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-port` | `17321` | 本地 HTTP 服务端口 |
| `-db` | exe 目录下 `<FQDN>.db` | 数据库文件路径 |
| `-headless` | `false` | 只启动 HTTP 服务、不创建桌面窗口（排障 / 自动化用），启动后打印 `READY pid=… url=…` |

## 构建

需要 Go 1.25+。

```bat
cd Code
build.bat
```

产物输出到 `Dev\build\TodoBoard.exe`（前端已 `go:embed` 进 exe，单文件发布）。
首次构建会自动调用 `rsrc` 生成 `rsrc_windows_amd64.syso`（应用图标 + manifest，含 DPI 感知），新机器无需手工准备资源。

## 配置

- **端口**：`17321`；默认只绑 `127.0.0.1`，可在设置页「🌐 监听地址」里添加多条地址（见下节）
- **数据库**：exe 同目录，文件名为本机 FQDN（未加域时用主机名）：`<FQDN>.db`
- **运行态目录**：`%LOCALAPPDATA%\AgentTodoBoard\`
  - `window.json` 窗口位置尺寸
  - `bark.json` 推送配置（服务器地址 / 设备 Key / 图标 URL / 总开关）
  - `listen.json` 监听地址列表（`{"binds":["127.0.0.1"]}`）
  - `settings.json` 应用设置：`poll_interval_sec` 自动刷新间隔（默认 5，范围 1–300）
  - `logs\app.log` 运行日志
  - `WebView2`、`WebView2Editor` 浏览器内核数据
- **Bark**：需自行填写服务器地址（如 `https://bark.example.com`）与设备 Key；图标 URL 可留空
- **开机自启动**：写 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`，可在设置页开关

## 监听地址

默认只绑 `127.0.0.1`（设置页「仅本机」默认打勾），也就是只有本机能访问。需要让局域网、IPv6 或某个域名访问时，在「设置 → 🌐 监听地址」里追加地址，保存即时生效、不用重启。

| 填什么 | 含义 |
| --- | --- |
| `127.0.0.1` | 仅本机（默认） |
| `0.0.0.0` | **只绑 IPv4**：本机所有 IPv4 网卡都可访问 |
| `::` | **只绑 IPv6**：本机所有 IPv6 网卡都可访问 |
| `::1` | 仅 IPv6 环回 |
| 域名（如 `host.example.com`） | 先解析，再绑到解析出的那个地址（v4 走 IPv4、v6 走 IPv6） |

- **要双栈（同一条端口同时服务 IPv4 与 IPv6）就把 `0.0.0.0` 与 `::` 两条都加上**——它们是两个独立监听，互不抢占。
- 某一条绑失败（端口被占用、域名解析不出来、地址不可用）只会把**这一条**报出来，其余照常生效；**全部失败**会自动回滚到改前的监听集合，不会把人锁在外面。
- 配置存在 `%LOCALAPPDATA%\AgentTodoBoard\listen.json`，形如 `{"binds":["127.0.0.1"]}`；文件损坏会备份成 `.bad` 并回退到仅本机。
- **安全提醒**：一旦绑了非环回地址，网页端（`/`、`/app`）没有鉴权，同网段任何人都能读写；`X-Auth-Key` 只保护 `/api/tasks` 那套给 Agent 用的接口。设置页会在这种情况下显示黄色警示。

## MCP：todoboard-pager（AionUI 常驻盯盘）

`Dev\build\todoboard-pager.exe`（源码 `mcp/todoboard-pager/`）是一个 stdio MCP 服务，用于在 AionUI 会话里
常驻盯一个分区：看板有变化就把新增任务标题推给当前会话（团队与单聊都适用）。

注册示例（AionUI 或任意 MCP 客户端）：

```json
{
  "mcpServers": {
    "todoboard-pager": {
      "command": "<项目目录>\\Dev\\build\\todoboard-pager.exe"
    }
  }
}
```

四个环境变量 `AIONUI_BASE_URL` / `AIONUI_RUNTIME_TOKEN` / `AIONUI_USER_ID` / `AIONUI_CONVERSATION_ID`
由 AionUI 的 agent 宿主注入，不必手填；缺失时 `initialize` / `tools/list` 仍可正常应答，只有「盯盘 / 发消息」
会返回工具错误，进程不会退出。

启动必需三项：**分区（名称或 `#<id>`）、该分区的 KEY、刷新间隔秒（默认 60，最小 1）**。

| 工具 | 参数 | 说明 |
| --- | --- | --- |
| `start_monitor` | `{section, key, interval_sec}` | 开始盯盘（非阻塞） |
| `stop_monitor` | `{}` | 停止盯盘 |
| `list_tasks` | `{status?}` | `open` / `doing` / `done` / `all` |
| `set_status` | `{task_id, status}` | `open` / `doing` / `done` |

- `start_monitor` 的参数相同时是**幂等**的：重复调用不会重建基线、也不会丢掉期间发生的变化；只有换分区或换间隔才会重新开始盯。
- `section` 可写分区名、数字 ID 或 `#数字`；盯盘按分区 ID 追踪，分区改名后仍能继续收到通知。

通知形如 `[通知] 看板有变化：共 N 条，新增 M 条`，随后逐条列出全部新增任务标题；被删除的任务带 `- [删除] 标题` 前缀，
只改描述/截止等也算变化。

**不写任何本地文件**（无快照、无状态文件），进程或团队重建后由 Agent 重新 `start_monitor` 即可。
日志默认关闭，只有设置 `TODOBOARD_DEBUGE`（文件或目录路径）时才写。

配套文档：外部 API 说明 `skills/todo-board/SKILL.md`（含 MCP 一节）、专用 Agent 系统提示词
`agents/todo-board-watcher.md`。

## API 概览

#### 代理接口（需 X-Auth-Key）

| 路径 | 说明 |
| --- | --- |
| `GET /api/section` | 当前 key 所属分区及其计数 |
| `GET /api/tasks` | 列任务，支持 `status` / `done` / `section` 过滤 |
| `POST /api/tasks` | 新建任务（一律为未完成） |
| `PUT /api/tasks/{id}` | 修改标题 / 描述 / 截止 / 推送覆盖 |
| `POST /api/tasks/{id}/status` | 改状态：`open` / `doing` / `done` |
| `POST /api/tasks/{id}/done` | 完成或撤销（`{"done":true/false}`） |
| `DELETE /api/tasks/{id}` | 删除任务 |

#### 桌面与网页

| 路径 | 说明 |
| --- | --- |
| `GET /api/health` | 健康检查 |
| `GET /api/board` | 看板全量数据（分区 + 任务 + 计数） |
| `GET /api/sections` | 分区列表（含每个分区的计数） |
| `POST /api/sections` | 新建分区（可带 key，留空随机） |
| `PUT /api/sections/{id}` | 改名 / 开关该分区的完成推送 |
| `DELETE /api/sections/{id}` | 删除分区（连同其任务，返回删除条数） |
| `PUT /api/sections/order` | 保存分区排序 |
| `GET /api/sections/{id}/key` | 查看该分区明文 key |
| `PUT /api/sections/{id}/key` | 修改该分区 key |
| `POST /api/board/sections/{sid}/tasks` | 新建任务 |
| `PUT /api/board/sections/{sid}/tasks/{tid}` | 修改任务 |
| `DELETE /api/board/sections/{sid}/tasks/{tid}` | 删除任务 |
| `POST /api/board/sections/{sid}/tasks/{tid}/status` | 改状态 |
| `POST /api/board/sections/{sid}/tasks/{tid}/done` | 完成 / 撤销 |
| `GET /api/web/sections` | 浏览器版分区分页（`limit` / `offset`） |
| `GET /api/web/tasks` | 浏览器版任务分页（`section` / `status` / `limit` / `offset`） |
| `GET /api/web/stats` | 浏览器版顶部概览计数 |
| `POST /api/window/hide` | 最小化到托盘 |
| `POST /api/window/close` | 退出程序 |
| `POST /api/window/drag` | 拖动窗口 |
| `POST /api/window/resize/start` | 开始缩放（`edge` / `target`） |
| `POST /api/window/resize/end` | 结束缩放 |
| `GET /api/editor/context` | 独立编辑窗口读取任务 |
| `POST /api/editor/open` | 打开编辑窗口 |
| `POST /api/editor/ready` | 编辑窗口就绪（显示窗口并设标题） |

#### 设置与推送

| 路径 | 说明 |
| --- | --- |
| `GET /api/settings` | 版本 / 接口地址 / 数据文件 / 自启状态 / `bind` / `listening` / `port` / `lan_open` / `default_bind` / `poll_interval_sec` |
| `PUT /api/settings/bind` | 保存监听地址：body `{"binds":[…]}`，返回 `ok` / `bind` / `listening` / `failures` / `error` |
| `PUT /api/settings/poll` | 保存自动刷新间隔：body `{"interval_sec":N}`（1–300），返回 `ok` / `interval_sec` |
| `POST /api/settings/autostart` | 开关开机自启动 |
| `GET /api/bark` | 推送配置（key 只返回掩码） |
| `PUT /api/bark` | 保存推送配置 |
| `POST /api/bark/test` | 发送测试推送 |

#### 实时

| 路径 | 说明 |
| --- | --- |
| `GET /api/events` | SSE 事件流（前端已不订阅，保留供外部用） |

详细字段、状态取值与错误码见 `skills/todo-board/SKILL.md`。

## 目录结构

本仓库根目录即 `Code\`（下列路径均相对于仓库根）。

```
Code/
  main.go            桌面壳：窗口、托盘、单实例、开机自启动、窗口状态
  server.go          HTTP 路由、SSE、鉴权、分页
  store.go           SQLite 数据层（分区 / 任务 / 状态 / 排序）
  bark.go            Bark 推送
  app.manifest       DPI / 权限清单
  rsrc_windows_amd64.syso  图标 + manifest 资源（build.bat 缺失时自动生成）
  frontend/
    index.html ui.css app.js     桌面版界面
    editor.html editor.js        独立编辑窗口
    web.html web.css web.js      浏览器版
    icon.png icon.ico            图标
  mcp/todoboard-pager/          AionUI 常驻盯盘 MCP（stdio JSON-RPC）
  agents/todo-board-watcher.md  盯盘专用 Agent 系统提示词
  skills/todo-board/SKILL.md    外部 API 说明（给 AI 代理）+ MCP 说明
  build.bat          构建脚本
Dev/
  design/  build/    设计稿与构建产物（不随源码发布）
```

## 已知限制

- **编辑窗口会启动第二个 WebView2 实例**（独立 profile），首次打开约 1 秒延迟并额外占用内存；之后复用同一窗口
- 窗口为无边框工具窗口，**不进入任务栏与 Alt+Tab**（有意设计，靠托盘常驻）
- 最小客户区 **560×140**（概览五个计数框 + 圆环 + 标题不重叠所需）
- 浏览器版列宽约 300px 是「铺满优先」的结果，不是 320px 硬下限
- 单实例：二次启动只会激活已有窗口
- 暂未提供导入 / 导出 / 备份功能
- 推送（Bark）为单向通知，不接收回执

## 许可

本项目以 MIT License 发布，见 `LICENSE`。
