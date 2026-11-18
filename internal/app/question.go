package app

import (
	"context"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

// pendingQuestion is the one AskUserQuestion call waiting for AnswerQuestion.
// The tool is Exclusive, so its calls run one at a time and one slot is enough.
type pendingQuestion struct {
	token   uint64
	work    *Work
	sendCtx context.Context
	// answer has room for the one answer, so AnswerQuestion never blocks.
	answer chan []tool.Answer
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
	p.work, p.sendCtx = c.turn, c.turn.sendCtx
	c.question = p
	emit := c.emit
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
	if c.question == p {
		c.question = nil
	}
	c.mu.Unlock()
	c.queue.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, context.Canceled
}

// AnswerQuestion resolves the pending question for token; nil answers dismisses
// it. It returns false when no question for token is pending. Unlike a
// rejection without a reason, a dismissal leaves the turn running.
func (c *Controller) AnswerQuestion(token uint64, answers []tool.Answer) bool {
	c.queue.Lock()
	c.mu.Lock()
	p := c.question
	if p == nil || p.token != token {
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	if p.work.stopped || p.sendCtx.Err() != nil {
		c.question = nil
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	c.question = nil
	c.mu.Unlock()
	c.queue.Unlock()
	p.answer <- answers
	return true
}
