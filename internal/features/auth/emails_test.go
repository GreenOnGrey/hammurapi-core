package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// IN-04: the primary address may be unverified; a verified one is used instead,
// and an account without verified addresses gives no email at all.
func TestGitEmails(t *testing.T) {
	var list []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(list)
	}))
	defer srv.Close()
	list = []map[string]any{
		{"email": "Main@x.org", "primary": true, "verified": false},
		{"email": "Work@x.org", "primary": false, "verified": true},
	}
	primary, all := GitEmails(context.Background(), "github", srv.URL, "t")
	if primary != "work@x.org" || len(all) != 1 {
		t.Fatalf("%q %v", primary, all)
	}
	list = []map[string]any{{"email": "a@x.org", "primary": true, "verified": false}}
	if primary, all = GitEmails(context.Background(), "github", srv.URL, "t"); primary != "" || len(all) != 0 {
		t.Fatalf("%q %v", primary, all)
	}
}
