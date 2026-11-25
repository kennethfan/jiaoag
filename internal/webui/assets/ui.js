// 视图层：把 state 画成 DOM。只读 state，不发起任何协议请求；
// 用户动作通过 handlers 回调交给 main.js。
//
// 整页重绘（对话、标签、状态）而不是细粒度 diff：单会话最多几百条消息，
// 重绘成本远小于维护两套「增量更新」逻辑的风险。流式增量用 rAF 节流。

import { state, currentTab, statusLabel, humanTokens } from './store.js';
import { ST } from './protocol.js';

const $ = (id) => document.getElementById(id);

let handlers = {};

export function initUi(h) {
  handlers = h;
  $('btn-new').onclick = () => h.newSession();
  $('btn-help').onclick = () => h.openHelp();
  $('btn-sessions').onclick = () => h.openSessions();
  $('btn-models').onclick = () => h.openModels(false);
  $('btn-once-model').onclick = () => h.openModels(true);
  $('btn-skills').onclick = () => h.openSkills();
  $('btn-mcp').onclick = () => h.openMcp();
  $('btn-roots').onclick = () => h.openRoots();
  $('btn-send').onclick = () => h.submitPrompt();
  $('btn-cancel').onclick = () => h.cancelTask();
  $('model-chip').onclick = () => h.openModels(false);
  $('modal-close').onclick = () => closeModal();
  $('modal').onclick = (ev) => {
    if (ev.target === $('modal')) closeModal();
  };

  const input = $('input');
  input.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter' && !ev.shiftKey) {
      ev.preventDefault();
      h.submitPrompt();
    }
  });
  input.addEventListener('input', () => autoGrow(input));
}

function autoGrow(el) {
  el.style.height = 'auto';
  el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
}

export function focusInput() {
  $('input').focus();
}

export function readInput() {
  const el = $('input');
  const text = el.value;
  el.value = '';
  autoGrow(el);
  return text.trim();
}

export function setInput(text) {
  const el = $('input');
  el.value = text;
  autoGrow(el);
  el.focus();
}

// ---------- 整页 ----------

export function render() {
  renderTabs();
  renderHeader();
  renderLog();
  renderApproval();
  renderComposer();
}

function renderTabs() {
  const box = $('tabs');
  box.textContent = '';
  for (const t of state.tabs) {
    const b = document.createElement('button');
    b.className = 'tab' + (t.id === state.currentId ? ' active' : '');
    const title = document.createElement('span');
    title.textContent = t.title || '(未命名)';
    b.appendChild(title);
    const marker = tabMarker(t);
    if (marker) {
      const m = document.createElement('span');
      m.className = 'marker ' + marker.tone;
      m.textContent = marker.text;
      b.appendChild(m);
    }
    b.title = t.title || t.id;
    b.onclick = () => handlers.switchSession(t.id);
    box.appendChild(b);
  }
}

function tabMarker(t) {
  if (t.pending > 0) return { text: '!', tone: 'warn' };
  if (t.status === ST.running) return { text: '●', tone: 'run' };
  if (t.status === ST.queued) return { text: '◌', tone: 'warn' };
  if (t.status === ST.failed) return { text: '✖', tone: 'err' };
  if (t.unread > 0) return { text: `·${t.unread}`, tone: 'muted' };
  return null;
}

