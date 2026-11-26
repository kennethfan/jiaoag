//go:build windows

package boot

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// fileLock 是运行时的独占锁。进程退出时系统会释放字节区间锁。
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
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, overlapped); err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
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
	overlapped := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, overlapped)
	_ = l.file.Close()
	l.file = nil
}
