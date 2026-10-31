'use strict';

const $ = (sel) => document.querySelector(sel);

const PAGE = 20;
const FILTERS = [
  ['open', '未完成'],
  ['doing', '处理中'],
  ['done', '已完成'],
  ['all', '全部'],
];

const state = {
  sections: [],          // 已加载的分区
  sectionTotal: 0,
  sectionOffset: 0,
  tasks: {},             // sid -> { items, total, filter, offset, loading }
  expanded: null,        // 展开的任务 id
  bark: null,
  settings: null,
  loadingMore: false,
  dragId: null,
  dataSig: '',
};

/* ---------------- 基础 ---------------- */

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

const pad2 = (n) => String(n).padStart(2, '0');

function nowStr() {
  const d = new Date();
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  let data = null;
  try { data = await res.json(); } catch (e) { data = null; }
  if (!res.ok) throw new Error((data && data.error) || `请求失败 (${res.status})`);
  return data;
}

let toastTimer = null;
function toast(msg, isErr) {
  const el = $('#toast');
  el.textContent = msg;
  el.classList.toggle('err', !!isErr);
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 3000);
}

function columnsFor(width) {
  if (width < 768) return 1;
  if (width < 1000) return 2;
  if (width < 1200) return 3;
  return 4; // 上限固定 4 列（宽屏不再出 5 列）
}

/* ---------------- Markdown ---------------- */

function inline(s) {
  return s
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/\[([^\]]+)\]\(((?:https?:|mailto:)[^)\s]*)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
}

