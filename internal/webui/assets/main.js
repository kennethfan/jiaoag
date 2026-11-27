// 入口：鉴权闸门 → 连接 → 恢复本目录会话 → 事件驱动渲染。
//
// 所有用户动作都在这里翻译成协议帧；视图层只负责画。

import { T, E, ST, isTerminal } from './protocol.js';
import { Link } from './net.js';
import {
  state, subscribe, notify, applyEvent, currentTab, tabById, upsertTab, removeTab, setTabTitle,
  resetView, push, noteBackground, setApproval, decisionLabel, statusLabel, humanTokens,
  syncCurrentTab,
} from './store.js';
import {
  initUi, render, focusInput, readInput, setInput, openModal, closeModal, isModalOpen,
  modalButton, el, showGateError, showApp, tokenValue, rememberToken,
} from './ui.js';

const TOKEN_KEY = 'jiaoag.token';
let link = null;
let restoring = false;
let pendingModal = null; // 'sessions' | 'sessions-all' | 'models' | 'skills' | 'mcp' | 'procs'
let modelsForTask = false;

// rAF 节流：流式 delta 每秒几十条，直接重绘会卡。
let raf = 0;
function scheduleRender() {
  if (raf) return;
  raf = requestAnimationFrame(() => {
    raf = 0;
    render();
  });
}

// ---------- 启动 ----------

initUi({
  newSession: openNewSession,
  openHelp,
  openSessions,
  openModels,
  openSkills,
  openMcp,
  openRoots,
  openProcs,
  openProject,
  trustProject,
  submitPrompt,
  cancelTask,
  switchSession,
  resolveApproval,
});

subscribe(scheduleRender);

document.getElementById('gate-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const token = tokenValue();
  if (!token) return;
  try {
    const info = await fetchInfo(token);
    if (rememberToken()) localStorage.setItem(TOKEN_KEY, token);
    state.workspace = info.workspace;
    state.serverVersion = info.version;
    showApp();
    connect(token);
  } catch (err) {
    showGateError(`连接失败：${err.message}`);
  }
});

(async function boot() {
  const saved = localStorage.getItem(TOKEN_KEY);
  if (!saved) return;
  try {
    const info = await fetchInfo(saved);
    state.workspace = info.workspace;
    state.serverVersion = info.version;
    document.getElementById('gate-token').value = saved;
    showApp();
    connect(saved);
  } catch {
    localStorage.removeItem(TOKEN_KEY);
  }
})();

async function fetchInfo(token) {
  const res = await fetch('/api/info', { headers: { Authorization: `Bearer ${token}` } });
  if (!res.ok) throw new Error(res.status === 401 ? '令牌无效' : `HTTP ${res.status}`);
  return res.json();
}

function connect(token) {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  link = new Link({
    url: `${scheme}://${location.host}/ws`,
    token,
    onFrame: handleFrame,
    onState: (s, detail) => {
      state.conn = s;
      state.connDetail = detail || '';
      if (s === 'open') onOpen();
      scheduleRender();
    },
  });
  link.connect();
}

function onOpen() {
  if (state.currentId) {
    // 重连：从 lastSeq 增量续传，服务端会用 event.snapshot 补待审批。
    link.send(T.eventSubscribe, { session_id: state.currentId, from_seq: state.lastSeq });
    return;
  }
  restoring = true;
  link.send(T.sessionManage, { workspace: state.workspace });
}

// ---------- 帧分发 ----------

