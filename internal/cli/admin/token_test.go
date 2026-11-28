package admin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/store"
)

func writeTokenConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "server:\n  listen: 127.0.0.1:0\n\nstorage:\n  db_path: " +
		filepath.Join(dir, "jiaoag.db") + "\n  output_dir: " + filepath.Join(dir, "out") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	return path
}

func runAdmin(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestTokenCreateRotateRevoke(t *testing.T) {
	cfgPath := writeTokenConfig(t)

	code, out, errOut := runAdmin(t, "token", "create", "ops", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("create 退出码 %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "明文只显示这一次") {
		t.Fatalf("create 应打印一次性明文提示: %s", out)
	}
	first := extractToken(out)
	if len(first) < 20 {
		t.Fatalf("没从输出里解析出令牌: %s", out)
	}

	// 明文不落库：库里只有 sha256。
	db, err := store.Open(context.Background(), dbPath(cfgPath))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	rows, err := db.ServerTokens(context.Background())
	if err != nil {
		t.Fatalf("读令牌: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "ops" || rows[0].ReadOnly {
		t.Fatalf("令牌记录不对: %+v", rows)
	}
	if rows[0].Hash == first || strings.Contains(rows[0].Hash, first[:8]) {
		t.Fatalf("库里不该出现明文: %+v", rows[0])
	}
	_ = db.Close()

	// 重名创建会被拒。
	if code, _, errOut := runAdmin(t, "token", "create", "ops", "-config", cfgPath); code == 0 {
		t.Fatal("重名创建应失败")
	} else if !strings.Contains(errOut, "已存在") {
		t.Fatalf("错误应提示已存在: %s", errOut)
	}

	// 轮换：明文变了。
	code, out, errOut = runAdmin(t, "token", "rotate", "ops", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("rotate 退出码 %d, stderr=%s", code, errOut)
	}
	second := extractToken(out)
	if second == "" || second == first {
		t.Fatalf("rotate 应产生新明文（first=%q second=%q）", first, second)
	}

	// 吊销需要 -yes。
	if code, out, _ := runAdmin(t, "token", "revoke", "ops", "-config", cfgPath); code != 1 {
		t.Fatalf("不带 -yes 应返回 1，实际 %d（输出 %s）", code, out)
	}
	if code, _, errOut := runAdmin(t, "token", "revoke", "ops", "-yes", "-config", cfgPath); code != 0 {
		t.Fatalf("带 -yes 应成功，实际 %d：%s", code, errOut)
	}

	code, out, errOut = runAdmin(t, "token", "list", "-config", cfgPath)
	if code != 0 {
		t.Fatalf("list 退出码 %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "ops") || !strings.Contains(out, "已吊销") {
		t.Fatalf("list 应显示已吊销状态: %s", out)
	}
}

func TestTokenReadOnlyFlag(t *testing.T) {
	cfgPath := writeTokenConfig(t)
	if code, _, errOut := runAdmin(t, "token", "create", "viewer", "-read-only", "-config", cfgPath); code != 0 {
		t.Fatalf("create -read-only 失败: %s", errOut)
	}
	db, err := store.Open(context.Background(), dbPath(cfgPath))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer db.Close()
	rows, err := db.ServerTokens(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("读令牌: %v %+v", err, rows)
	}
	if !rows[0].ReadOnly {
		t.Fatalf("viewer 应标记为只读: %+v", rows[0])
	}
}

func TestTokenUnknownSubcommand(t *testing.T) {
	code, _, errOut := runAdmin(t, "token", "nope")
	if code != 2 {
		t.Fatalf("未知子命令应返回 2，实际 %d", code)
	}
	if !strings.Contains(errOut, "未知子命令") {
		t.Fatalf("错误应说明未知子命令: %s", errOut)
	}
}

// extractToken 抓输出里那行缩进两格的令牌。
func extractToken(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 32 && !strings.ContainsAny(line, " ：,:") {
			return line
		}
	}
	return ""
}

func dbPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "jiaoag.db")
}
