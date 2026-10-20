'use strict';

const $ = (sel) => document.querySelector(sel);

// 分区色板（8 色，桌面与浏览器版同一套、同一顺序，按 sort_order 循环）
const SEC_COLORS = [
  { hex: '#3B82F6', rgb: '59,130,246' },
  { hex: '#A78BFA', rgb: '167,139,250' },
  { hex: '#22D3EE', rgb: '34,211,238' },
  { hex: '#34D399', rgb: '52,211,153' },
  { hex: '#FBBF24', rgb: '251,191,36' },
  { hex: '#F472B6', rgb: '244,114,182' },
  { hex: '#FB923C', rgb: '251,146,60' },
  { hex: '#F87171', rgb: '248,113,113' },
];

function secStyle(index) {
  const c = SEC_COLORS[index % SEC_COLORS.length];
  return `--sec-color:${c.hex};--sec-soft:rgba(${c.rgb},0.07);--sec-hover:rgba(${c.rgb},0.13);--sec-active:rgba(${c.rgb},0.2)`;
}
const MODE_LABEL = { open: '未完成', doing: '处理中', done: '已完成', all: '全部' };
const MODE_NEXT = { open: 'doing', doing: 'done', done: 'all', all: 'open' };
const STATUS_TAG = { doing: '🔧 处理中', done: '' };
const VIEWS = {
  board: '#boardView',
  detail: '#detailView',
  sections: '#sectionsView',
  settings: '#settingsView',
};

const state = {
  view: 'board',
  board: null,
  sections: [],
  settings: null,
  detailId: null,
  modes: {},
  collapsed: {},
  revealed: {},
  editing: null,
  bindDraft: null,
  modal: null,
  lastTaskId: null,
};

/* ---------------- 基础工具 ---------------- */

function isLoopbackBind(host) {
  const h = String(host || '').trim().toLowerCase().replace(/^\[(.*)\]$/, '$1');
  if (h === 'localhost' || h === '::1') return true;
  return /^127\./.test(h);
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  }[c]));
}

const pad2 = (n) => String(n).padStart(2, '0');

function nowStr() {
  const d = new Date();
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

function todayStr() {
  const d = new Date();
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
}

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  let data = null;
  try {
    data = await res.json();
  } catch (e) {
    data = null;
  }
  if (!res.ok) {
    throw new Error((data && data.error) || `请求失败 (${res.status})`);
  }
  return data;
}

let toastTimer = null;
function toast(msg, isErr) {
  const el = $('#toast');
  el.textContent = msg;
  el.classList.toggle('err', !!isErr);
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    el.hidden = true;
  }, 3000);
}

/* ---------------- 偏好 ---------------- */

function getMode(sid) {
  if (!state.modes[sid]) {
    state.modes[sid] = localStorage.getItem('todo.mode.' + sid) || 'open';
  }
  return state.modes[sid];
}

function setMode(sid, mode) {
  state.modes[sid] = mode;
  localStorage.setItem('todo.mode.' + sid, mode);
}

function isCollapsed(sid) {
  if (state.collapsed[sid] === undefined) {
    state.collapsed[sid] = localStorage.getItem('todo.collapsed.' + sid) === '1';
  }
  return state.collapsed[sid];
}

/* ---------------- 数据查询 ---------------- */

function findTask(id) {
  if (!state.board) return null;
  for (const section of state.board.sections || []) {
    for (const task of section.tasks || []) {
      if (task.id === id) return { task, section };
    }
  }
  return null;
}

/* ---------------- 渲染入口 ---------------- */

function render() {
  for (const key in VIEWS) {
    $(VIEWS[key]).hidden = key !== state.view;
  }
  $('#btnSections').classList.toggle('on', state.view === 'sections');
  $('#btnSettings').classList.toggle('on', state.view === 'settings');

  if (state.view === 'board') renderBoard();
  else if (state.view === 'detail') renderDetail();
  else if (state.view === 'sections') renderSections();
  else if (state.view === 'settings') renderSettings();
}

function showView(name) {
  if (name === 'detail' && !findTask(state.detailId)) name = 'board';
  state.view = name;
  render();
}

async function openSections() {
  try {
    const res = await api('GET', '/api/sections');
    state.sections = res.sections || [];
  } catch (e) {
    toast(e.message, true);
  }
  showView('sections');
}

async function openSettings() {
  try {
    state.settings = await api('GET', '/api/settings');
    state.bark = await api('GET', '/api/bark');
    state.bindDraft = (state.settings.bind || ['127.0.0.1']).slice();
  } catch (e) {
    toast(e.message, true);
  }
  showView('settings');
}

async function refresh() {
  if (state.editing) return;
  try {
    state.board = await api('GET', '/api/board');
    if (state.view === 'sections') {
      const res = await api('GET', '/api/sections');
      state.sections = res.sections || [];
    } else if (state.view === 'settings') {
      state.settings = await api('GET', '/api/settings');
      state.bark = await api('GET', '/api/bark');
    }
  } catch (e) {
    return;
  }
  render();
}

/* ---------------- 看板 ---------------- */

