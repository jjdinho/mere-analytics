package oauth

import "testing"

func TestValidateRedirectURI(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  bool // true = allowed
	}{
		{"https with host", "https://app.example.com/cb", true},
		{"https no host", "https:///cb", false},
		{"http localhost", "http://localhost:9999/cb", true},
		{"http 127.0.0.1", "http://127.0.0.1:9999/cb", true},
		{"http evil", "http://evil.example/cb", false},
		{"ftp", "ftp://example.com/", false},
		{"javascript", "javascript:alert(1)", false},
		{"fragment", "https://app.example.com/cb#x", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateRedirectURI(c.in)
			if (err == nil) != c.want {
				t.Errorf("validateRedirectURI(%q): err=%v want allowed=%v", c.in, err, c.want)
			}
		})
	}
}
