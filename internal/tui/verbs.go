package tui

import "math/rand/v2"

// verb names a turn while it runs (ing) and once it has completed (ed).
type verb struct{ ing, ed string }

var verbs = []verb{
	{"Brewing", "Brewed"},
	{"Roasting", "Roasted"},
	{"Grinding", "Ground"},
	{"Steeping", "Steeped"},
	{"Percolating", "Percolated"},
	{"Pouring", "Poured"},
	{"Stirring", "Stirred"},
	{"Frothing", "Frothed"},
	{"Simmering", "Simmered"},
	{"Filtering", "Filtered"},
}

// fallbackVerb names turns that no live TurnStart picked a verb for, such as
// restored ones.
var fallbackVerb = verb{"Working", "Worked"}

func randomVerb() verb { return verbs[rand.IntN(len(verbs))] }
