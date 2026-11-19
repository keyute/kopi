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

// pendingPlan is the one ExitPlanMode call waiting for AnswerPlan. The tool is
// Exclusive, so its calls run one at a time and one slot is enough.
type pendingPlan struct {
	token   uint64
	work    *Work
	sendCtx context.Context
	ctx     context.Context
	emit    func(event.Event)
	// answer has room for the one answer, so AnswerPlan never blocks.
	answer  chan tool.PlanAnswer
	claimed bool // guarded by Controller.mu; cancellation must wait for a claimed answer
}

func (c *Controller) reviewPlan(ctx context.Context, plan string) (tool.PlanAnswer, policy.Mode, error) {
	p := &pendingPlan{answer: make(chan tool.PlanAnswer, 1)}
	c.queue.Lock()
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return tool.PlanAnswer{}, policy.Plan, err
	}
	if c.turn == nil || c.turn.stopped || c.turn.sendCtx.Err() != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return tool.PlanAnswer{}, policy.Plan, context.Canceled
	}
	c.planSeq++
	p.token = c.planSeq
	p.work, p.ctx, p.sendCtx, p.emit = c.turn, ctx, c.turn.sendCtx, c.emit
	c.plan = p
	emit := p.emit
	c.mu.Unlock()
	emit(event.PlanRequested{Token: p.token, Path: c.cfg.Plans.Path(), Plan: plan})
	c.queue.Unlock()
	defer emit(event.PlanEnded{Token: p.token})
	select {
	case a := <-p.answer:
		return a, c.mode(), nil
	case <-ctx.Done():
	case <-p.sendCtx.Done():
	}
	c.queue.Lock()
	c.mu.Lock()
	claimed := p.claimed
	if c.plan == p {
		c.plan = nil
	}
	c.mu.Unlock()
	c.queue.Unlock()
	if claimed {
		// AnswerPlan claimed the slot first and its answer is on the way; a
		// dismissal interrupts before it is sent, and must still reach the model
		// as a dismissal rather than as a cancel.
		return <-p.answer, c.mode(), nil
	}
	if err := ctx.Err(); err != nil {
		return tool.PlanAnswer{}, policy.Plan, err
	}
	return tool.PlanAnswer{}, policy.Plan, context.Canceled
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
	c.queue.Lock()
	c.mu.Lock()
	p := c.plan
	if p == nil || p.token != token {
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	if p.work.stopped || p.sendCtx.Err() != nil || p.ctx.Err() != nil {
		c.plan = nil
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	p.claimed = true
	c.plan = nil
	c.mu.Unlock()
	dismissed := !a.Approved && a.Feedback == ""
	if dismissed {
		c.stop(p.work, p.emit)
	}
	if a.Approved {
		// The call's result tells the model the mode, so no mode line follows.
		c.told = modeLine(policy.Execute, "")
	}
	c.queue.Unlock()
	if a.Approved {
		c.EnterExecute()
	}
	if dismissed {
		// Cancellation forwarding is asynchronous. Stop PiG synchronously
		// before releasing the dismissal, without holding controller locks.
		pigruntime.InterruptTurn(p.ctx)
	}
	p.answer <- a
	return true
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
	live := p != nil && p.token == token && !p.work.stopped && p.sendCtx.Err() == nil && p.ctx.Err() == nil
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

// modeLine is what the model is told about the session's mode.
func modeLine(mode policy.Mode, planPath string) string {
	if mode == policy.Plan && planPath != "" {
		return "The session is in plan mode. Its plan file is " + planPath + "; Read, Write and Edit work on that file in both modes."
	}
	return "The session is in " + mode.String() + " mode."
}

// tell attaches the current mode line to the last of ps unless the model
// already read it, and records it as told. ps carry no line of their own: an
// earlier one that was not delivered has been taken back by untell. It must be
// called with queue held.
func (c *Controller) tell(ps []pigruntime.Prompt) []pigruntime.Prompt {
	if len(ps) == 0 {
		return ps
	}
	ps = append([]pigruntime.Prompt(nil), ps...)
	for i := range ps {
		ps[i].Mode = ""
	}
	if line := modeLine(c.mode(), c.cfg.Plans.Path()); line != c.told {
		ps[len(ps)-1].Mode = line
		c.told = line
	}
	return ps
}

// untell forgets what was told when a prompt that carried the line was not
// delivered. It must be called with queue held.
func (c *Controller) untell(ps ...[]pigruntime.Prompt) {
	for _, group := range ps {
		for _, p := range group {
			if p.Mode != "" {
				c.told = ""
			}
		}
	}
}