function handleFrame(env) {
  switch (env.type) {
    case T.hello:
      state.serverVersion = env.payload?.server_version || state.serverVersion;
      document.title = `jiaoag ${state.serverVersion}`;
      break;
    case T.error:
      push({ kind: 'note', tone: 'err', text: `${env.payload?.code || 'error'}：${env.payload?.message || ''}` });
      break;
    case T.sessionCreated:
      adoptSession(env.payload);
      break;
    case T.sessionManageResult:
      handleSessions(env.payload?.sessions || []);
      break;
    case T.sessionRenamed:
      setTabTitle(env.payload.id, env.payload.title);
      push({ kind: 'note', text: env.payload.title ? `已改名：${env.payload.title}` : '已清空标题' });
      break;
    case T.sessionDeleted:
      handleDeleted(env.payload.session_id);
      break;
    case T.sessionUpdated:
      handleSessionUpdated(env.payload);
      break;
    case T.providerModelsResult:
      state.models = env.payload?.models || [];
      state.modelSource = env.payload?.source || '';
      renderModelsModal();
      break;
    case T.skillListResult:
      state.skills = env.payload?.skills || [];
      renderSkillsModal();
      break;
    case T.mcpListResult:
      state.mcp = env.payload?.servers || [];
      renderMcpModal();
      break;
    case T.procListResult:
      state.procs = env.payload?.procs || [];
      renderProcsModal();
      break;
    case T.procActionResult:
      push({
        kind: 'note',
        tone: 'ok',
        text: env.payload?.message || '已处理后台进程',
      });
      // 动作是异步生效的（远程属主最多 1 秒才真正杀掉），拉两次列表：
      // 只看一次可能还停在 running。
      if (pendingModal === 'procs') {
        setTimeout(openProcs, 400);
        setTimeout(openProcs, 1600);
      }
      break;
    case T.projectTrusted:
      state.project = env.payload?.project || null;
      push({
        kind: 'note',
        tone: state.project?.trusted ? 'ok' : '',
        text: state.project?.trusted
          ? `已信任项目级扩展：${state.project.summary || ''}`
          : '已撤销项目级扩展信任，下一条任务起不再加载',
      });
      break;
    case T.taskQueued:
      state.taskId = env.payload.id;
      if (!state.runTaskId) state.taskStatus = env.payload.status || ST.queued;
      if (!state.queue.find((q) => q.id === env.payload.id)) {
        state.queue.push({ id: env.payload.id, prompt: env.payload.prompt || '', at: Date.now() });
      }
      syncCurrentTab();
      break;
    case T.taskCanceled:
      push({ kind: 'note', text: '已请求取消（等终态确认）' });
      break;
    case T.eventSnapshot:
      handleSnapshot(env.payload);
      break;
    case T.event:
      if (env.session_id && env.session_id !== state.currentId) noteBackground(env);
      else applyEvent(env);
      break;
  }
  scheduleRender();
}

function handleSnapshot(p) {
  if (!p) return;
  if (p.reset) {
    resetView();
    link.send(T.eventSubscribe, { session_id: state.currentId, from_seq: 0 });
    return;
  }
  for (const ev of p.pending || []) {
    if (ev.kind === E.approvalRequested) setApproval(ev.payload);
  }
}

function adoptSession(p) {
  if (!p) return;
  if (state.currentId && state.currentId !== p.id) {
    syncTab();
    resetView();
  }
  state.currentId = p.id;
  state.project = p.project || null;
  state.roots = p.roots || [];
  const t = upsertTab({ id: p.id, title: p.title || '', provider: p.provider || '', model: p.model || '', status: '' });
  if (t) {
    t.pending = 0;
    t.unread = 0;
  }
  push({ kind: 'note', text: p.title ? `已进入会话：${p.title}` : '已进入会话（未命名，可在「会话」里改名）' });
  link.send(T.eventSubscribe, { session_id: p.id, from_seq: 0 });
}

function handleSessions(sessions) {
  state.sessions = sessions;
  if (restoring) {
    restoring = false;
    restoreSessions(sessions);
    return;
  }
  if (pendingModal === 'sessions' || pendingModal === 'sessions-all') {
    renderSessionsModal();
  }
}

