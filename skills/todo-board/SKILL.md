---
name: todo-board
description: Read and write tasks in the local TodoBoard desktop app (待办看板) through its local HTTP API. Use when the user asks to add / list / update / complete / delete a todo item, or mentions "待办看板", "todo board", "task board".
---

# TodoBoard HTTP API

TodoBoard is a local desktop app that exposes a small JSON API on the loopback
address. Every request is scoped to **one section** by its **auth key**: an agent
holding a key can only see and modify tasks in that section.

Base URL: `http://127.0.0.1:17321` (the port is configurable with `-port`).

By default the service binds only the loopback address. If extra listen addresses are added in the
desktop settings (e.g. `0.0.0.0` for IPv4, `::` for IPv6, or a hostname), the same API is also
reachable on those addresses; authentication is still per-section via `X-Auth-Key`.

## Authentication

Send the section's key in the header on every request:

```
X-Auth-Key: <section auth key>
```

A section may also be addressed by name with `"section": "<name>"` in a body.
That only works for sections reachable from the key you sent.

## Data shapes

Task:

```json
{
  "id": 12,
  "section_id": 1,
  "title": "整理巡检脚本",
  "description": "Markdown text",
  "due_at": "2026-10-21T18:00",
  "status": "open",
  "done": false,
  "bark_override": "inherit",
  "created_at": "2026-10-20T09:12:31+08:00",
  "updated_at": "2026-10-20T09:12:31+08:00"
}
```

- `due_at` is `YYYY-MM-DDTHH:mm` in local time, or `null` when unset.
  On input, `2026-10-21` (date only) is stored as `2026-10-21T23:59`.
  Accepted input forms: RFC3339, `YYYY-MM-DDTHH:mm[:ss]`, `YYYY-MM-DD HH:mm[:ss]`,
  `YYYY/MM/DD HH:mm`, `YYYY-MM-DD`. Send `null` to clear it.
- `description` is Markdown (headings, lists, fenced code, inline code, bold, links).
- `status` is `open` (unfinished), `doing` (in progress) or `done` (finished).
- `bark_override` is `inherit` (default), `on` or `off`; it overrides the section's
  Bark switch when a task is completed.
- `done` is a read-only convenience boolean derived from `status` (`status == "done"`).
  On input it is only accepted by the legacy `/done` endpoint.

Section:

```json
{ "id": 1, "name": "工作", "key_masked": "••••••••••fcb7", "sort_order": 1 }
```

## Endpoints

### GET /api/section

Returns the section that owns the key, plus its counts.

```json
{
  "section": { "id": 1, "name": "工作", "key_masked": "••••••••••fcb7", "sort_order": 1 },
  "counts": { "total": 4, "open": 3, "done": 1, "overdue": 1 }
}
```

### GET /api/tasks

Query parameters (all optional):

| Param | Values | Meaning |
| --- | --- | --- |
| `status` | `open` / `doing` / `done` / `all` | filter by status (preferred) |
| `done` | `true` / `false` / … | legacy filter: `true` = done only, `false` = everything not done (open + doing) |
| `section` | section name or id | narrow to a section (must be reachable from the key) |

Response: `{ "section": {...}, "tasks": [...], "count": 3 }`

Ordering: unfinished first, then unset due dates, then due date ascending, then id.

### POST /api/tasks

```json
{ "title": "买牛奶", "description": "可选", "due": "2026-10-22T09:00", "section": "工作" }
```

`title` is required. `section` is optional when the key already identifies one
section. Returns `201` with `{ "task": {...} }`.

New tasks are always created as **unfinished**: any `done` field in the create
body is ignored. Use `POST /api/tasks/{id}/done` to complete a task.

### PUT /api/tasks/{id}

Partial update; omit a field to leave it unchanged, send `"due": null` to clear it.

```json
{ "title": "新标题", "description": "新描述", "due": "2026-10-23" }
```

### POST /api/tasks/{id}/done

```json
{ "done": true }
```

Omit `done` to toggle the current state.

### POST /api/tasks/{id}/status

```json
{ "status": "doing" }
```

`status` must be `open`, `doing` or `done` (anything else returns `400`).
`POST /api/tasks/{id}/done` still works as a shortcut for `{"status":"done"}`.

