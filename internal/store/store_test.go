package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func seedSession(t *testing.T, s *store.Store) domain.Session {
	t.Helper()
	sess := domain.Session{
		ID:        "ses_test",
		Workspace: "/tmp/workspace",
		Title:     "测试会话",
	}
	ev, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{
		Workspace: sess.Workspace,
		Title:     sess.Title,
	})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := s.CreateSession(context.Background(), sess, []domain.Event{ev}); err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	return sess
}

func seedTask(t *testing.T, s *store.Store, sess domain.Session) domain.Task {
	t.Helper()
	task := domain.Task{
		ID:        "tsk_test",
		SessionID: sess.ID,
		Status:    domain.TaskQueued,
		Prompt:    "给 X 函数加测试",
	}
	ev, err := domain.NewEvent(domain.EventTaskQueued, sess.ID, task.ID, domain.TaskQueuedPayload{Prompt: task.Prompt})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := s.CreateTask(context.Background(), task, []domain.Event{ev}); err != nil {
		t.Fatalf("创建任务: %v", err)
	}
	return task
}

func TestCreateAndReplay(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	sess := seedSession(t, s)
	task := seedTask(t, s, sess)

	toRunning, _ := domain.NewEvent(domain.EventTaskStatus, sess.ID, task.ID,
		domain.TaskStatusPayload{From: domain.TaskQueued, To: domain.TaskRunning})
	if _, err := s.Apply(ctx, domain.Transition{TaskID: task.ID, To: domain.TaskRunning, Events: []domain.Event{toRunning}}); err != nil {
		t.Fatalf("迁移到 running: %v", err)
	}
	toDone, _ := domain.NewEvent(domain.EventTaskStatus, sess.ID, task.ID,
		domain.TaskStatusPayload{From: domain.TaskRunning, To: domain.TaskCompleted})
	if _, err := s.Apply(ctx, domain.Transition{TaskID: task.ID, To: domain.TaskCompleted, Events: []domain.Event{toDone}}); err != nil {
		t.Fatalf("迁移到 completed: %v", err)
	}

	events, err := s.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	want := []domain.EventKind{domain.EventSessionCreated, domain.EventTaskQueued, domain.EventTaskStatus, domain.EventTaskStatus}
	if len(events) != len(want) {
		t.Fatalf("事件数 = %d，期望 %d", len(events), len(want))
	}
	for i, ev := range events {
		if ev.Type != want[i] {
			t.Fatalf("第 %d 条类型 = %s，期望 %s", i, ev.Type, want[i])
		}
		if int64(ev.Seq) != int64(i+1) {
			t.Fatalf("第 %d 条 seq = %d，期望 %d", i, ev.Seq, i+1)
		}
		if ev.At.IsZero() {
			t.Fatalf("第 %d 条缺少时间", i)
		}
		if ev.V != domain.EventVersion {
			t.Fatalf("第 %d 条版本 = %d，期望 %d", i, ev.V, domain.EventVersion)
		}
	}

	last, err := s.LastSeq(ctx, sess.ID)
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if last != 4 {
		t.Fatalf("LastSeq = %d，期望 4", last)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取任务: %v", err)
	}
	if got.Status != domain.TaskCompleted {
		t.Fatalf("状态 = %s，期望 completed", got.Status)
	}
	if got.StartedAt.IsZero() || got.FinishedAt.IsZero() {
		t.Fatal("时间戳未落库")
	}
	if got.LastSeq != 4 {
		t.Fatalf("LastSeq 投影 = %d，期望 4", got.LastSeq)
	}
}

func TestIllegalTransitionRollsBack(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	sess := seedSession(t, s)
	task := seedTask(t, s, sess)

	ev, _ := domain.NewEvent(domain.EventTaskStatus, sess.ID, task.ID,
		domain.TaskStatusPayload{From: domain.TaskQueued, To: domain.TaskCompleted})
	_, err := s.Apply(ctx, domain.Transition{TaskID: task.ID, To: domain.TaskCompleted, Events: []domain.Event{ev}})
	var te *domain.TransitionError
	if !errors.As(err, &te) {
		t.Fatalf("期望 *TransitionError，得到 %v", err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取任务: %v", err)
	}
	if got.Status != domain.TaskQueued {
		t.Fatalf("非法迁移改变了状态: %s", got.Status)
	}
	last, _ := s.LastSeq(ctx, sess.ID)
	if last != 2 {
		t.Fatalf("非法迁移写入了事件，LastSeq = %d", last)
	}
}

func TestApplyUnknownTask(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Apply(context.Background(), domain.Transition{TaskID: "tsk_missing", To: domain.TaskRunning})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，得到 %v", err)
	}
}

func TestConcurrentAppliesKeepSeqUnique(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	sess := seedSession(t, s)
	task := seedTask(t, s, sess)

	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				ev, err := domain.NewEvent(domain.EventRoundStarted, sess.ID, task.ID, domain.RoundStartedPayload{Round: i})
				if err != nil {
					errs <- err
					return
				}
				if _, err := s.Apply(ctx, domain.Transition{TaskID: task.ID, Events: []domain.Event{ev}}); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发写入失败: %v", err)
	}

	events, err := s.Replay(ctx, sess.ID, 0, 1000)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	total := 2 + workers*perWorker
	if len(events) != total {
		t.Fatalf("事件数 = %d，期望 %d", len(events), total)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("seq 未严格递增: %d 后出现 %d", events[i-1].Seq, events[i].Seq)
		}
	}
}