function restoreSessions(sessions) {
  if (sessions.length === 0) {
    link.send(T.sessionCreate, {
      workspace: state.workspace,
      roots: state.roots.map((r) => ({ alias: r.alias, path: r.path })),
      provider: '',
      model: '',
    });
    return;
  }
  // 服务端按 updated_at 倒序给：标签按时间正序（新的在右），接到最新那条。
  for (let i = sessions.length - 1; i >= 0; i--) {
    const s = sessions[i];
    const t = upsertTab({ id: s.id, title: s.title, provider: s.provider, model: s.model, status: '' });
    if (t && s.active_tasks > 0) t.status = ST.running;
  }
  const cur = sessions[0];
  state.currentId = cur.id;
  push({ kind: 'note', text: cur.title ? `继续会话：${cur.title}` : '继续会话（未命名）' });
  if (cur.model) push({ kind: 'note', text: `模型：${cur.provider ? cur.provider + '/' : ''}${cur.model}` });
  if (sessions.length > 1) push({ kind: 'note', text: `本目录有 ${sessions.length} 条会话，已全部开成标签` });
  link.send(T.eventSubscribe, { session_id: cur.id, from_seq: 0 });
}

function handleDeleted(id) {
  removeTab(id);
  push({ kind: 'note', text: '已删除会话' });
  if (id !== state.currentId) return;
  state.currentId = null;
  resetView();
  const next = state.tabs[state.tabs.length - 1];
  if (next) {
    switchSession(next.id);
  } else {
    link.send(T.sessionCreate, { workspace: state.workspace });
  }
}

function handleSessionUpdated(p) {
  const t = currentTab();
  if (t) {
    if (p.provider) t.provider = p.provider;
    if (p.model) t.model = p.model;
  }
  push({ kind: 'note', text: `模型已切换：${p.model}` });
}

function syncTab() {
  const t = currentTab();
  if (!t) return;
  t.status = state.taskStatus;
}

// ---------- 用户动作 ----------

function submitPrompt() {
  const text = readInput();
  if (!text) return;
  if (!state.currentId) {
    push({ kind: 'note', tone: 'err', text: '还没进入会话，稍后再试' });
    scheduleRender();
    return;
  }
  const t = currentTab();
  link.send(T.taskSubmit, {
    session_id: state.currentId,
    text,
    skills: state.forcedSkills,
    provider: state.forcedModel ? (t?.provider || '') : '',
    model: state.forcedModel || '',
  });
  state.forcedModel = '';
  state.forcedSkills = [];
  scheduleRender();
}

function cancelTask() {
  if (!state.currentId) return;
  const id = state.runTaskId || state.taskId;
  if (!id) return;
  link.send(T.taskCancel, { session_id: state.currentId, task_id: id });
}

function switchSession(id) {
  if (id === state.currentId) {
    closeModal();
    return;
  }
  closeModal();
  syncTab();
  resetView();
  state.currentId = id;
  const t = tabById(id);
  if (t) {
    t.unread = 0;
    state.roots = [];
  }
  push({ kind: 'note', text: t?.title ? `切换到会话：${t.title}` : '切换到会话' });
  // 不退订旧会话：后台任务的徽标要继续更新（与 TUI 一致）。
  link.send(T.eventSubscribe, { session_id: id, from_seq: 0 });
}

function closeTab(id) {
  const wasCurrent = id === state.currentId;
  link.send(T.eventUnsubscribe, { session_id: id });
  removeTab(id);
  if (!wasCurrent) {
    scheduleRender();
    return;
  }
  state.currentId = null;
  resetView();
  const next = state.tabs[state.tabs.length - 1];
  if (next) switchSession(next.id);
  else link.send(T.sessionCreate, { workspace: state.workspace });
}

