'use strict';

const $ = (sel) => document.querySelector(sel);

const params = new URLSearchParams(location.search);
const sectionId = Number(params.get('sid') || 0);
const taskId = Number(params.get('tid') || 0);

let section = null;
let task = null;

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
  }, 2500);
}

function hideWindow() {
  fetch('/api/window/hide?target=editor', { method: 'POST' }).catch(() => {});
}

async function load() {
  const query = `sid=${sectionId}${taskId ? `&tid=${taskId}` : ''}`;
  const res = await api('GET', `/api/editor/context?${query}`);
  section = res.section || null;
  task = res.task || null;

  $('#edHeader').textContent = task ? '✏️ 编辑任务' : '➕ 新建任务';

  $('#edTitle').value = task ? task.title : '';
  $('#edDesc').value = task ? task.description : '';
  $('#edDue').value = task && task.due_at ? task.due_at : '';
  $('#edBark').value = (task && task.bark_override) || 'inherit';
  $('#edSection').textContent = section ? `所属分区：${section.name}` : '';

  $('#edTitle').focus();
  $('#edTitle').select();

  fetch('/api/editor/ready', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title: task ? '编辑任务' : '新建任务' }),
  }).catch(() => {});
}

async function save() {
  const title = $('#edTitle').value.trim();
  if (!title) {
    toast('请填写任务标题', true);
    $('#edTitle').focus();
    return;
  }
  const payload = {
    title,
    description: $('#edDesc').value,
    due: $('#edDue').value || null,
    bark_override: $('#edBark').value || 'inherit',
  };
  const btn = $('#btnSave');
  btn.disabled = true;
  try {
    if (task) {
      await api('PUT', `/api/board/sections/${sectionId}/tasks/${task.id}`, payload);
    } else {
      const created = await api('POST', `/api/board/sections/${sectionId}/tasks`, payload);
      task = created.task || null;
    }
    hideWindow();
  } catch (e) {
    toast(e.message, true);
  } finally {
    btn.disabled = false;
  }
}

$('#btnSave').addEventListener('click', save);
$('#btnCancel').addEventListener('click', hideWindow);
$('#btnClose').addEventListener('click', hideWindow);

$('#titlebar').addEventListener('pointerdown', (e) => {
  if (e.button !== 0 || e.target.closest('.winbtn')) return;
  fetch('/api/window/drag?target=editor', { method: 'POST' }).catch(() => {});
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    hideWindow();
  } else if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    save();
  }
});

load().catch((e) => {
  toast(e.message, true);
  fetch('/api/editor/ready', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title: task ? '编辑任务' : '新建任务' }),
  }).catch(() => {});
});

/* ---------------- 无边框窗边缘缩放（编辑窗口） ---------------- */

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
  fetch(`/api/window/resize/start?edge=${edge}&target=editor`, { method: 'POST' }).catch(() => {});
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
