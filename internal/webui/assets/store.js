// 页面状态。
//
// 与 TUI 采用同一条原则：标签只存徽标（标题、状态、待审批数、未读数），
// 对话正文只保留当前会话的一份；切回某条会话时从 seq=0 全量重放重建，
// 所以不会出现「两份状态谁是对的」。

import { E, ST, isTerminal } from './protocol.js';

export const state = {
  workspace: '',
  roots: [],
  serverVersion: '',
  conn: 'connecting',
  connDetail: '',

  tabs: [], // [{id,title,provider,model,status,pending,unread}]
  currentId: null,

  messages: [], // 当前会话的对话项，见 push()
  stream: '',
  taskId: '',
  runTaskId: '',
  taskStatus: '',
  round: 0,
  tokens: 0,
  ctxTokens: 0,
  ctxBudget: 0,

  queue: [], // [{id,prompt,at}]
  approval: null, // {payload, hunks, selectMode, selected:Set}
  project: null,

  models: [],
  modelSource: '',
  forcedModel: '',
  forcedSkills: [],

  skills: [],
  mcp: [],
  sessions: [], // 最近一次 session.manage 的结果

  lastSeq: 0,
  unread: 0,
};

const listeners = new Set();

export function subscribe(fn) {
  listeners.add(fn);
}

export function notify() {
  for (const fn of listeners) fn();
}

export function currentTab() {
  return state.tabs.find((t) => t.id === state.currentId) || null;
}

export function tabById(id) {
  return state.tabs.find((t) => t.id === id) || null;
}

export function upsertTab({ id, title, provider, model, status }) {
  if (!id) return null;
  let t = tabById(id);
  if (!t) {
    t = { id, title: title || '', provider: provider || '', model: model || '', status: status || '', pending: 0, unread: 0 };
    state.tabs.push(t);
  } else {
    if (title !== undefined) t.title = title;
    if (provider !== undefined) t.provider = provider;
    if (model !== undefined) t.model = model;
    if (status !== undefined) t.status = status;
  }
  sortTabs();
  return t;
}

// 当前会话的标签要跟着前台状态走：状态与模型变了就同步到标签上，
// 否则标签栏会一直停在「打开时」的样子（后台会话走 noteBackground）。
export function syncCurrentTab() {
  const t = currentTab();
  if (!t) return;
  if (state.taskStatus) t.status = state.taskStatus;
  if (state.modelInUse) t.model = state.modelInUse;
}

// 标签按打开顺序排列（新的在右），切回来时不跳动。
function sortTabs() {
  // 顺序由 upsert 的先后决定，这里只保证当前会话一定在。
}

export function setTabTitle(id, title) {
  const t = tabById(id);
  if (t) t.title = title;
}

export function removeTab(id) {
  state.tabs = state.tabs.filter((t) => t.id !== id);
}

export function resetView() {
  state.messages = [];
  state.stream = '';
  state.taskId = '';
  state.runTaskId = '';
  state.taskStatus = '';
  state.round = 0;
  state.tokens = 0;
  state.ctxTokens = 0;
  state.ctxBudget = 0;
  state.queue = [];
  state.approval = null;
  state.lastSeq = 0;
  state.unread = 0;
}

export function push(item) {
  state.messages.push(item);
}

// 后台会话只动徽标：两条会话的对话流混在一起没法读。
export function noteBackground(env) {
  const t = tabById(env.session_id);
  if (!t) return;
  const p = env.payload || {};
  switch (p.kind) {
    case E.taskQueued:
      t.status = ST.queued;
      break;
    case E.taskStatus: {
      const to = p.payload?.to;
      if (to) t.status = to;
      break;
    }
    case E.approvalRequested:
      t.pending = (t.pending || 0) + 1;
      break;
    case E.approvalResolved:
      t.pending = Math.max(0, (t.pending || 0) - 1);
      break;
    case E.assistantMessage:
    case E.toolResult:
      t.unread = (t.unread || 0) + 1;
      break;
  }
}