function resolveApproval(decision) {
  const a = state.approval;
  if (!a) return;
  const payload = {
    session_id: state.currentId,
    approval_id: a.payload.approval_id,
    decision,
  };
  // 只勾了部分块：带上下标（0 起）。allow_session 与 hunks 同时出现时
  // 服务端会降级为 allow_once 且不写白名单（ADR-0028）。
  if (a.selectMode && a.selected.size > 0 && a.selected.size < a.hunks.length) {
    payload.hunks = [...a.selected].sort((x, y) => x - y);
  }
  link.send(T.approvalResolve, payload);
  state.approval = null;
  scheduleRender();
}

// ---------- 模态框 ----------

function openNewSession() {
  const body = document.createDocumentFragment();
  body.appendChild(el('p', 'muted', '新会话的工作区（默认当前服务端目录）。'));
  const input = document.createElement('input');
  input.type = 'text';
  input.value = state.workspace;
  input.className = 'wide';
  body.appendChild(input);
  openModal('新开会话', body, [
    modalButton('取消', '', () => closeModal()),
    modalButton('创建', 'primary', () => {
      link.send(T.sessionCreate, { workspace: input.value.trim() });
      closeModal();
    }),
  ]);
  input.focus();
}

function openSessions() {
  pendingModal = 'sessions';
  link.send(T.sessionManage, { workspace: state.workspace });
}

function renderSessionsModal() {
  const all = pendingModal === 'sessions-all';
  const body = document.createDocumentFragment();
  const bar = el('div', 'modal-bar');
  const toggle = modalButton(all ? '只看本目录' : '所有工作区', 'link', () => {
    pendingModal = all ? 'sessions' : 'sessions-all';
    link.send(T.sessionManage, all ? { workspace: state.workspace } : { all: true });
  });
  bar.appendChild(toggle);
  body.appendChild(bar);

  const list = el('div', 'session-list');
  if (state.sessions.length === 0) {
    list.appendChild(el('p', 'muted', '没有会话'));
  }
  for (const s of state.sessions) {
    const row = el('div', 'session-row' + (s.id === state.currentId ? ' active' : ''));
    const main = el('div', 'session-main');
    const title = el('div', 'session-title', s.title || '(未命名)');
    main.appendChild(title);
    const meta = el('div', 'session-meta');
    const bits = [s.id.slice(0, 12), `${s.tasks} 任务`];
    if (s.active_tasks > 0) bits.push(`${s.active_tasks} 进行中`);
    if (s.model) bits.push(`${s.provider ? s.provider + '/' : ''}${s.model}`);
    if (all) bits.push(s.workspace);
    meta.textContent = bits.join(' · ');
    main.appendChild(meta);
    row.appendChild(main);

    const actions = el('div', 'session-actions');
    if (s.id !== state.currentId) {
      actions.appendChild(modalButton('打开', 'link', () => switchSession(s.id)));
    } else {
      actions.appendChild(el('span', 'chip', '当前'));
    }
    actions.appendChild(modalButton('改名', 'link', () => {
      const title = window.prompt('新标题（留空则清除）', s.title || '');
      if (title === null) return;
      link.send(T.sessionRename, { session_id: s.id, title: title.trim() });
      setTimeout(() => link.send(T.sessionManage, { workspace: all ? undefined : state.workspace, all }), 200);
    }));
    if (state.tabs.some((t) => t.id === s.id)) {
      actions.appendChild(modalButton('关标签', 'link', () => {
        closeTab(s.id);
        openSessions();
      }));
    }
    actions.appendChild(modalButton('删除', 'link danger', () => {
      if (!window.confirm(`删除会话 ${s.title || s.id}？任务、事件与审计会一起删除且不可撤销。`)) return;
      link.send(T.sessionDelete, { session_id: s.id });
      setTimeout(() => link.send(T.sessionManage, { workspace: all ? undefined : state.workspace, all }), 200);
    }));
    row.appendChild(actions);
    list.appendChild(row);
  }
  body.appendChild(list);
  openModal('会话', body, [modalButton('新开会话', 'primary', openNewSession), modalButton('关闭', '', () => closeModal())]);
}

