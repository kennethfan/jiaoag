package exec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// ErrProcessExited 表示后台进程已经结束，操作无从施加。
var ErrProcessExited = errors.New("exec: 后台进程已结束")

// Background 是后端的可选能力：非阻塞启动一条命令。
//
// 不放进 Backend 接口是刻意的——所有现有后端都实现 Run，但不是每个后端
// 都值得支持后台（例如未来只支持一次性执行的远端后端）；工具层拿到
// ErrNotAvailable 时应明确报错，而不是假装启动成功。
type Background interface {
	Start(plan Plan, spec Spec) (*Process, error)
}

// Process 是一个已经启动、还在运行（或刚结束）的后台命令。
//
// 它不等待退出：调用方拿到句柄就返回，输出持续写日志文件。收尾责任在
// 持有者（ProcTable）：没人 stop 就会一直跑到进程自己退出或服务端关闭。
type Process struct {
	ID         string
	Argv       []string
	OutputPath string
	PID        int
	StartedAt  time.Time

	cmd  *exec.Cmd
	pg   *procGroup
	log  *cappedFile
	done chan struct{}

	mu     sync.Mutex
	err    error
	exited bool
}

// Running 报告进程是否还在运行。
func (p *Process) Running() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Err 返回进程的退出错误（正常退出为 nil）。进程未结束时返回 nil。
func (p *Process) Err() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Wait 阻塞到进程退出。调用方必须自己保证不会无限等待（后台命令本就
// 可能长期运行）。
func (p *Process) Wait() {
	if p == nil {
		return
	}
	<-p.done
}

// Stop 杀掉整个进程组（Unix）或 Job（Windows）。已退出的进程视为成功。
func (p *Process) Stop() error {
	if p == nil {
		return nil
	}
	if !p.Running() {
		return nil
	}
	p.pg.kill(p.cmd)
	return nil
}

// startBackground 是 Direct 与 Sandbox 共用的启动实现，argv 是后端最终形状。
func startBackground(plan Plan, spec Spec, argv []string) (*Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("exec: 后台命令为空")
	}
	if spec.LogPath == "" {
		return nil, errors.New("exec: 后台命令必须指定日志路径")
	}
	logCap := spec.LogCap
	if logCap <= 0 {
		logCap = 4 << 20
	}
	log, err := openCappedFile(spec.LogPath, int64(logCap))
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = plan.Cwd
	if plan.Env != nil {
		cmd.Env = plan.Env
	}
	cmd.Stdout = log
	cmd.Stderr = log

	// 后台命令不施加资源限制：ulimit 的 CPU 时间与内存上限会把长驻的
	// 开发服务器直接杀掉，与「让它一直跑」的目的冲突。
	pg, err := newProcGroup(Limits{})
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	pg.beforeStart(cmd)
	if err := cmd.Start(); err != nil {
		pg.close()
		_ = log.Close()
		return nil, err
	}
	if err := pg.afterStart(cmd); err != nil {
		pg.kill(cmd)
		pg.close()
		_ = log.Close()
		return nil, err
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	p := &Process{
		Argv:       argv,
		OutputPath: spec.LogPath,
		PID:        pid,
		StartedAt:  time.Now(),
		cmd:        cmd,
		pg:         pg,
		log:        log,
		done:       make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.exited = true
		p.mu.Unlock()
		// 直接子进程退出后关掉 Job（Windows 的 KILL_ON_JOB_CLOSE 会顺带
		// 收掉残留子进程）；Unix 的进程组没有句柄，close 是空操作。
		pg.close()
		_ = log.Close()
		close(p.done)
	}()
	return p, nil
}

// cappedFile 把日志写到上限为止：后台进程可能长期运行，不能让它把磁盘写满。
type cappedFile struct {
	f       *os.File
	limit   int64
	written int64
	dropped bool
}

func openCappedFile(path string, limit int64) (*cappedFile, error) {
	// 与 capture 保持一致：日志目录由执行层创建，工具层不关心层级。
	// 少了这一步，任务里的第一条命令若是后台启动，就会因目录不存在失败。
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &cappedFile{f: f, limit: limit}, nil
}

func (w *cappedFile) Write(p []byte) (int, error) {
	if w.f == nil {
		return len(p), nil
	}
	room := w.limit - w.written
	if room <= 0 {
		w.dropped = true
		return len(p), nil
	}
	if int64(len(p)) > room {
		if _, err := w.f.Write(p[:room]); err != nil {
			return 0, err
		}
		w.written += room
		w.dropped = true
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.written += int64(n)
	return n, err
}

func (w *cappedFile) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	if w.dropped {
		_, _ = w.f.WriteString("\n（日志超过上限，后续输出已丢弃）\n")
	}
	err := w.f.Close()
	w.f = nil
	return err
}