function ringPath(frac) {
  if (!(frac > 0)) return '';
  const f = Math.min(1, frac);
  const start = (135 * Math.PI) / 180;
  const end = start + 2 * Math.PI * 0.75 * f;
  const cx = 23;
  const cy = 23;
  const r = 19.5;
  const x0 = cx + r * Math.cos(start);
  const y0 = cy + r * Math.sin(start);
  const x1 = cx + r * Math.cos(end);
  const y1 = cy + r * Math.sin(end);
  const large = 270 * f > 180 ? 1 : 0;
  return `M${x0.toFixed(2)} ${y0.toFixed(2)} A${r} ${r} 0 ${large} 1 ${x1.toFixed(2)} ${y1.toFixed(2)}`;
}

function overviewHtml(board) {
  const t = board.totals || { total: 0, open: 0, doing: 0, done: 0, overdue: 0 };
  const frac = t.total > 0 ? t.done / t.total : 0;
  const pct = Math.round(frac * 100);
  return `<div class="overview">
    <div class="ov-head">
      <div class="ov-title">📊 今日概览</div>
      <div class="ov-sub">${esc(board.date || todayStr())}</div>
    </div>
    <div class="ov-group">
      <div class="ov-stats">
        <div class="stat"><span class="n">${t.total}</span><span class="l"><span class="em">📋</span>总数</span></div>
        <div class="stat"><span class="n">${t.open}</span><span class="l"><span class="em">⏳</span>未完成</span></div>
        <div class="stat doing"><span class="n">${t.doing || 0}</span><span class="l"><span class="em">🔧</span>处理中</span></div>
        <div class="stat done"><span class="n">${t.done}</span><span class="l"><span class="em">✅</span>已完成</span></div>
        <div class="stat late"><span class="n">${t.overdue}</span><span class="l"><span class="em">⚠️</span>已逾期</span></div>
      </div>
      <div class="ov-progress">
        <div class="ring">
          <svg class="ring-svg" viewBox="0 0 46 46">
            <path class="ring-track" d="M9.21 36.79 A19.5 19.5 0 1 1 36.79 36.79"/>
            <path class="ring-fill" d="${ringPath(frac)}"/>
          </svg>
          <span class="ring-num">${pct}%</span>
        </div>
      </div>
    </div>
  </div>`;
}

function metaFor(task, now) {
  let cls = '';
  let tag = '';
  let date = task.due_at ? task.due_at.replace('T', ' ') : '未设截止';

  if (task.status === 'doing') {
    cls = 'doing';
    tag = STATUS_TAG.doing;
  }

  if (!task.due_at) {
    cls = cls || 'no-due';
  } else if (task.status !== 'done') {
    if (task.due_at < now) {
      cls = 'overdue';
      tag = '⚠️ 已逾期';
    } else if (task.due_at.slice(0, 10) === now.slice(0, 10)) {
      if (!tag) {
        cls = 'due-today';
        tag = '🔔 今日到期';
      }
    } else {
      const diff = new Date(task.due_at.replace(' ', 'T')).getTime() - Date.now();
      if (diff <= 3 * 86400000 && !tag) {
        cls = 'due-soon';
        tag = '⏰ 临近到期';
      }
    }
  }
  return { cls, tag, date };
}

function barkBadge(task) {
  if (task.bark_override === 'on') return ' <span class="bark-badge" title="强制推送">🔔</span>';
  if (task.bark_override === 'off') return ' <span class="bark-badge" title="不推送">🔕</span>';
  return '';
}

function taskHtml(task, now) {
  const m = metaFor(task, now);
  const selected = state.lastTaskId === task.id ? ' selected' : '';
  const isDone = task.status === 'done';
  const check = isDone ? '<svg class="i sm"><use href="#i-check"/></svg>' : '';
  const toggle = isDone
    ? '<button class="rowbtn undo" data-act="undone">↩️ 撤销</button>'
    : '<button class="rowbtn ok" data-act="done">✅ 完成</button>';
  const cls = [isDone ? 'done' : '', task.status || 'open'].join(' ').trim();
  return `<div class="task ${cls}${selected}" data-id="${task.id}">
    <div class="check ${task.status || 'open'}" title="左键完成 / 右键处理中">${check}</div>
    <div class="task-main">
      <div class="task-line">
        <span class="task-title">${esc(task.title)}${barkBadge(task)}</span>
      </div>
    </div>
    <div class="actions">
      <span class="task-meta ${m.cls}">${m.tag ? `<span class="tag">${m.tag}</span>` : ''}<span class="mdate">${esc(m.date)}</span></span>
      <div class="task-btns">
        ${toggle}
        <button class="rowbtn" data-act="edit">✏️ 编辑</button>
        <button class="rowbtn danger" data-act="del">🗑️ 删除</button>
      </div>
    </div>
  </div>`;
}

function emptyBodyHtml(mode) {
  let title = '暂无任务';
  let sub = '该分区下暂无任务';
  if (mode === 'open') {
    title = '该分区暂无未完成的任务';
    sub = '未完成的任务会显示在这里';
  } else if (mode === 'doing') {
    title = '该分区暂无处理中的任务';
    sub = '标记为处理中的任务会显示在这里';
  } else if (mode === 'done') {
    title = '该分区暂无已完成的任务';
    sub = '完成的任务会显示在这里';
  }
  return `<div class="empty">
    <div class="ico">🗂️</div>
    <div class="etitle">${title}</div>
    <div class="esub">${sub}</div>
  </div>`;
}

