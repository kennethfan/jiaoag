package tools

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Match 判断工作区相对路径是否匹配 glob 模式。
//
// 比 path.Match 多一条规则：`**` 匹配任意层目录（含零层），因此
// `**/*.go` 能匹配根目录下的 a.go。分隔符统一用 `/`。
func Match(pattern, name string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	if pattern == "" {
		return false
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, parts []string) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchSegments(pat[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 || parts[0] == "" {
		return false
	}
	ok, err := path.Match(pat[0], parts[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], parts[1:])
}

// Validate 只检查 glob 模式的语法，不关心能否匹配到文件。
//
// 逐段用 path.Match 探测：它只在模式非法时报错，拿一个占位名字足以区分
// 「语法错」和「没匹配上」——后者由 Match 返回 false，不该报错。
func Validate(pattern string) error {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	if pattern == "" {
		return errors.New("glob 模式为空")
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, "placeholder"); err != nil {
			return fmt.Errorf("glob 模式 %q 无效：%w", pattern, err)
		}
	}
	return nil
}
