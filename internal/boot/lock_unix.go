//go:build !windows

package boot

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// fileLock 是运行时的独占锁。进程崩溃时内核自动释放，不会留下需要手工清理的
// 陈锁文件；文件本身留着无妨，判断依据是 flock 能否加上。
type fileLock struct {
	file *os.File
}

func acquireLock(path string) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &fileLock{file: f}, nil
}

func (l *fileLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