function sectionHtml(section, index, now) {
  const mode = getMode(section.id);
  const collapsed = isCollapsed(section.id);
  const tasks = section.tasks || [];
  const total = tasks.length;
  const done = tasks.filter((t) => t.status === 'done').length;
  const pct = total > 0 ? Math.round((done / total) * 100) : 0;
  const shown = tasks.filter((t) => (mode === 'all' ? true : t.status === mode));

  const body = shown.length ? shown.map((t) => taskHtml(t, now)).join('') : emptyBodyHtml(mode);

  return `<div class="section${collapsed ? ' collapsed' : ''}" style="${secStyle(index)}" data-sid="${section.id}">
    <div class="section-head" draggable="true">
      <svg class="i sm chev"><use href="#i-chev"/></svg>
      <span class="section-name">${esc(section.name)}</span><span class="section-id">#${section.id}</span>
      <span class="section-count">${shown.length}</span>
      <div class="secprog"><div class="sp-bar"><div class="sp-fill" style="width:${pct}%"></div></div></div>
      <div class="grow"></div>
      <button class="newbtn" data-act="tail" title="置于队尾" aria-label="置于队尾">⤓</button>
      <button class="newbtn" data-act="new-task" title="新建任务" aria-label="新建任务">➕</button>
      <button class="modebtn" data-act="mode"><svg class="i sm"><use href="#i-cycle"/></svg>${MODE_LABEL[mode]}</button>
    </div>
    <div class="section-body">${body}</div>
  </div>`;
}

function renderBoard() {
  const view = $('#boardView');
  const scroll = view.scrollTop;
  const board = state.board;

  if (!board) {
    view.innerHTML = '';
    return;
  }

  const now = nowStr();
  let html = overviewHtml(board);
  const sections = board.sections || [];

  if (!sections.length) {
    html += `<div class="section"><div class="empty">
      <div class="ico">🗂️</div>
      <div class="etitle">还没有任何分区</div>
      <div class="esub">创建分区后即可在其中添加任务</div>
      <button class="btn btn-primary" data-act="new-section">➕ 新建分区</button>
    </div></div>`;
  } else {
    sections.forEach((s, i) => {
      html += sectionHtml(s, i, now);
    });
  }

  view.innerHTML = html;
  view.scrollTop = scroll;
}

/* ---------------- 任务详情 ---------------- */

function renderMarkdown(src) {
  const text = String(src || '').replace(/\r\n/g, '\n').trim();
  if (!text) return '<div class="md-empty">暂无描述</div>';

  const blocks = [];
  const withPlaceholders = text.replace(/```[^\n]*\n([\s\S]*?)```/g, (m, code) => {
    blocks.push(code);
    return `\u0000${blocks.length - 1}\u0000`;
  });

  const lines = esc(withPlaceholders).split('\n');
  const out = [];
  let items = null;

  const flush = () => {
    if (items) {
      out.push(`<ul>${items.join('')}</ul>`);
      items = null;
    }
  };

  for (const raw of lines) {
    const line = raw.trim();
    if (!line) {
      flush();
      continue;
    }
    const ph = line.match(/^\u0000(\d+)\u0000$/);
    if (ph) {
      flush();
      out.push(`<pre><code>${esc(blocks[Number(ph[1])])}</code></pre>`);
      continue;
    }
    const heading = line.match(/^#{1,6}\s+(.*)$/);
    if (heading) {
      flush();
      out.push(`<h3>${inline(heading[1])}</h3>`);
      continue;
    }
    const bullet = line.match(/^[-*]\s+(.*)$/);
    if (bullet) {
      if (!items) items = [];
      items.push(`<li>${inline(bullet[1])}</li>`);
      continue;
    }
    flush();
    out.push(`<p>${inline(line)}</p>`);
  }
  flush();
  return out.join('');
}

function inline(s) {
  return s
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/\[([^\]]+)\]\(((?:https?:|mailto:)[^)\s]*)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
}

function renderDetail() {
  const found = findTask(state.detailId);
  if (!found) {
    state.view = 'board';
    state.detailId = null;
    render();
    return;
  }
  const { task, section } = found;
  $('#detailTitle').textContent = task.title;
  const due = task.due_at ? task.due_at.replace('T', ' ') : '未设截止';
  $('#detailSub').textContent = `${section.name} · ${due} · ${task.done ? '已完成' : '未完成'}`;

  const body = $('#detailBody');
  const scroll = body.scrollTop;
  body.innerHTML = renderMarkdown(task.description);
  body.scrollTop = scroll;

  $('#detailToggle').textContent = task.done ? '↩️ 撤销完成' : '✅ 标记完成';
}

/* ---------------- 分区管理 ---------------- */

function secRow(section) {
  const revealed = state.revealed[section.id];
  const key = revealed ? esc(revealed) : esc(section.key_masked || '');
  return `<div class="secrow secrow-card" data-sid="${section.id}">
    <div class="secrow-head">
      <span class="secrow-name">${esc(section.name)}</span>
      <span class="secrow-key">认证密钥：<span class="key">${key}</span></span>
    </div>
    <div class="actions">
      <div class="inline-toggle"><span class="mini-label">完成推送</span>
        <button class="switch${section.bark_enabled ? ' on' : ''}" data-act="bark-toggle" role="switch" aria-checked="${section.bark_enabled}"></button>
      </div>
      <button class="rowbtn" data-act="tail" title="置于队尾">⤓ 置尾</button>
      <button class="rowbtn" data-act="reveal">${revealed ? '🙈 隐藏密钥' : '👁️ 查看密钥'}</button>
      <button class="rowbtn" data-act="rename">✏️ 修改名称</button>
      <button class="rowbtn" data-act="rekey">🔑 修改密钥</button>
      <button class="rowbtn danger" data-act="del-sec">🗑️ 删除分区</button>
    </div>
  </div>`;
}

