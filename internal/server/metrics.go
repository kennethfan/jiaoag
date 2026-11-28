package server

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"time"

	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
)

// metricsTimeout 限制聚合查询的时间：指标抓取不该因为库忙而挂住。
const metricsTimeout = 2 * time.Second

// handleMetrics 输出 Prometheus 文本格式。
//
// 需要令牌：库的行数与连接数属于侦察材料，和 /healthz 的匿名响应分级一致
// （见 ADR-0030 与 docs/tokens.md）。手写 exposition 而不引 prometheus 客户端
// 库：指标只有十几个，标准格式就四行语法。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorized(r); !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="jiaoag metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
	defer cancel()

	var (
		stats    store.Stats
		statsErr error
	)
	if s.app == nil {
		statsErr = fmt.Errorf("服务端还没有注入应用层")
	} else {
		stats, statsErr = s.app.Stats(ctx)
	}

	s.mu.Lock()
	connections := s.connCountLocked()
	subscriptions := 0
	for c := range s.conns {
		subscriptions += c.subCount()
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	writeGauge := func(name, help string, value any, labels string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		if labels == "" {
			fmt.Fprintf(w, "%s %v\n", name, value)
			return
		}
		fmt.Fprintf(w, "%s{%s} %v\n", name, labels, value)
	}

	writeGauge("jiaoag_info", "构建与协议版本，值恒为 1。", 1,
		fmt.Sprintf(`version=%q,protocol=%q`, s.version, fmt.Sprint(protocol.Version)))
	writeGauge("jiaoag_uptime_seconds", "服务端启动至今的秒数。",
		int64(time.Since(s.startedAt).Seconds()), "")
	writeGauge("jiaoag_goroutines", "当前 goroutine 数。", runtime.NumGoroutine(), "")
	writeGauge("jiaoag_connections", "当前 WebSocket 连接数。", connections, "")
	writeGauge("jiaoag_subscriptions", "当前活跃的事件订阅数（跨连接累加）。", subscriptions, "")
	writeGauge("jiaoag_memory_alloc_bytes", "堆上仍在使用的字节数。", mem.Alloc, "")
	writeGauge("jiaoag_memory_sys_bytes", "从系统申请的总字节数。", mem.Sys, "")
	writeGauge("jiaoag_gc_cycles", "已完成的 GC 次数。", mem.NumGC, "")

	if statsErr != nil {
		// 库读失败不该让整次抓取失败：进程指标仍然有意义，用 error 指标说明。
		writeGauge("jiaoag_metrics_error", "库聚合失败时为 1。", 1, "")
		return
	}
	writeGauge("jiaoag_metrics_error", "库聚合失败时为 1。", 0, "")
	writeGauge("jiaoag_sessions", "会话总数。", stats.Sessions, "")
	writeGauge("jiaoag_events", "事件行总数。", stats.Events, "")
	writeGauge("jiaoag_audit_rows", "审计行总数。", stats.Audit, "")

	statuses := make([]string, 0, len(stats.TasksByStatus))
	for status := range stats.TasksByStatus {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	fmt.Fprintf(w, "# HELP jiaoag_tasks 按状态统计的任务数。\n# TYPE jiaoag_tasks gauge\n")
	for _, status := range statuses {
		fmt.Fprintf(w, "jiaoag_tasks{status=%q} %d\n", status, stats.TasksByStatus[status])
	}

	fmt.Fprintf(w, "# HELP jiaoag_background_procs 后台进程数：running 与 orphaned。\n# TYPE jiaoag_background_procs gauge\n")
	fmt.Fprintf(w, "jiaoag_background_procs{status=\"running\"} %d\n", stats.ProcsRunning)
	fmt.Fprintf(w, "jiaoag_background_procs{status=\"orphaned\"} %d\n", stats.ProcsOrphaned)
}