function renderHeader() {
  const t = currentTab();
  const chip = $('model-chip');
  if (t?.model) {
    chip.textContent = t.provider ? `${t.provider}/${t.model}` : t.model;
    chip.hidden = false;
  } else {
    chip.textContent = '模型 —';
  }
  const parts = [];
  if (state.ctxTokens > 0) {
    const budget = state.ctxBudget > 0 ? `/${humanBudget(state.ctxBudget)}` : '';
    parts.push(`ctx ${humanTokens(state.ctxTokens)}${budget}`);
  }
  if (state.tokens > 0) parts.push(`${humanTokens(state.tokens)} tok`);
  if (state.round > 0) parts.push(`第 ${state.round} 轮`);
  $('usage-chip').textContent = parts.join(' · ');

  const conn = $('conn');
  const map = { connecting: ['连接中…', 'warn'], open: ['已连接', 'ok'], closed: [state.connDetail || '未连接', 'err'] };
  const [text, tone] = map[state.conn] || ['—', 'muted'];
  conn.textContent = text;
  conn.className = `conn ${tone}`;

  const badge = $('project-badge');
  const p = state.project;
  if (p && p.detected) {
    badge.hidden = false;
    if (p.loaded) {
      badge.textContent = '项目扩展已生效';
      badge.className = 'badge ok';
      badge.onclick = () => handlers.openProject();
      badge.title = p.summary || '';
    } else {
      badge.textContent = '项目扩展未确认 · 启用';
      badge.className = 'badge warn';
      badge.onclick = () => handlers.trustProject();
      badge.title = p.summary || '';
    }
  } else {
    badge.hidden = true;
  }
}

function humanBudget(n) {
  if (n >= 1_000_000) return `${Math.round(n / 1_000_000)}M`;
  if (n >= 1000) return `${Math.round(n / 1000)}k`;
  return String(n);
}

// ---------- 对话 ----------

function renderLog() {
  const log = $('log');
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 60;
  const frag = document.createDocumentFragment();
  for (const m of state.messages) frag.appendChild(renderItem(m));
  if (state.stream) frag.appendChild(renderStream());
  log.replaceChildren(frag);
  if (atBottom) log.scrollTop = log.scrollHeight;
}

function renderStream() {
  const wrap = document.createElement('div');
  wrap.className = 'msg assistant streaming';
  wrap.appendChild(label('jiaoag'));
  const body = document.createElement('div');
  body.className = 'text';
  body.textContent = state.stream;
  const caret = document.createElement('span');
  caret.className = 'caret';
  body.appendChild(caret);
  wrap.appendChild(body);
  return wrap;
}

function label(text, tone) {
  const el = document.createElement('div');
  el.className = 'label ' + (tone || '');
  el.textContent = text;
  return el;
}

function renderItem(m) {
  switch (m.kind) {
    case 'prompt': {
      const el = document.createElement('div');
      el.className = 'msg prompt';
      const body = document.createElement('div');
      body.className = 'text';
      body.textContent = m.text;
      el.appendChild(body);
      const meta = [];
      if (m.model) meta.push(`单次模型 ${m.model}`);
      if (m.skills?.length) meta.push(`技能 ${m.skills.join(', ')}`);
      if (meta.length) {
        const sub = document.createElement('div');
        sub.className = 'meta';
        sub.textContent = meta.join(' · ');
        el.appendChild(sub);
      }
      return el;
    }
    case 'assistant': {
      const el = document.createElement('div');
      el.className = 'msg assistant';
      el.appendChild(label('jiaoag'));
      const body = document.createElement('div');
      body.className = 'text';
      body.textContent = m.text;
      el.appendChild(body);
      return el;
    }
    case 'tool': {
      const el = document.createElement('div');
      el.className = 'line tool';
      const name = document.createElement('span');
      name.className = 'tool-name';
      name.textContent = `⚙ ${m.name}`;
      el.appendChild(name);
      if (m.args) {
        const args = document.createElement('span');
        args.className = 'tool-args';
        args.textContent = compact(m.args, 200);
        el.appendChild(args);
      }
      return el;
    }
    case 'result': {
      const el = document.createElement('div');
      el.className = 'line result ' + (m.status === '出错' || m.status === '已拒绝' ? 'err' : m.status === '已取消' ? 'muted' : 'ok');
      const head = document.createElement('div');
      let text = `↳ ${m.tool} ${m.status}（${m.durationMs} ms）`;
      if (m.exitCode !== undefined && m.exitCode !== null) text += ` · 退出码 ${m.exitCode}`;
      if (m.truncated) text += ' · 输出已截断';
      if (m.outputPath) text += ' · 全文已落盘';
      head.textContent = text;
      el.appendChild(head);
      if (m.output) {
        const details = document.createElement('details');
        const summary = document.createElement('summary');
        summary.textContent = '输出';
        details.appendChild(summary);
        const pre = document.createElement('pre');
        pre.textContent = m.output;
        details.appendChild(pre);
        // 短输出默认展开；长输出折叠，避免刷屏。
        if (m.output.split('\n').length <= 12) details.open = true;
        el.appendChild(details);
      }
      return el;
    }
    case 'status': {
      const el = document.createElement('div');
      el.className = 'line status';
      el.textContent = `— ${m.from} → ${m.to}${m.reason ? `（${m.reason}）` : ''}`;
      return el;
    }
    default: {
      const el = document.createElement('div');
      el.className = 'line note ' + (m.tone || '');
      el.textContent = m.text;
      return el;
    }
  }
}

