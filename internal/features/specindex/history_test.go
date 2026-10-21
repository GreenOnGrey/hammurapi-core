package specindex

import "testing"

// API-01–API-03 (FTR.HMR.SPEC-0001 qa): the history of a file at GitHub and
// GitLab; no link when the path or the form of the commit URL is unknown.
func TestHistoryURL(t *testing.T) {
	cases := []struct{ name, commit, path, want string }{
		{"github", "https://github.com/o/specs/commit/" + marker, "specs/A/B/x/product/spec.md",
			"https://github.com/o/specs/commits/main/specs/A/B/x/product/spec.md"},
		{"gitlab", "https://gitlab.example/o/specs/-/commit/" + marker, "specs/A/B/x/product/spec.md",
			"https://gitlab.example/o/specs/-/commits/main/specs/A/B/x/product/spec.md"},
		{"empty path", "https://github.com/o/specs/commit/" + marker, "", ""},
		{"unknown form", "https://git.example/o/specs/c?sha=" + marker, "specs/x/spec.md", ""},
		{"no provider url", "", "specs/x/spec.md", ""},
	}
	for _, c := range cases {
		if got := historyURL(c.commit, "main", c.path); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// API-04 (FTR.HMR.SPEC-0001 qa): the view link escapes the name.
func TestViewURL(t *testing.T) {
	got := viewURL("FTR.HMR.CMN-0005", "design", "flow & map.html")
	want := "/api/v1/spec/files/FTR.HMR.CMN-0005/design/raw?mode=view&name=flow+%26+map.html"
	if got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}