function secRowEditing(section, editing) {
  if (editing.sid !== section.id) return secRow(section);
  const isKey = editing.field === 'key';
  return `<div class="secrow secrow-card editing" data-sid="${section.id}">
    <div class="secrow-head">
      <input class="input${isKey ? ' key-input' : ''}" id="editInput"
        placeholder="${isKey ? '输入新密钥，留空则随机生成' : ''}"
        value="${isKey ? '' : esc(section.name)}">
    </div>
    <div class="actions">
      <button class="rowbtn ok" data-act="save">💾 保存</button>
      <button class="rowbtn" data-act="cancel">取消</button>
    </div>
  </div>`;
}

function renderSections() {
  const list = $('#sectionsList');
  const scroll = list.scrollTop;
  const sections = state.sections || [];

  $('#sectionsCount').textContent = `共 ${sections.length} 个分区`;

  if (!sections.length) {
    list.innerHTML = `<div class="section"><div class="empty">
      <div class="ico">🗂️</div>
      <div class="etitle">还没有任何分区</div>
      <div class="esub">点击左上角「新建分区」开始</div>
    </div></div>`;
  } else if (state.editing) {
    list.innerHTML = sections.map((s) => secRowEditing(s, state.editing)).join('');
    const input = $('#editInput');
    if (input) {
      input.focus();
      input.setSelectionRange(input.value.length, input.value.length);
    }
  } else {
    list.innerHTML = sections.map(secRow).join('');
  }

  list.scrollTop = scroll;
}

/* ---------------- 设置 ---------------- */

function renderSettings() {
  const s = state.settings;
  if (!s) {
    $('#settingsList').innerHTML = '';
    return;
  }
  const binds = state.bindDraft || state.settings.bind || ['127.0.0.1'];
  const listening = state.settings.listening || [];
  const port = state.settings.port || 17321;
  const lanOpen = (state.settings.lan_open || binds.some((b) => !isLoopbackBind(b))) ? true : false;
  $('#settingsList').innerHTML = `
    <div class="secrow bark-card">
      <div class="bark-fields">
        <div class="secrow-name">🌐 监听地址</div>
        <div class="secrow-key">默认只监听本机（127.0.0.1）。也可以添加 IPv4 / IPv6 / 主机名，保存后立即生效。</div>
        ${lanOpen ? '<div class="warn-note">⚠️ 已对局域网开放：网页端没有鉴权，同网段任何人都能读写。</div>' : ''}
        <label class="bind-local"><input type="checkbox" id="bindLocal" ${binds.length === 1 && binds[0] === '127.0.0.1' ? 'checked' : ''}> 仅本机（127.0.0.1）</label>
        <div class="bind-list">
          ${binds.map((b, i) => `<div class="bind-row"><span class="key">${esc(b)}</span>${listening.indexOf(b) >= 0 ? '<span class="bind-ok">已生效</span>' : '<span class="bind-bad">未生效</span>'}<button class="rowbtn danger" data-act="bind-del" data-i="${i}" title="移除">✕</button></div>`).join('') || '<div class="secrow-key">（尚未添加任何地址）</div>'}
        </div>
        <div class="bind-add">
          <input class="input" id="bindInput" placeholder="如 0.0.0.0、::、::1、192.168.1.5、host.example.com">
          <button class="rowbtn" data-act="bind-add">➕ 添加</button>
        </div>
        <div class="secrow-key">说明：<code>0.0.0.0</code> 绑定全部 IPv4 网卡；<code>::</code> 绑定全部 IPv6 网卡（两者互不冲突，可同时添加）；<code>::1</code> 只听 IPv6 环回；填域名则按解析出的地址绑定。</div>
        <div class="secrow-key">当前生效：${esc(listening.join('、') || '（无）')} · 端口 ${port}</div>
      </div>
      <div class="bark-actions"><button class="rowbtn" data-act="bind-save">💾 保存并生效</button></div>
    </div>
    <div class="secrow">
      <div>
        <div class="secrow-name">⚡ 开机自启动</div>
        <div class="secrow-key">登录 Windows 后自动启动并驻留托盘</div>
      </div>
      <div class="actions">
        <button class="switch${s.autostart ? ' on' : ''}" data-act="autostart" role="switch" aria-checked="${s.autostart}"></button>
      </div>
    </div>
    <div class="secrow">
      <div>
        <div class="secrow-name">🔔 任务完成推送（Bark）</div>
        <div class="secrow-key">任务标记完成时推送到手机；失败不影响任务状态</div>
      </div>
      <div class="actions">
        <button class="switch${state.bark && state.bark.enabled ? ' on' : ''}" data-act="bark-enabled" role="switch" aria-checked="${state.bark && state.bark.enabled}"></button>
      </div>
    </div>
    <div class="secrow bark-card">
      <div class="bark-fields">
        <div class="field"><label>服务器地址</label>
          <input class="input" id="barkServer" placeholder="https://bark.example.com" value="${esc((state.bark && state.bark.server) || '')}"></div>
        <div class="field"><label>设备 Key</label>
          <input class="input" id="barkKey" type="password" placeholder="${state.bark && state.bark.key_set ? '已保存 ' + esc(state.bark.key_masked) + '，留空则不修改' : '粘贴设备 Key'}">
        </div>
        <div class="field"><label>图标 URL（可留空）</label>
          <input class="input" id="barkIcon" value="${esc((state.bark && state.bark.icon) || '')}" placeholder="留空则不带图标"></div>
      </div>
      <div class="bark-actions">
        <button class="rowbtn" data-act="bark-save">💾 保存</button>
        <button class="rowbtn" data-act="bark-test">📨 测试推送</button>
      </div>
    </div>
    <div class="secrow">
      <div>
        <div class="secrow-name">🔌 接口地址</div>
        <div class="secrow-key"><span class="key">${esc(s.api_base)}</span></div>
      </div>
      <div class="actions"><button class="rowbtn" data-act="copy">📋 复制</button></div>
    </div>
    <div class="secrow">
      <div>
        <div class="secrow-name">📦 版本</div>
        <div class="secrow-key">${esc(s.version)}</div>
      </div>
      <div class="actions"></div>
    </div>
    <div class="secrow">
      <div>
        <div class="secrow-name">🗄️ 数据文件</div>
        <div class="secrow-key"><span class="key">${esc(s.db_path)}</span></div>
      </div>
      <div class="actions"></div>
    </div>`;
}

