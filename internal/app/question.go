package app

import (
	"context"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

func (c *Controller) askQuestion(ctx context.Context, qs []tool.Question) ([]tool.Answer, error) {
	return request(c, ctx, &c.question,
		func(token uint64) event.Event { return event.QuestionRequested{Token: token, Questions: qs} },
		func(token uint64) event.Event { return event.QuestionEnded{Token: token} })
}

// AnswerQuestion resolves the pending question for token; nil answers dismisses
// it. It returns false when no question for token is pending. A dismissal also
// interrupts the turn, which hands queued input back; the model still sees the
// dismissal as the call's result.
func (c *Controller) AnswerQuestion(token uint64, answers []tool.Answer) bool {
	return resolve(c, &c.question, token, answers, answers == nil, nil)
}
