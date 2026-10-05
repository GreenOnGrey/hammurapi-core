package nabu

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Verifier checks the JWTs Nabu presents when its personal agents call the
// MCP of Hammurapi (FTR.HMR.CMN-0006 tech §3.6): the EdDSA signature by the
// keys of NABU_URL/.well-known/jwks.json (cached; refreshed on an unknown kid),
// the issuer NABU_URL, the audience "hammurapi" and the expiry.
type Verifier struct {
	Issuer   string
	Audience string
	TTL      time.Duration
	HTTP     *http.Client

	mu      sync.Mutex
	keys    map[string]ed25519.PublicKey
	fetched time.Time
	now     func() time.Time
}

// Claims are the claims Hammurapi reads.
type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	ExpiresAt int64  `json:"exp"`
	Email     string `json:"email"`
}

// Errors of Verify (NB-06: 401).
var (
	ErrBadToken = errors.New("invalid Nabu token")
	ErrExpired  = errors.New("the Nabu token expired")
)

func (v *Verifier) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

func (v *Verifier) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && v.clock().Sub(v.fetched) < v.TTL {
		return k, nil
	}
	if v.clock().Sub(v.fetched) < 10*time.Second && v.keys != nil {
		if k, ok := v.keys[kid]; ok {
			return k, nil
		}
		return nil, ErrBadToken // unknown kid again: do not hammer the JWKS
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(v.Issuer, "/")+"/.well-known/jwks.json", nil)
	if err != nil {
		return nil, err
	}
	hc := v.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct {
			Kty, Crv, Kid, X string
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		if x, err := base64.RawURLEncoding.DecodeString(k.X); err == nil && len(x) == ed25519.PublicKeySize {
			keys[k.Kid] = ed25519.PublicKey(x)
		}
	}
	v.keys, v.fetched = keys, v.clock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, ErrBadToken
}

// Verify checks a token and returns its claims.
func (v *Verifier) Verify(ctx context.Context, tok string) (*Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, ErrBadToken
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &head) != nil || head.Alg != "EdDSA" {
		return nil, ErrBadToken
	}
	key, err := v.key(ctx, head.Kid)
	if err != nil {
		return nil, ErrBadToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, ErrBadToken
	}
	var c Claims
	bb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(bb, &c) != nil {
		return nil, ErrBadToken
	}
	if strings.TrimRight(c.Issuer, "/") != strings.TrimRight(v.Issuer, "/") || c.Audience != v.Audience {
		return nil, ErrBadToken
	}
	if v.clock().Unix() >= c.ExpiresAt {
		return nil, ErrExpired
	}
	return &c, nil
}