### DELETE /api/tasks/{id}

```json
{ "ok": true }
```

## Errors

Errors are `{"error": "<message>"}` with a Chinese message:

| Status | Meaning |
| --- | --- |
| 400 | invalid body, empty title, bad due format, invalid `status`, or unknown section name |
| 401 | missing or invalid `X-Auth-Key` |
| 403 | the task belongs to a different section than the key |
| 404 | task not found |
| 500 | internal error (details are in the app log) |

## Notes

- All responses are UTF-8 JSON; send `Content-Type: application/json`.
- Sections can be deleted, but only through the web-side endpoint `DELETE /api/sections/{id}`
  (it is not part of the `X-Auth-Key` agent API). Deleting a section also deletes every task in it
  and returns how many were removed; deleting the same id twice returns `404`.
- There is still no server-side search: fetch `/api/tasks` and filter locally.
- New tasks are always `open`; `status` / `done` are ignored on create.
- The built-in UI subscribes to the `/api/events` SSE stream, so changes show up within about a second. If SSE is unavailable it falls back to polling every `poll_interval_sec` (default 5 s, 1-300, configurable in settings). Agents may subscribe to the same stream: events look like `data: {"type":"<kind>"}`, a `: ping` comment arrives every 25 s, and the server sends `retry: 2000`.

## MCP vs. the in-app watcher

The bundled `todoboard-pager` MCP tool no longer delivers the notifications itself when a
current build of the board is running: it registers the watch through `POST /api/monitors`,
reports `PUT /api/monitors/heartbeat` on every poll, and unregisters with
`DELETE /api/monitors/{id}/self` (section key required) on `stop_monitor`. Listing and deleting
watches from the built-in UI is a separate management surface that takes no key. Delivery (AIONUI HELPER subprocess) is owned by
the board process, so the same change is never announced twice. If a heartbeat returns **404**
the watch was cleared from another client: the pager stops polling immediately and never
re-registers on its own. Restart the monitor from the agent side if you still need it.

## MCP: todoboard-pager

A companion MCP server (`Dev\build\todoboard-pager.exe`, source in `mcp/todoboard-pager/`) that keeps
watching one section inside an AionUI session and pushes a notice to the current conversation whenever
the board changes. Works both in a team member slot and in a one-to-one conversation.

**Required before starting** (ask the user, never guess):

1. the section (name or numeric id — the `#<id>` shown next to the section title),
2. that section's auth key,
3. the poll interval in seconds (60 recommended, minimum 1).

The section may be given by name, numeric id, or `#<id>`; monitoring tracks the **resolved section id**,
so it keeps notifying after the section is renamed.

**Tools (4):**

| Tool | Arguments | Notes |
| --- | --- | --- |
| `start_monitor` | `{section, key, interval_sec}` | non-blocking; sends `[通知]` on changes |
| `stop_monitor` | `{}` | stops watching |
| `list_tasks` | `{status?}` | `status` = `open` / `doing` / `done` / `all` |
| `set_status` | `{task_id, status}` | `status` = `open` / `doing` / `done` |

**Notification format** — the headline, then every newly added task title, one per line, never truncated:

```
[通知] 看板有变化：共 N 条，新增 M 条

- 任务标题 1
- 任务标题 2
```

If something changed but **nothing was newly added**, the notice carries an extra section instead:
`另有 N 条变更：` followed by the changed task titles, one per line.

The MCP writes **no local files** (no snapshots, no state file); after a restart or a team rebuild the
agent simply calls `start_monitor` again.

**MCP vs HTTP API**: use the HTTP API above for one-off reads/writes; use the MCP only when you need to
keep watching a board and push changes into the conversation.

**Runtime prerequisites**: AionUI injects `AIONUI_BASE_URL`, `AIONUI_RUNTIME_TOKEN`, `AIONUI_USER_ID`,
`AIONUI_CONVERSATION_ID`. The server does **not** validate them at startup: `initialize` and `tools/list`
work without them. Only actions that actually send a message (`start_monitor`, lease renewal) return a tool
error naming the missing variables, and the process keeps running. Logging is off by default — it only
writes a log when `TODOBOARD_DEBUGE` points to a file or directory.