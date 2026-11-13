package exec

import (
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// capture 把子进程输出按「头 + 尾 + 全文日志」三路收敛。
//
// 关键约束（issue #5）：Write 永不阻塞、内存只留头尾、日志有上限。
// 白名单里有 cat，cat /dev/zero 是无限流——任何一路没收敛住都会出事。
type capture struct {
	spec     Spec
	head     []byte
	tail     []byte
	total    int
	log      *os.File
	logBytes int
	dropped  int
	logOK    bool
	logErr   error
}

func newCapture(spec Spec) *capture {
	spec = spec.normalize()
	c := &capture{spec: spec}
	if spec.LogPath == "" {
		return c
	}
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o750); err != nil {
		c.logErr = err
		return c
	}
	// 日志可能含命令输出里的敏感内容，只给属主读写。
	f, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		c.logErr = err
		return c
	}
	c.log = f
	c.logOK = true
	return c
}

// Write 实现 io.Writer，同时喂给内存缓冲与日志文件。
//
// 返回值永远等于 len(p)：即使被丢弃，也要让子进程以为自己写成功了，
// 否则它会在管道上阻塞，超时与取消就都失效了。
func (c *capture) Write(p []byte) (int, error) {
	original := p
	c.total += len(p)

	if !c.spec.TailOnly {
		if n := c.spec.HeadBytes - len(c.head); n > 0 {
			if n > len(p) {
				n = len(p)
			}
			c.head = append(c.head, p[:n]...)
			p = p[n:]
		}
	}
	if len(p) > 0 {
		c.tail = append(c.tail, p...)
		if len(c.tail) > c.spec.TailBytes {
			c.tail = c.tail[len(c.tail)-c.spec.TailBytes:]
		}
	}

	c.writeLog(original)
	return len(original), nil
}

func (c *capture) writeLog(p []byte) {
	if c.log == nil {
		return
	}
	remain := c.spec.LogCap - c.logBytes
	if remain <= 0 {
		c.dropped += len(p)
		return
	}
	if len(p) > remain {
		if _, err := c.log.Write(p[:remain]); err != nil {
			c.logErr = err
			return
		}
		c.logBytes += remain
		c.dropped += len(p) - remain
		return
	}
	if _, err := c.log.Write(p); err != nil {
		c.logErr = err
		return
	}
	c.logBytes += len(p)
}

// Close 关闭日志文件；超出上限的部分在末尾留一条说明。
func (c *capture) Close() {
	if c.log == nil {
		return
	}
	if c.dropped > 0 {
		_, _ = fmt.Fprintf(c.log, "\n…（日志已达 %d 字节上限，后续丢弃 %d 字节）…\n",
			c.spec.LogCap, c.dropped)
	}
	_ = c.log.Sync()
	_ = c.log.Close()
	c.log = nil
}

// Result 组装回给工具层的结果。OutputPath 只在日志完整写成功时才有值：
// 写了一半的文件不能让模型照着路径去读。
func (c *capture) Result() Result {
	var res Result
	if c.logOK && c.logErr == nil {
		res.OutputPath = c.spec.LogPath
	}
	switch {
	case c.logErr != nil:
		res.LogError = c.logErr.Error()
	case c.spec.LogPath != "" && !c.logOK:
		res.LogError = "无法创建日志文件"
	}
	body, gap := c.render()
	res.Output = body
	// 事件截断与日志写满各走各的字段：日志超限不代表模型看到的事件载荷
	// 不完整，混在一起会让 tool.result.truncated 撒谎。
	res.Truncated = gap
	res.LogTruncated = c.dropped > 0
	return res
}

// render 拼出进事件的那段文本：能全放就全放，放不下就头 + 提示 + 尾。
func (c *capture) render() (string, bool) {
	if c.spec.TailOnly {
		if c.total <= len(c.tail) {
			return string(c.tail), false
		}
		marker := fmt.Sprintf("\n…（已截断 %d 字节）…\n", c.total-len(c.tail))
		avail := c.spec.EventCap - len(marker)
		if avail < 0 {
			avail = 0
		}
		tail := c.tail
		if len(tail) > avail {
			tail = tail[len(tail)-avail:]
		}
		return marker + string(trimRuneHead(tail)), true
	}
	if c.total <= len(c.head)+len(c.tail) {
		return string(c.head) + string(c.tail), false
	}

	marker := fmt.Sprintf("\n…（已截断 %d 字节）…\n", c.total-len(c.head)-len(c.tail))
	avail := c.spec.EventCap - len(marker) - len(c.tail)
	if avail < 0 {
		avail = 0
	}
	head := c.head
	if len(head) > avail {
		head = head[:avail]
	}
	// 不为凑满预算切坏 UTF-8：中文日志切在半个 rune 上会产出非法字符串。
	return string(trimRuneTail(head)) + marker + string(trimRuneHead(c.tail)), true
}

// trimRuneTail 去掉尾部不完整的 UTF-8 前缀。
func trimRuneTail(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size > 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// trimRuneHead 去掉头部不完整的 UTF-8 续字节。
func trimRuneHead(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError || size > 1 {
			return b
		}
		b = b[1:]
	}
	return b
}
