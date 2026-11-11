// Package app 是应用层：唯一的事务边界与唯一的事件发布者。
//
// M1 覆盖「账本」：会话与任务的创建、状态迁移落库、事件回放。M3 在本层
// 接上任务执行：app 负责协程生命周期、取消与忙时拒绝，agent 只负责一轮轮
// 对话。执行依赖通过 ConfigureRuntime 注入，未注入时退回只入队的行为。
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kennethfan/jiaoag/internal/agent"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
)

// App 组合持久化与事件扇出。除它之外没有第二个写者。
type App struct {
	store Store
	hub   *event.Hub

	// commitMu 保证「提交顺序 = 广播顺序」。
	//
	// 存储层的写锁在写方法返回时就释放了；若在此之后才广播，两个并发写入者
	// 可能先广播较大的 seq，在线订阅者按 seq 去重时就会丢掉较小的那条。
	// 跨会话共用一把锁是可接受的：存储层本来就是全局写锁，这里不引入新的
	// 串行点。所有写入都必须经由 App.commit，不要绕过它直接写库再广播。
	commitMu sync.Mutex

	// baseCtx 是所有任务的父 context：Close 时统一取消。
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// runtimeMu 保护 runtime 与 runs。它只覆盖「查表 + 登记」，不与 commitMu
	// 嵌套：提交仍在 commitMu 内完成，这里不引入第二个串行点。
	runtimeMu sync.Mutex
	runtime   *RuntimeDeps
	runs      map[domain.SessionID]*taskRun
}

// commit 在 commitMu 内完成「存储提交 → hub 广播」，是应用层唯一的写入入口。
// 任何新增写操作（M2 的 Apply 等）都必须走这里，否则提交顺序会与广播顺序脱钩。
// 对外只返回错误：事件的去向只有 hub 与事件日志，调用方不需要副本。
func (a *App) commit(fn func() ([]domain.Event, error)) error {
	a.commitMu.Lock()
	defer a.commitMu.Unlock()

	stored, err := fn()
	if err != nil {
		return err
	}
	a.hub.Publish(stored)
	return nil
}

func New(store Store, hub *event.Hub) *App {
	if hub == nil {
		hub = event.NewHub()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		store:      store,
		hub:        hub,
		baseCtx:    ctx,
		baseCancel: cancel,
		runs:       make(map[domain.SessionID]*taskRun),
	}
}

// Close 取消运行中的任务、断开订阅并关闭存储。
//
// 先取消再等待：任务收到取消会记录 canceled 终态，此时数据库仍然打开；
// 直接关库会让这些写入失败，任务永远停在 running。
func (a *App) Close() error {
	if a.baseCancel != nil {
		a.baseCancel()
	}
	a.waitRuns(5 * time.Second)
	a.hub.CloseAll()
	return a.store.Close()
}

// Hub 暴露扇出器给传输层（订阅在线事件）。
func (a *App) Hub() *event.Hub { return a.hub }

// NewID 生成带前缀的随机标识，例如 ses_1f3c…。
func NewID(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("app: 生成标识: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(buf[:]), nil
}

// CreateSession 创建工作区会话，并写入 session.created。
func (a *App) CreateSession(ctx context.Context, workspace, provider, model, title string) (domain.Session, error) {
	if !filepath.IsAbs(workspace) {
		return domain.Session{}, fmt.Errorf("app: 工作区必须是绝对路径: %q", workspace)
	}
	id, err := NewID("ses")
	if err != nil {
		return domain.Session{}, err
	}
	now := domain.Now()
	sess := domain.Session{
		ID:        domain.SessionID(id),
		Workspace: filepath.Clean(workspace),
		Provider:  strings.TrimSpace(provider),
		Model:     strings.TrimSpace(model),
		Title:     strings.TrimSpace(title),
		CreatedAt: now,
		UpdatedAt: now,
	}
	ev, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{
		Workspace: sess.Workspace,
		Provider:  sess.Provider,
		Model:     sess.Model,
		Title:     sess.Title,
	})
	if err != nil {
		return domain.Session{}, err
	}
	if err := a.commit(func() ([]domain.Event, error) {
		return a.store.CreateSession(ctx, sess, []domain.Event{ev})
	}); err != nil {
		return domain.Session{}, err
	}
	return sess, nil
}

// SubmitTask 把用户请求入队（M1 不执行），并写入 task.queued。
func (a *App) SubmitTask(ctx context.Context, sessionID domain.SessionID, prompt string) (domain.Task, error) {
	if strings.TrimSpace(prompt) == "" {
		return domain.Task{}, fmt.Errorf("app: 任务内容为空")
	}
	sess, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Task{}, err
	}

	// 两次加锁：先快检，再复查。
	//
	// newAgent 要读工作区、构造供应商（可能读密钥文件）。把这些 I/O 放在
	// runtimeMu 里会让所有会话的提交与取消一起排队等它；但「忙判断 + 任务
	// 登记」必须原子，所以锁外只做构造，锁内重新检查一次再落库。
	deps := a.currentRuntime()
	if deps != nil {
		if err := a.checkIdle(sessionID); err != nil {
			return domain.Task{}, err
		}
	}

	var (
		ag               *agent.Agent
		systemHash       string
		extensionsDigest string
	)
	if deps != nil {
		ag, systemHash, extensionsDigest, err = a.newAgent(sess, deps)
		if err != nil {
			return domain.Task{}, err
		}
	}

	id, err := NewID("tsk")
	if err != nil {
		return domain.Task{}, err
	}
	task := domain.Task{
		ID:               domain.TaskID(id),
		SessionID:        sessionID,
		Status:           domain.TaskQueued,
		Prompt:           prompt,
		SystemPromptHash: systemHash,
		ExtensionsDigest: extensionsDigest,
		CreatedAt:        domain.Now(),
	}
	ev, err := domain.NewEvent(domain.EventTaskQueued, sessionID, task.ID, domain.TaskQueuedPayload{
		Prompt:           task.Prompt,
		SystemPromptHash: task.SystemPromptHash,
		ExtensionsDigest: task.ExtensionsDigest,
	})
	if err != nil {
		return domain.Task{}, err
	}
	// 忙时拒绝要在建任务之前：先建后拒会留下一条永远排不上的 queued 记录。
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if deps != nil {
		if run, ok := a.runs[sessionID]; ok && run != nil {
			return domain.Task{}, ErrSessionBusy
		}
	}
	if err := a.commit(func() ([]domain.Event, error) {
		return a.store.CreateTask(ctx, task, []domain.Event{ev})
	}); err != nil {
		return domain.Task{}, err
	}
	if ag != nil {
		a.startLocked(sess, task, ag, deps)
	}
	return task, nil
}

// Replay 返回从 from 之后的事件，供断线重连补齐。
func (a *App) Replay(ctx context.Context, sessionID domain.SessionID, from domain.Seq, limit int) ([]domain.Event, error) {
	return a.store.Replay(ctx, sessionID, from, limit)
}

func (a *App) LastSeq(ctx context.Context, sessionID domain.SessionID) (domain.Seq, error) {
	return a.store.LastSeq(ctx, sessionID)
}

func (a *App) ListSessions(ctx context.Context) ([]domain.Session, error) {
	return a.store.ListSessions(ctx)
}

func (a *App) GetSession(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	return a.store.GetSession(ctx, id)
}

func (a *App) GetTask(ctx context.Context, id domain.TaskID) (domain.Task, error) {
	return a.store.GetTask(ctx, id)
}

func (a *App) ListTasks(ctx context.Context, sessionID domain.SessionID) ([]domain.Task, error) {
	return a.store.ListTasks(ctx, sessionID)
}
