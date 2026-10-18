// uuid.go 上游会话身份三件套所需的 UUID v4 生成。
//
// 只服务 headers.go 的 applyConversationIdentity，与 internal/server 的
// protocolID（对外响应 id）刻意分开：那个带 `resp_` / `msg_` 前缀、面向客户端，
// 这个必须是**裸 UUID**（上游按 UUID 形态解析）。
package upstream

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"
)

// uuidFallbackSeq 熵源不可用时的单调序号（保证同一进程内三件套互不相同）。
var uuidFallbackSeq atomic.Uint64

// newUUIDv4 生成标准形态（8-4-4-4-12）的随机 UUID v4。
//
// 上游对三件套**不校验来源**（不要求它真的存在于某次会话），只拿它做关联，
// 所以本地生成即可；但同一请求内三个值必须互不相同 —— 复用等于拿一个 id
// 冒充三次会话身份（官方客户端每次各生成一个 randomUUID）。
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 取不到熵（极罕见）：退回纳秒时间 + 单调序号，形态仍是 UUID v4。
		binary.BigEndian.PutUint64(b[0:8], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint64(b[8:16], uuidFallbackSeq.Add(1))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
