// Package localshell 在客户端本机执行用户输入的 !命令。
//
// 它刻意不走 internal/exec：那边的入口是 agent 的工具，带审批、审计与
// exec.backend 沙箱。这里的命令是人显式敲的，等价于在自己终端里执行——
// 不需要审批，也不该被 agent 的写白名单限制。两边共用的只有「整组杀进程」
// 这一个细节，因此各自保留十几行实现而不是把 localshell 塞进执行后端。
package localshell

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// DefaultTimeout 是本地命令的默认上限：跑测试、构建够用，卡死也不会
	// 一直挂着。到点整组杀掉。
	DefaultTimeout = 180 * time.Second

	// MaxLineRunes 是单行输出的展示上限。压缩过的 JS、base64 之类一行能到
	// 几 MB，原样塞进对话区会把界面冲垮。
	MaxLineRunes = 2000

	// eventBuffer 让读协程不必等 TUI 处理完上一条才继续读；太小会让子进程
	// 卡在写管道上。
	eventBuffer = 64
)

// Options 控制一次本地执行。
type Options struct {
	// Workdir 是子进程的工作目录，通常是 TUI 的工作区。
	Workdir string
	// Timeout 为零时用 DefaultTimeout。
	Timeout time.Duration
}

// Event 是流式回传的一条消息：要么是一行输出，要么是结束态。
type Event struct {
	Text    string
	Clipped bool

	Done     bool
	ExitCode int
	Err      error
	TimedOut bool
	Stopped  bool
	Duration time.Duration
}

// Run 是一次本地执行。Events 按顺序给出所有输出行，最后一条 Done=true，
// 之后 channel 关闭。
type Run struct {
	cmd    *exec.Cmd
	events chan Event
	cancel context.CancelFunc

	mu       sync.Mutex
	stopped  bool
	timedOut bool
}

// Start 启动命令并立即返回。命令经平台 shell 执行（Unix 用 sh -c，
// Windows 用 cmd /c），因此管道、重定向、&& 都按 shell 语义生效。
func Start(opts Options, command string) *Run {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	cmd := shellCommand(command)
	cmd.Dir = opts.Workdir
	// 本地命令就是用户自己的 shell：继承完整环境。agent 的 commandEnv
	// 白名单是为了不把密钥交给模型选的命令，这里没有这个前提。
	cmd.Env = os.Environ()

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	run := &Run{cmd: cmd, events: make(chan Event, eventBuffer), cancel: cancel}
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		run.finish(Event{Err: err, ExitCode: -1}, time.Now())
		return run
	}
	go run.stream(ctx, pr, pw)
	return run
}

// Events 返回输出流。调用方必须持续读到 Done 为止：读端停下，子进程就会
// 卡在写管道上。
func (r *Run) Events() <-chan Event { return r.events }

// Stop 终止命令（整组杀进程）。已经结束时调用无副作用。
func (r *Run) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	// 先 cancel 再杀：cancel 让超时看门狗把这次的 Done 记成「已终止」
	// 而不是「超时」。
	r.cancel()
	r.kill()
}

func (r *Run) stream(ctx context.Context, pr *io.PipeReader, pw *io.PipeWriter) {
	start := time.Now()

	// 超时看门狗只认 DeadlineExceeded：Stop 走的是 cancel，不该被记成超时。
	go func() {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.mu.Lock()
			r.timedOut = true
			r.mu.Unlock()
			r.kill()
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- r.cmd.Wait() }()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		r.readLines(pr)
	}()

	// Wait 返回时 exec 内部把子进程输出拷进 pw 的协程已经结束，此时关掉
	// 写端，readLines 才能读到 EOF；否则它会永远停在最后一行之后。
	err := <-waitCh
	_ = pw.Close()
	<-readDone

	r.mu.Lock()
	timedOut, stopped := r.timedOut, r.stopped
	r.mu.Unlock()

	r.finish(Event{
		Err:      err,
		ExitCode: exitCode(err),
		TimedOut: timedOut,
		Stopped:  stopped,
	}, start)
}

func (r *Run) readLines(pr *io.PipeReader) {
	defer pr.Close()
	reader := bufio.NewReaderSize(pr, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			r.events <- lineEvent(line)
		}
		if err != nil {
			return
		}
	}
}

// finish 投递结束态并关闭 channel。只能调用一次（成功启动走 stream，
// 启动失败走 Start）。
func (r *Run) finish(ev Event, start time.Time) {
	ev.Done = true
	ev.Duration = time.Since(start)
	// 停掉超时看门狗：正常结束的进程不该让 ctx 一直挂着。
	r.cancel()
	r.events <- ev
	close(r.events)
}

func (r *Run) kill() {
	if r.cmd.Process == nil {
		return
	}
	killTree(r.cmd)
}

func lineEvent(line string) Event {
	text := strings.TrimRight(line, "\r\n")
	if utf8.RuneCountInString(text) <= MaxLineRunes {
		return Event{Text: text}
	}
	return Event{Text: string([]rune(text)[:MaxLineRunes]) + "…", Clipped: true}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
