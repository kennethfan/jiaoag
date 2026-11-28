package events

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// sink 是导出目标的抽象：stdout，或一个会被日志轮转挪走的文件。
//
// 轮转这件事只能由写入方处理：外部工具（logrotate / newsyslog）把文件改名
// 后，进程手里的 fd 还指着旧 inode，继续写下去日志就丢了。所以每写一行
// 前用 os.SameFile 对一下路径当前指向的文件，变了就重开。
type sink interface {
	writeLine(b []byte) error
	Close() error
}

type streamSink struct{ w io.Writer }

func (s *streamSink) writeLine(b []byte) error {
	_, err := s.w.Write(append(b, '\n'))
	return err
}

func (s *streamSink) Close() error { return nil }

// fileSink 追加写一个文件，并在检测到轮转（inode 变化或文件被删）后重开。
type fileSink struct {
	path string

	mu   sync.Mutex
	f    *os.File
	info os.FileInfo
}

// openFileSink 打开导出文件。appendMode=false 用于一次性的快照导出（覆盖旧
// 内容），true 用于 -follow（接着写，别把上一段历史抹掉）。
func openFileSink(path string, appendMode bool) (*fileSink, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析输出路径失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flags, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开输出文件失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("读取输出文件状态失败: %w", err)
	}
	return &fileSink{path: abs, f: f, info: info}, nil
}

func (s *fileSink) writeLine(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reopenIfRotated(); err != nil {
		return err
	}
	_, err := s.f.Write(append(b, '\n'))
	return err
}

func (s *fileSink) reopenIfRotated() error {
	cur, err := os.Stat(s.path)
	if err == nil && s.info != nil && os.SameFile(cur, s.info) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("检查输出文件失败: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("重开输出文件失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("读取输出文件状态失败: %w", err)
	}
	if s.f != nil {
		_ = s.f.Close()
	}
	s.f, s.info = f, info
	return nil
}

func (s *fileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// newSink 按选项选目标：没给 -output 就写 stdout，给了就写文件（-follow 时
// 追加，否则覆盖）。
func newSink(opts options) (sink, error) {
	if opts.output == "" {
		return &streamSink{w: os.Stdout}, nil
	}
	return openFileSink(opts.output, opts.follow)
}
