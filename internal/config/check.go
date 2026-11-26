package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Issue 是 config check 的一条结果。
type Issue struct {
	Level   string // "error" | "warn"
	Field   string
	Message string
}

// Check 做不联网的自检：语法、字段校验、令牌文件、目录可写、以及几条
// 值得提醒但不阻止启动的事实。
//
// 它不探测供应商与 MCP——那些要出网、要起子进程，属于 --online。
func Check(path, role string) (string, []Issue, *Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	p, err := ExpandHome(path)
	if err != nil {
		return "", nil, nil, err
	}
	cfg, err := Load(p)
	if err != nil {
		return p, nil, nil, err
	}
	var issues []Issue
	addErr := func(field, format string, args ...any) {
		issues = append(issues, Issue{Level: "error", Field: field, Message: fmt.Sprintf(format, args...)})
	}
	addWarn := func(field, format string, args ...any) {
		issues = append(issues, Issue{Level: "warn", Field: field, Message: fmt.Sprintf(format, args...)})
	}

	switch role {
	case "client":
		if err := cfg.ValidateClient(); err != nil {
			addErr("", "%v", err)
		}
	default:
		role = "server"
		if err := cfg.ValidateServer(); err != nil {
			addErr("", "%v", err)
		}
	}

	// 令牌逐个解析：ValidateServer 已经把错误汇总成一句话，这里按名字拆开，
	// 让人直接知道是哪一把坏了。
	for i, t := range cfg.Server.AllTokens() {
		if _, err := t.Value.Resolve(); err != nil {
			addErr(fmt.Sprintf("server.tokens[%d].value", i), "令牌 %q 无法解析：%v", t.Name, err)
		}
	}

	if role == "server" {
		if err := dirWritable(filepath.Dir(cfg.Storage.DBPath)); err != nil {
			addErr("storage.db_path", "数据目录不可写：%v", err)
		}
		if err := dirWritable(cfg.Storage.OutputDir); err != nil {
			addErr("storage.output_dir", "输出目录不可写：%v", err)
		}
		if host, _, err := net.SplitHostPort(cfg.Server.Listen); err == nil {
			if !isLoopback(host) && !cfg.Server.TLSEnabled() {
				addWarn("server.listen", "监听 %s 且未配置 TLS，令牌会以明文传输", cfg.Server.Listen)
			}
		}
		if cfg.Agent.WriteApprovalMode() == WriteApprovalOff {
			addWarn("agent.write_approval", "写入永不审批：模型改文件不再询问，只能事后审计")
		}
	}
	return p, issues, cfg, nil
}

func dirWritable(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".jiaoag-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func isLoopback(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
