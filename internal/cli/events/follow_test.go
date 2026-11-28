package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// bufferSink 收集写出的行，用来断言过滤结果。
type bufferSink struct {
	lines []string
}

func (b *bufferSink) writeLine(line []byte) error {
	b.lines = append(b.lines, strings.TrimRight(string(line), "\n"))
	return nil
}

func (b *bufferSink) Close() error { return nil }

func TestPrinterFiltersByTypeAndActionPrefix(t *testing.T) {
	sink := &bufferSink{}
	p := &printer{
		format: "json",
		sink:   sink,
		filter: eventFilter{types: []string{"approval"}, actions: []string{"exec"}},
	}

	env := protocol.Envelope{Type: protocol.TypeEvent, Seq: 3, SessionID: "ses_1"}
	p.event(env, protocol.StreamEventPayload{Kind: "approval.requested", Payload: json.RawMessage(`{}`)})
	p.event(env, protocol.StreamEventPayload{Kind: "tool.result", Payload: json.RawMessage(`{}`)})
	p.event(env, protocol.StreamEventPayload{Kind: "assistant.message", Payload: json.RawMessage(`{}`)})

	p.audit(protocol.AuditRecordPayload{ID: 1, Action: "exec.allow"})
	p.audit(protocol.AuditRecordPayload{ID: 2, Action: "write.applied"})
	p.audit(protocol.AuditRecordPayload{ID: 3, Action: "exec.approved"})

	if len(sink.lines) != 3 {
		t.Fatalf("期望 3 行（1 个事件 + 2 条 exec 审计），实际 %d：%v", len(sink.lines), sink.lines)
	}
	if !strings.Contains(sink.lines[0], "approval.requested") {
		t.Errorf("第一行应是 approval.requested: %s", sink.lines[0])
	}
	for _, line := range sink.lines[1:] {
		if !strings.Contains(line, `"action":"exec.`) {
			t.Errorf("审计行应只含 exec.*：%s", line)
		}
	}
}

func TestPrinterWithoutFilterKeepsEverything(t *testing.T) {
	sink := &bufferSink{}
	p := &printer{format: "json", sink: sink}
	env := protocol.Envelope{Type: protocol.TypeEvent, Seq: 1}
	p.event(env, protocol.StreamEventPayload{Kind: "task.queued", Payload: json.RawMessage(`{}`)})
	p.audit(protocol.AuditRecordPayload{ID: 7, Action: "context.compact"})
	if len(sink.lines) != 2 {
		t.Fatalf("无过滤时应全部输出，实际 %d 行", len(sink.lines))
	}
}

func TestFileSinkReopensAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "events.jsonl")
	s, err := openFileSink(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.writeLine([]byte("first")); err != nil {
		t.Fatal(err)
	}

	// 模拟 logrotate：文件被改名，路径上不再有文件。
	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatal(err)
	}

	if err := s.writeLine([]byte("second")); err != nil {
		t.Fatal(err)
	}

	old, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "first\n" {
		t.Errorf("轮转后的旧文件应只含第一行，实际 %q", old)
	}
	fresh, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("轮转后应在原路径重建文件: %v", err)
	}
	if string(fresh) != "second\n" {
		t.Errorf("新文件应只含第二行，实际 %q", fresh)
	}
}

func TestFileSinkTruncatesWithoutFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := openFileSink(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeLine([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh\n" {
		t.Errorf("非 -follow 应覆盖旧内容，实际 %q", got)
	}
}

func TestNewSinkDefaultsToStdout(t *testing.T) {
	s, err := newSink(options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*streamSink); !ok {
		t.Fatalf("没有 -output 时应写 stdout，得到 %T", s)
	}
}