/* ---------------- 模态 ---------------- */

function closeModal() {
  if (state.modal) {
    state.modal.remove();
    state.modal = null;
  }
}

function openModal(inner) {
  closeModal();
  const wrap = document.createElement('div');
  wrap.className = 'modal';
  wrap.innerHTML = inner;
  document.body.appendChild(wrap);
  state.modal = wrap;

  wrap.querySelectorAll('[data-close]').forEach((btn) => {
    btn.addEventListener('click', closeModal);
  });

  const bar = wrap.querySelector('[data-drag]');
  const win = wrap.querySelector('.window');
  if (bar && win) {
    let active = false;
    let sx = 0;
    let sy = 0;
    let ox = 0;
    let oy = 0;
    bar.addEventListener('pointerdown', (e) => {
      if (e.button !== 0 || e.target.closest('button')) return;
      active = true;
      sx = e.clientX;
      sy = e.clientY;
      ox = wrap._dx || 0;
      oy = wrap._dy || 0;
      bar.setPointerCapture(e.pointerId);
      e.preventDefault();
    });
    bar.addEventListener('pointermove', (e) => {
      if (!active) return;
      wrap._dx = ox + (e.clientX - sx);
      wrap._dy = oy + (e.clientY - sy);
      win.style.transform = `translate(${wrap._dx}px, ${wrap._dy}px)`;
    });
    const stop = () => {
      active = false;
    };
    bar.addEventListener('pointerup', stop);
    bar.addEventListener('pointercancel', stop);
  }
  return wrap;
}

function modalShell(title, bodyHtml, footHtml, width) {
  const size = width ? ` style="width:${width}px"` : '';
  return `<div class="window"${size}>
    <div class="titlebar" data-drag>
      <div class="app">${esc(title)}</div>
      <div class="spacer"></div>
      <button class="winbtn close" data-close title="关闭"><svg class="i sm"><use href="#i-close"/></svg></button>
    </div>
    <div class="modal-body">${bodyHtml}</div>
    <div class="foot">
      <button class="btn" data-close>取消</button>
      ${footHtml}
    </div>
  </div>`;
}

async function openTaskEditor(sectionId, task) {
  try {
    await api('POST', '/api/editor/open', {
      section_id: sectionId,
      task_id: task ? task.id : 0,
    });
  } catch (e) {
    toast(e.message, true);
  }
}

function openDeleteConfirm(sectionId, task) {
  const body = `<div class="form">
    <p>确认删除该任务？删除后不可恢复。</p>
    <p style="color:var(--text-2);font-size:12px">${esc(task.title)}</p>
  </div>`;
  const wrap = openModal(modalShell('🗑️ 删除任务', body, '<button class="btn btn-danger" id="cOk">🗑️ 删除</button>', 360));

  wrap.querySelector('#cOk').addEventListener('click', async () => {
    try {
      await api('DELETE', `/api/board/sections/${sectionId}/tasks/${task.id}`);
      closeModal();
      if (state.detailId === task.id) {
        state.detailId = null;
        state.view = 'board';
      }
      await refresh();
      toast('已删除');
    } catch (e) {
      toast(e.message, true);
    }
  });
}

function openSectionDeleteConfirm(sid) {
  const sec = (state.sections || []).find((s) => s.id === sid);
  const name = sec ? sec.name : `#${sid}`;
  const count = sec ? (sec.total || 0) : 0;
  const body = `<div class="form">
    <p>将删除分区「${esc(name)}」，并连同 <b>${count}</b> 条任务一起删除，不可恢复。</p>
  </div>`;
  const wrap = openModal(modalShell('🗑️ 删除分区', body, '<button class="btn btn-danger" id="dsec">🗑️ 删除</button>', 420));
  wrap.querySelector('#dsec').addEventListener('click', async () => {
    try {
      const res = await api('DELETE', `/api/sections/${sid}`);
      closeModal();
      await refresh();
      toast(`已删除分区，连同 ${(res && res.deleted_tasks) || 0} 条任务`);
    } catch (e) {
      toast(e.message, true);
    }
  });
}

