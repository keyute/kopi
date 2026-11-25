package app

import (
	"context"
	"log/slog"
	"strings"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

func (c *Controller) reviewPlan(ctx context.Context, plan string) (tool.PlanAnswer, policy.Mode, error) {
	a, err := request(c, ctx, &c.plan,
		func(token uint64) event.Event {
			return event.PlanRequested{Token: token, Path: c.cfg.Plans.Path(), Plan: plan}
		},
		func(token uint64) event.Event { return event.PlanEnded{Token: token} })
	if err != nil {
		return tool.PlanAnswer{}, policy.Plan, err
	}
	return a, c.mode(), nil
}

func (c *Controller) mode() policy.Mode {
	mode, _ := c.authority.Snapshot()
	return mode
}

// AnswerPlan resolves the pending plan for token and reports whether one was
// pending. Approval switches the session to execute mode and lets the turn
// continue; feedback returns it to the model with the mode unchanged; neither
// dismisses the plan, which also interrupts the turn, handing queued input back.
// The model sees the dismissal as the call's result. This is the only way a
// plan call changes the mode.
func (c *Controller) AnswerPlan(token uint64, a tool.PlanAnswer) bool {
	a.Feedback = strings.TrimSpace(a.Feedback)
	dismissed := !a.Approved && a.Feedback == ""
	return resolve(c, &c.plan, token, a, dismissed, func() {
		if a.Approved {
			// The call's result tells the model the mode, so no mode line follows.
			c.main.told = modeLine(policy.Execute, "", false)
			c.EnterExecute()
		}
	})
}

// RefreshPlan reads the plan file again while the plan for token is pending
// and emits PlanRequested for it with the new text, for after the human edited
// the file. It reports whether it did.
func (c *Controller) RefreshPlan(token uint64) bool {
	plan, err := c.cfg.Plans.Read()
	if err != nil {
		slog.Warn("failed to read the plan file", "err", err)
		return false
	}
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	p := c.plan
	live := p != nil && p.token == token && p.live()
	c.mu.Unlock()
	if !live {
		return false
	}
	p.emit(event.PlanRequested{Token: token, Path: c.cfg.Plans.Path(), Plan: plan})
	return true
}

// PlanFile is the current session's plan file; "" when it has none.
func (c *Controller) PlanFile() string {
	return c.cfg.Plans.Path()
}

// modeLine is what the model is told about the session's mode. Plan mode with
// a plan file says how the mode is left: with approval, through ExitPlanMode.
func modeLine(mode policy.Mode, planPath string, approval bool) string {
	switch {
	case mode != policy.Plan || planPath == "":
		return "The session is in " + mode.String() + " mode."
	case !approval:
		return "The session is in plan mode. Its plan file is " + planPath + "; Read, Write and Edit work on that file in both modes."
	}
	return "Plan mode is on: do not change the workspace. Research the request; if you end with changes to propose, asked for or not, write them as a plan to " + planPath +
		" (Read, Write and Edit work on that file in both modes) and call ExitPlanMode for the user's approval instead of ending your turn. " +
		"With no changes to propose, just answer."
}

// recall is what the main session's model is told again after a compaction,
// which may have summarised the mode line away: that line, and in execute mode
// the plan file when it has content. It does not count as told.
func (c *Controller) recall() string {
	mode, path := c.mode(), c.cfg.Plans.Path()
	line := modeLine(mode, path, c.planApproval())
	if mode != policy.Execute || path == "" {
		return line
	}
	if plan, err := c.cfg.Plans.Read(); err != nil || strings.TrimSpace(plan) == "" {
		return line
	}
	return line + " Its plan file is " + path + "."
}

// planApproval reports whether the session offers ExitPlanMode.
func (c *Controller) planApproval() bool {
	return c.cfg.Plans != nil && !c.cfg.Unattended && !c.cfg.Rules.Removes("ExitPlanMode")
}

// tell attaches the current mode line to the last of ps unless the model
// already read it, and records it as told. ps carry no line of their own: an
// earlier one that was not delivered has been taken back by untell. Only the
// main session is told its mode. It must be called with queue held.
//
// Every prompt about to be sent to the main session's model passes through tell,
// so it is also where the session's first human prompt is noticed for its title.
func (r *runner) tell(ps []pigruntime.Prompt) []pigruntime.Prompt {
	if len(ps) == 0 || r != r.c.main {
		return ps
	}
	r.c.nameSessionLocked(ps)
	ps = append([]pigruntime.Prompt(nil), ps...)
	for i := range ps {
		ps[i].Mode = ""
	}
	if line := modeLine(r.c.mode(), r.c.cfg.Plans.Path(), r.c.planApproval()); line != r.told {
		ps[len(ps)-1].Mode = line
		r.told = line
	}
	return ps
}

// untell forgets what was told when a prompt that carried the line was not
// delivered. It must be called with queue held.
func (r *runner) untell(ps ...[]pigruntime.Prompt) {
	for _, group := range ps {
		for _, p := range group {
			if p.Mode != "" {
				r.told = ""
			}
		}
	}
}
