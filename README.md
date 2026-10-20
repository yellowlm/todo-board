# 待办看板 (TodoBoard)

## 关于本项目

- **主包小学学历职业是杀猪师傅，完全不懂代码，本项目纯VIBE，主要目的是杀猪自动化，主包转行赛博包工头没看过一个字代码，不保证安全好用。**
- **README文件由DeepSeek生成，他满嘴黑话主包也没办法，管也管不动骂也骂不好，请大佬们找DeepSeek读然后解释给你们听。**
- **遇事请大佬们直接咨询DeepSeek，写issue应该没卵用，主包比较懒可能不太看😭😭😭。**

一个本地优先的待办工具：Windows 桌面壳（Go + WebView2）+ 内嵌 HTTP 服务 + SQLite。
既可作为常驻托盘的桌面小工具使用，也向 AI 代理提供一套可编程的任务接口。

## 功能

### 桌面版

- 深色界面，按「分区」组织任务；分区头可折叠、可拖动排序、可一键「⤓ 置于队尾」
- 任务状态三态：未完成 / 处理中 / 已完成（打勾控件：左键 = 完成，右键 = 处理中）
- 每个分区四种过滤：未完成 / 处理中 / 已完成 / 全部，选择会被记住
- 顶部概览：总数 / 未完成 / 处理中 / 已完成 / 已逾期，配 3/4 圆环完成率
- 任务详情：Markdown 渲染；编辑与新建都在独立窗口中完成
- 分区管理：auth key 打码显示、查看、改名、改 key；每个分区独立的「完成推送」开关
- 托盘常驻：左键恢复窗口，右键菜单为「打开主界面 / 开机自启动 / 退出程序」；不显示任务栏图标
- 关闭按钮即退出程序；「—」最小化到托盘；单实例运行
- 窗口位置与尺寸自动记忆（含拔掉副屏后的屏外兜底），最小客户区 560×140

### 浏览器版

- 与桌面版同一服务、同一数据库，浏览器直接打开根路径即可使用（桌面壳内部走 `/app`）
- 多列瀑布流：`<768` 1 列 / `768–999` 2 列 / `1000–1199` 3 列 / `1200–1499` 4 列 / `≥1500` 5 列
- 任务可展开为卡片（Markdown 正文、截止、状态、操作按钮）
- 分区与任务都分页加载（默认 20 条/页），滚到底自动加载更多分区，栏内可「加载更多」任务
- 通过 SSE 实时刷新，任何来源的增删改 1 秒内可见

### 外部 API（给 AI 代理）

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
- 排序写入 `sections.sort_order` 并在增删分区后保持连续，变更通过 SSE 广播

## 安装 / 运行

要求：Windows 10/11，且已安装 **Microsoft Edge WebView2 Runtime**（Win11 默认自带）。

直接双击 `TodoBoard.exe` 即可运行。命令行参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-port` | `17321` | 本地 HTTP 服务端口（只监听 `127.0.0.1`） |
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

- **端口**：`17321`，仅绑定 `127.0.0.1`
- **数据库**：exe 同目录，文件名为本机 FQDN（未加域时用主机名）：`<FQDN>.db`
- **运行态目录**：`%LOCALAPPDATA%\AgentTodoBoard\`
  - `window.json` 窗口位置尺寸
  - `bark.json` 推送配置（服务器地址 / 设备 Key / 图标 URL / 总开关）
  - `logs\app.log` 运行日志
  - `WebView2`、`WebView2Editor` 浏览器内核数据
- **Bark**：需自行填写服务器地址（如 `https://bark.example.com`）与设备 Key；图标 URL 可留空
- **开机自启动**：写 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`，可在设置页开关

## API 概览

| 分组 | 路径 | 说明 |
| --- | --- | --- |
| 代理接口 | `/api/tasks`、`/api/tasks/{id}`、`/api/tasks/{id}/status`、`/api/tasks/{id}/done`、`/api/section` | 需 `X-Auth-Key`，仅作用本分区 |
| 桌面 / 网页 | `/api/board`、`/api/sections`、`/api/sections/order`、`/api/web/sections`、`/api/web/tasks`、`/api/web/stats` | 本机使用，无鉴权 |
| 设置 | `/api/settings`、`/api/bark`、`/api/bark/test` | 设置页与推送配置 |
| 实时 | `/api/events` | SSE 事件流 |

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
  skills/todo-board/SKILL.md     外部 API 说明（给 AI 代理）
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
