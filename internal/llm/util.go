package llm

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// compactJSON 去掉多余空白，让两家协议返回的工具参数在归一后按字节可比。
// 解析失败时原样返回：坏数据应在调用方那里暴露，而不是被这里吞掉。
func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

// ptrString 让线格式区分「字段缺省」与「显式空串」。
//
// 工具结果为空（例如静默成功的 run_command）时两家都要求 content 字段存在，
// 但 omitempty 会把空串整个丢掉，下一轮请求就会 400。指针是唯一能同时表达
// 「不发送」与「发送空串」的写法。
func ptrString(s string) *string {
	return &s
}

// truncateUTF8 按字节上限截断，但不切断 UTF-8 码点。
//
// 错误信息里混着中文与 emoji，按字节切会把最后一个字符变成乱码。
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		return "…"
	}
	return s[:cut] + "…"
}
