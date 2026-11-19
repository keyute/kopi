package app

import (
	"context"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// pendingQuestion is the one AskUserQuestion call waiting for AnswerQuestion.
// The tool is Exclusive, so its calls run one at a time and one slot is enough.
type pendingQuestion struct {
	token   uint64
	work    *Work
	sendCtx context.Context
	ctx     context.Context
	emit    func(event.Event)
	// answer has room for the one answer, so AnswerQuestion never blocks.
	answer  chan []tool.Answer
	claimed bool // guarded by Controller.mu; cancellation must wait for a claimed answer
}

func (c *Controller) askQuestion(ctx context.Context, qs []tool.Question) ([]tool.Answer, error) {
	p := &pendingQuestion{answer: make(chan []tool.Answer, 1)}
	c.queue.Lock()
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return nil, err
	}
	if c.turn == nil || c.turn.stopped || c.turn.sendCtx.Err() != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return nil, context.Canceled
	}
	c.questionSeq++
	p.token = c.questionSeq
	p.work, p.ctx, p.sendCtx, p.emit = c.turn, ctx, c.turn.sendCtx, c.emit
	c.question = p
	emit := p.emit
	c.mu.Unlock()
	emit(event.QuestionRequested{Token: p.token, Questions: qs})
	c.queue.Unlock()
	defer emit(event.QuestionEnded{Token: p.token})
	select {
	case a := <-p.answer:
		return a, nil
	case <-ctx.Done():
	case <-p.sendCtx.Done():
	}
	c.queue.Lock()
	c.mu.Lock()
	claimed := p.claimed
	if c.question == p {
		c.question = nil
	}
	c.mu.Unlock()
	c.queue.Unlock()
	if claimed {
		// AnswerQuestion claimed the slot first and its answer is on the way; a
		// dismissal interrupts before it is sent, and must still reach the model
		// as a dismissal rather than as a cancel.
		return <-p.answer, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, context.Canceled
}

// AnswerQuestion resolves the pending question for token; nil answers dismisses
// it. It returns false when no question for token is pending. A dismissal also
// interrupts the turn, which hands queued input back; the model still sees the
// dismissal as the call's result.
func (c *Controller) AnswerQuestion(token uint64, answers []tool.Answer) bool {
	c.queue.Lock()
	c.mu.Lock()
	p := c.question
	if p == nil || p.token != token {
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	if p.work.stopped || p.sendCtx.Err() != nil || p.ctx.Err() != nil {
		c.question = nil
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	p.claimed = true
	c.question = nil
	c.mu.Unlock()
	dismissed := answers == nil
	if dismissed {
		c.stop(p.work, p.emit)
	}
	c.queue.Unlock()
	if dismissed {
		// Cancellation forwarding is asynchronous. Stop PiG synchronously
		// before releasing the dismissal, without holding controller locks.
		pigruntime.InterruptTurn(p.ctx)
	}
	p.answer <- answers
	return true
}
