package policy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var reviewSpecs = map[string]ToolSpec{
	"Edit":  {Capability: Write, Target: pathTarget},
	"Bash":  {Capability: Exec},
	"Fetch": {Capability: Read, Ask: true},
	"Shell": {Capability: Exec, Sandboxed: true},
}

// reviewer returns the scripted reviews in order, then the last one forever,
// and records each request.
type reviewer struct {
	reviews []Review
	err     error
	reqs    []Request
}

func (f *reviewer) review(_ context.Context, r Request) (Review, error) {
	f.reqs = append(f.reqs, r)
	rev := f.reviews[min(len(f.reqs), len(f.reviews))-1]
	return rev, f.err
}

func newReviewGate(t *testing.T, f *reviewer, approve Approver, src ...Source) (*Authority, *Gate) {
	t.Helper()
	d := newDirs(t)
	rules, err := NewRules(d.ws, d.home, reviewSpecs, src...)
	require.NoError(t, err)
	a := NewAuthority()
	a.EnterExecute()
	return a, NewGate(a, reviewSpecs, rules, approve, f.review)
}

func TestReviewAllowPassesWithoutAsking(t *testing.T) {
	f, h := &reviewer{reviews: []Review{{Verdict: VerdictAllow}}}, &approver{}
	_, g := newReviewGate(t, f, h.approve)
	require.NoError(t, g.Before(t.Context(), "c1", "Bash", nil))
	require.NoError(t, admitApproved(g, "c1", "Bash"))
	assert.Len(t, f.reqs, 1)
	assert.Empty(t, h.reqs)
}

func TestReviewDenyIsDeniedWithReason(t *testing.T) {
	f, h := &reviewer{reviews: []Review{{Verdict: VerdictDeny, Reason: "it wipes the disk"}}}, &approver{}
	_, g := newReviewGate(t, f, h.approve)
	err := g.Before(t.Context(), "c1", "Bash", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), `tool "Bash" was denied by the approval classifier: it wipes the disk`)
	assert.Empty(t, h.reqs)

	f.reviews = []Review{{Verdict: VerdictDeny}}
	err = g.Before(t.Context(), "c2", "Bash", nil)
	assert.EqualError(t, err, `tool "Bash" was denied by the approval classifier: `+ErrDenied.Error())
}

func TestReviewUndecidedGoesToTheHuman(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *reviewer
		want Review
	}{
		{"ask", &reviewer{reviews: []Review{{Verdict: VerdictAsk, Reason: "unsure"}}}, Review{Verdict: VerdictAsk, Reason: "unsure"}},
		{"error", &reviewer{reviews: []Review{{Verdict: VerdictAllow}}, err: errors.New("no model")}, Review{Verdict: VerdictAsk}},
		{"bad verdict", &reviewer{reviews: []Review{{Verdict: 42, Reason: "odd"}}}, Review{Verdict: 42, Reason: "odd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &approver{answer: Allow}
			_, g := newReviewGate(t, tc.f, h.approve)
			require.NoError(t, g.Before(t.Context(), "c1", "Bash", nil))
			require.Len(t, h.reqs, 1)
			require.NotNil(t, h.reqs[0].Review)
			assert.Equal(t, tc.want, *h.reqs[0].Review)
		})
	}
}

func TestReviewNeverSeesRuledCalls(t *testing.T) {
	gitFile := func(ws string) string { return filepath.Join(ws, ".git", "config") }
	for _, tc := range []struct {
		name      string
		src       Source
		tool      string
		wantAsked bool
		wantErr   error
	}{
		{"ask rule", Source{Name: "config", Ask: []string{"Bash"}}, "Bash", true, nil},
		{"allow rule", Source{Name: "config", Allow: []string{"Bash"}}, "Bash", false, nil},
		{"deny rule", Source{Name: "config", Deny: []string{"Bash"}}, "Bash", false, ErrDenied},
		{"built-in ask", Source{Name: "config"}, "Edit", true, nil},
		{"sandboxed", Source{Name: "config"}, "Shell", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := &reviewer{reviews: []Review{{Verdict: VerdictDeny}}}, &approver{answer: Allow}
			d := newDirs(t)
			rules, err := NewRules(d.ws, d.home, reviewSpecs, tc.src)
			require.NoError(t, err)
			a := NewAuthority()
			a.EnterExecute()
			g := NewGate(a, reviewSpecs, rules, h.approve, f.review)
			args := pathArgs(gitFile(d.ws))
			err = g.Before(t.Context(), "c1", tc.tool, args)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Empty(t, f.reqs)
			assert.Equal(t, tc.wantAsked, len(h.reqs) == 1)
			for _, r := range h.reqs {
				assert.Nil(t, r.Review)
			}
		})
	}
}

