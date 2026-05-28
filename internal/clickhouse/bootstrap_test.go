package clickhouse

import "testing"

func TestQuoteIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"analytics", "`analytics`"},
		{"weird`name", "`weird``name`"},
		{"", "``"},
	}
	for _, c := range cases {
		if got := quoteIdent(c.in); got != c.want {
			t.Errorf("quoteIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQuoteString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hex32bytes", "'hex32bytes'"},
		{"pw with 'quote'", "'pw with ''quote'''"},
		{"", "''"},
	}
	for _, c := range cases {
		if got := quoteString(c.in); got != c.want {
			t.Errorf("quoteString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
