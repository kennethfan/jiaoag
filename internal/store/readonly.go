package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// OpenReadOnly 以只读方式打开一个已经初始化过的库，供 sessions/events 这类
// 纯读取命令使用。
//
// 与 Open 的区别是刻意的：不建目录、不建库、不跑迁移、不拿写锁。只读命令
// 不该为了看一眼历史数据而拉起一个服务端，也不该在库缺失时悄悄建一个空库
// 让人误以为数据没了。因此这三件事在这里都变成明确的报错。
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: 数据库路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: 解析数据库路径 %q: %w", path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("store: 数据库 %s 还不存在；先启动一次 jiaoag 让它初始化", abs)
		}
		return nil, fmt.Errorf("store: 检查数据库 %s: %w", abs, err)
	}

	db, err := sql.Open("sqlite", readonlyDSN(abs))
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库 %s: %w", abs, err)
	}
	s := &Store{db: db, path: abs}

	migrations, err := loadMigrations()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	newest := migrations[len(migrations)-1].version

	var current int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		_ = db.Close()
		// 空文件或别的应用的 SQLite 库：没有 schema_migrations 就是没初始化，
		// 不该让人去猜「版本读取失败」是不是数据损坏。
		if strings.Contains(err.Error(), "no such table") {
			return nil, fmt.Errorf("store: 数据库 %s 还没有初始化；先启动一次 jiaoag", abs)
		}
		return nil, fmt.Errorf("store: 读取 %s 的结构版本失败（可能不是 jiaoag 的库）: %w", abs, err)
	}
	switch {
	case current == 0:
		_ = db.Close()
		return nil, fmt.Errorf("store: 数据库 %s 还没有初始化；先启动一次 jiaoag", abs)
	case current < newest:
		_ = db.Close()
		return nil, fmt.Errorf("store: 数据库 %s 的结构版本是 v%d，当前程序需要 v%d：先启动一次 jiaoag 完成迁移", abs, current, newest)
	case current > newest:
		_ = db.Close()
		return nil, fmt.Errorf("store: 数据库 %s 的结构版本 v%d 高于当前程序支持的 v%d，请升级 jiaoag", abs, current, newest)
	}
	if err := s.verifyMigrationChecksums(ctx, migrations, current); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.verifySchema(ctx, current); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// readonlyDSN 只带读取需要的 pragma。刻意不加 journal_mode/synchronous：
// 库的日志模式是创建时定下的，只读打开也不该去改它。
func readonlyDSN(abs string) string {
	return sqliteDSN(abs, "mode=ro")
}
