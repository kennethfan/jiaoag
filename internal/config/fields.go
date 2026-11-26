package config

import (
	"reflect"
	"sort"
	"strings"
)

// secretType 用于识别需要脱敏的字段类型。Secret 的 String 已返回占位符，
// 但这里仍单独标记：show 要显示「来源 + 长度」，而不是一句 <已隐藏>。
var secretType = reflect.TypeOf(Secret(""))

// Field 是配置里一个叶子字段，path 是 config set/show 使用的点号路径
// （例如 providers[0].model、mcp.servers[0].env.GOPATH）。
type Field struct {
	Path   string
	Value  reflect.Value
	Type   reflect.Type
	Secret bool
	// Empty 表示这个字段没有值：空切片、空 map、nil 指针。
	Empty bool
}

// Fields 按结构体顺序列出所有叶子字段。
//
// 用反射而不是手写清单：新增配置项时不需要再同步维护一份列表，
// config show 与 config set 的路径索引天然一致。
func Fields(cfg *Config) []Field {
	if cfg == nil {
		return nil
	}
	var out []Field
	walkFields(reflect.ValueOf(cfg).Elem(), "", false, &out, 0)
	return out
}

func walkFields(v reflect.Value, prefix string, secret bool, out *[]Field, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			if prefix != "" {
				*out = append(*out, Field{Path: prefix, Type: v.Type(), Secret: secret, Empty: true})
			}
			return
		}
		walkFields(v.Elem(), prefix, secret, out, depth+1)
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			walkFields(v.Field(i), joinPath(prefix, name),
				secret || f.Type == secretType || secretName(name), out, depth+1)
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem() == secretType {
			// []Secret 只报数量，不逐项展开：一把密钥一行会把 show 冲垮。
			*out = append(*out, Field{Path: prefix, Value: v, Type: v.Type(), Secret: true, Empty: v.Len() == 0})
			return
		}
		if v.Len() == 0 {
			*out = append(*out, Field{Path: prefix, Type: v.Type(), Secret: secret, Empty: true})
			return
		}
		for i := 0; i < v.Len(); i++ {
			walkFields(v.Index(i), prefix+"["+itoa(i)+"]", secret, out, depth+1)
		}
	case reflect.Map:
		if v.Len() == 0 {
			*out = append(*out, Field{Path: prefix, Type: v.Type(), Secret: secret, Empty: true})
			return
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			walkFields(v.MapIndex(k), joinPath(prefix, k.String()), secret, out, depth+1)
		}
	default:
		if prefix == "" {
			return
		}
		*out = append(*out, Field{Path: prefix, Value: v, Type: v.Type(), Secret: secret})
	}
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// secretName 兜住少数用普通 string 承载的敏感字段（例如未来新增的 value 字段）。
func secretName(name string) bool {
	switch name {
	case "api_key", "api_keys", "token", "secret", "value", "password":
		return true
	default:
		return false
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// FindField 按路径查找字段；未命中时返回最相近的候选，便于报错时提示。
func FindField(cfg *Config, path string) (Field, bool, string) {
	fields := Fields(cfg)
	for _, f := range fields {
		if f.Path == path {
			return f, true, ""
		}
	}
	best, bestScore := "", 0.0
	for _, f := range fields {
		if s := similarity(path, f.Path); s > bestScore {
			best, bestScore = f.Path, s
		}
	}
	return Field{}, false, best
}

// similarity 是一个够用的近似匹配：共同前缀越长越像，长度差越大越不像。
// 不引入编辑距离库——这里只是为了让报错有建设性，不是做模糊搜索。
func similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	longest := len(a)
	if len(b) > longest {
		longest = len(b)
	}
	return float64(n) / float64(longest)
}
