package web

import "testing"

func TestSafeRedirect(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"oauth authorize", "/oauth/authorize?response_type=code&client_id=x", true},
		{"plain path", "/projects/abc", true},
		{"empty", "", false},
		{"external https", "http://evil.com/x", false},
		{"external https 2", "https://evil.com/x", false},
		{"protocol relative //", "//evil.com/x", false},
		{"backslash bypass", "/\\evil.com/x", false},
		{"javascript scheme", "javascript:alert(1)", false},
		{"reentrant /login", "/login", false},
		{"reentrant /login query", "/login?next=/oauth/", false},
		{"reentrant /login subpath", "/login/foo", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := safeRedirect(c.in); got != c.want {
				t.Errorf("safeRedirect(%q): got %v want %v", c.in, got, c.want)
			}
		})
	}
}
