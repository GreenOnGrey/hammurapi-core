package cicheck

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// issuerServer serves the discovery document and the JWKS of an issuer with an RSA and a P-256 key.
type issuerServer struct {
	*httptest.Server
	rsa     *rsa.PrivateKey
	ec      *ecdsa.PrivateKey
	fetches atomic.Int32
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newIssuer(t *testing.T) *issuerServer {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuerServer{rsa: rk, ec: ek}
	is.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.URL, "jwks_uri": is.URL + "/keys"})
		case "/keys":
			is.fetches.Add(1)
			pt, _ := ek.PublicKey.Bytes() // 0x04 || x || y
			x, y := pt[1:33], pt[33:]
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
				{"kty": "RSA", "kid": "r1", "n": b64(rk.N.Bytes()), "e": b64(big.NewInt(int64(rk.E)).Bytes())},
				{"kty": "EC", "kid": "e1", "crv": "P-256", "x": b64(x), "y": b64(y)},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(is.Close)
	return is
}

func (is *issuerServer) sign(t *testing.T, alg, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	signed := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	switch alg {
	case "RS256":
		var err error
		if sig, err = rsa.SignPKCS1v15(rand.Reader, is.rsa, crypto.SHA256, sum[:]); err != nil {
			t.Fatal(err)
		}
	case "ES256":
		r, s, err := ecdsa.Sign(rand.Reader, is.ec, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	}
	return signed + "." + b64(sig)
}

// FTR.HMR.SPEC-0003 qa OIDC-01–OIDC-07.
func TestOIDC(t *testing.T) {
	gh, gl := newIssuer(t), newIssuer(t)
	now := time.Unix(1_800_000_000, 0)
	o := &OIDC{Issuers: map[string]string{gh.URL: "github", gl.URL: "gitlab"}, Audience: "https://hammurapi-api.example", now: func() time.Time { return now }}
	ctx := context.Background()
	base := func(iss string) map[string]any {
		return map[string]any{"iss": iss, "aud": "https://hammurapi-api.example/", "exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
			"repository": "Fleet/Booking", "project_path": "fleet/booking-gl"}
	}
	// OIDC-01: GitHub, RS256; the audience matches without the trailing slash
	id, err := o.Verify(ctx, gh.sign(t, "RS256", "r1", base(gh.URL)))
	if err != nil || id.Provider != "github" || id.Repository != "Fleet/Booking" {
		t.Fatalf("OIDC-01: %+v %v", id, err)
	}
	// OIDC-02: GitLab, ES256, the repository is project_path; aud as a list
	c := base(gl.URL)
	c["aud"] = []string{"other", "https://hammurapi-api.example"}
	if id, err := o.Verify(ctx, gl.sign(t, "ES256", "e1", c)); err != nil || id.Provider != "gitlab" || id.Repository != "fleet/booking-gl" {
		t.Fatalf("OIDC-02: %+v %v", id, err)
	}
	refused := func(name string, token string) {
		t.Helper()
		if _, err := o.Verify(ctx, token); !errors.Is(err, errToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// OIDC-03: another audience
	c = base(gh.URL)
	c["aud"] = "https://other.example"
	refused("OIDC-03", gh.sign(t, "RS256", "r1", c))
	// OIDC-04: expired
	c = base(gh.URL)
	c["exp"] = now.Add(-2 * time.Minute).Unix()
	refused("OIDC-04", gh.sign(t, "RS256", "r1", c))
	// OIDC-05: an issuer that is not configured is refused without a request to it
	stranger := newIssuer(t)
	refused("OIDC-05", stranger.sign(t, "RS256", "r1", base(stranger.URL)))
	if stranger.fetches.Load() != 0 {
		t.Error("OIDC-05: the keys of a stranger were fetched")
	}
	// OIDC-07: a changed token
	tok := gh.sign(t, "RS256", "r1", base(gh.URL))
	parts := strings.Split(tok, ".")
	c = base(gh.URL)
	c["repository"] = "fleet/pricing"
	forged, _ := json.Marshal(c)
	refused("OIDC-07", parts[0]+"."+b64(forged)+"."+parts[2])
	// a key of the wrong kind for the algorithm, an algorithm that is not accepted
	refused("alg", gh.sign(t, "ES256", "r1", base(gh.URL)))
	refused("none", b64([]byte(`{"alg":"none","kid":"r1"}`))+"."+parts[1]+".")
	// OIDC-06: an unknown kid reads the keys again once a minute after the last reading, then waits
	now = now.Add(2 * time.Minute)
	before := gh.fetches.Load()
	refused("OIDC-06", gh.sign(t, "RS256", "r9", base(gh.URL)))
	refused("OIDC-06 again", gh.sign(t, "RS256", "r9", base(gh.URL)))
	if got := gh.fetches.Load() - before; got != 1 {
		t.Errorf("OIDC-06: %d fetches of the keys", got)
	}
}