function compact(s, n) {
  const t = s.replace(/\s+/g, ' ').trim();
  return t.length > n ? t.slice(0, n) + '…' : t;
}

// ---------- 审批 ----------

function renderApproval() {
  const box = $('approval');
  const a = state.approval;
  if (!a) {
    box.hidden = true;
    box.replaceChildren();
    return Promise.resolve();
  }
  box.hidden = false;
  const p = a.payload;
  box.replaceChildren();

  const head = document.createElement('div');
  head.className = 'approval-head';
  const title = document.createElement('span');
  const kindLabel = p.kind === 'command' ? '待审批命令' : p.kind === 'external' ? '待审批外部工具' : '待审批写入';
  title.textContent = `${kindLabel}${p.path ? `：${p.path}` : ''}`;
  title.className = 'approval-title';
  head.appendChild(title);
  if (p.kind !== 'command' && p.kind !== 'external' && a.hunks.length > 1) {
    const btn = document.createElement('button');
    btn.className = 'link';
    btn.textContent = a.selectMode ? '返回全量视图' : `逐块（${a.selected.size}/${a.hunks.length}）`;
    btn.onclick = () => {
      a.selectMode = !a.selectMode;
      render();
    };
    head.appendChild(btn);
  }
  box.appendChild(head);

  if (p.kind === 'command') box.appendChild(renderCommandApproval(p));
  else if (p.kind === 'external') box.appendChild(renderExternalApproval(p));
  else box.appendChild(renderDiffApproval(a));

  if (p.truncated) {
    const warn = document.createElement('p');
    warn.className = 'warn-text';
    warn.textContent = 'diff 过长已截断：批准前请用编辑器核对全文。';
    box.appendChild(warn);
  }

  const foot = document.createElement('div');
  foot.className = 'approval-foot';
  const mk = (text, cls, fn) => {
    const b = document.createElement('button');
    b.textContent = text;
    b.className = cls;
    b.onclick = fn;
    return b;
  };
  foot.appendChild(mk('批准一次', 'primary', () => handlers.resolveApproval('allow_once')));
  foot.appendChild(mk('本会话内都批准', '', () => handlers.resolveApproval('allow_session')));
  foot.appendChild(mk('拒绝', 'danger', () => handlers.resolveApproval('deny')));
  if (a.selectMode) {
    const info = document.createElement('span');
    info.className = 'muted small';
    info.textContent = `只写入勾中的 ${a.selected.size} 块；未勾的块保持原内容`;
    foot.appendChild(info);
  }
  box.appendChild(foot);
  return Promise.resolve();
}