func TestReviewAllowsUnruledWritesWithoutEitherAsking(t *testing.T) {
	f, h := &reviewer{reviews: []Review{{Verdict: VerdictDeny}}}, &approver{}
	_, g := newReviewGate(t, f, h.approve)
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", pathArgs("/ws/a.go")))
	require.NoError(t, admitApproved(g, "c1", "Edit"))
	assert.Empty(t, f.reqs)
	assert.Empty(t, h.reqs)

	h.answer = Reject
	d := newDirs(t)
	rules, err := NewRules(d.ws, d.home, reviewSpecs)
	require.NoError(t, err)
	a := NewAuthority()
	a.EnterExecute()
	g = NewGate(a, reviewSpecs, rules, h.approve, nil)
	require.ErrorIs(t, g.Before(t.Context(), "c1", "Edit", pathArgs("/ws/a.go")), ErrRejected)
	assert.Len(t, h.reqs, 1)
}

func TestReviewedAskToolGoesToReviewer(t *testing.T) {
	f, h := &reviewer{reviews: []Review{{Verdict: VerdictAllow}}}, &approver{}
	_, g := newReviewGate(t, f, h.approve)
	require.NoError(t, g.Before(t.Context(), "c1", "Fetch", nil))
	assert.Len(t, f.reqs, 1)
	assert.Empty(t, h.reqs)
}

func TestReviewEscalatesThirdConsecutiveDeny(t *testing.T) {
	f, h := &reviewer{reviews: []Review{{Verdict: VerdictDeny, Reason: "no"}}}, &approver{answer: Allow}
	_, g := newReviewGate(t, f, h.approve)
	for _, id := range []string{"c1", "c2"} {
		require.ErrorIs(t, g.Before(t.Context(), id, "Bash", nil), ErrDenied)
	}
	assert.Empty(t, h.reqs)
	require.NoError(t, g.Before(t.Context(), "c3", "Bash", nil))
	require.Len(t, h.reqs, 1)
	assert.Equal(t, &Review{Verdict: VerdictDeny, Reason: "no"}, h.reqs[0].Review)
}

func TestReviewCountersReset(t *testing.T) {
	deny, allow := Review{Verdict: VerdictDeny}, Review{Verdict: VerdictAllow}
	call := func(g *Gate, id string) error { return g.Before(t.Context(), id, "Bash", nil) }

	t.Run("reviewer allow", func(t *testing.T) {
		f, h := &reviewer{reviews: []Review{deny, deny, allow, deny, deny, deny}}, &approver{answer: Allow}
		_, g := newReviewGate(t, f, h.approve)
		for _, id := range []string{"c1", "c2"} {
			require.ErrorIs(t, call(g, id), ErrDenied)
		}
		require.NoError(t, call(g, "c3"))
		for _, id := range []string{"c4", "c5"} {
			require.ErrorIs(t, call(g, id), ErrDenied)
		}
		assert.Empty(t, h.reqs)
		require.NoError(t, call(g, "c6"))
		assert.Len(t, h.reqs, 1)
	})

	t.Run("human approval", func(t *testing.T) {
		f, h := &reviewer{reviews: []Review{deny}}, &approver{answer: Allow}
		_, g := newReviewGate(t, f, h.approve)
		for _, id := range []string{"c1", "c2"} {
			require.ErrorIs(t, call(g, id), ErrDenied)
		}
		require.NoError(t, call(g, "c3"))
		for _, id := range []string{"c4", "c5"} {
			require.ErrorIs(t, call(g, id), ErrDenied)
		}
		assert.Len(t, h.reqs, 1)
		require.NoError(t, call(g, "c6"))
		assert.Len(t, h.reqs, 2)
	})

	for _, tc := range []struct {
		name string
		src  Source
		path string
	}{
		{"explicit ask approval", Source{Name: "config", Ask: []string{"Edit"}}, "a.go"},
		{"built-in ask approval", Source{Name: "config"}, filepath.Join(".git", "config")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := &reviewer{reviews: []Review{deny}}, &approver{answer: Allow}
			d := newDirs(t)
			rules, err := NewRules(d.ws, d.home, reviewSpecs, tc.src)
			require.NoError(t, err)
			a := NewAuthority()
			a.EnterExecute()
			g := NewGate(a, reviewSpecs, rules, h.approve, f.review)
			for _, id := range []string{"c1", "c2"} {
				require.ErrorIs(t, call(g, id), ErrDenied)
			}
			require.NoError(t, g.Before(t.Context(), "approval", "Edit", pathArgs(filepath.Join(d.ws, tc.path))))
			require.Len(t, h.reqs, 1)
			assert.NotEmpty(t, h.reqs[0].Rule)
			assert.Nil(t, h.reqs[0].Review)
			for _, id := range []string{"c3", "c4"} {
				require.ErrorIs(t, call(g, id), ErrDenied)
			}
			assert.Len(t, h.reqs, 1)
			assert.Equal(t, 4, g.totalDenials)
			require.NoError(t, call(g, "c5"))
			assert.Len(t, h.reqs, 2)
		})
	}

	t.Run("human rejection", func(t *testing.T) {
		f, h := &reviewer{reviews: []Review{deny}}, &approver{answer: Reject}
		_, g := newReviewGate(t, f, h.approve)
		for _, id := range []string{"c1", "c2"} {
			require.ErrorIs(t, call(g, id), ErrDenied)
		}
		require.ErrorIs(t, call(g, "c3"), ErrRejected)
		require.ErrorIs(t, call(g, "c4"), ErrRejected)
		assert.Len(t, h.reqs, 2)
	})
}

