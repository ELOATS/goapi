// Package authtest 在测试里签发 JWT，不需要真实的身份提供方。
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"ELOATS/goapi/internal/auth"
)

const (
	IssuerURL = "https://issuer.test"
	Audience  = "goapi-test"
)

// Issuer 持有一把 RSA 私钥。不同的 Issuer 密钥不同，可以用来测试"签名不匹配"。
type Issuer struct {
	key *rsa.PrivateKey
}

func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Issuer{key: key}
}

// Verifier 返回只信任这把密钥的校验器。
func (i *Issuer) Verifier(rolesClaim string) *auth.OIDCVerifier {
	return auth.NewStaticVerifier(IssuerURL, Audience, rolesClaim, &i.key.PublicKey)
}

// Token 签发一个 RS256 token，默认 1 小时有效。claims 覆盖默认值，值为 nil 表示删除该声明。
func (i *Issuer) Token(t testing.TB, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{
		"iss": IssuerURL,
		"aud": Audience,
		"sub": "user-1",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}

	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test"})
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + enc.EncodeToString(sig)
}

// JWKS 返回这把密钥的 JWK Set（JSON），用来模拟身份提供方的公钥地址。kid 与 Token 签发时一致。
func (i *Issuer) JWKS() []byte {
	pub := i.key.PublicKey
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"kid": "test",
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	return b
}
