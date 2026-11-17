package policy

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAuthorityStartsInPlanAtGenerationOne(t *testing.T) {
	mode, gen := NewAuthority().Snapshot()
	assert.Equal(t, Plan, mode)
	assert.Equal(t, uint64(1), gen)
}

func TestModeChangesBumpGenerationOnce(t *testing.T) {
	a := NewAuthority()
	assert.Equal(t, uint64(2), a.EnterExecute())
	assert.Equal(t, uint64(3), a.EnterPlan())
	mode, gen := a.Snapshot()
	assert.Equal(t, Plan, mode)
	assert.Equal(t, uint64(3), gen)
}

func TestReenteringCurrentModeDoesNotBump(t *testing.T) {
	a := NewAuthority()
	assert.Equal(t, uint64(1), a.EnterPlan())
	assert.Equal(t, uint64(2), a.EnterExecute())
	assert.Equal(t, uint64(2), a.EnterExecute())
}

func TestPermitsByModeAndCapability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		execute bool
		spec    ToolSpec
		want    bool
	}{
		{"plan read", false, ToolSpec{Capability: Read}, true},
		{"plan write", false, ToolSpec{Capability: Write}, false},
		{"plan exec", false, ToolSpec{Capability: Exec}, false},
		{"plan sandboxed exec", false, ToolSpec{Capability: Exec, Sandboxed: true}, false},
		{"plan plan-sandboxed exec", false, ToolSpec{Capability: Exec, Sandboxed: true, PlanSandboxed: true}, true},
		{"plan unknown", false, ToolSpec{Capability: Capability("network")}, false},
		{"execute read", true, ToolSpec{Capability: Read}, true},
		{"execute write", true, ToolSpec{Capability: Write}, true},
		{"execute exec", true, ToolSpec{Capability: Exec}, true},
		{"execute unknown", true, ToolSpec{Capability: Capability("network")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAuthority()
			if tc.execute {
				a.EnterExecute()
			}
			wantMode, wantGen := a.Snapshot()
			mode, gen, ok := a.Permits(tc.spec)
			assert.Equal(t, tc.want, ok)
			assert.Equal(t, wantMode, mode)
			assert.Equal(t, wantGen, gen)
		})
	}
}

func TestConcurrentTransitionsAndPermitsAreRaceFree(t *testing.T) {
	a := NewAuthority()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				a.EnterExecute()
				a.EnterPlan()
			}
		})
		wg.Go(func() {
			for range 100 {
				a.Permits(ToolSpec{Capability: Exec})
				a.Snapshot()
			}
		})
	}
	wg.Wait()
	mode, gen := a.Snapshot()
	assert.Equal(t, Plan, mode)
	assert.Equal(t, uint64(1), gen%2, "every completed execute/plan pair bumps twice")
}