function renderMarkdown(src) {
  const text = String(src || '').replace(/\r\n/g, '\n').trim();
  if (!text) return '<div class="empty">暂无描述</div>';
  const blocks = [];
  const withPh = text.replace(/```[^\n]*\n([\s\S]*?)```/g, (m, code) => {
    blocks.push(code);
    return `\u0000${blocks.length - 1}\u0000`;
  });
  const lines = esc(withPh).split('\n');
  const out = [];
  let items = null;
  const flush = () => {
    if (items) { out.push(`<ul>${items.join('')}</ul>`); items = null; }
  };
  for (const raw of lines) {
    const line = raw.trim();
    if (!line) { flush(); continue; }
    const ph = line.match(/^\u0000(\d+)\u0000$/);
    if (ph) {
      flush();
      out.push(`<pre><code>${esc(blocks[Number(ph[1])])}</code></pre>`);
      continue;
    }
    const h = line.match(/^#{1,6}\s+(.*)$/);
    if (h) { flush(); out.push(`<h3>${inline(h[1])}</h3>`); continue; }
    const li = line.match(/^[-*]\s+(.*)$/);
    if (li) { if (!items) items = []; items.push(`<li>${inline(li[1])}</li>`); continue; }
    flush();
    out.push(`<p>${inline(line)}</p>`);
  }
  flush();
  return out.join('');
}

/* ---------------- 概览 ---------------- */

function ringPath(frac) {
  if (!(frac > 0)) return '';
  const f = Math.min(1, frac);
  const start = (135 * Math.PI) / 180;
  const end = start + 2 * Math.PI * 0.75 * f;
  const cx = 23, cy = 23, r = 19.5;
  const x0 = cx + r * Math.cos(start), y0 = cy + r * Math.sin(start);
  const x1 = cx + r * Math.cos(end), y1 = cy + r * Math.sin(end);
  const large = 270 * f > 180 ? 1 : 0;
  return `M${x0.toFixed(2)} ${y0.toFixed(2)} A${r} ${r} 0 ${large} 1 ${x1.toFixed(2)} ${y1.toFixed(2)}`;
}

async function loadStats() {
  try {
    const t = await api('GET', '/api/web/stats');
    $('#cTotal').textContent = t.total;
    $('#cOpen').textContent = t.open;
    $('#cDoing').textContent = t.doing;
    $('#cDone').textContent = t.done;
    $('#cOverdue').textContent = t.overdue;
    const frac = t.total > 0 ? t.done / t.total : 0;
    $('#ringFill').setAttribute('d', ringPath(frac));
    $('#ringNum').textContent = `${Math.round(frac * 100)}%`;
    $('#ovDate').textContent = t.date || '';
  } catch (e) { /* 忽略 */ }
}

/* ---------------- 数据 ---------------- */

function taskState(sid) {
  if (!state.tasks[sid]) {
    state.tasks[sid] = { items: [], total: 0, filter: 'open', offset: 0, loading: false };
  }
  return state.tasks[sid];
}

async function loadSections(append) {
  if (state.loadingMore) return;
  state.loadingMore = true;
  try {
    const offset = append ? state.sectionOffset : 0;
    const res = await api('GET', `/api/web/sections?limit=${PAGE}&offset=${offset}`);
    const list = res.sections || [];
    if (!append) {
      state.sections = [];
      state.tasks = {};
    }
    state.sectionTotal = res.total || 0;
    state.sectionOffset = (append ? state.sectionOffset : 0) + list.length;
    for (const sec of list) {
      state.sections.push(sec);
      const st = taskState(sec.id);
      await loadTasks(sec.id, st.filter, false);
    }
  } catch (e) {
    toast(e.message, true);
  } finally {
    state.loadingMore = false;
  }
  render();
  $('#pagefoot').textContent = state.sections.length >= state.sectionTotal
    ? (state.sectionTotal ? `共 ${state.sectionTotal} 个分区` : '')
    : `已加载 ${state.sections.length} / ${state.sectionTotal} 个分区，继续向下滚动加载更多`;
}

async function loadTasks(sid, filter, append) {
  const st = taskState(sid);
  if (st.loading) return;
  st.loading = true;
  try {
    const offset = append ? st.offset : 0;
    const res = await api('GET', `/api/web/tasks?section=${sid}&status=${filter}&limit=${PAGE}&offset=${offset}`);
    const items = res.tasks || [];
    st.items = append ? st.items.concat(items) : items;
    st.total = res.total || 0;
    st.offset = offset + items.length;
    st.filter = filter;
  } catch (e) {
    toast(e.message, true);
  } finally {
    st.loading = false;
  }
}

/* ---------------- 渲染 ---------------- */

// 与桌面版同一套 8 色色板、同一取值顺序（按 sort_order 循环）
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

function secVars(index) {
  const c = SEC_COLORS[index % SEC_COLORS.length];
  return `--sec-color:${c.hex};--sec-soft:rgba(${c.rgb},0.06);--sec-hover:rgba(${c.rgb},0.12)`;
}

function dueMeta(task) {
  if (!task.due_at) return { text: '未设截止', cls: '' };
  const text = task.due_at.replace('T', ' ');
  const now = nowStr();
  if (task.status !== 'done' && task.due_at < now) return { text, cls: 'overdue' };
  if (task.date === undefined && task.due_at.slice(0, 10) === now.slice(0, 10)) return { text, cls: 'today' };
  return { text, cls: '' };
}

function taskTag(task) {
  if (task.bark_override === 'on') return { cls: 'doing', text: '🔔 强制推送' };
  if (task.bark_override === 'off') return { cls: '', text: '🔕 不推送' };
  return null;
}

function taskRowHtml(task) {
  const open = state.expanded === task.id;
  const due = dueMeta(task);
  const tag = taskTag(task);
  const dot = task.status === 'done' ? 'done' : task.status === 'doing' ? 'doing' : '';
  return `<div class="task ${task.status} ${open ? 'open' : ''}" data-tid="${task.id}">
    <div class="task-row" data-act="toggle">
      <span class="dot ${dot}"></span>
      <span class="task-title">${esc(task.title)}</span>
      ${tag ? `<span class="tag ${tag.cls}">${tag.text}</span>` : ''}
      <span class="task-date ${due.cls}">${esc(due.text)}</span>
      <svg class="i sm chev"><use href="#i-chev"/></svg>
    </div>
    ${open ? taskCardHtml(task) : ''}
  </div>`;
}

function taskCardHtml(task) {
  const due = task.due_at ? task.due_at.replace('T', ' ') : '未设截止';
  const statusText = task.status === 'done' ? '已完成' : task.status === 'doing' ? '处理中' : '未完成';
  const btns = [];
  if (task.status === 'open') {
    btns.push('<button class="btn ok" data-act="done">✅ 完成</button>');
    btns.push('<button class="btn" data-act="doing">▶️ 处理中</button>');
  } else if (task.status === 'doing') {
    btns.push('<button class="btn ok" data-act="done">✅ 完成</button>');
    btns.push('<button class="btn" data-act="open">↩️ 撤销</button>');
  } else {
    btns.push('<button class="btn" data-act="open">↩️ 撤销</button>');
  }
  btns.push('<button class="btn" data-act="edit">✏️ 编辑</button>');
  btns.push('<button class="btn danger" data-act="del">🗑️ 删除</button>');
  return `<div class="task-card">
    <div class="md">${renderMarkdown(task.description)}</div>
    <div class="card-meta"><span>📅 ${esc(due)}</span><span>${statusText}</span></div>
    <div class="card-actions">${btns.join('')}</div>
  </div>`;
}

function colHtml(sec, index) {
  const st = taskState(sec.id);
  const pct = sec.total > 0 ? Math.round((sec.done / sec.total) * 100) : 0;
  const filters = FILTERS.map(([k, label]) =>
    `<button class="filter ${st.filter === k ? 'on' : ''}" data-filter="${k}">${label}</button>`).join('');
  const rows = st.items.length
    ? st.items.map(taskRowHtml).join('')
    : `<div class="empty-col">${st.filter === 'all' ? '暂无任务' : `暂无${FILTERS.find((f) => f[0] === st.filter)[1]}任务`}</div>`;
  const more = st.items.length < st.total
    ? `<button class="more" data-act="more">加载更多（${st.items.length}/${st.total}）</button>`
    : '';
  return `<div class="col" data-sid="${sec.id}" draggable="true" style="${secVars(index)};border-left:3px solid var(--sec-color)">
    <div class="col-head">
      <div class="col-row1">
        <svg class="i sm chev" data-act="collapse"><use href="#i-chev"/></svg>
        <span class="col-name">${esc(sec.name)}</span>
        <span class="head-meta"><span class="col-id">#${sec.id}</span><span class="col-count">📋 ${sec.total}</span><span class="mon-badge" title="Pager 连接数">💡 ${Number(sec.monitor_count || 0)}</span></span>
        <!-- 完成进度条暂时隐藏，勿删（DOM 与 CSS 都保留）：
        <div class="sp-bar"><div class="sp-fill" style="width:${pct}%"></div></div>
        -->
        <div class="grow"></div>
        <button class="minibtn ${sec.bark_enabled ? 'on' : ''}" data-act="bark" title="分区完成推送" aria-label="分区完成推送">${sec.bark_enabled ? '🔔' : '🔕'}</button>
        <button class="minibtn" data-act="tail" title="置于队尾" aria-label="置于队尾">⤓</button>
        <button class="minibtn" data-act="new" title="新建任务" aria-label="新建任务">➕</button>
      </div>
      <div class="col-filters">${filters}</div>
    </div>
    <div class="col-body">${rows}${more}</div>
  </div>`;
}

function render() {
  const board = $('#board');
  const cols = columnsFor(window.innerWidth);
  board.style.gridTemplateColumns = `repeat(${cols}, minmax(0, 1fr))`;
  if (!state.sections.length) {
    board.innerHTML = `<div class="col"><div class="empty-col">还没有分区。请在桌面端「分区管理」里创建。</div></div>`;
  } else {
    board.innerHTML = state.sections.map(colHtml).join('');
  }
  requestAnimationFrame(layoutMasonry);
}

function layoutMasonry() {
  const cols = Array.from(document.querySelectorAll('.col'));
  for (const col of cols) {
    col.style.gridRowEnd = 'auto';
  }
  for (const col of cols) {
    const h = col.getBoundingClientRect().height;
    col.style.gridRowEnd = `span ${Math.max(1, Math.ceil(h + 14))}`;
  }
}

let layoutTimer = null;
function scheduleLayout() {
  clearTimeout(layoutTimer);
  layoutTimer = setTimeout(layoutMasonry, 60);
}

/* ---------------- 操作 ---------------- */

async function setStatus(secId, taskId, status) {
  try {
    const res = await api('POST', `/api/board/sections/${secId}/tasks/${taskId}/status`, { status });
    await refreshAll();
    if (status === 'done' && res && res.bark) {
      if (res.bark.error) toast('已完成，但推送失败：' + res.bark.error, true);
      else if (res.bark.pushed) toast('✅ 已完成，已推送');
    }
  } catch (e) {
    toast(e.message, true);
  }
}

async function deleteTask(secId, taskId) {
  if (!window.confirm('确认删除该任务？删除后不可恢复。')) return;
  try {
    await api('DELETE', `/api/board/sections/${secId}/tasks/${taskId}`);
    state.expanded = null;
    await refreshAll();
    toast('已删除');
  } catch (e) {
    toast(e.message, true);
  }
}

async function toggleSectionBark(sec) {
  try {
    await api('PUT', `/api/sections/${sec.id}`, { bark_enabled: !sec.bark_enabled });
    await refreshAll();
  } catch (e) {
    toast(e.message, true);
  }
}

async function moveToTail(sid) {
  const ids = state.sections.map((s) => s.id).filter((id) => id !== sid);
  ids.push(sid);
  try {
    await api('PUT', '/api/sections/order', ids.map((id, i) => ({ id, sort_order: i })));
    await refreshAll();
  } catch (e) {
    toast(e.message, true);
  }
}

async function saveOrder(ids) {
  try {
    await api('PUT', '/api/sections/order', ids.map((id, i) => ({ id, sort_order: i })));
    await refreshAll();
  } catch (e) {
    toast(e.message, true);
  }
}

/* 编辑模态 */
function openEdit(sec, task) {
  const wrap = document.createElement('div');
  wrap.className = 'modal';
  wrap.innerHTML = `<div class="window">
    <div class="titlebar"><div class="app">${task ? '✏️ 编辑任务' : '➕ 新建任务'}</div>
      <div class="spacer"></div><button class="winbtn close" data-act="close">✕</button></div>
    <div class="modal-body"><div class="form">
      <div class="field"><label>标题</label><input class="input" id="fTitle" value="${task ? esc(task.title) : ''}"></div>
      <div class="field"><label>详细描述（Markdown）</label><textarea class="input" id="fDesc" rows="8">${task ? esc(task.description) : ''}</textarea></div>
      <div class="field"><label>截止时间（可留空）</label><input class="input" id="fDue" type="datetime-local" value="${task && task.due_at ? esc(task.due_at) : ''}"></div>
      <div class="field"><label>完成推送</label>
        <select class="input" id="fBark">
          <option value="inherit">继承分区</option>
          <option value="on">开</option>
          <option value="off">关</option>
        </select></div>
      <div class="actions" style="justify-content:flex-end">
        <button class="btn" data-act="close">取消</button>
        <button class="btn primary" data-act="save">保存</button>
      </div>
    </div></div>
  </div>`;
  document.body.appendChild(wrap);
  wrap.querySelector('#fBark').value = (task && task.bark_override) || 'inherit';
  wrap.querySelector('#fTitle').focus();
  const close = () => wrap.remove();
  wrap.addEventListener('click', async (e) => {
    const act = e.target.closest('[data-act]');
    if (!act) return;
    if (act.dataset.act === 'close') { close(); return; }
    if (act.dataset.act === 'save') {
      const title = wrap.querySelector('#fTitle').value.trim();
      if (!title) { toast('请填写标题', true); return; }
      const payload = {
        title,
        description: wrap.querySelector('#fDesc').value,
        due: wrap.querySelector('#fDue').value || null,
        bark_override: wrap.querySelector('#fBark').value,
      };
      try {
        if (task) await api('PUT', `/api/board/sections/${sec.id}/tasks/${task.id}`, payload);
        else await api('POST', `/api/board/sections/${sec.id}/tasks`, payload);
        close();
        await refreshAll();
        toast('已保存');
      } catch (err) {
        toast(err.message, true);
      }
    }
  });
}

/* ---------------- 刷新 ---------------- */

async function refreshAll() {
  const keep = state.sections.length;
  const limit = Math.max(PAGE, keep);
  try {
    const res = await api('GET', `/api/web/sections?limit=${limit}&offset=0`);
    const list = res.sections || [];
    state.sectionTotal = res.total || 0;
    state.sectionOffset = list.length;
    state.sections = list;
    for (const sec of list) {
      const st = taskState(sec.id);
      await loadTasks(sec.id, st.filter, false);
    }
  } catch (e) {
    toast(e.message, true);
  }
  const sig = JSON.stringify({ sections: state.sections, tasks: state.tasks });
  if (sig === state.dataSig) {
    loadStats();
    return; // 数据没变：不重绘，避免闪动/滚动丢失
  }
  state.dataSig = sig;
  render();
  loadStats();
}

/* ---------------- 事件 ---------------- */

function bind() {
  $('#board').addEventListener('click', async (e) => {
    const col = e.target.closest('.col');
    if (!col) return;
    const sid = Number(col.dataset.sid);
    const sec = state.sections.find((s) => s.id === sid);
    const taskEl = e.target.closest('.task');
    const taskId = taskEl ? Number(taskEl.dataset.tid) : null;
    const st = taskState(sid);
    const task = taskEl ? st.items.find((t) => t.id === taskId) : null;

    const filterBtn = e.target.closest('[data-filter]');
    if (filterBtn) {
      await loadTasks(sid, filterBtn.dataset.filter, false);
      render();
      return;
    }

    const act = e.target.closest('[data-act]');
    if (act) {
      const kind = act.dataset.act;
      if (kind === 'collapse') { col.classList.toggle('collapsed'); scheduleLayout(); return; }
      if (kind === 'new') { openEdit(sec, null); return; }
      if (kind === 'tail') { moveToTail(sid); return; }
      if (kind === 'bark') { toggleSectionBark(sec); return; }
      if (kind === 'more') { await loadTasks(sid, st.filter, true); render(); return; }
      if (kind === 'toggle' && task) {
        state.expanded = state.expanded === task.id ? null : task.id;
        render();
        return;
      }
      if (!task) return;
      if (kind === 'done') { setStatus(sid, task.id, 'done'); return; }
      if (kind === 'doing') { setStatus(sid, task.id, 'doing'); return; }
      if (kind === 'open') { setStatus(sid, task.id, 'open'); return; }
      if (kind === 'edit') { openEdit(sec, task); return; }
      if (kind === 'del') { deleteTask(sid, task.id); return; }
    }
  });

  // 拖动排序
  $('#board').addEventListener('dragstart', (e) => {
    const col = e.target.closest('.col');
    if (!col) return;
    state.dragId = Number(col.dataset.sid);
    col.classList.add('dragging');
    try {
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', String(state.dragId));
    } catch (err) { /* ignore */ }
  });
  $('#board').addEventListener('dragend', (e) => {
    const col = e.target.closest('.col');
    if (col) col.classList.remove('dragging');
    state.dragId = null;
  });
  $('#board').addEventListener('dragover', (e) => {
    if (state.dragId === null) return;
    if (e.target.closest('.col')) {
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
    }
  });
  $('#board').addEventListener('drop', (e) => {
    const col = e.target.closest('.col');
    if (!col || state.dragId === null) return;
    e.preventDefault();
    const target = Number(col.dataset.sid);
    const moving = state.dragId;
    state.dragId = null;
    if (target === moving) return;
    const ids = state.sections.map((s) => s.id).filter((id) => id !== moving);
    const at = ids.indexOf(target);
    ids.splice(at < 0 ? ids.length : at, 0, moving);
    saveOrder(ids);
  });

  window.addEventListener('resize', () => {
    const cols = columnsFor(window.innerWidth);
    $('#board').style.gridTemplateColumns = `repeat(${cols}, minmax(0, 1fr))`;
    scheduleLayout();
  });

  // 无限滚动加载分区
  const io = new IntersectionObserver((entries) => {
    for (const en of entries) {
      if (en.isIntersecting && state.sections.length < state.sectionTotal) {
        loadSections(true);
      }
    }
  }, { rootMargin: '300px' });
  io.observe($('#sentinel'));

  // 设置
  $('#btnSettings').addEventListener('click', openSettings);
  $('#settingsClose').addEventListener('click', () => { $('#settingsModal').hidden = true; });
  $('#barkEnabled').addEventListener('click', async () => {
    try {
      state.bark = await api('PUT', '/api/bark', { enabled: !(state.bark && state.bark.enabled) });
      fillBark();
      toast(state.bark.enabled ? '已启用完成推送' : '已关闭完成推送');
    } catch (e) { toast(e.message, true); }
  });
  $('#barkSave').addEventListener('click', async () => {
    const body = {
      server: $('#barkServer').value.trim(),
      icon: $('#barkIcon').value.trim(),
    };
    const key = $('#barkKey').value.trim();
    if (key !== '') body.key = key;
    try {
      state.bark = await api('PUT', '/api/bark', body);
      fillBark();
      toast('推送配置已保存');
    } catch (e) { toast(e.message, true); }
  });
  $('#barkTest').addEventListener('click', async () => {
    try {
      const res = await api('POST', '/api/bark/test');
      if (res && res.ok) toast('📨 测试推送已发送');
      else toast('推送失败：' + ((res && res.error) || '未知错误'), true);
    } catch (e) { toast(e.message, true); }
  });

  $('#sectionAdmin').addEventListener('click', async (e) => {
    const monBtn = e.target.closest('[data-act="mon-del"],[data-act="mon-clear"],[data-act="mon-clear-all"]');
    if (monBtn) {
      const act = monBtn.dataset.act;
      try {
        if (act === 'mon-del') {
          const sid = Number(monBtn.dataset.sid);
          const mid = Number(monBtn.dataset.mid);
          if (!confirm(`确定删除这一条 Pager 连接（#${sid}）？该会话将不再收到本分区的变化通知。`)) return;
          await api('DELETE', `/api/monitors/${mid}`);
          toast('已删除该 Pager 连接');
        } else if (act === 'mon-clear') {
          const sid = Number(monBtn.dataset.sid);
          const n = monBtn.closest('.mon-admin').querySelectorAll('.mon-row').length;
          if (!confirm(`确定清除分区 #${sid} 的全部 ${n} 个 Pager 连接？清除后相关会话不再收到通知。`)) return;
          await api('DELETE', `/api/monitors?section=${sid}`);
          toast(`已清除分区 #${sid} 的 ${n} 个 Pager 连接`);
        } else {
          await clearAllMonitors();
        }
        await afterMonitorChange();
      } catch (err) {
        toast(err.message, true);
      }
      return;
    }
    const btn = e.target.closest('[data-del]');
    if (!btn) return;
    const row = btn.closest('.admin-row');
    const sid = Number(btn.dataset.del);
    const name = row ? row.querySelector('.admin-name').textContent.trim() : '#' + sid;
    const count = row ? Number((row.querySelector('.admin-count').textContent || '0').replace(/[^0-9]/g, '')) : 0;
    deleteSection(sid, name, count);
  });

  // 盯盘推送设置保存
  $('#aionSave').addEventListener('click', async () => {
    const skip = Number(($('#aionSkipSec') || {}).value || 0);
    try {
      const res = await api('PUT', '/api/settings/aion', {
        skip_interval_sec: skip,
      });
      state.settings = await api('GET', '/api/settings');
      toast(`已保存：重试间隔 ${res.skip_interval_sec} 秒`);
    } catch (e) {
      toast(e.message, true);
    }
  });

  $('#monClearAll').addEventListener('click', clearAllMonitors);

  // 自动刷新间隔保存
  $('#pollSave').addEventListener('click', async () => {
    const v = Number($('#pollSec').value || 0);
    try {
      const res = await api('PUT', '/api/settings/poll', { interval_sec: v });
      state.settings = await api('GET', '/api/settings');
      if (sseAlive) {
        refreshAll();
      } else {
        startPolling(true);
      }
      toast('自动刷新间隔已改为 ' + res.interval_sec + ' 秒');
    } catch (e) {
      toast(e.message, true);
    }
  });
}

/* ---- 实时刷新：SSE 为主，断开回退定时轮询 ----
   有 SSE 就不轮询；SSE 连上后立刻停掉轮询，断开期间轮询兜底。 */
let pollTimer = null;
let sse = null;
let sseAlive = false;
let sseFallbackTimer = null;
let sseRebuildTimer = null;
let sseSoonTimer = null;

const diag = (window.__todoDiag = { sse: 'idle', sseOpens: 0, pollTicks: 0 });

function pollIntervalMs() {
  const n = Number((state.settings && state.settings.poll_interval_sec) || 5);
  return Math.max(1, Math.min(300, n)) * 1000;
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

// 已在轮询时不要重建定时器：SSE 每次重试失败都会触发 onerror，
// 若每次都重建，间隔永远走不满，回退轮询就等于停了。
function ensurePolling() {
  if (pollTimer) return;
  startPolling(false);
}

function startPolling(immediate) {
  stopPolling();
  if (immediate) refreshAll();
  pollTimer = setInterval(() => {
    diag.pollTicks++;
    refreshAll();
  }, pollIntervalMs());
}

function refreshSoon() {
  clearTimeout(sseSoonTimer);
  sseSoonTimer = setTimeout(refreshAll, 150);
}

function connectSSE() {
  if (!window.EventSource) {
    diag.sse = 'unavailable';
    startPolling(false);
    return;
  }
  clearTimeout(sseRebuildTimer);
  if (sse) {
    try { sse.close(); } catch (e) { /* ignore */ }
    sse = null;
  }
  try {
    sse = new EventSource('/api/events');
  } catch (e) {
    diag.sse = 'unavailable';
    startPolling(false);
    return;
  }
  sse.onopen = () => {
    sseAlive = true;
    diag.sse = 'open';
    diag.sseOpens++;
    clearTimeout(sseFallbackTimer);
    clearTimeout(sseRebuildTimer);
    stopPolling(); // 有 SSE 就不轮询
  };
  sse.onmessage = () => refreshSoon();
  sse.onerror = () => {
    sseAlive = false;
    diag.sse = 'closed';
    ensurePolling(); // 断开期间轮询保持开着（已在跑就不重建）
    // 兜底自建重连：WebView2 里 EventSource 有时断几次就不再自己重试，久等不回来。
    clearTimeout(sseRebuildTimer);
    sseRebuildTimer = setTimeout(() => {
      if (!sseAlive) connectSSE();
    }, 5000);

  };
  // 3 秒内未 onopen 就先开轮询兜底
  clearTimeout(sseFallbackTimer);
  sseFallbackTimer = setTimeout(() => {
    if (!sseAlive) ensurePolling();
  }, 3000);
}

function fillBark() {
  const b = state.bark || {};
  $('#barkServer').value = b.server || '';
  $('#barkIcon').value = b.icon || '';
  $('#barkKey').value = '';
  $('#barkKey').placeholder = b.key_set ? `已保存 ${b.key_masked}，留空则不修改` : '粘贴设备 Key';
  $('#barkEnabled').classList.toggle('on', !!b.enabled);
  $('#barkEnabled').setAttribute('aria-checked', String(!!b.enabled));
  $('#barkHint').textContent = b.enabled ? '总开关：已启用' : '总开关：已关闭（分区开关生效但不会推送）';
}

/* ---------------- 盯盘（分区管理） ---------------- */


function relTime(iso) {
  if (!iso) return '从未心跳';
  const t = Date.parse(iso);
  if (!t) return iso;
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (s < 60) return s + ' 秒前';
  if (s < 3600) return Math.round(s / 60) + ' 分钟前';
  if (s < 86400) return Math.round(s / 3600) + ' 小时前';
  return Math.round(s / 86400) + ' 天前';
}

function monStatusClass(code) {
  if (code >= 200 && code < 300) return 'ok';
  if (code >= 400 && code < 500) return 'err';
  return 'muted';
}

function monStatusText(code) {
  if (!code) return '未推送';
  if (code === -1) return '-1 未配置';
  return String(code);
}

function monName(m) {
  if (m.display_name) return m.display_name;
  if (m.team_name || m.slot_name) return [m.team_name, m.slot_name].filter(Boolean).join(' - ');
  if (m.conv_name) return m.conv_name;
  const tail = String(m.slot_id || m.conversation_id || '').slice(-6);
  return `${m.target_type}…${tail || '?'}`;
}

function monitorRowHtml(sid, m) {
  const name = monName(m);
  return `<div class="mon-row">
    <span class="mon-name" title="${esc(name)}">${esc(name)}</span>
    <span class="mon-meta">#${sid}</span>
    <span class="mon-meta">间隔 ${Number(m.interval_sec) || 0} 秒</span>
    <span class="mon-meta" title="${esc(m.last_seen || '')}">${esc(relTime(m.last_seen))}</span>
    <span class="mon-status ${monStatusClass(m.last_status)}">${esc(monStatusText(m.last_status))}</span>
    <button class="btn danger" data-act="mon-del" data-mid="${m.id}" data-sid="${sid}" title="删除该 Pager 连接">🗑</button>
  </div>`;
}

// 给每个分区行补盯盘列表（管理面，不需要分区 key）
async function loadMonitorPanels() {
  const nodes = Array.from(document.querySelectorAll('.mon-admin[data-mon]'));
  if (!nodes.length) return;
  await Promise.all(nodes.map(async (el) => {
    const sid = Number(el.dataset.mon);
    try {
      const res = await api('GET', `/api/monitors?section=${sid}`);
      const list = res.monitors || [];
      el.innerHTML = `<div class="mon-head">💡 当前 Pager 连接：${list.length}</div>` +
        (list.length ? list.map((m) => monitorRowHtml(sid, m)).join('') : '<div class="mon-meta">该分区暂无 Pager 连接</div>') +
        `<div class="mon-actions"><button class="btn danger" data-act="mon-clear" data-sid="${sid}"${list.length ? '' : ' disabled'}>清除本分区 Pager 连接</button></div>`;
    } catch (e) {
      el.innerHTML = `<div class="mon-meta">Pager 读取失败：${esc(e.message)}</div>`;
    }
  }));
}

// clearAllMonitors 清全部 Pager 连接（管理面，带 all=1）
async function clearAllMonitors() {
  const n = document.querySelectorAll('.mon-row').length;
  if (!confirm(`确定清除全部 Pager 连接共 ${n} 个？所有会话都不再收到变化通知。`)) return;
  try {
    await api('DELETE', '/api/monitors?all=1');
    toast(`已清除全部 ${n} 个 Pager 连接`);
    await afterMonitorChange();
  } catch (err) {
    toast(err.message, true);
  }
}

async function afterMonitorChange() {
  await refreshAll();
  await renderSectionAdmin();
}

async function renderSectionAdmin() {
  try {
    const res = await api('GET', '/api/sections');
    const rows = (res.sections || []).map((s) => `<div class="admin-row" data-sid="${s.id}">
      <span class="admin-name">${esc(s.name)} <span class="admin-id">#${s.id}</span></span>
      <span class="admin-count">${s.total || 0} 条任务</span>
      <button class="btn danger" data-del="${s.id}">🗑️ 删除</button>
    </div>
    <div class="mon-admin" data-mon="${s.id}"><div class="mon-meta">Pager：读取中…</div></div>`).join('');
    $('#sectionAdmin').innerHTML = rows || '<div class="secrow-key">还没有分区</div>';
    loadMonitorPanels();
  } catch (e) {
    $('#sectionAdmin').textContent = '读取分区失败：' + e.message;
  }
}

async function deleteSection(sid, name, count) {
  if (!window.confirm(`将删除分区「${name}」，并连同 ${count} 条任务一起删除，不可恢复。确定删除？`)) return;
  try {
    const res = await api('DELETE', `/api/sections/${sid}`);
    toast(`已删除分区，连同 ${(res && res.deleted_tasks) || 0} 条任务`);
    await renderSectionAdmin();
    await refreshAll();
  } catch (e) {
    toast(e.message, true);
  }
}

async function openSettings() {
  $('#settingsModal').hidden = false;
  try {
    const [bark, settings] = await Promise.all([
      api('GET', '/api/bark'),
      api('GET', '/api/settings'),
    ]);
    state.bark = bark;
    state.settings = settings;
    fillBark();
    $('#pollSec').value = settings.poll_interval_sec || 5;
    $('#aionSkipSec').value = settings.skip_interval_sec || 60;
    $('#versionInfo').textContent = `版本 ${settings.version} · 数据文件 ${settings.db_path}`;
  } catch (e) {
    toast(e.message, true);
  }
  renderSectionAdmin();
}

(async function init() {
  bind();
  try {
    state.settings = await api('GET', '/api/settings');
  } catch (e) {
    /* 默认 5 秒 */
  }
  loadStats();
  await loadSections(false);
  connectSSE();
  if (location.hash === '#settings') openSettings();
})();