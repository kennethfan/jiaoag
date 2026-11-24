package launch

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethfan/jiaoag/internal/config"
)

// serverProc 是 TUI 拉起的本地服务端子进程。
type serverProc struct {
	cmd     *exec.Cmd
	done    chan error
	log     *os.File
	logPath string
}

// IdleExit 是自动拉起的服务端的空闲退出时长。调用方正常退出时不发中断，
// 让服务端在最后一个连接断开后再等一会儿：同一台机器上的其它 TUI 还能继续用，
// 没人用时会自己退出，不留下孤儿进程。
const IdleExit = 30 * time.Second

// Ensure 保证本地服务端可用：已有实例则直接复用；连不上才在回环地址上
// 拉起 `jiaoag serve`（同一份二进制的子命令）。返回的收尾函数只关闭日志句柄，
// 不杀进程——服务端由自己的空闲倒计时负责退出。
//
// 只对回环地址自动拉起：-url 指向远端时不猜用户意图，仍然按原样连接。
func Ensure(ctx context.Context, cfg *config.Config, cfgPath, serverBin, target string) (func(), error) {
	addr, err := targetAddr(target)
	if err != nil {
		return nil, err
	}
	if !isLoopbackHost(addr.host) {
		return func() {}, nil
	}
	if tcpReachable(addr.hostport, 300*time.Millisecond) {
		return func() {}, nil
	}

	bin, err := resolveServerBinary(serverBin)
	if err != nil {
		return nil, err
	}
	proc, err := startServer(bin, cfgPath, cfg.Server.Listen)
	if err != nil {
		return nil, err
	}
	healthURL, client := healthProbe(cfg, addr.hostport)
	if err := waitHealthy(ctx, proc, healthURL, client); err != nil {
		proc.stop()
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "已启动本地服务端：%s（日志 %s）\n", bin, proc.logPath)
	return proc.detach, nil
}

type serverAddr struct {
	host     string
	port     string
	hostport string
}

// targetAddr 从 WS 地址里取出可拨号的主机与端口。
func targetAddr(target string) (serverAddr, error) {
	u, err := url.Parse(target)
	if err != nil {
		return serverAddr{}, fmt.Errorf("解析服务端地址 %q: %w", target, err)
	}
	host := u.Hostname()
	if host == "" {
		return serverAddr{}, fmt.Errorf("服务端地址 %q 缺少主机名", target)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return serverAddr{host: host, port: port, hostport: net.JoinHostPort(host, port)}, nil
}

// isLoopbackHost 判断是否本机回环地址。只在这类地址上自动拉起服务端。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// tcpReachable 只探测端口是否有人监听，不关心协议层是否健康。
func tcpReachable(hostport string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", hostport, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// resolveServerBinary 返回用来跑 serve 子命令的可执行文件。
//
// 默认就是自己：服务端与 TUI 是同一个二进制，靠 `serve` 区分。不能再去找同目录
// 的 jiaoag serve，也不能把符号链接当真——两种写法都会绕回 TUI 自己。
// -server-bin 可以覆盖（同样按 `<bin> serve` 调用），用于调试别的构建。
func resolveServerBinary(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位 jiaoag 可执行文件: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// startServer 启动服务端并把日志落到缓存目录。stdin 不继承，避免和 TUI 抢终端。
func startServer(bin, cfgPath, listen string) (*serverProc, error) {
	args := make([]string, 0, 7)
	args = append(args, "serve")
	if cfgPath != "" {
		args = append(args, "-config", cfgPath)
	}
	if listen != "" {
		args = append(args, "-listen", listen)
	}
	args = append(args, "-exit-when-idle", IdleExit.String())

	logPath, err := serverLogPath()
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开服务端日志 %s: %w", logPath, err)
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("启动 %s: %w", bin, err)
	}

	proc := &serverProc{cmd: cmd, done: make(chan error, 1), log: logFile, logPath: logPath}
	go func() { proc.done <- cmd.Wait() }()
	return proc, nil
}

// serverLogPath 选一个稳定位置放服务端日志，TUI 退出后仍可回看。
func serverLogPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "jiaoag")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建日志目录 %s: %w", dir, err)
	}
	return filepath.Join(dir, "server.log"), nil
}

// healthProbe 给出探活用的 URL 与客户端。
//
// 启用 TLS 时会跳到 https：自动拉起的服务端用的是配置里的证书，可能就是自签的，
// 而这里只关心「进程起来没有」，所以不验证书链。真正的连接是否跳校验由 TUI 的
// -insecure 决定，两者互不代替。
func healthProbe(cfg *config.Config, hostport string) (string, *http.Client) {
	if !cfg.Server.TLSEnabled() {
		return "http://" + hostport + "/healthz", &http.Client{Timeout: 500 * time.Millisecond}
	}
	return "https://" + hostport + "/healthz", &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
}

// waitHealthy 轮询 /healthz，直到服务端可用、子进程退出或超时。
func waitHealthy(ctx context.Context, proc *serverProc, url string, client *http.Client) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case err := <-proc.done:
			proc.done <- err // 保留结果给 stop 再取
			return fmt.Errorf("服务端启动失败：%v\n%s", err, tailFile(proc.logPath, 2<<10))
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("服务端 15 秒内未就绪（%s）\n%s", url, tailFile(proc.logPath, 2<<10))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop 先发中断让服务端走优雅收尾，超时才强杀。
func (p *serverProc) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	_ = p.log.Close()
}

// detach 关掉父进程这一侧的日志句柄，不碰子进程：自动拉起的服务端带
// -exit-when-idle，会在最后一个连接断开后自己退出。
func (p *serverProc) detach() {
	if p == nil || p.log == nil {
		return
	}
	_ = p.log.Close()
}

// tailFile 读取文件末尾，用来把启动失败原因直接展示给用户。
func tailFile(path string, size int64) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("（无法读取日志 %s: %v）", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("（无法读取日志 %s: %v）", path, err)
	}
	if info.Size() > size {
		if _, err := f.Seek(-size, io.SeekEnd); err != nil {
			return fmt.Sprintf("（无法读取日志 %s: %v）", path, err)
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Sprintf("（无法读取日志 %s: %v）", path, err)
	}
	return strings.TrimSpace(string(data))
}