function openModels(forTask) {
  modelsForTask = forTask;
  pendingModal = 'models';
  if (!state.currentId) {
    push({ kind: 'note', tone: 'err', text: '还没进入会话' });
    scheduleRender();
    return;
  }
  link.send(T.providerModels, { session_id: state.currentId });
}

function renderModelsModal() {
  if (pendingModal !== 'models') return;
  const body = document.createDocumentFragment();
  const hint = el('p', 'muted small', modelsForTask
    ? '选中后只用于下一条任务，提交后自动恢复会话模型。'
    : `切换当前会话的模型（来源：${state.modelSource === 'config' ? '配置' : '供应商接口'}）`);
  body.appendChild(hint);
  const list = el('div', 'model-list');
  const t = currentTab();
  const current = modelsForTask ? (state.forcedModel || t?.model || '') : (t?.model || '');
  for (const m of state.models) {
    const b = modalButton(m + (m === current ? '  (当前)' : ''), 'model-item' + (m === current ? ' active' : ''), () => {
      applyModel(m);
    });
    list.appendChild(b);
  }
  body.appendChild(list);
  openModal(modelsForTask ? '单次模型' : '会话模型', body, [modalButton('关闭', '', () => closeModal())]);
}

function applyModel(model) {
  if (modelsForTask) {
    state.forcedModel = model;
    push({ kind: 'note', text: `下一条任务使用模型：${model}` });
    closeModal();
    scheduleRender();
    return;
  }
  link.send(T.sessionUpdate, { session_id: state.currentId, provider: currentTab()?.provider || '', model });
  closeModal();
}

function openSkills() {
  if (!state.currentId) {
    push({ kind: 'note', tone: 'err', text: '还没进入会话' });
    scheduleRender();
    return;
  }
  pendingModal = 'skills';
  link.send(T.skillList, { session_id: state.currentId });
}

function renderSkillsModal() {
  if (pendingModal !== 'skills') return;
  const body = document.createDocumentFragment();
  body.appendChild(el('p', 'muted small', '选中后，下一条任务会先加载该技能正文（可反复点选切换）。'));
  const list = el('div', 'model-list');
  for (const s of state.skills) {
    const armed = state.forcedSkills.includes(s.name);
    const label = `${s.name}${s.manual ? ' [仅手动]' : ''}${armed ? '  ✓' : ''}`;
    const b = modalButton(label, 'model-item' + (armed ? ' active' : ''), () => {
      state.forcedSkills = armed ? [] : [s.name];
      renderSkillsModal();
      scheduleRender();
    });
    if (s.description) b.title = s.description;
    list.appendChild(b);
    if (s.description) list.appendChild(el('div', 'muted small skill-desc', s.description));
  }
  body.appendChild(list);
  openModal('技能', body, [
    modalButton('清除指定', '', () => {
      state.forcedSkills = [];
      closeModal();
      scheduleRender();
    }),
    modalButton('关闭', 'primary', () => closeModal()),
  ]);
}

function openMcp() {
  if (!state.currentId) {
    push({ kind: 'note', tone: 'err', text: '还没进入会话' });
    scheduleRender();
    return;
  }
  pendingModal = 'mcp';
  link.send(T.mcpList, { session_id: state.currentId });
}

function renderMcpModal() {
  if (pendingModal !== 'mcp') return;
  const body = document.createDocumentFragment();
  body.appendChild(el('p', 'muted small', '只读：未连接的 server 会在首次调用工具时启动。'));
  for (const s of state.mcp) {
    const row = el('div', 'mcp-row');
    const head = el('div', 'mcp-head');
    head.appendChild(el('strong', '', s.name));
    head.appendChild(el('span', 'chip ' + (s.trust ? 'ok' : 'warn'), s.trust ? '配置即信任' : '逐次审批'));
    head.appendChild(el('span', 'chip', s.source === 'project' ? '项目' : '全局'));
    head.appendChild(el('span', 'chip ' + (s.connected ? 'ok' : 'muted'), s.connected ? `${s.tools} 工具` : '未连接'));
    if (s.version) head.appendChild(el('span', 'muted small', s.version));
    row.appendChild(head);
    row.appendChild(el('div', 'muted small mono', s.command));
    body.appendChild(row);
  }
  if (state.mcp.length === 0) body.appendChild(el('p', 'muted', '没有配置 MCP server'));
  openModal('MCP server', body, [modalButton('关闭', '', () => closeModal())]);
}

