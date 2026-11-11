# jiaoag

一个单用户的编码自动化 agent：服务端在用户本机直接操作真实文件系统与 shell，TUI 客户端通过网络驱动它。

## Language

**会话 (Session)**:
一次有边界的工作单元，绑定一个工作区与一段连续的消息历史；客户端的连接存在与否不影响会话的生命期。
_Avoid_: Chat、Conversation、Thread

**任务 (Task)**:
用户发给 agent 的一条完整指令，从接收到终止（完成、失败或取消）为一个任务；一个会话可包含多个任务。
_Avoid_: Run、Job、Turn

**轮次 (Round)**:
任务内的一次模型调用及其引发的工具调用，是自主循环的最小迭代单位。
_Avoid_: Step、Iteration、Loop

**工具调用 (Tool Call)**:
agent 请求执行一件具体能力的记录，包含入参、结果与终态。
_Avoid_: Action、Function Call、Invocation

**审批 (Approval)**:
对一次工具调用是否允许执行的裁决，由权限策略自动给出，或由用户在客户端手动给出。
_Avoid_: Confirmation、Permission、Consent

**工作区 (Workspace)**:
一个会话被允许触碰的文件系统边界，通常是一个 git 仓库根目录；越界路径一律拒绝。
_Avoid_: Project、Repo、Working Directory

**变更提案 (Change Proposal)**:
对工作区内文件的拟修改，以待批准的 diff 形式存在；批准并落盘之前不改变磁盘内容。
_Avoid_: Patch、Edit、Write

**事件 (Event)**:
服务端产生的、按会话内单调序号排列的可回放事实，包括模型增量输出、工具调用、审批请求与状态迁移。
_Avoid_: Message、Notification、Update

**权限策略 (Permission Policy)**:
判定一次工具调用属于「自动放行」「需审批」「硬拒绝」的规则集合，是本项目安全边界的唯一实现。
_Avoid_: Sandbox、Guard、ACL

**审计记录 (Audit Record)**:
每次工具调用留下的不可变事实：入参、结果状态、审批来源、耗时与关联的变更提案。
_Avoid_: Log、Trace

**供应商适配器 (Provider Adapter)**:
把统一的消息与工具调用语义翻译成某一家模型 API 协议的实现。
_Avoid_: Driver、Client、Backend
