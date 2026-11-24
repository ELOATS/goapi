package handler

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"ELOATS/goapi/internal/domain"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, MinCursorKeyLen) }

func mustCodec(t *testing.T, current []byte, previous ...[]byte) *CursorCodec {
	t.Helper()
	c, err := NewCursorCodec(current, previous...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var sample = domain.UserCursor{CreatedAt: time.Date(2026, 11, 24, 3, 0, 0, 123, time.UTC), ID: "11111111-1111-4111-8111-111111111111"}

func TestCursorRoundTrip(t *testing.T) {
	c := mustCodec(t, testKey(1))
	got, err := c.Decode(c.Encode(sample))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sample.ID || !got.CreatedAt.Equal(sample.CreatedAt) {
		t.Fatalf("round trip = %+v, want %+v", got, sample)
	}
}

func TestCursorRejectsTampering(t *testing.T) {
	c := mustCodec(t, testKey(1))
	s := c.Encode(sample)
	payload, sig, _ := strings.Cut(s, ".")
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"t":"2026-11-24T03:00:00Z","id":"someone-else"}`))

	for name, in := range map[string]string{
		"payload swapped":   forged + "." + sig,
		"unsigned":          payload,
		"unsigned forged":   forged,
		"empty signature":   payload + ".",
		"empty payload":     "." + sig,
		"wrong signature":   payload + ".AAAAAAAAAAAAAAAAAAAAAA",
		"not base64":        "!!!.!!!",
		"too long":          strings.Repeat("a", maxCursorLen+1),
		"signature flipped": payload + "." + flipMiddle(sig),
		"padding bits set":  payload + "." + setPaddingBit(sig),
	} {
		if _, err := c.Decode(in); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	if _, err := mustCodec(t, testKey(2)).Decode(s); err == nil {
		t.Error("cursor signed with another key must be rejected")
	}
}

func TestCursorKeyRotation(t *testing.T) {
	old := mustCodec(t, testKey(1))
	rotated := mustCodec(t, testKey(2), testKey(1))

	if _, err := rotated.Decode(old.Encode(sample)); err != nil {
		t.Fatalf("cursor signed with the previous key should still be accepted: %v", err)
	}
	fresh := rotated.Encode(sample)
	if _, err := mustCodec(t, testKey(2)).Decode(fresh); err != nil {
		t.Fatalf("new cursors must be signed with the current key: %v", err)
	}
	if _, err := old.Decode(fresh); err == nil {
		t.Fatal("old codec must not accept cursors signed with the new key")
	}
}

func TestNewCursorCodecRequiresLongKeys(t *testing.T) {
	if _, err := NewCursorCodec(make([]byte, MinCursorKeyLen-1)); err == nil {
		t.Error("short current key must be rejected")
	}
	if _, err := NewCursorCodec(testKey(1), []byte("short")); err == nil {
		t.Error("short previous key must be rejected")
	}
}

// flipMiddle 改签名中间的一个字符：中间字符的 6 位都是有效数据，改了签名一定变
func flipMiddle(s string) string {
	i := len(s) / 2
	repl := byte('A')
	if s[i] == 'A' {
		repl = 'B'
	}
	return s[:i] + string(repl) + s[i+1:]
}

// setPaddingBit 只改最后一个字符里的填充位：16 字节的签名编码成 22 个字符，最后一个字符只有高 2 位是数据。
// 非 Strict 的解码会忽略填充位、解出同样的签名；Strict 解码应该拒绝
func setPaddingBit(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, s[len(s)-1])
	return s[:len(s)-1] + string(alphabet[last|1])
}
