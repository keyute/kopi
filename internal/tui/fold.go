package tui

import "github.com/keyute/kopi/internal/transcript"

// folds is the frontend's own view state over the transcript: which sealed
// groups are open (by group key), which calls show their output (by call
// ID), whether everything is shown, and the verb each live turn picked (by
// turn key).
type folds struct {
	open     map[string]bool
	expanded map[string]bool
	all      bool
	verbs    map[string]verb
}

// blockFolds is the part of the view state one block's lines depend on.
// expanded is per call: a group's calls in order, or a row's one call. verb
// is a turn's.
type blockFolds struct {
	open     bool
	expanded []bool
	verb     verb
}

func newFolds() folds {
	return folds{open: map[string]bool{}, expanded: map[string]bool{}, verbs: map[string]verb{}}
}

func (f *folds) of(b transcript.Block) blockFolds {
	switch b := b.(type) {
	case transcript.Group:
		bf := blockFolds{open: f.all || f.open[b.Key()]}
		for _, c := range b.Calls {
			bf.expanded = append(bf.expanded, f.all || f.expanded[c.ID])
		}
		return bf
	case transcript.Row:
		if b.Call.State != transcript.Running {
			return blockFolds{expanded: []bool{f.all || f.expanded[b.Call.ID]}}
		}
	case transcript.Turn:
		if v, ok := f.verbs[b.Key()]; ok {
			return blockFolds{verb: v}
		}
		return blockFolds{verb: fallbackVerb}
	}
	return blockFolds{}
}

// settle forgets the expansions of every call in a group drawn closed, so a
// group reopens collapsed whether the user closed it or it sealed itself.
func (f *folds) settle(blocks []transcript.Block) {
	if f.all {
		return
	}
	for _, b := range blocks {
		g, ok := b.(transcript.Group)
		if !ok || !g.Sealed || f.open[g.Key()] {
			continue
		}
		for _, c := range g.Calls {
			delete(f.expanded, c.ID)
		}
	}
}

// click toggles what h names.
func (f *folds) click(h hit) {
	switch h.kind {
	case hitHandle:
		f.open[h.id] = !f.open[h.id]
	case hitOutput:
		f.expanded[h.id] = !f.expanded[h.id]
	}
}

// toggleAll flips show-all; turning it off clears every expansion.
func (f *folds) toggleAll() {
	f.all = !f.all
	if !f.all {
		f.expanded = map[string]bool{}
	}
}
