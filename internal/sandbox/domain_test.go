package sandbox

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDomain(t *testing.T) {
	for entry, want := range map[string]domain{
		"a.b":         {host: "a.b"},
		"*.a.b":       {host: "a.b", wildcard: true},
		"a.b:443":     {host: "a.b", port: 443},
		"*.a.b:8080":  {host: "a.b", wildcard: true, port: 8080},
		"A.B":         {host: "a.b"},
		"a_b.c-d.e":   {host: "a_b.c-d.e"},
		"a.0xg":       {host: "a.0xg"},
		"a.b:65535":   {host: "a.b", port: 65535},
		"1.2.3.4.xyz": {host: "1.2.3.4.xyz"},
	} {
		got, err := parseDomain(entry)
		require.NoError(t, err, entry)
		assert.Equal(t, want, got, entry)
		assert.NoError(t, ValidateDomain(entry), entry)
	}
}

func TestParseDomainRejects(t *testing.T) {
	for _, entry := range []string{
		"", "*.com", "*", "*.", "a.b:0", "a.b:080", "a.b:65536", "a.b:", "http://a.b", "a.b/x",
		"1.2.3.4", "1.2.3.4:80", "[::1]", "::1", "localhost", "localhost:80", "*.localhost", "a",
		"1.2.3", "0x7f.1", "1.0x7f", "1.2.0x1", "a.0x", "*.2.0x1", "*.0.1", "a.b.1", "a..b", ".a.b", "a.b.", "a.*.b", "*a.b", "a.b*", "**.a.b", "a b.c", "é.com",
		strings.Repeat("a", 250) + ".com",
	} {
		_, err := parseDomain(entry)
		assert.Error(t, err, entry)
		require.ErrorIs(t, ValidateDomain(entry), ErrInvalidDomain, entry)
	}
}

func TestDomainMatches(t *testing.T) {
	tests := []struct {
		entry, host string
		port        int
		want        bool
	}{
		{"a.b", "a.b", 443, true},
		{"a.b", "x.a.b", 443, false},
		{"A.B", "a.b", 443, true},
		{"*.a.b", "a.b", 443, false},
		{"*.a.b", "x.a.b", 443, true},
		{"*.a.b", "x.y.a.b", 443, true},
		{"*.a.b", "xa.b", 443, false},
		{"*.a.b", "x.a.bc", 443, false},
		{"a.b:443", "a.b", 443, true},
		{"a.b:443", "a.b", 80, false},
		{"*.a.b:443", "x.a.b", 80, false},
	}
	for _, tt := range tests {
		d, err := parseDomain(tt.entry)
		require.NoError(t, err, tt.entry)
		assert.Equal(t, tt.want, d.matches(tt.host, tt.port), "%s vs %s:%d", tt.entry, tt.host, tt.port)
	}
}

func TestDomainWildcardNeverMatchesAnIPLiteral(t *testing.T) {
	// parseDomain cannot produce this entry; matches guards on its own.
	d := domain{host: "3.4", wildcard: true}
	assert.False(t, d.matches("1.2.3.4", 80))
	assert.False(t, d.matches("::ffff:1.2.3.4", 80))
}