function openSectionCreator() {
  const body = `<div class="form">
    <div class="field"><label>📝 分区名称</label>
      <input class="input" id="sName" placeholder="例如：💼 工作">
    </div>
    <div class="field"><label>🔑 认证密钥（可留空）</label>
      <input class="input key-input" id="sKey" placeholder="留空则随机生成">
    </div>
  </div>`;
  const wrap = openModal(modalShell('➕ 新建分区', body, '<button class="btn btn-primary" id="sSave">✅ 创建</button>'));

  const name = wrap.querySelector('#sName');
  name.focus();

  wrap.querySelector('#sSave').addEventListener('click', async () => {
    const value = name.value.trim();
    if (!value) {
      toast('请填写分区名称', true);
      name.focus();
      return;
    }
    try {
      const res = await api('POST', '/api/sections', {
        name: value,
        key: wrap.querySelector('#sKey').value.trim(),
      });
      if (res && res.section) state.revealed[res.section.id] = res.auth_key;
      closeModal();
      state.view = 'sections';
      await refresh();
      toast('分区已创建');
    } catch (e) {
      toast(e.message, true);
    }
  });
}

/* ---------------- 动作 ---------------- */

async function setStatus(sectionId, task, status) {
  try {
    const res = await api('POST', `/api/board/sections/${sectionId}/tasks/${task.id}/status`, { status });
    await refresh();
    if (status === 'done' && res && res.bark) {
      const b = res.bark;
      if (b.error) toast('已完成，但推送失败：' + b.error, true);
      else if (b.pushed) toast('✅ 已完成，已推送');
    }
  } catch (e) {
    toast(e.message, true);
  }
}

async function moveSectionToTail(sectionId) {
  const list = state.view === 'sections' ? state.sections : (state.board ? state.board.sections : []);
  const ids = list.map((s) => s.id).filter((id) => id !== sectionId);
  ids.push(sectionId);
  await saveSectionOrder(ids);
}

async function saveSectionOrder(ids) {
  try {
    await api('PUT', '/api/sections/order', ids.map((id, i) => ({ id, sort_order: i })));
    await refresh();
  } catch (e) {
    toast(e.message, true);
  }
}

async function toggleDone(sectionId, task) {
  return setStatus(sectionId, task, task.status === 'done' ? 'open' : 'done');
}

/* 打勾控件：点同一状态再点一次回到未完成 */
async function cycleCheck(sectionId, task, mark) {
  return setStatus(sectionId, task, task.status === mark ? 'open' : mark);
}

function cycleMode(sid) {
  setMode(sid, MODE_NEXT[getMode(sid)] || 'open');
  render();
}

function toggleCollapsed(sid) {
  const next = !isCollapsed(sid);
  state.collapsed[sid] = next;
  localStorage.setItem('todo.collapsed.' + sid, next ? '1' : '0');
  render();
}

async function hideWindow() {
  try {
    await api('POST', '/api/window/hide');
  } catch (e) {
    /* 忽略 */
  }
}

/* ---------------- 事件 ---------------- */

