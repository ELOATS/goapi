package auth

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Verifier 校验原始 token 并返回调用方。
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Principal, error)
}

type OIDCConfig struct {
	IssuerURL  string // 必须与 discovery 文档里的 issuer、token 里的 iss 完全一致，包括末尾斜杠
	Audience   string // token 的 aud 必须包含它，否则拒绝（防止接受签给其它应用的 token）
	RolesClaim string // 角色所在的声明，支持点号路径，例如 realm_access.roles
	// DiscoveryURL 可选：服务访问身份提供方的地址和 issuer 不同时设置。典型场景是本地 docker compose：
	// 浏览器和命令行通过 http://localhost:8180 拿 token（iss 是这个地址），容器里的 api 只能通过
	// http://keycloak:8080 访问。设置后 discovery 和拉公钥走这个地址，iss 仍然按 IssuerURL 校验。
	DiscoveryURL string
}

// ErrKeysUnavailable 表示暂时拿不到身份提供方的签名公钥（JWKS），无法判断 token 是否有效。
// 中间件据此返回 503 而不是 401：客户端不该因此丢掉 token 或让用户重新登录。
var ErrKeysUnavailable = errors.New("auth: signing keys unavailable")

// OIDCVerifier 校验签名（JWKS）、iss、aud、exp，然后提取角色。
type OIDCVerifier struct {
	verifier   *oidc.IDTokenVerifier
	rolesClaim string
	keys       *keyFetchTracker // 静态公钥时为 nil
}

// keyFetchTracker 记录向身份提供方发请求（discovery、拉取 JWKS）失败的次数。
type keyFetchTracker struct {
	failures atomic.Uint64
}

type trackingTransport struct {
	next http.RoundTripper
	t    *keyFetchTracker
}

func (tr trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := tr.next.RoundTrip(req)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		tr.t.failures.Add(1)
	}
	return resp, err
}

func newKeyClient(t *keyFetchTracker) *http.Client {
	// 拉公钥发生在请求的关键路径上（缓存里找不到能验签的公钥时），超时不能太长
	return &http.Client{Timeout: 5 * time.Second, Transport: trackingTransport{next: http.DefaultTransport, t: t}}
}

// NewOIDCVerifier 在启动时做一次 OIDC discovery，拿到 JWKS 地址；
// 公钥在第一次校验时拉取并缓存，遇到未知的 kid（身份提供方轮换密钥）会自动刷新。
func NewOIDCVerifier(c OIDCConfig) (*OIDCVerifier, error) {
	tracker := &keyFetchTracker{}
	// 不用调用方传入的、可能会被取消的 ctx：discovery 之后的公钥刷新也要用这里的 HTTP client。
	ctx := oidc.ClientContext(context.Background(), newKeyClient(tracker))
	discovery := c.IssuerURL
	if c.DiscoveryURL != "" && c.DiscoveryURL != c.IssuerURL {
		// discovery 文档里的 issuer 和请求地址不一致时，go-oidc 默认拒绝；这里明确告诉它 issuer 是什么。
		// 只放宽"从哪里取 discovery 文档"，token 的 iss 仍然必须等于 IssuerURL
		ctx = oidc.InsecureIssuerURLContext(ctx, c.IssuerURL)
		discovery = c.DiscoveryURL
	}
	provider, err := oidc.NewProvider(ctx, discovery)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", discovery, err)
	}
	return &OIDCVerifier{
		verifier:   provider.Verifier(verifierConfig(c.Audience)),
		rolesClaim: c.RolesClaim,
		keys:       tracker,
	}, nil
}

// NewRemoteVerifier 不做 discovery，直接从 jwksURL 拉取公钥。用于不支持 discovery 的身份提供方和测试。
func NewRemoteVerifier(issuer, jwksURL, audience, rolesClaim string) *OIDCVerifier {
	tracker := &keyFetchTracker{}
	ctx := oidc.ClientContext(context.Background(), newKeyClient(tracker))
	return &OIDCVerifier{
		verifier:   oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(ctx, jwksURL), verifierConfig(audience)),
		rolesClaim: rolesClaim,
		keys:       tracker,
	}
}

// NewStaticVerifier 用固定公钥校验，不做 discovery。用于测试，或者不支持 discovery 的身份提供方。
func NewStaticVerifier(issuer, audience, rolesClaim string, keys ...crypto.PublicKey) *OIDCVerifier {
	ks := &oidc.StaticKeySet{PublicKeys: keys}
	return &OIDCVerifier{
		verifier:   oidc.NewVerifier(issuer, ks, verifierConfig(audience)),
		rolesClaim: rolesClaim,
	}
}

// 显式限定非对称签名算法：绝不接受 none 或 HS256（否则攻击者可以用公钥当 HMAC 密钥伪造 token）。
func verifierConfig(audience string) *oidc.Config {
	return &oidc.Config{
		ClientID:             audience,
		SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256},
	}
}

// Verify 校验 token。暂时拿不到公钥时返回包裹了 ErrKeysUnavailable 的错误。
func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	var before uint64
	if v.keys != nil {
		before = v.keys.failures.Load()
	}
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		// go-oidc 先检查 iss、aud、exp，再验签；缓存里没有能验签的公钥时会重新拉取 JWKS。
		// 这次校验期间拉取失败了，说明不是 token 本身的问题，而是暂时无法判断。
		// （极少数情况下，并发的另一个请求恰好拉取失败，也会让这次的普通失败被归为 503。）
		if v.keys != nil && v.keys.failures.Load() != before {
			return Principal{}, fmt.Errorf("%w: %w", ErrKeysUnavailable, err)
		}
		return Principal{}, err
	}
	if tok.Subject == "" {
		return Principal{}, errors.New("token has no sub claim")
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Principal{}, fmt.Errorf("decode claims: %w", err)
	}

	p := Principal{
		Subject: tok.Subject,
		Roles:   stringList(lookup(claims, v.rolesClaim)),
		Scopes:  stringList(claims["scope"]), // OAuth2 标准：空格分隔的字符串
	}
	if len(p.Scopes) == 0 {
		p.Scopes = stringList(claims["scp"]) // Microsoft Entra ID 等使用 scp
	}
	if p.Roles == nil {
		p.Roles = []string{}
	}
	p.Email, _ = claims["email"].(string)
	return p, nil
}

// lookup 先按完整键名查找（Auth0 的自定义声明形如 "https://example.com/roles"，本身带点），
// 找不到再按点号路径逐层查找（Keycloak 的 realm_access.roles）。
func lookup(claims map[string]any, path string) any {
	if path == "" {
		return nil
	}
	if v, ok := claims[path]; ok {
		return v
	}
	var cur any = claims
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// stringList 接受字符串数组，或空格分隔的字符串。
func stringList(v any) []string {
	switch x := v.(type) {
	case string:
		return strings.Fields(x)
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
