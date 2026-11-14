// Package store 把领域状态与事件持久化到 SQLite。
//
// 约定（见架构文档 §4）：
//   - 每个写方法自身是一个事务：投影与事件要么都成功，要么都回滚。
//   - 事件 seq 在追加事务内按会话分配，允许空洞（流式增量不落库）。
//   - 写操作由进程内互斥锁串行化；读走 WAL 快照，不阻塞写。
//   - 迁移只增不改：已提交的 000N_*.sql 不可原地修改。改了老文件，旧库的
//     schema_migrations 仍记着旧版本号，迁移会被跳过，之后查询新列直接报
//     "no such column"。结构变更一律新增迁移文件。
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kennethfan/jiaoag/internal/domain"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store 是 SQLite 的薄封装。它不是线程安全的写者：
// 所有写路径都经过 writeMu，保证 seq 分配不出现竞争。
type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex

	// hookBeforeCommit 仅供包内测试注入失败，用于验证事务原子性。
	hookBeforeCommit func() error
}

// Open 打开（必要时创建）数据库并跑完迁移。
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: 数据库路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: 解析数据库路径: %w", err)
	}
	if dir := filepath.Dir(abs); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: 创建数据目录 %s: %w", dir, err)
		}
	}

	// Windows 的盘符（C:\…）在 file URI 里必须写成 file:///C:/…：直接把
	// Path 设成 "C:\…" 会得到 file:C:%5C…，被 SQLite 当成非法 authority。
	// Unix 路径已经是绝对路径（以 / 开头），转换后行为不变。
	slash := filepath.ToSlash(abs)
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash
	}

	dsn := (&url.URL{
		Scheme:   "file",
		Path:     slash,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
	}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库: %w", err)
	}
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: abs}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("store: 读取内嵌迁移: %w", err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("store: 迁移文件名缺少版本前缀: %s", e.Name())
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("store: 迁移文件名版本非法: %s", e.Name())
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: 读取迁移 %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  v,
			name:     e.Name(),
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (s *Store) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return fmt.Errorf("store: 没有内嵌迁移，拒绝以空结构启动")
	}

	if err := s.ensureSchemaMigrations(ctx); err != nil {
		return err
	}

	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("store: 读取结构版本: %w", err)
	}
	newest := migrations[len(migrations)-1].version
	if current > newest {
		return fmt.Errorf("store: 数据库 %s 的结构版本 %d 高于当前程序支持的 %d，请升级 jiaoag-server", s.path, current, newest)
	}
	if err := s.verifyMigrationChecksums(ctx, migrations, current); err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: 开始迁移 %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: 应用迁移 %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at, checksum) VALUES(?, ?, ?)`,
			m.version, ts(domain.Now()), m.checksum); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: 记录迁移 %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: 提交迁移 %s: %w", m.name, err)
		}
	}
	return s.verifySchema(ctx, newest)
}

// ensureSchemaMigrations 建迁移表，并给早期（M1 未发布前）建的库补上 checksum 列。
func (s *Store) ensureSchemaMigrations(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL,
		checksum   TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return fmt.Errorf("store: 创建迁移表: %w", err)
	}
	has, err := s.hasColumn(ctx, "schema_migrations", "checksum")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE schema_migrations ADD COLUMN checksum TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("store: 补充迁移表 checksum 列: %w", err)
	}
	return nil
}

func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("store: 读取表结构 %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid, notNull, pk int
			name, columnType string
			defaultValue     sql.NullString
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("store: 扫描表结构 %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: 遍历表结构 %s: %w", table, err)
	}
	return false, nil
}

// verifyMigrationChecksums 确认已应用迁移的内容没有被原地改过。
//
// 版本号相等但内容变了，只有一种可能：历史迁移文件被编辑。这时必须拒绝启动，
// 否则新列永远不会被建出来，后续查询才以 "no such column" 的形式炸掉。
// 早期库没有记录校验和（空串），跳过比对。
func (s *Store) verifyMigrationChecksums(ctx context.Context, migrations []migration, current int) error {
	applied := make(map[int]string, len(migrations))
	rows, err := s.db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations WHERE version <= ?`, current)
	if err != nil {
		return fmt.Errorf("store: 读取迁移校验和: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			version  int
			checksum string
		)
		if err := rows.Scan(&version, &checksum); err != nil {
			return fmt.Errorf("store: 扫描迁移校验和: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: 遍历迁移校验和: %w", err)
	}

	for _, m := range migrations {
		if m.version > current {
			continue
		}
		got, ok := applied[m.version]
		if !ok || got == "" {
			continue
		}
		if got != m.checksum {
			return fmt.Errorf(
				"store: 迁移 %s 在应用之后被修改过（校验和不一致）；迁移只增不改，请还原该文件或新增迁移文件，数据库 %s",
				m.name, s.path)
		}
	}
	return nil
}