func TestReplayFromAndLimit(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	sess := seedSession(t, s)
	task := seedTask(t, s, sess)
	for i := 0; i < 5; i++ {
		ev, _ := domain.NewEvent(domain.EventRoundStarted, sess.ID, task.ID, domain.RoundStartedPayload{Round: i})
		if _, err := s.Apply(ctx, domain.Transition{TaskID: task.ID, Events: []domain.Event{ev}}); err != nil {
			t.Fatalf("写入: %v", err)
		}
	}
	// 共 7 条事件：会话 1 + 任务 1 + 5 轮。
	events, err := s.Replay(ctx, sess.ID, 3, 2)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	if len(events) != 2 || events[0].Seq != 4 || events[1].Seq != 5 {
		t.Fatalf("分页结果错误: %+v", events)
	}
	events, err = s.Replay(ctx, sess.ID, 100, 0)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("超出范围应返回空，得到 %d 条", len(events))
	}
}

func TestReopenKeepsHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	sess := seedSession(t, s)
	seedTask(t, s, sess)
	first, err := s.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	again, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer again.Close()
	second, err := again.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("重开回放: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("事件数不同: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Seq != second[i].Seq || first[i].Type != second[i].Type || string(first[i].Payload) != string(second[i].Payload) {
			t.Fatalf("第 %d 条事件不一致", i)
		}
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	for i := 0; i < 3; i++ {
		s, err := store.Open(ctx, path)
		if err != nil {
			t.Fatalf("第 %d 次打开: %v", i+1, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("关闭: %v", err)
		}
	}
}

func TestRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	raw, err := sql.Open("sqlite", testDSN(path))
	if err != nil {
		t.Fatalf("直连: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(999, ?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("写入高版本: %v", err)
	}
	raw.Close()

	if _, err := store.Open(ctx, path); err == nil {
		t.Fatal("结构版本过高时应拒绝启动")
	} else if !strings.Contains(err.Error(), "高于当前程序支持") {
		t.Fatalf("错误信息不清晰: %v", err)
	}
}

func TestListSessionsOrderByActivity(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	first := seedSession(t, s)
	second := domain.Session{ID: "ses_later", Workspace: "/tmp/b", Title: "后来的"}
	ev, _ := domain.NewEvent(domain.EventSessionCreated, second.ID, "", domain.SessionCreatedPayload{Workspace: second.Workspace})
	if _, err := s.CreateSession(ctx, second, []domain.Event{ev}); err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	sessions, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("列出会话: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("会话数 = %d", len(sessions))
	}
	if sessions[0].ID != second.ID {
		t.Fatalf("最近活动的会话应排在最前，得到 %s", sessions[0].ID)
	}
	if sessions[1].ID != first.ID {
		t.Fatalf("较早的会话应排在后面，得到 %s", sessions[1].ID)
	}
}

// TestRejectsIncompleteSchema：版本号相等但结构缺失（历史迁移被原地修改过）
// 必须给出明确报错，而不是让后续查询抛出裸的 SQL 错误。
func TestRejectsIncompleteSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")

	raw, err := sql.Open("sqlite", testDSN(path))
	if err != nil {
		t.Fatalf("直连: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("建迁移表: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(1, '')`); err != nil {
		t.Fatalf("写入版本: %v", err)
	}
	raw.Close()

	if _, err := store.Open(ctx, path); err == nil {
		t.Fatal("结构不完整时应拒绝启动")
	} else if !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("错误信息未说明结构问题: %v", err)
	}
}

// TestRejectsModifiedMigration：历史迁移被原地修改后，校验和不匹配必须拒绝启动。
func TestRejectsModifiedMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	raw, err := sql.Open("sqlite", testDSN(path))
	if err != nil {
		t.Fatalf("直连: %v", err)
	}
	if _, err := raw.Exec(`UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 1`); err != nil {
		t.Fatalf("篡改校验和: %v", err)
	}
	raw.Close()

	if _, err := store.Open(ctx, path); err == nil {
		t.Fatal("迁移内容被修改后应拒绝启动")
	} else if !strings.Contains(err.Error(), "被修改过") {
		t.Fatalf("错误信息未指出迁移被修改: %v", err)
	}
}

// TestAddsChecksumColumnToLegacyTable：M1 未发布前建的库没有 checksum 列，
// 打开时应自动补列而不是报错（补列后旧记录的校验和为空，跳过比对）。
func TestAddsChecksumColumnToLegacyTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	raw, err := sql.Open("sqlite", testDSN(path))
	if err != nil {
		t.Fatalf("直连: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE schema_migrations DROP COLUMN checksum`); err != nil {
		raw.Close()
		t.Skipf("当前 SQLite 不支持 DROP COLUMN，跳过：%v", err)
	}
	raw.Close()

	again, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("旧库应能自动补列后打开: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	check, err := sql.Open("sqlite", testDSN(path))
	if err != nil {
		t.Fatalf("直连: %v", err)
	}
	defer check.Close()
	var columns int
	if err := check.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('schema_migrations') WHERE name = 'checksum'`).Scan(&columns); err != nil {
		t.Fatalf("读取表结构: %v", err)
	}
	if columns != 1 {
		t.Fatalf("checksum 列数 = %d，期望 1", columns)
	}
}
