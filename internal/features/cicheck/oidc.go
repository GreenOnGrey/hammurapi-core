package cicheck

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDC verifies the OIDC tokens of CI jobs: the signature by the keys of a configured issuer, the audience
// (the public address of Hammurapi) and the time; it returns the repository the job runs in.
type OIDC struct {
	// Issuers maps an issuer to its provider: github or gitlab. A token of another issuer is refused before any
	// request to it.
	Issuers  map[string]string
	Audience string
	HTTP     *http.Client
	now      func() time.Time

	mu   sync.Mutex
	sets map[string]*keySet
}

type keySet struct {
	keys    map[string]crypto.PublicKey
	fetched time.Time
	tried   time.Time // the last fetch, for an unknown kid at most once a minute
}

const (
	keysTTL  = time.Hour
	keysWait = time.Minute
	leeway   = 60 * time.Second
)

// errToken is a token Hammurapi does not accept; its text goes to the CI.
var errToken = errors.New("the OIDC token is not accepted")

// Identity is who presents an OIDC token: the provider and the repository.
type Identity struct {
	Provider   string
	Repository string
}

// Verify checks a JWT and returns the identity of the job.
func (o *OIDC) Verify(ctx context.Context, token string) (*Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a JWT", errToken)
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &head); err != nil {
		return nil, fmt.Errorf("%w: %v", errToken, err)
	}
	var claims struct {
		Iss         string          `json:"iss"`
		Aud         json.RawMessage `json:"aud"`
		Exp         int64           `json:"exp"`
		Nbf         int64           `json:"nbf"`
		Iat         int64           `json:"iat"`
		Repository  string          `json:"repository"`   // GitHub
		ProjectPath string          `json:"project_path"` // GitLab
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("%w: %v", errToken, err)
	}
	provider, ok := o.Issuers[strings.TrimRight(claims.Iss, "/")]
	if !ok {
		return nil, fmt.Errorf("%w: issuer %q is not configured", errToken, claims.Iss)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature: %v", errToken, err)
	}
	key, err := o.key(ctx, strings.TrimRight(claims.Iss, "/"), head.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifySignature(head.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, fmt.Errorf("%w: %v", errToken, err)
	}
	if !o.audienceOK(claims.Aud) {
		return nil, fmt.Errorf("%w: the audience is not %s", errToken, o.Audience)
	}
	now := o.clock()
	switch {
	case claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(leeway)):
		return nil, fmt.Errorf("%w: expired", errToken)
	case claims.Nbf != 0 && now.Add(leeway).Before(time.Unix(claims.Nbf, 0)):
		return nil, fmt.Errorf("%w: not valid yet", errToken)
	case claims.Iat != 0 && now.Add(leeway).Before(time.Unix(claims.Iat, 0)):
		return nil, fmt.Errorf("%w: issued in the future", errToken)
	}
	repo := claims.Repository
	if provider == "gitlab" {
		repo = claims.ProjectPath
	}
	if repo == "" {
		return nil, fmt.Errorf("%w: no repository in the token", errToken)
	}
	return &Identity{Provider: provider, Repository: repo}, nil
}

func (o *OIDC) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// audienceOK: aud is a string or a list; the address is compared without a trailing slash.
func (o *OIDC) audienceOK(raw json.RawMessage) bool {
	want := strings.TrimRight(o.Audience, "/")
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return want != "" && strings.TrimRight(one, "/") == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if want != "" && strings.TrimRight(a, "/") == want {
				return true
			}
		}
	}
	return false
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func verifySignature(alg string, key crypto.PublicKey, signed, sig []byte) error {
	h := sha256.Sum256(signed)
	switch alg {
	case "RS256":
		k, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("RS256 with a key that is not RSA")
		}
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, h[:], sig)
	case "ES256":
		k, ok := key.(*ecdsa.PublicKey)
		if !ok || len(sig) != 64 {
			return errors.New("ES256 with a key that is not P-256 or a bad signature")
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k, h[:], r, s) {
			return errors.New("bad signature")
		}
		return nil
	}
	return fmt.Errorf("algorithm %q is not accepted", alg)
}

// key returns the key kid of the issuer: from the cache while it is fresh; otherwise the keys are read again —
// at most once a minute per issuer, whatever the cache holds, so forged tokens cannot make Hammurapi hammer the
// issuer. Between readings, and when a reading fails, the keys already known serve.
func (o *OIDC) key(ctx context.Context, issuer, kid string) (crypto.PublicKey, error) {
	o.mu.Lock()
	if o.sets == nil {
		o.sets = map[string]*keySet{}
	}
	set := o.sets[issuer]
	if set == nil {
		set = &keySet{}
		o.sets[issuer] = set
	}
	now := o.clock()
	k, known := set.keys[kid]
	if known && now.Sub(set.fetched) < keysTTL {
		o.mu.Unlock()
		return k, nil
	}
	if !set.tried.IsZero() && now.Sub(set.tried) < keysWait {
		o.mu.Unlock()
		if known {
			return k, nil // stale, but read again a moment ago
		}
		return nil, fmt.Errorf("%w: unknown key %q", errToken, kid)
	}
	set.tried = now
	o.mu.Unlock()

	keys, err := o.fetchKeys(ctx, issuer)
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		if known {
			return k, nil // the issuer is down: the key known before still verifies
		}
		return nil, fmt.Errorf("the keys of %s: %w", issuer, err)
	}
	set.keys, set.fetched = keys, o.clock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown key %q", errToken, kid)
}

func (o *OIDC) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (o *OIDC) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// fetchKeys reads the JWKS of the issuer through its discovery document.
func (o *OIDC) fetchKeys(ctx context.Context, issuer string) (map[string]crypto.PublicKey, error) {
	var disc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := o.getJSON(ctx, issuer+"/.well-known/openid-configuration", &disc); err != nil {
		return nil, err
	}
	if disc.JWKSURI == "" {
		return nil, errors.New("no jwks_uri in the discovery document")
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := o.getJSON(ctx, disc.JWKSURI, &set); err != nil {
		return nil, err
	}
	out := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		switch k.Kty {
		case "RSA":
			n, err1 := base64.RawURLEncoding.DecodeString(k.N)
			e, err2 := base64.RawURLEncoding.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
				continue
			}
			out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case "EC":
			if k.Crv != "P-256" {
				continue
			}
			x, err1 := base64.RawURLEncoding.DecodeString(k.X)
			y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
			if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
				continue
			}
			// the uncompressed point 0x04 || x || y; a point off the curve is refused
			pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
			if err != nil {
				continue
			}
			out[k.Kid] = pub
		}
	}
	return out, nil
}