function renderDiffApproval(a) {
  const p = a.payload;
  const wrap = document.createElement('div');
  wrap.className = 'diff';
  if (a.hunks.length === 0) {
    // 兜底：解析不出块（例如新文件没有 @@ 之外的内容）时按原文显示。
    wrap.textContent = p.diff || '';
    return wrap;
  }
  a.hunks.forEach((h, i) => {
    const sec = document.createElement('section');
    sec.className = 'hunk';
    const hh = document.createElement('header');
    if (a.selectMode) {
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = a.selected.has(i);
      cb.onchange = () => {
        if (cb.checked) a.selected.add(i);
        else a.selected.delete(i);
        render();
      };
      hh.appendChild(cb);
    }
    const htext = document.createElement('code');
    htext.textContent = h.header;
    hh.appendChild(htext);
    sec.appendChild(hh);
    const pre = document.createElement('pre');
    for (const [cls, text] of h.lines) {
      const line = document.createElement('div');
      line.className = `dline ${cls}`;
      line.textContent = (cls === 'add' ? '+' : cls === 'del' ? '-' : ' ') + text;
      pre.appendChild(line);
    }
    sec.appendChild(pre);
    if (a.selectMode && !a.selected.has(i)) sec.classList.add('skipped');
    wrap.appendChild(sec);
  });
  return wrap;
}

function renderCommandApproval(p) {
  const wrap = document.createElement('div');
  wrap.className = 'cmd';
  const pre = document.createElement('pre');
  pre.textContent = p.shell ? p.shell : (p.command || []).join(' ');
  wrap.appendChild(pre);
  const meta = document.createElement('div');
  meta.className = 'meta-grid';
  const rows = [
    ['cwd', p.cwd],
    ['backend', p.backend],
    ['timeout', p.timeout_ms ? `${(p.timeout_ms / 1000).toFixed(0)}s` : ''],
    ['判定', p.reason],
  ];
  for (const [k, v] of rows) {
    if (!v) continue;
    const dt = document.createElement('span');
    dt.className = 'k';
    dt.textContent = k;
    const dd = document.createElement('span');
    dd.className = 'v';
    dd.textContent = v;
    meta.appendChild(dt);
    meta.appendChild(dd);
  }
  wrap.appendChild(meta);
  return wrap;
}

function renderExternalApproval(p) {
  const wrap = document.createElement('div');
  wrap.className = 'cmd';
  const pre = document.createElement('pre');
  pre.textContent = `${p.tool || ''}（${p.source || ''}）\n${p.args || ''}`;
  wrap.appendChild(pre);
  if (p.reason) {
    const r = document.createElement('p');
    r.className = 'muted small';
    r.textContent = p.reason;
    wrap.appendChild(r);
  }
  return wrap;
}

// ---------- 输入区 ----------

function renderComposer() {
  const cancel = $('btn-cancel');
  cancel.hidden = !(state.runTaskId || state.taskStatus === ST.queued);
  const chip = $('queue-chip');
  if (state.queue.length > 0) {
    chip.hidden = false;
    chip.textContent = `排队 ${state.queue.length}`;
  } else {
    chip.hidden = true;
  }
  const armed = [];
  if (state.forcedModel) armed.push(`下一条模型 ${state.forcedModel}`);
  if (state.forcedSkills.length) armed.push(`技能 ${state.forcedSkills.join(',')}`);
  $('armed').textContent = armed.join(' · ');
}

// ---------- 模态框 ----------

export function openModal(title, body, foot) {
  $('modal-title').textContent = title;
  const bodyBox = $('modal-body');
  bodyBox.replaceChildren(body);
  const footBox = $('modal-foot');
  footBox.replaceChildren();
  if (foot) for (const b of foot) footBox.appendChild(b);
  $('modal').hidden = false;
  closeModal.haystack = null;
}

export function closeModal() {
  $('modal').hidden = true;
  $('modal-body').replaceChildren();
  $('modal-foot').replaceChildren();
  focusInput();
}

export function isModalOpen() {
  return !$('modal').hidden;
}

export function modalButton(text, cls, fn, disabled = false) {
  const b = document.createElement('button');
  b.textContent = text;
  b.className = cls || '';
  b.disabled = disabled;
  b.onclick = fn;
  return b;
}

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

export function showGateError(msg) {
  const box = $('gate-error');
  box.hidden = false;
  box.textContent = msg;
}

export function showApp() {
  $('gate').hidden = true;
  $('app').hidden = false;
}

export function tokenValue() {
  return $('gate-token').value.trim();
}

export function rememberToken() {
  return $('gate-remember').checked;
}
