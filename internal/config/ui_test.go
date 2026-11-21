package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFormat(t *testing.T) {
	lit := func(s string) Part { return Part{Kind: Literal, Text: s} }
	seg := func(s string) Part { return Part{Kind: Segment, Text: s} }
	group := func(ps ...Part) Part { return Part{Kind: Group, Parts: ps} }
	for _, tc := range []struct {
		name, in string
		want     Format
		segments []string
	}{
		{"segment", `$a`, Format{seg("a")}, []string{"a"}},
		{"name characters", `$a_1B`, Format{seg("a_1B")}, []string{"a_1B"}},
		{"text and segment", `ctx $context`, Format{lit("ctx "), seg("context")}, []string{"context"}},
		{"adjacent segments", `$a$b`, Format{seg("a"), seg("b")}, []string{"a", "b"}},
		{"name ends at punctuation", `$a-b`, Format{seg("a"), lit("-b")}, []string{"a"}},
		{"group", `$a( $b)`, Format{seg("a"), group(lit(" "), seg("b"))}, []string{"a", "b"}},
		{"nested groups", `(x($a)$b)`, Format{group(lit("x"), group(seg("a")), seg("b"))}, []string{"a", "b"}},
		{"escaped dollar", `\$a`, Format{lit("$a")}, nil},
		{"escaped parentheses", `\(\)$a`, Format{lit("()"), seg("a")}, []string{"a"}},
		{"escaped backslash", `\\$a`, Format{lit(`\`), seg("a")}, []string{"a"}},
		{"empty", ``, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFormat(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.segments, got.Segments())
		})
	}
}

func TestSegmentNames(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c", "a"}, SegmentNames([]string{"$a( $b)", "text"}, nil, []string{"$c$a"}))
	assert.Empty(t, SegmentNames())
}

func TestDefaultStatusLineReadsNoPlanLimits(t *testing.T) {
	line := DefaultStatusLine()

	for _, name := range SegmentNames(line.Left, line.Right) {
		assert.NotContains(t, LimitSegments, name)
	}
}
