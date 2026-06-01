package web

import (
	"net/url"
	"testing"
)

// appendQuery must use "?" when the URI has no query yet, and "&" when it
// does — so a registered redirect_uri like https://app/cb?env=prod doesn't
// produce a malformed ...?env=prod?code=... URL on the OAuth success path
// (RFC 6749 §3.1.2 allows a query in the registered redirect URI).
func TestAppendQuery(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		vals url.Values
		want string
	}{
		{
			name: "no existing query",
			uri:  "https://app.example.com/cb",
			vals: url.Values{"code": {"abc"}, "state": {"xyz"}},
			want: "https://app.example.com/cb?code=abc&state=xyz",
		},
		{
			name: "existing query",
			uri:  "https://app.example.com/cb?env=prod",
			vals: url.Values{"code": {"abc"}, "state": {"xyz"}},
			want: "https://app.example.com/cb?env=prod&code=abc&state=xyz",
		},
		{
			name: "existing query trailing &",
			uri:  "https://app.example.com/cb?env=prod&",
			vals: url.Values{"code": {"abc"}},
			want: "https://app.example.com/cb?env=prod&code=abc",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := appendQuery(c.uri, c.vals); got != c.want {
				t.Errorf("appendQuery(%q): got %q want %q", c.uri, got, c.want)
			}
		})
	}
}
