package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseShortstat(t *testing.T) {
	for _, tc := range []struct {
		name, out      string
		added, removed int
	}{
		{"both", " 2 files changed, 12 insertions(+), 3 deletions(-)\n", 12, 3},
		{"insertions only", " 1 file changed, 5 insertions(+)\n", 5, 0},
		{"deletions only", " 1 file changed, 7 deletions(-)\n", 0, 7},
		{"singular", " 1 file changed, 1 insertion(+), 1 deletion(-)\n", 1, 1},
		{"no line changes", " 1 file changed, 0 insertions(+), 0 deletions(-)\n", 0, 0},
		{"empty", "", 0, 0},
		{"garbage", "fatal: not a git repository", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			added, removed := parseShortstat(tc.out)
			assert.Equal(t, tc.added, added)
			assert.Equal(t, tc.removed, removed)
		})
	}
}
