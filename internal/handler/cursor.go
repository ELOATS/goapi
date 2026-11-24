package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"example.com/goapi/internal/domain"
)

// 游标格式：base64url(JSON) "." base64url(HMAC-SHA256 前 16 字节)。
// 对客户端是不透明字符串；被篡改或伪造的游标解码失败，接口返回 400。
// 现在游标里只有排序位置，签名的意义是：以后往游标里加过滤条件或权限相关的信息时不用改格式，
// 也不会出现"客户端自己拼一个游标"被当成合法输入的情况。

// MinCursorKeyLen 是游标签名密钥的最小长度（字节）。
const MinCursorKeyLen = 32

const (
	maxCursorLen = 512
	macLen       = 16
	macDomain    = "goapi-cursor-v1:" // 防止同一把密钥签的其它东西被当成游标
)

var b64 = base64.RawURLEncoding.Strict()

type cursorPayload struct {
	T  time.Time `json:"t"`
	ID string    `json:"id"`
}

// CursorCodec 负责游标的编码、签名和校验。用 current 签名；校验时 current 和 previous 都接受，
// 轮换密钥时把旧密钥放进 previous，旧游标在过渡期内仍然有效。
type CursorCodec struct {
	keys [][]byte
}

// NewCursorCodec 创建游标编解码器，每把密钥至少 MinCursorKeyLen 字节。
func NewCursorCodec(current []byte, previous ...[]byte) (*CursorCodec, error) {
	keys := append([][]byte{current}, previous...)
	for _, k := range keys {
		if len(k) < MinCursorKeyLen {
			return nil, errors.New("cursor key must be at least 32 bytes")
		}
	}
	return &CursorCodec{keys: keys}, nil
}

// Encode 返回带签名的游标字符串。
func (c *CursorCodec) Encode(cur domain.UserCursor) string {
	b, _ := json.Marshal(cursorPayload{T: cur.CreatedAt, ID: cur.ID}) // 结构固定，不会失败
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(sign(c.keys[0], payload))
}

// Decode 校验签名并解析游标。
func (c *CursorCodec) Decode(s string) (domain.UserCursor, error) {
	if len(s) > maxCursorLen {
		return domain.UserCursor{}, errors.New("cursor too long")
	}
	payload, sigStr, ok := strings.Cut(s, ".")
	if !ok || payload == "" || sigStr == "" {
		return domain.UserCursor{}, errors.New("malformed cursor")
	}
	// Strict：拒绝最后一个字符里不为 0 的填充位。否则签名末尾字符有几种写法都能解出同样的字节，
	// 同一个游标会有多个"合法"的字符串表示
	sig, err := b64.DecodeString(sigStr)
	if err != nil {
		return domain.UserCursor{}, errors.New("malformed cursor")
	}
	valid := false
	for _, k := range c.keys {
		if hmac.Equal(sig, sign(k, payload)) { // 常量时间比较
			valid = true
			break
		}
	}
	if !valid {
		return domain.UserCursor{}, errors.New("invalid cursor signature")
	}

	b, err := b64.DecodeString(payload)
	if err != nil {
		return domain.UserCursor{}, err
	}
	var p cursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return domain.UserCursor{}, err
	}
	if p.ID == "" || p.T.IsZero() {
		return domain.UserCursor{}, errors.New("incomplete cursor")
	}
	return domain.UserCursor{CreatedAt: p.T, ID: p.ID}, nil
}

func sign(key []byte, payload string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(macDomain))
	h.Write([]byte(payload))
	return h.Sum(nil)[:macLen]
}