// verifySchema 是校验和机制之外的二次兜底：确认热路径列确实存在。
//
// 只覆盖 events / tasks 的关键列，不是完整性校验；加列时不需要同步维护。
func (s *Store) verifySchema(ctx context.Context, version int) error {
	probes := [...]string{
		`SELECT session_id, seq, v, type, payload, created_at, task_id FROM events LIMIT 1`,
		`SELECT id, session_id, status, last_seq FROM tasks LIMIT 1`,
	}
	for _, query := range probes {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf(
				"store: 数据库 %s 的结构与迁移版本 %d 不一致（%v）；请先备份该文件，再重建数据库或升级 jiaoag-server",
				s.path, version, err)
		}
		rows.Close()
	}
	return nil
}

// ts 把时间编码为固定宽度的 UTC 文本；零值存空串，便于区分「未发生」。
//
// 必须用定长小数位：RFC3339Nano 会裁掉尾部零，导致字符串比较不等于时间
// 比较（".7Z" 会排在 ".75Z" 之后），ORDER BY created_at 就会错序。
func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func parseTS(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: 解析时间 %q: %w", v, err)
	}
	return t.UTC(), nil
}

// appendEventsTx 在事务内为一批事件分配 seq 与时间并写入。
func appendEventsTx(ctx context.Context, tx *sql.Tx, sessionID domain.SessionID, events []domain.Event) ([]domain.Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	var last int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0) FROM events WHERE session_id = ?`, string(sessionID)).Scan(&last); err != nil {
		return nil, fmt.Errorf("store: 读取最后序号: %w", err)
	}

	now := domain.Now()
	out := make([]domain.Event, 0, len(events))
	for i, ev := range events {
		if ev.SessionID == "" {
			ev.SessionID = sessionID
		}
		if ev.SessionID != sessionID {
			return nil, fmt.Errorf("store: 事件 %s 属于会话 %s，与目标会话 %s 不一致", ev.Type, ev.SessionID, sessionID)
		}
		if ev.V == 0 {
			ev.V = domain.EventVersion
		}
		if ev.At.IsZero() {
			ev.At = now
		}
		ev.Seq = domain.Seq(last + int64(i) + 1)
		if err := ev.Validate(); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events(session_id, seq, v, type, payload, created_at, task_id) VALUES(?,?,?,?,?,?,?)`,
			string(ev.SessionID), int64(ev.Seq), ev.V, string(ev.Type), string(ev.Payload),
			ts(ev.At), string(ev.TaskID)); err != nil {
			return nil, fmt.Errorf("store: 写入事件 %s#%d: %w", ev.Type, ev.Seq, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

func decodeEvents(rows *sql.Rows) ([]domain.Event, error) {
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var (
			ev      domain.Event
			session string
			task    string
			kind    string
			payload string
			created string
			seq     int64
			version int
		)
		if err := rows.Scan(&session, &seq, &version, &kind, &payload, &created, &task); err != nil {
			return nil, fmt.Errorf("store: 扫描事件: %w", err)
		}
		at, err := parseTS(created)
		if err != nil {
			return nil, err
		}
		ev = domain.Event{
			V:         version,
			Type:      domain.EventKind(kind),
			SessionID: domain.SessionID(session),
			TaskID:    domain.TaskID(task),
			Seq:       domain.Seq(seq),
			At:        at,
			Payload:   []byte(payload),
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历事件: %w", err)
	}
	return out, nil
}
