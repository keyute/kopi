package app

import (
	"regexp"
	"strconv"
)

// changesCommand counts tracked files, staged and unstaged, against HEAD. A
// clean or process filter that the repository's or the user's git config
// defines still runs on files .gitattributes assigns to it. safe.bareRepository
// refuses a bare repository embedded in a clone; autoRefreshIndex keeps git
// from writing the index and running its hook; GIT_NO_LAZY_FETCH keeps a
// partial clone's missing object from being fetched from a promisor remote,
// which can run repository-configured commands and reach the network.
const changesCommand = "LC_ALL=C GIT_NO_LAZY_FETCH=1 git --no-optional-locks --no-pager -c core.fsmonitor=false -c safe.bareRepository=explicit -c diff.autoRefreshIndex=false diff --shortstat --no-ext-diff --no-textconv --ignore-submodules=dirty HEAD --"

var (
	insertionsPattern = regexp.MustCompile(`(\d+) insertions?\(\+\)`)
	deletionsPattern  = regexp.MustCompile(`(\d+) deletions?\(-\)`)
)

// parseShortstat reads the counts from `git diff --shortstat` output; unparseable output counts zero.
func parseShortstat(out string) (added, removed int) {
	return shortstatCount(insertionsPattern, out), shortstatCount(deletionsPattern, out)
}

func shortstatCount(pattern *regexp.Regexp, out string) int {
	m := pattern.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