function openRoots() {
  const body = document.createDocumentFragment();
  const rows = [['工作区（可写）', state.workspace]];
  for (const r of state.roots) rows.push([`附加根 ${r.alias}`, r.path]);
  for (const [k, v] of rows) {
    const row = el('div', 'kv');
    row.appendChild(el('span', 'k', k));
    row.appendChild(el('span', 'v mono', v));
    body.appendChild(row);
  }
  body.append(el('p', 'muted small', '附加根用 alias:path 读取/写入，命令用 root= 选根。'));
  openModal('工作区与附加根', body, [modalButton('关闭', '', () => closeModal())]);
}

// 后台进程：按会话查、列出全部（空 session_id = 全部会话，和 TUI 的 /ps 一致）。
// 显示范围故意跨会话：孤儿进程的会话可能已经没人开着标签了。
function openProcs() {
  pendingModal = 'procs';
  link.send(T.procList, {});
}

function renderProcsModal() {
  if (pendingModal !== 'procs') return;
  const body = document.createDocumentFragment();
  const running = state.procs.filter((p) => p.status === 'running').length;
  const orphaned = state.procs.filter((p) => p.status === 'orphaned').length;
  body.appendChild(el('p', 'muted small',
    state.procs.length === 0
      ? '没有后台进程。'
      : `${state.procs.length} 条进程：运行中 ${running}，孤儿 ${orphaned}。「停止」发给属主执行；孤儿只能「终止」（会先核对启动时间）或删记录。`));

  for (const p of state.procs) {
    const row = el('div', 'mcp-row');
    const head = el('div', 'mcp-head');
    head.appendChild(el('strong', 'mono', (p.argv && p.argv.length ? p.argv.join(' ') : '未知命令')));
    head.appendChild(el('span', 'chip ' + procTone(p.status), procStatusLabel(p.status)));
    if (p.pid) head.appendChild(el('span', 'muted small mono', `pid ${p.pid}`));
    head.appendChild(el('span', 'muted small mono', (p.session_id || '').slice(0, 12)));
    if (p.started_at) head.appendChild(el('span', 'muted small', procAge(p.started_at)));
    if (p.stop_requested) head.appendChild(el('span', 'chip warn', '已请求停止'));
    row.appendChild(head);

    const foot = el('div', 'session-actions');
    if (p.status === 'running') {
      foot.appendChild(modalButton('停止', 'link', () => {
        link.send(T.procStop, { id: p.id });
      }));
    }
    if (p.status === 'orphaned') {
      foot.appendChild(modalButton('终止', 'link danger', () => {
        if (!window.confirm(`终止进程 ${p.pid || '?'}（${(p.argv || []).join(' ') || '未知命令'}）？\n` +
          '会先核对进程启动时间，与记录不一致就拒绝，避免误杀复用了 pid 的进程。')) return;
        link.send(T.procKill, { id: p.id });
      }));
    }
    if (p.status !== 'running') {
      foot.appendChild(modalButton('删记录', 'link danger', () => {
        if (!window.confirm('只从列表里删掉这条记录，不动日志文件，可撤销不了。继续？')) return;
        link.send(T.procClean, { id: p.id });
      }));
    }
    if (foot.children.length) row.appendChild(foot);
    if (p.output_path) row.appendChild(el('div', 'muted small mono', `日志 ${p.output_path}`));
    body.appendChild(row);
  }

  openModal('后台进程', body, [
    modalButton('刷新', '', openProcs),
    modalButton('关闭', '', () => closeModal()),
  ]);
}

