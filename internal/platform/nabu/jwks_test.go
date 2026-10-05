package nabu

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sign(t *testing.T, priv ed25519.PrivateKey, kid string, c map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	b, _ := json.Marshal(c)
	u := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	return u + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(u)))
}

// NB-06: a wrong signature, issuer, audience or an expired token is refused.
func TestVerifier(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub)}}})
	}))
	defer srv.Close()
	v := &Verifier{Issuer: srv.URL, Audience: "hammurapi", TTL: time.Minute}
	now := time.Now().Unix()
	ok := map[string]any{"iss": srv.URL, "aud": "hammurapi", "exp": now + 60, "email": "ann@x.org"}
	c, err := v.Verify(context.Background(), sign(t, priv, "k1", ok))
	if err != nil || c.Email != "ann@x.org" {
		t.Fatalf("%v %v", c, err)
	}
	for name, tok := range map[string]string{
		"signature": sign(t, other, "k1", ok),
		"issuer":    sign(t, priv, "k1", map[string]any{"iss": "https://evil", "aud": "hammurapi", "exp": now + 60}),
		"audience":  sign(t, priv, "k1", map[string]any{"iss": srv.URL, "aud": "other", "exp": now + 60}),
		"kid":       sign(t, priv, "k2", ok),
	} {
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrBadToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := v.Verify(context.Background(), sign(t, priv, "k1", map[string]any{"iss": srv.URL, "aud": "hammurapi", "exp": now - 1})); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: %v", err)
	}
}
