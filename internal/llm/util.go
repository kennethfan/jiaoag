package llm

import (
	"bytes"
	"encoding/json"
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