func TestReviewEscalatesTwentiethTotalDenial(t *testing.T) {
	deny, allow := Review{Verdict: VerdictDeny}, Review{Verdict: VerdictAllow}
	var script []Review
	for range maxTotalDenials - 1 {
		script = append(script, deny, allow)
	}
	script = append(script, deny)
	f, h := &reviewer{reviews: script}, &approver{answer: Reject}
	_, g := newReviewGate(t, f, h.approve)
	for i := range len(script) - 1 {
		err := g.Before(t.Context(), "c"+string(rune('A'+i)), "Bash", nil)
		if script[i] == allow {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrDenied)
		}
	}
	assert.Empty(t, h.reqs)
	require.ErrorIs(t, g.Before(t.Context(), "last", "Bash", nil), ErrRejected)
	assert.Len(t, h.reqs, 1)

	f.reviews = []Review{deny}
	f.reqs = nil
	require.ErrorIs(t, g.Before(t.Context(), "again", "Bash", nil), ErrDenied)
	assert.Len(t, h.reqs, 1)
}

func TestReviewWithoutApprover(t *testing.T) {
	f := &reviewer{reviews: []Review{{Verdict: VerdictDeny, Reason: "no"}}}
	_, g := newReviewGate(t, f, nil)
	for i := range maxConsecutiveDenials + 1 {
		err := g.Before(t.Context(), "c"+string(rune('1'+i)), "Bash", nil)
		require.ErrorIs(t, err, ErrDenied)
		assert.Contains(t, err.Error(), "denied by the approval classifier: no")
	}

	f.reviews = []Review{{Verdict: VerdictAsk}}
	err := g.Before(t.Context(), "u1", "Bash", nil)
	require.ErrorIs(t, err, ErrDenied)
	assert.Contains(t, err.Error(), `tool "Bash" needs approval, the approval classifier could not decide and this run cannot ask for it; add an allow rule`)
	assert.Contains(t, err.Error(), "such as Bash:")
}

func TestReviewCancelledNeverAsks(t *testing.T) {
	h := &approver{answer: Allow}
	started := make(chan struct{})
	f := func(ctx context.Context, _ Request) (Review, error) {
		close(started)
		<-ctx.Done()
		return Review{Verdict: VerdictAllow}, ctx.Err()
	}
	d := newDirs(t)
	rules, err := NewRules(d.ws, d.home, reviewSpecs)
	require.NoError(t, err)
	a := NewAuthority()
	a.EnterExecute()
	g := NewGate(a, reviewSpecs, rules, h.approve, f)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- g.Before(ctx, "c1", "Bash", nil) }()
	<-started
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
	assert.Empty(t, h.reqs)
}

func TestReviewInPlanMode(t *testing.T) {
	f, h := &reviewer{reviews: []Review{{Verdict: VerdictAllow}}}, &approver{answer: Allow}
	a, g := newReviewGate(t, f, h.approve)
	a.EnterPlan()
	require.ErrorIs(t, g.Before(t.Context(), "c1", "Edit", pathArgs("/ws/a.go")), ErrDenied)
	require.ErrorIs(t, g.Before(t.Context(), "c1", "Bash", nil), ErrDenied)
	assert.Empty(t, f.reqs)
	assert.Empty(t, h.reqs)

	require.NoError(t, g.Before(t.Context(), "c2", "Fetch", nil))
	assert.Len(t, f.reqs, 1)
	assert.Empty(t, h.reqs)
}

func TestReviewNeverSeesBuiltInMCPConfigAsk(t *testing.T) {
	f, h := &reviewer{reviews: []Review{{Verdict: VerdictDeny}}}, &approver{answer: Allow}
	d := newDirs(t)
	rules, err := NewRules(d.ws, d.home, reviewSpecs)
	require.NoError(t, err)
	a := NewAuthority()
	a.EnterExecute()
	g := NewGate(a, reviewSpecs, rules, h.approve, f.review)
	require.NoError(t, g.Before(t.Context(), "c1", "Edit", pathArgs(filepath.Join(d.ws, ".mcp.json"))))
	assert.Empty(t, f.reqs)
	require.Len(t, h.reqs, 1)
	assert.NotEmpty(t, h.reqs[0].Rule)
	assert.Nil(t, h.reqs[0].Review)
}

func TestModeChangeDuringReviewRevokes(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := func(context.Context, Request) (Review, error) {
		close(started)
		<-release
		return Review{Verdict: VerdictAllow}, nil
	}
	d := newDirs(t)
	rules, err := NewRules(d.ws, d.home, reviewSpecs)
	require.NoError(t, err)
	a := NewAuthority()
	a.EnterExecute()
	g := NewGate(a, reviewSpecs, rules, nil, f)

	done := make(chan error, 1)
	go func() { done <- g.Before(t.Context(), "c1", "Bash", nil) }()
	<-started
	a.EnterPlan()
	a.EnterExecute()
	close(release)
	require.NoError(t, <-done)
	assert.ErrorIs(t, admitApproved(g, "c1", "Bash"), ErrRevoked)
}