function procStatusLabel(status) {
  switch (status) {
    case 'running': return '运行中';
    case 'orphaned': return '孤儿';
    case 'exited': return '已退出';
    case 'stopped': return '已停止';
    default: return status || '未知';
  }
}

function procTone(status) {
  if (status === 'running') return 'ok';
  if (status === 'orphaned') return 'warn';
  return 'muted';
}

function procAge(ms) {
  const secs = Math.max(0, (Date.now() - ms) / 1000);
  if (secs < 60) return `${Math.round(secs)} 秒前`;
  if (secs < 3600) return `${Math.round(secs / 60)} 分钟前`;
  if (secs < 86400) return `${Math.round(secs / 3600)} 小时前`;
  return `${Math.round(secs / 86400)} 天前`;
}

function openProject() {
  const p = state.project;
  const body = document.createDocumentFragment();
  if (!p) {
    body.appendChild(el('p', 'muted', '当前会话没有项目级扩展。'));
  } else {
    body.appendChild(el('p', '', p.summary || ''));
    if (p.prompts?.length) body.appendChild(el('div', 'muted small', `提示：${p.prompts.join('、')}`));
    if (p.skills?.length) body.appendChild(el('div', 'muted small', `技能：${p.skills.join('、')}`));
    if (p.servers?.length) body.appendChild(el('div', 'muted small mono', `MCP：${p.servers.join('；')}`));
    if (p.error) body.appendChild(el('p', 'error', p.error));
  }
  const foot = [modalButton('关闭', '', () => closeModal())];
  if (p?.trusted) {
    foot.unshift(modalButton('撤销信任', 'danger', () => {
      link.send(T.projectRevoke, { session_id: state.currentId });
      closeModal();
    }));
  }
  openModal('项目级扩展', body, foot);
}

function trustProject() {
  link.send(T.projectTrust, { session_id: state.currentId });
}

function openHelp() {
  const body = document.createElement('div');
  body.className = 'help';
  const rows = [
    ['Enter', '发送任务（Shift+Enter 换行）'],
    ['Esc', '关闭弹层 / 收起审批'],
    ['会话', '切换、改名、删除、新开'],
    ['模型', '切换当前会话的模型'],
    ['单次模型', '只给下一条任务指定模型'],
    ['技能', '指定下一条任务加载的技能'],
    ['MCP', '查看 server 与工具数（只读）'],
    ['根', '工作区与附加根'],
    ['进程', '后台进程（含孤儿），可停止 / 终止 / 删记录'],
    ['审批', '批准一次 / 本会话内都批准 / 拒绝；diff 多块时可逐块勾选'],
  ];
  for (const [k, v] of rows) {
    const row = el('div', 'kv');
    row.appendChild(el('span', 'k', k));
    row.appendChild(el('span', 'v', v));
    body.appendChild(row);
  }
  body.appendChild(el('p', 'muted small', `服务端 ${state.serverVersion} · 工作区 ${state.workspace}`));
  body.appendChild(el('p', 'muted small', '本机命令（!cmd）只在 TUI 里可用：它执行在客户端机器上，浏览器没有 shell。'));
  openModal('帮助', body, [modalButton('关闭', '', () => closeModal())]);
}

// ---------- 全局按键 ----------

window.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape') {
    if (state.approval) {
      state.approval.collapsed = !state.approval.collapsed;
      scheduleRender();
      return;
    }
    if (isModalOpen()) closeModal();
  }
});

// 切回页面时补一次渲染：后台标签的徽标可能已经变了。
document.addEventListener('visibilitychange', () => {
  if (!document.hidden) scheduleRender();
});

focusInput();