function bindEvents() {
  $('#btnSections').addEventListener('click', () => {
    if (state.view === 'sections') showView('board');
    else openSections();
  });
  $('#btnSettings').addEventListener('click', () => {
    if (state.view === 'settings') showView('board');
    else openSettings();
  });
  $('#btnMin').addEventListener('click', hideWindow);
  $('#btnClose').addEventListener('click', async () => {
    try {
      await api('POST', '/api/window/close');
    } catch (e) {
      /* 退出中被中断属正常 */
    }
  });

  $('#titlebar').addEventListener('pointerdown', (e) => {
    if (e.button !== 0 || e.target.closest('.winbtn')) return;
    fetch('/api/window/drag', { method: 'POST' }).catch(() => {});
  });

  $('#boardView').addEventListener('click', (e) => {
    const actionEl = e.target.closest('[data-act]');
    const taskEl = e.target.closest('.task');
    const secEl = e.target.closest('.section');
    const sid = secEl ? Number(secEl.dataset.sid) : null;
    const found = taskEl ? findTask(Number(taskEl.dataset.id)) : null;

    if (actionEl) {
      const act = actionEl.dataset.act;
      if (act === 'new-section') return openSectionCreator();
      if (act === 'mode') {
        if (sid) cycleMode(sid);
        return;
      }
      if (act === 'tail') {
        if (sid) moveSectionToTail(sid);
        return;
      }
      if (act === 'new-task') {
        if (sid) openTaskEditor(sid, null);
        return;
      }
      if (!found) return;
      if (act === 'done' || act === 'undone') return toggleDone(found.section.id, found.task);
      if (act === 'edit') return openTaskEditor(found.section.id, found.task);
      if (act === 'del') return openDeleteConfirm(found.section.id, found.task);
      return;
    }

    if (e.target.closest('.check') && found) {
      return cycleCheck(found.section.id, found.task, 'done');
    }
    if (e.target.closest('.task-title') && found) {
      state.lastTaskId = found.task.id;
      state.detailId = found.task.id;
      showView('detail');
      return;
    }
    if (e.target.closest('.section-head') && sid !== null) {
      if (e.target.closest('button')) return;
      toggleCollapsed(sid);
    }
  });

  let dragSid = null;
  $('#boardView').addEventListener('dragstart', (e) => {
    const head = e.target.closest('.section-head');
    if (!head) return;
    const sec = head.closest('.section');
    dragSid = Number(sec.dataset.sid);
    sec.classList.add('dragging');
    try {
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', String(dragSid));
    } catch (err) { /* ignore */ }
  });
  $('#boardView').addEventListener('dragend', (e) => {
    const sec = e.target.closest('.section');
    if (sec) sec.classList.remove('dragging');
    dragSid = null;
  });
  $('#boardView').addEventListener('dragover', (e) => {
    if (dragSid === null) return;
    if (e.target.closest('.section-head')) {
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
    }
  });
  $('#boardView').addEventListener('drop', (e) => {
    const head = e.target.closest('.section-head');
    if (!head || dragSid === null) return;
    e.preventDefault();
    const targetSid = Number(head.closest('.section').dataset.sid);
    const moving = dragSid;
    dragSid = null;
    if (targetSid === moving) return;
    const ids = (state.board ? state.board.sections : []).map((s) => s.id).filter((id) => id !== moving);
    const at = ids.indexOf(targetSid);
    ids.splice(at < 0 ? ids.length : at, 0, moving);
    saveSectionOrder(ids);
  });

  $('#boardView').addEventListener('contextmenu', (e) => {
    const check = e.target.closest('.check');
    if (!check) {
      e.preventDefault();
      return;
    }
    e.preventDefault();
    const taskEl = e.target.closest('.task');
    const found = taskEl ? findTask(Number(taskEl.dataset.id)) : null;
    if (found) cycleCheck(found.section.id, found.task, 'doing');
  });

  $('#detailBack').addEventListener('click', () => showView('board'));
  $('#detailEdit').addEventListener('click', () => {
    const found = findTask(state.detailId);
    if (found) openTaskEditor(found.section.id, found.task);
  });
  $('#detailToggle').addEventListener('click', () => {
    const found = findTask(state.detailId);
    if (found) toggleDone(found.section.id, found.task);
  });

  $('#btnNewSection').addEventListener('click', openSectionCreator);

  $('#sectionsList').addEventListener('click', async (e) => {
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    const act = btn.dataset.act;
    const row = e.target.closest('.secrow');
    const sid = row ? Number(row.dataset.sid) : null;

    if (act === 'cancel') {
      state.editing = null;
      render();
      return;
    }
    if (act === 'save') {
      const editing = state.editing;
      if (!editing) return;
      const input = $('#editInput');
      const value = input ? input.value.trim() : '';
      try {
        if (editing.field === 'name') {
          if (!value) {
            toast('分区名称不能为空', true);
            return;
          }
          await api('PUT', `/api/sections/${editing.sid}`, { name: value });
        } else {
          const res = await api('PUT', `/api/sections/${editing.sid}/key`, { key: value });
          state.revealed[editing.sid] = res.auth_key;
        }
        state.editing = null;
        await refresh();
        toast('已保存');
      } catch (err) {
        toast(err.message, true);
      }
      return;
    }
    if (sid === null) return;

    if (act === 'bark-toggle') {
      const sec = (state.sections || []).find((s) => s.id === sid);
      try {
        await api('PUT', `/api/sections/${sid}`, { bark_enabled: !(sec && sec.bark_enabled) });
        await refresh();
        toast('已更新分区推送开关');
      } catch (err) {
        toast(err.message, true);
      }
      return;
    }
    if (act === 'tail') {
      await moveSectionToTail(sid);
      return;
    }
    if (act === 'del-sec') {
      openSectionDeleteConfirm(sid);
      return;
    }

    if (act === 'reveal') {
      if (state.revealed[sid]) {
        delete state.revealed[sid];
        render();
        return;
      }
      try {
        const res = await api('GET', `/api/sections/${sid}/key`);
        state.revealed[sid] = res.auth_key;
        render();
      } catch (err) {
        toast(err.message, true);
      }
      return;
    }
    if (act === 'rename') {
      state.editing = { sid, field: 'name' };
      render();
      return;
    }
    if (act === 'rekey') {
      state.editing = { sid, field: 'key' };
      render();
    }
  });

  $('#sectionsList').addEventListener('keydown', (e) => {
    if (e.target.id !== 'editInput') return;
    if (e.key === 'Enter') {
      e.preventDefault();
      const save = $('#sectionsList .rowbtn.ok');
      if (save) save.click();
      e.stopPropagation();
    }
  });

  $('#settingsList').addEventListener('click', async (e) => {
    const btn = e.target.closest('[data-act]');
    if (!btn || !state.settings) return;
    if (btn.dataset.act === 'autostart') {
      const next = !state.settings.autostart;
      try {
        await api('POST', '/api/settings/autostart', { enabled: next });
        state.settings = await api('GET', '/api/settings');
        render();
        toast(next ? '已开启开机自启动' : '已关闭开机自启动');
      } catch (err) {
        toast(err.message, true);
      }
    } else if (btn.dataset.act === 'bark-enabled') {
      try {
        state.bark = await api('PUT', '/api/bark', { enabled: !(state.bark && state.bark.enabled) });
        render();
        toast(state.bark.enabled ? '已启用完成推送' : '已关闭完成推送');
      } catch (err) {
        toast(err.message, true);
      }
    } else if (btn.dataset.act === 'bind-add') {
      const input = $('#bindInput');
      const v = input ? input.value.trim().replace(/^\[(.*)\]$/, '$1') : '';
      if (!v) return;
      const draft = state.bindDraft || [];
      if (draft.indexOf(v) < 0) draft.push(v);
      state.bindDraft = draft;
      render();
    } else if (btn.dataset.act === 'bind-del') {
      const i = Number(btn.dataset.i);
      const draft = (state.bindDraft || []).slice();
      draft.splice(i, 1);
      state.bindDraft = draft;
      render();
    } else if (btn.dataset.act === 'bind-save') {
      try {
        const res = await api('PUT', '/api/settings/bind', { binds: state.bindDraft || [] });
        const fails = res.failures || {};
        const failText = Object.keys(fails).map((k) => `${k}：${fails[k]}`).join('；');
        if (res.ok) {
          toast(failText ? '部分地址失败：' + failText : '监听地址已更新并生效', !!failText);
        } else {
          toast('保存失败，' + (res.error || '未知原因') + (failText ? '：' + failText : ''), true);
        }
        state.settings = await api('GET', '/api/settings');
        state.bindDraft = (state.settings.bind || ['127.0.0.1']).slice();
        render();
      } catch (err) {
        toast(err.message, true);
      }
    } else if (btn.dataset.act === 'bark-save') {
      const server = ($('#barkServer') || {}).value || '';
      const key = ($('#barkKey') || {}).value || '';
      const icon = ($('#barkIcon') || {}).value || '';
      const body = { server: server.trim(), icon: icon.trim() };
      if (key.trim() !== '') body.key = key.trim();
      try {
        state.bark = await api('PUT', '/api/bark', body);
        render();
        toast('推送配置已保存');
      } catch (err) {
        toast(err.message, true);
      }
    } else if (btn.dataset.act === 'bark-test') {
      try {
        const res = await api('POST', '/api/bark/test');
        if (res && res.ok) toast('📨 测试推送已发送');
        else toast('推送失败：' + ((res && res.error) || '未知错误'), true);
      } catch (err) {
        toast(err.message, true);
      }
    } else if (btn.dataset.act === 'copy') {
      try {
        await navigator.clipboard.writeText(state.settings.api_base);
        toast('已复制接口地址');
      } catch (err) {
        toast('复制失败，请手动选择', true);
      }
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (state.modal) {
      closeModal();
      return;
    }
    if (state.editing) {
      state.editing = null;
      render();
      return;
    }
    if (state.view !== 'board') showView('board');
  });
}

/* ---------------- 启动 ---------------- */

function connectSSE() {
  const source = new EventSource('/api/events');
  let timer = null;
  source.onmessage = () => {
    if (state.editing) return;
    clearTimeout(timer);
    timer = setTimeout(refresh, 120);
  };
}

async function init() {
  bindEvents();
  window.addEventListener('focus', () => {
    if (!state.modal && !state.editing) refresh();
  });
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && !state.modal && !state.editing) refresh();
  });
  try {
    state.board = await api('GET', '/api/board');
  } catch (e) {
    /* 首次加载失败时留空页 */
  }
  render();
  connectSSE();
  if (location.hash === '#settings') openSettings();
}