export function applyEvent(env) {
  const p = env.payload || {};
  const kind = p.kind;
  const body = p.payload || {};

  // 流式增量是临时帧（seq=0），只拼到当前段落。
  if (env.seq === 0 && kind === E.assistantDelta) {
    state.stream += body.text || '';
    return;
  }
  if (env.seq > 0 && env.seq > state.lastSeq) state.lastSeq = env.seq;

  if (p.task_id && (!state.runTaskId || p.task_id === state.runTaskId)) {
    state.taskId = p.task_id;
  }

  switch (kind) {
    case E.taskQueued: {
      state.queue.push({ id: p.task_id, prompt: body.prompt || '', at: Date.now() });
      if (body.prompt) push({ kind: 'prompt', text: body.prompt, skills: body.skills || [], model: body.model || '' });
      if (!state.runTaskId) state.taskStatus = ST.queued;
      syncCurrentTab();
      break;
    }
    case E.taskStatus: {
      const to = body.to;
      if (to === ST.running || to === ST.waiting) {
        if (p.task_id) {
          state.runTaskId = p.task_id;
          state.taskId = p.task_id;
          dropQueued(p.task_id);
        }
        state.taskStatus = to;
      } else if (isTerminal(to)) {
        dropQueued(p.task_id);
        if (p.task_id === state.runTaskId || !state.runTaskId) {
          state.taskStatus = to;
          state.stream = '';
          if (p.task_id === state.runTaskId) state.runTaskId = '';
        }
      }
      if (body.prompt_tokens || body.completion_tokens) {
        state.tokens = (body.prompt_tokens || 0) + (body.completion_tokens || 0);
      }
      if (to === ST.failed && body.error) push({ kind: 'note', tone: 'err', text: body.error });
      if (to) push({ kind: 'status', from: body.from, to, reason: body.reason || '' });
      syncCurrentTab();
      break;
    }
    case E.assistantStart:
      state.stream = '';
      if (body.context_tokens > 0) state.ctxTokens = body.context_tokens;
      if (body.context_budget > 0) state.ctxBudget = body.context_budget;
      if (body.model) {
        const t = currentTab();
        if (t) t.model = body.model;
        state.modelInUse = body.model;
        syncCurrentTab();
      }
      break;
    case E.roundStarted:
      state.round = body.round || state.round;
      break;
    case E.contextCompacted:
      push({ kind: 'note', text: `上下文已压缩：${body.covered_messages} 条较早消息 → ${body.summary_chars} 字摘要` });
      break;
    case E.assistantMessage: {
      state.stream = '';
      if (body.text) push({ kind: 'assistant', text: body.text });
      for (const call of body.tool_calls || []) {
        let args = '';
        try {
          args = typeof call.args === 'string' ? call.args : JSON.stringify(call.args);
        } catch {
          args = '';
        }
        push({ kind: 'tool', name: call.name, args });
      }
      break;
    }
    case E.toolResult: {
      let status = '完成';
      if (body.status === 'denied') status = '已拒绝';
      else if (body.status === 'canceled') status = '已取消';
      else if (body.is_error) status = '出错';
      push({
        kind: 'result',
        tool: body.tool,
        status,
        exitCode: body.exit_code,
        durationMs: body.duration_ms || 0,
        output: body.output || '',
        truncated: !!body.truncated,
        outputPath: body.output_path || '',
      });
      break;
    }
    case E.approvalRequested:
      setApproval(body);
      break;
    case E.approvalResolved:
      if (state.approval && state.approval.payload.approval_id === body.approval_id) state.approval = null;
      push({ kind: 'note', tone: 'ok', text: `审批 ${decisionLabel(body.decision)}` });
      break;
    case E.proposalApplied:
      push({
        kind: 'note',
        tone: 'ok',
        text: body.auto
          ? `自动应用（${body.mode === 'tmp' ? '临时目录内' : body.mode === 'workspace' ? '工作区内' : '按配置免审'}）：${body.path}`
          : body.hunks && body.hunks.length
            ? `已写入 ${body.path}（第 ${body.hunks.map((n) => n + 1).join('、')} 块，共 ${body.total_hunks} 块）`
            : `已写入 ${body.path}`,
      });
      break;
    case E.proposalRejected:
      push({ kind: 'note', tone: 'err', text: `未写入 ${body.path}（${body.status}）` });
      break;
    case E.sessionRenamed:
      setTabTitle(state.currentId, body.title);
      if (currentTab()) currentTab().title = body.title;
      break;
    case E.sessionUpdated: {
      const t = currentTab();
      if (t) {
        if (body.provider) t.provider = body.provider;
        if (body.model) t.model = body.model;
      }
      push({ kind: 'note', text: `模型已切换：${body.model}` });
      break;
    }
    case E.error:
      push({ kind: 'note', tone: 'err', text: body.message || '错误' });
      break;
  }
}

function dropQueued(taskId) {
  if (!taskId) return;
  state.queue = state.queue.filter((q) => q.id !== taskId);
}

export function setApproval(payload) {
  const hunks = parseHunks(payload.diff || '');
  state.approval = {
    payload,
    hunks,
    selectMode: false,
    selected: new Set(hunks.map((_, i) => i)),
  };
}

// parseHunks 把 unified diff 切成块。客户端只用它做展示与「发哪几块」，
// 真正的写入由服务端按 BaseHash + 块内容重放——这里算错不会造成越权，
// 只会在服务端报「上下文对不上」。
export function parseHunks(diff) {
  const lines = diff.replace(/\r\n/g, '\n').split('\n');
  const out = [];
  let cur = null;
  for (const line of lines) {
    if (line.startsWith('@@')) {
      cur = { header: line, lines: [] };
      out.push(cur);
      continue;
    }
    if (!cur) continue;
    if (line.startsWith('---') || line.startsWith('+++')) continue;
    if (line.startsWith(' ')) {
      cur.lines.push(['ctx', line.slice(1)]);
    } else if (line.startsWith('-')) {
      cur.lines.push(['del', line.slice(1)]);
    } else if (line.startsWith('+')) {
      cur.lines.push(['add', line.slice(1)]);
    } else if (line.startsWith('\\')) {
      cur.lines.push(['meta', line]);
    } else if (line === '') {
      // 末尾空行不是 diff 内容。
    }
  }
  return out;
}

export function decisionLabel(d) {
  switch (d) {
    case 'allow_once':
      return '已批准（一次）';
    case 'allow_session':
      return '已批准（本会话内同类请求）';
    case 'deny':
      return '已拒绝';
    case 'expired':
      return '已过期';
    default:
      return d;
  }
}

export function statusLabel(s) {
  switch (s) {
    case ST.running:
      return ['运行中', 'run'];
    case ST.waiting:
      return ['等待审批', 'warn'];
    case ST.queued:
      return ['排队中', 'warn'];
    case ST.completed:
      return ['已完成', 'ok'];
    case ST.failed:
      return ['失败', 'err'];
    case ST.canceled:
      return ['已取消', 'muted'];
    default:
      return ['空闲', 'muted'];
  }
}

export function humanTokens(n) {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}
