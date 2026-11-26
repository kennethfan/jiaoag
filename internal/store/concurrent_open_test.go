package store_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kennethfan/jiaoag/internal/store"
)

// 两个运行时同时启动会并发迁移：后到的那个必须看到前者已提交的版本并跳过，
// 而不是撞上「table sessions already exists」。
func TestConcurrentOpenMigratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	stores := make([]*store.Store, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := store.Open(context.Background(), path)
			errs[i], stores[i] = err, db
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个 Open 失败: %v", i+1, err)
		}
	}
	for _, db := range stores {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Fatalf("关闭: %v", err)
			}
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据库文件不存在: %v", err)
	}
}