init();

/* ---------------- 无边框窗边缘缩放 ----------------
   WebView2 铺满整个客户区，父窗口收不到 WM_NCHITTEST，系统无法进入缩放；
   因此由页面在边缘接手，把按下转成系统缩放路径（WM_NCLBUTTONDOWN + HTxxx）。 */

const EDGE = 6;
const EDGE_CURSOR = {
  left: 'ew-resize', right: 'ew-resize', top: 'ns-resize', bottom: 'ns-resize',
  topleft: 'nwse-resize', bottomright: 'nwse-resize',
  topright: 'nesw-resize', bottomleft: 'nesw-resize',
};

function edgeAt(x, y) {
  const w = window.innerWidth;
  const h = window.innerHeight;
  const l = x <= EDGE, r = x >= w - EDGE, t = y <= EDGE, b = y >= h - EDGE;
  if (t && l) return 'topleft';
  if (t && r) return 'topright';
  if (b && l) return 'bottomleft';
  if (b && r) return 'bottomright';
  if (l) return 'left';
  if (r) return 'right';
  if (t) return 'top';
  if (b) return 'bottom';
  return '';
}

document.addEventListener('pointermove', (e) => {
  const edge = edgeAt(e.clientX, e.clientY);
  document.body.style.cursor = edge ? EDGE_CURSOR[edge] : '';
});

document.addEventListener('pointerdown', (e) => {
  if (e.button !== 0) return;
  const edge = edgeAt(e.clientX, e.clientY);
  if (!edge) return;
  e.preventDefault();
  e.stopPropagation();
  try { document.body.setPointerCapture(e.pointerId); } catch (err) { /* ignore */ }
  fetch(`/api/window/resize/start?edge=${edge}`, { method: 'POST' }).catch(() => {});
  const end = () => {
    for (const ev of ['pointerup', 'pointercancel', 'blur', 'lostpointercapture']) {
      window.removeEventListener(ev, end);
    }
    fetch('/api/window/resize/end', { method: 'POST' }).catch(() => {});
  };
  for (const ev of ['pointerup', 'pointercancel', 'blur', 'lostpointercapture']) {
    window.addEventListener(ev, end);
  }
}, true);
