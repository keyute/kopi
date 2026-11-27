package app

import (
	"context"
	"fmt"

	"github.com/keyute/kopi/internal/event"
)

// sideQuestionPrefix wraps a side question: the tools declared as of the
// session's last request stay in this one, so the model is told a call does
// nothing.
const sideQuestionPrefix = "Side question, answered outside the conversation. Reply once in text; no tool runs here."

// btwCommand asks the main session's model a side question: the question and
// the answer never enter the session, and a running turn is left alone. It
// returns once the question is announced; the answer arrives as
// SideQuestionEnded. An earlier side question still running is cancelled.
func (c *Controller) btwCommand(_ context.Context, question string) error {
	if question == "" {
		return fmt.Errorf("%w: /btw needs a question", ErrMissingArgument)
	}
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return ErrNoSession
	case c.busyLocked():
		c.mu.Unlock()
		return ErrBusy
	case c.main.sess == nil:
		c.mu.Unlock()
		return ErrNoSession
	}
	sess := c.main.sess
	c.stopSideLocked()
	c.sideSeq++
	id := c.sideSeq
	ctx, cancel := context.WithCancel(context.Background())
	c.sideStop = cancel
	// Added under mu, which Close sets closed under, so it never races Wait.
	c.wg.Add(1)
	c.mu.Unlock()

	// Before the request starts, so Ended never precedes it.
	c.relay.emit(event.SideQuestionAsked{ID: id, Question: question})
	go func() {
		defer c.wg.Done()
		defer cancel()
		text, used, err := c.rt.AnswerFrom(ctx, sess, sideQuestionPrefix+"\n\n"+question)
		c.charge(sess, used)
		// Not through the session's epoch: a request cancelled by a switch or
		// by Close must still end, and the epoch drops what a switch outdates.
		c.relay.emit(event.SideQuestionEnded{ID: id, Answer: text, Err: err})
	}()
	return nil
}

// StopSideQuestion cancels the running side question if it is id; its
// SideQuestionEnded still follows. It does nothing for another id or once the
// question has ended.
func (c *Controller) StopSideQuestion(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sideSeq == id {
		c.stopSideLocked()
	}
}

// stopSideLocked cancels the running side question, if any; mu must be held.
func (c *Controller) stopSideLocked() {
	if c.sideStop != nil {
		c.sideStop()
		c.sideStop = nil
	}
}
