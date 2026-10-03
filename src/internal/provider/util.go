// util.go 小工具：ID 生成、时间、字节处理。
package provider

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// timeNow 抽出来便于测试时替换（当前直接返回系统时间）。
var timeNow = time.Now

// randomID 生成 n 个十六进制字符的随机 ID。
//
// 取随机失败时退化为时间戳，绝不 panic —— 这里的 ID 只用于响应的 id 字段，
// 唯一性够用即可，不值得为一个 ID 把整个请求打挂。
func randomID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(timeNow().Format(time.RFC3339Nano)))[:n]
	}
	return hex.EncodeToString(b)[:n]
}
