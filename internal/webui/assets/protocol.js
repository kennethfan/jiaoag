// 协议常量：与 internal/protocol/protocol.go 一一对应。
// 这些字符串就是线上格式；改服务端常量必须同步改这里（webui_test 会比对）。
export const T = {
  hello: 'hello',
  clientHello: 'client.hello',
  ping: 'ping',
  pong: 'pong',
  error: 'error',
  event: 'event',

  sessionCreate: 'session.create',
  sessionCreated: 'session.created',
  sessionUpdate: 'session.update',
  sessionUpdated: 'session.updated',
  sessionList: 'session.list',
  sessionListResult: 'session.list.result',
  sessionManage: 'session.manage',
  sessionManageResult: 'session.manage.result',
  sessionRename: 'session.rename',
  sessionRenamed: 'session.renamed',
  sessionDelete: 'session.delete',
  sessionDeleted: 'session.deleted',

  providerModels: 'provider.models',
  providerModelsResult: 'provider.models.result',
  skillList: 'skill.list',
  skillListResult: 'skill.list.result',
  mcpList: 'mcp.list',
  mcpListResult: 'mcp.list.result',
  procList: 'proc.list',
  procListResult: 'proc.list.result',
  procStop: 'proc.stop',
  procKill: 'proc.kill',
  procClean: 'proc.clean',
  procActionResult: 'proc.action.result',

  projectTrust: 'project.trust',
  projectRevoke: 'project.revoke',
  projectTrusted: 'project.trusted',

  taskSubmit: 'task.submit',
  taskQueued: 'task.queued',
  taskCancel: 'task.cancel',
  taskCanceled: 'task.canceled',

  approvalResolve: 'approval.resolve',
  eventSubscribe: 'event.subscribe',
  eventUnsubscribe: 'event.unsubscribe',
  eventSnapshot: 'event.snapshot',
};

// 事件种类（internal/domain/events.go 的 EventKind）。
export const E = {
  sessionCreated: 'session.created',
  sessionRenamed: 'session.renamed',
  sessionUpdated: 'session.updated',
  taskQueued: 'task.queued',
  taskStatus: 'task.status',
  roundStarted: 'round.started',
  assistantStart: 'assistant.start',
  assistantDelta: 'assistant.delta',
  assistantMessage: 'assistant.message',
  toolRequested: 'tool.requested',
  toolPrepared: 'tool.prepared',
  toolResult: 'tool.result',
  approvalRequested: 'approval.requested',
  approvalResolved: 'approval.resolved',
  proposalApplied: 'proposal.applied',
  proposalRejected: 'proposal.rejected',
  contextCompacted: 'context.compacted',
  error: 'error',
};

// 任务状态值。
export const ST = {
  queued: 'queued',
  running: 'running',
  waiting: 'waiting_approval',
  completed: 'completed',
  failed: 'failed',
  canceled: 'canceled',
};

export function isTerminal(status) {
  return status === ST.completed || status === ST.failed || status === ST.canceled;
}