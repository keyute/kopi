package pigruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// heldProbe closes started when called and returns once release is closed.
func heldProbe(started chan<- struct{}, release <-chan struct{}) tool.Tool {
	return probeTool(func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		select {
		case <-release:
			return toolOutput, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
}

func sendAsync(t *testing.T, sess *Session, text string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := sess.Send(t.Context(), Prompt{Text: text}, nil, nil)
		done <- err
	}()
	return done
}

// recordDelivered collects the UserMessage and TaskEnded events; all of a
// turn's have been sent once its Send returned.
func recordDelivered(t *testing.T, sess *Session) <-chan event.Event {
	t.Helper()
	evs := make(chan event.Event, 32)
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		switch ev.(type) {
		case event.UserMessage, event.TaskEnded:
			evs <- ev
		}
	}))
	return evs
}

func drain(evs <-chan event.Event) []event.Event {
	var out []event.Event
	for len(evs) > 0 {
		out = append(out, <-evs)
	}
	return out
}

func userMessages(msgs []ai.Message) []ai.UserMessage {
	var out []ai.UserMessage
	for _, m := range msgs {
		switch m := m.(type) {
		case ai.UserMessage:
			out = append(out, m)
		case *ai.UserMessage:
			out = append(out, *m)
		}
	}
	return out
}

func userTexts(msgs []ai.Message) []string {
	var out []string
	for _, m := range userMessages(msgs) {
		out = append(out, userText(m.Content))
	}
	return out
}

func userImages(msgs []ai.Message) []ai.ImageContent {
	var out []ai.ImageContent
	for _, m := range userMessages(msgs) {
		blocks, _ := m.Content.(ai.UserContentBlocks)
		for _, b := range blocks {
			if img, ok := b.(ai.ImageContent); ok {
				out = append(out, img)
			}
		}
	}
	return out
}

// onFirstTurnEnd runs fn from the listener for the first turn_end, which PiG
// emits just before it polls for steering.
func onFirstTurnEnd(t *testing.T, sess *Session, fn func()) {
	t.Helper()
	done := false
	t.Cleanup(sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.TurnEndEvent); ok && !done {
			done = true
			fn()
		}
	}))
}

// recordUserStarts returns the texts of the user message_start events seen so
// far; all of a run's have been seen once its SendContent returned.
func recordUserStarts(t *testing.T, sess *Session) func() []string {
	t.Helper()
	var texts []string
	t.Cleanup(sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if start, ok := ev.(agent.MessageStartEvent); ok && start.Message.User != nil {
			texts = append(texts, userText(start.Message.User.Content))
		}
	}))
	return func() []string { return texts }
}

func sendInner(t *testing.T, sess *Session, text string) {
	t.Helper()
	_, err := sess.inner.SendContent(t.Context(), coding.BuildUserContent(text, nil))
	require.NoError(t, err)
}

func lastRequest(requests <-chan []ai.Message) []ai.Message {
	var last []ai.Message
	for len(requests) > 0 {
		last = <-requests
	}
	return last
}

func TestPiGSteerFromTurnEndAfterToolTurn(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		return toolOutput, nil
	}))
	starts := recordUserStarts(t, sess)
	onFirstTurnEnd(t, sess, func() {
		sess.inner.QueueSteer("x", nil)
		sess.inner.QueueSteer("y", nil)
	})

	sendInner(t, sess, "prompt")

	assert.Equal(t, []string{"prompt", "x", "y"}, userTexts(<-requests))
	assert.Equal(t, []string{"prompt", "x", "y"}, starts())
	assert.Zero(t, sess.inner.PendingMessageCount())
}

func TestPiGSteerFromTurnEndAfterTextReply(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), captureRequest(requests, "done")})
	sess := openTestSession(t, r)
	onFirstTurnEnd(t, sess, func() { sess.inner.QueueSteer("x", nil) })

	sendInner(t, sess, "prompt")

	assert.Equal(t, 2, faux.CallCount())
	assert.Equal(t, []string{"prompt", "x"}, userTexts(<-requests))
}

func TestPiGSteerFromTurnEndThenAbort(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 2)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "aborted"), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		return toolOutput, nil
	}))
	starts := recordUserStarts(t, sess)
	onFirstTurnEnd(t, sess, func() {
		sess.inner.QueueSteer("x", nil)
		sess.inner.RequestAbort()
	})

	sendInner(t, sess, "prompt")
	assert.Equal(t, 1, countOf(starts(), "x"), "user message starts: %v", starts())
	steering, _ := sess.inner.ClearQueue()
	assert.Empty(t, steering)

	sendInner(t, sess, "next")
	texts := userTexts(lastRequest(requests))
	assert.Contains(t, texts, "next")
	assert.Equal(t, 1, countOf(texts, "x"), "next request user texts: %v", texts)
}

func TestPiGSteerFromTurnEndAfterAbortAtStart(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
	sess := openTestSession(t, r)
	starts := recordUserStarts(t, sess)
	started := false
	t.Cleanup(sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.AgentStartEvent); ok && !started {
			started = true
			sess.inner.RequestAbort()
		}
	}))
	onFirstTurnEnd(t, sess, func() { sess.inner.QueueSteer("x", nil) })

	sendInner(t, sess, "prompt")
	assert.Zero(t, faux.CallCount())
	assert.NotContains(t, starts(), "x")
	steering, _ := sess.inner.ClearQueue()
	assert.Equal(t, []string{"x"}, steering)

	sendInner(t, sess, "next")
	texts := userTexts(<-requests)
	assert.Contains(t, texts, "next")
	assert.NotContains(t, texts, "x")
}

func TestPiGSteerFromTurnEndAfterProviderError(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	providerError := ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "provider exploded"})
	faux.SetResponses([]ai.FauxResponseStep{providerError, captureRequest(requests, "done")})
	sess := openTestSession(t, r)
	onFirstTurnEnd(t, sess, func() { sess.inner.QueueSteer("x", nil) })

	sendInner(t, sess, "prompt")

	require.Equal(t, 2, faux.CallCount())
	assert.Contains(t, userTexts(<-requests), "x")
}

// moreOnce hands over prompts at its first call and nothing after; calls
// counts its calls.
func moreOnce(prompts ...Prompt) (more func() ([]Prompt, []Notice), calls *int) {
	return handOnce(prompts, nil)
}

// handOnce is moreOnce for prompts and notices.
func handOnce(prompts []Prompt, notices []Notice) (more func() ([]Prompt, []Notice), calls *int) {
	n := 0
	return func() ([]Prompt, []Notice) {
		n++
		if n > 1 {
			return nil, nil
		}
		return prompts, notices
	}, &n
}

func okProbe(context.Context, json.RawMessage) (string, error) { return toolOutput, nil }

func TestSendHandsOverAtToolTurnEnd(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	evs := recordDelivered(t, sess)
	more, _ := moreOnce(Prompt{Text: "x"}, Prompt{Text: "y"})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
	assert.True(t, res.Sent)
	assert.Empty(t, res.Undelivered)
	assert.Equal(t, 2, faux.CallCount())
	assert.Equal(t, []string{"prompt", "x", "y"}, userTexts(<-requests))
	assert.Equal(t, []event.Event{event.UserMessage{Text: "x"}, event.UserMessage{Text: "y"}}, drain(evs))
}

func TestSendHandsOverAfterTextReply(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), captureRequest(requests, "done")})
	sess := openTestSession(t, r)
	more, _ := moreOnce(Prompt{Text: "x"})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.True(t, res.Sent)
	assert.Empty(t, res.Undelivered)
	assert.Equal(t, 2, faux.CallCount())
	assert.Equal(t, []string{"prompt", "x"}, userTexts(<-requests))
}

func TestSendSkipsMore(t *testing.T) {
	providerError := ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "provider exploded"})
	t.Run("after a failed turn", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		faux.SetResponses([]ai.FauxResponseStep{providerError, reply("should not be requested")})
		sess := openTestSession(t, r)
		more, calls := moreOnce(Prompt{Text: "x"})

		res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
		require.NoError(t, err)
		assert.Equal(t, event.Failed, res.Outcome.Status)
		assert.Zero(t, *calls)
		assert.Equal(t, 1, faux.CallCount())
	})
	t.Run("after an aborted turn", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		aborted := ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonAborted)})
		faux.SetResponses([]ai.FauxResponseStep{aborted, reply("should not be requested")})
		sess := openTestSession(t, r)
		more, calls := moreOnce(Prompt{Text: "x"})

		res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
		require.NoError(t, err)
		assert.Equal(t, event.Interrupted, res.Outcome.Status)
		assert.Zero(t, *calls)
		assert.Equal(t, 1, faux.CallCount())
	})
	t.Run("once ctx is cancelled", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("done")})
		turnCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		sess := openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
			cancel()
			return toolOutput, nil
		}))
		more, calls := moreOnce(Prompt{Text: "x"})

		_, err := sess.Send(turnCtx, Prompt{Text: "prompt"}, nil, more)
		require.NoError(t, err)
		assert.Zero(t, *calls)
	})
}

func TestSendHandOverOmitsUnprocessableImage(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	good := Image{MimeType: "image/png", Data: pngBytes(t)}
	more, _ := moreOnce(Prompt{Text: "x [image #1] [image #2]", Images: []Image{corruptPNG(), good}})

	_, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)

	msgs := <-requests
	assert.Equal(t, []string{"prompt", "x [image #1] [image #2]\n\n" + tool.OmittedImageNote}, userTexts(msgs))
	images := userImages(msgs)
	require.Len(t, images, 1)
	assert.Equal(t, "image/png", images[0].MimeType)
}

func TestSendHandOverCarriesImages(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	sent := Image{MimeType: "image/png", Data: pngBytes(t)}
	handed := Image{MimeType: "image/jpeg", Data: jpegBytes(t)}
	more, _ := moreOnce(Prompt{Text: "and [image #2]", Images: []Image{handed}})

	_, err := sess.Send(t.Context(), Prompt{Text: "look [image #1]", Images: []Image{sent}}, nil, more)
	require.NoError(t, err)

	assert.Equal(t, []ai.ImageContent{
		{Data: base64.StdEncoding.EncodeToString(sent.Data), MimeType: "image/png"},
		{Data: base64.StdEncoding.EncodeToString(handed.Data), MimeType: "image/jpeg"},
	}, userImages(<-requests))
}

func TestSendHandOverResizesLargeImage(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	more, _ := moreOnce(Prompt{Text: "x [image #1]", Images: []Image{{MimeType: "image/png", Data: widePNG(t)}}})

	_, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)

	images := userImages(<-requests)
	require.Len(t, images, 1)
	data, err := base64.StdEncoding.DecodeString(images[0].Data)
	require.NoError(t, err)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Less(t, cfg.Width, 6000)
	assert.Less(t, cfg.Height, 40)
}

func TestSendRejectedPromptIsNotSent(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))

	done := sendAsync(t, sess, "first")
	<-started
	res, err := sess.Send(t.Context(), Prompt{Text: "second"}, nil, nil)
	close(release)
	require.NoError(t, <-done)

	require.Error(t, err)
	assert.False(t, res.Sent)
	assert.Empty(t, res.Undelivered)
}

func TestSendRejectedCallPreservesOwner(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 2)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done"), captureRequest(requests, "next done")})
	handed, release := make(chan struct{}), make(chan struct{})
	var sess *Session
	sess = openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		// Installed after Send's listener, so the queue is populated but
		// PiG cannot drain it until this listener returns.
		onFirstTurnEnd(t, sess, func() {
			close(handed)
			<-release
		})
		return toolOutput, nil
	}))
	evs := recordDelivered(t, sess)
	more, _ := moreOnce(Prompt{Text: "steering"})
	type result struct {
		res SendResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := sess.Send(t.Context(), Prompt{Text: "owner"}, nil, more)
		done <- result{res: res, err: err}
	}()
	<-handed

	ctx, cancel := context.WithCancel(t.Context())
	rejected := []Notice{testNotice("t1")}
	res, err := sess.Send(ctx, Prompt{Text: "rejected"}, rejected, nil)
	cancel()
	assert.ErrorIs(t, err, ErrSendInProgress)
	assert.False(t, res.Sent)
	assert.Empty(t, res.Undelivered)
	assert.Equal(t, rejected, res.UndeliveredNotices)

	res, err = sess.Send(ctx, Prompt{Text: "already cancelled"}, nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, event.Interrupted, res.Outcome.Status)
	assert.False(t, res.Sent)
	assert.Equal(t, 1, sess.inner.PendingMessageCount())
	close(release)

	owner := <-done
	require.NoError(t, owner.err)
	assert.Equal(t, event.Completed, owner.res.Outcome.Status)
	assert.True(t, owner.res.Sent)
	assert.Empty(t, owner.res.Undelivered)
	require.Equal(t, 2, faux.CallCount())
	assert.Equal(t, []string{"owner", "steering"}, userTexts(<-requests))
	assert.Equal(t, []event.Event{event.UserMessage{Text: "steering"}}, drain(evs))

	res, err = sess.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
	require.NoError(t, err)
	assert.True(t, res.Sent)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.Equal(t, []string{"owner", "steering", "next"}, userTexts(<-requests))
}

func TestSendReturnsUndeliveredHandOver(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	var sess *Session
	// Subscribed while the run is going, so after Send's listener: it empties
	// PiG's queue after the handover and before the steering poll.
	sess = openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		onFirstTurnEnd(t, sess, func() { _, _ = sess.inner.ClearQueue() })
		return toolOutput, nil
	}))
	// Handed back as given: neither resized nor omitted.
	wide := Image{Path: "/shots/wide.png", MimeType: "image/png", Data: widePNG(t)}
	broken := corruptPNG()
	broken.Path = "/shots/broken.png"
	handed := Prompt{Text: "x [image #1] [image #2]", Images: []Image{wide, broken}}
	more, _ := moreOnce(handed)

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.True(t, res.Sent)
	assert.Equal(t, []Prompt{handed}, res.Undelivered)
	assert.Equal(t, []string{"prompt"}, userTexts(<-requests))
}

func testNotice(id string) Notice {
	return Notice{
		Text:  "Background task " + id + " completed: make " + id,
		Shown: event.TaskEnded{ID: id, Command: "make " + id, Status: "completed", Elapsed: 1500 * time.Millisecond},
	}
}

// toolResultBefore reports whether msgs has a tool result before the user
// message whose text is text.
func toolResultBefore(msgs []ai.Message, text string) bool {
	seenResult := false
	for _, m := range msgs {
		switch m := m.(type) {
		case ai.ToolResultMessage, *ai.ToolResultMessage:
			seenResult = true
		case ai.UserMessage:
			if userText(m.Content) == text {
				return seenResult
			}
		case *ai.UserMessage:
			if userText(m.Content) == text {
				return seenResult
			}
		}
	}
	return false
}

func TestSendNoticeOnlyStartsRun(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "noted")})
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	n := testNotice("t1")

	res, err := sess.Send(t.Context(), Prompt{}, []Notice{n}, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
	assert.Equal(t, "noted", res.Reply)
	assert.True(t, res.Sent)
	assert.Empty(t, res.UndeliveredNotices)
	texts := userTexts(<-requests)
	require.NotEmpty(t, texts)
	assert.Equal(t, n.Text, texts[len(texts)-1])
	assert.Equal(t, []event.Event{n.Shown}, drain(evs))
}

func TestSendWakeWithTwoNotices(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "noted")})
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	n1, n2 := testNotice("t1"), testNotice("t2")

	res, err := sess.Send(t.Context(), Prompt{}, []Notice{n1, n2}, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.True(t, res.Sent)
	assert.Empty(t, res.UndeliveredNotices)
	assert.Equal(t, []string{n1.Text, n2.Text}, userTexts(<-requests))
	assert.Equal(t, []event.Event{n1.Shown, n2.Shown}, drain(evs))
}

func TestSendPromptThenNotices(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	n1, n2 := testNotice("t1"), testNotice("t2")

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, []Notice{n1, n2}, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.True(t, res.Sent)
	assert.Empty(t, res.UndeliveredNotices)
	assert.Equal(t, []string{"prompt", n1.Text, n2.Text}, userTexts(<-requests))
	assert.Equal(t, []event.Event{n1.Shown, n2.Shown}, drain(evs))
}

func TestSendHandsOverNoticeAfterToolBatch(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	evs := recordDelivered(t, sess)
	n := testNotice("t1")
	more, _ := handOnce(nil, []Notice{n})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.Empty(t, res.UndeliveredNotices)
	assert.Equal(t, 2, faux.CallCount())
	msgs := <-requests
	assert.Equal(t, []string{"prompt", n.Text}, userTexts(msgs))
	assert.True(t, toolResultBefore(msgs, n.Text))
	assert.Equal(t, []event.Event{n.Shown}, drain(evs))
}

func TestSendNoticeAtFinalTurnExtendsRun(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), captureRequest(requests, "second")})
	sess := openTestSession(t, r)
	n := testNotice("t1")
	more, _ := handOnce(nil, []Notice{n})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.Equal(t, "second", res.Reply)
	assert.Equal(t, 2, faux.CallCount())
	assert.Equal(t, []string{"prompt", n.Text}, userTexts(<-requests))
}

func TestSendHandsOverPromptBeforeNotice(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	evs := recordDelivered(t, sess)
	n := testNotice("t1")
	more, _ := handOnce([]Prompt{{Text: "x"}}, []Notice{n})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.Empty(t, res.Undelivered)
	assert.Empty(t, res.UndeliveredNotices)
	assert.Equal(t, []string{"prompt", "x", n.Text}, userTexts(<-requests))
	assert.Equal(t, []event.Event{event.UserMessage{Text: "x"}, n.Shown}, drain(evs))
}

// PiG delivers a handover the same run reaches, so the notice is kept in PiG's
// queue past the run by draining steering one message at a time and cancelling
// at the handover; the next run must not replay it.
func TestSendReturnsUndeliveredNoticeAndClearsIt(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 2)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "aborted"), captureRequest(requests, "done")})
	turnCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var sess *Session
	// Subscribed while the run is going, so after Send's listener.
	sess = openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		onFirstTurnEnd(t, sess, func() {
			sess.inner.Agent().SetSteeringMode(agent.QueueModeOneAtATime)
			cancel()
		})
		return toolOutput, nil
	}))
	n := testNotice("t1")
	more, _ := handOnce([]Prompt{{Text: "x"}}, []Notice{n})

	res, err := sess.Send(turnCtx, Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.Equal(t, event.Interrupted, res.Outcome.Status)
	assert.Empty(t, res.Undelivered)
	assert.Equal(t, []Notice{n}, res.UndeliveredNotices)

	res, err = sess.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	texts := userTexts(lastRequest(requests))
	assert.Contains(t, texts, "next")
	assert.NotContains(t, texts, n.Text)
}

func TestSendWakeIsAbortable(t *testing.T) {
	t.Run("during a tool", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		requests := make(chan []ai.Message, 1)
		faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "after")})
		started := make(chan struct{})
		sess := openTestSession(t, r, heldProbe(started, make(chan struct{})))
		turnCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			<-started
			cancel()
		}()

		res, err := sess.Send(turnCtx, Prompt{}, []Notice{testNotice("t1")}, nil)
		require.NoError(t, err)
		assert.Equal(t, event.Interrupted, res.Outcome.Status)
		assert.True(t, res.Sent)
		assert.Empty(t, res.UndeliveredNotices)

		res, err = sess.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, event.Completed, res.Outcome.Status)
		assert.Equal(t, "after", res.Reply)
		assert.Contains(t, userTexts(<-requests), "next")
	})
	t.Run("at run start", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		faux.SetResponses([]ai.FauxResponseStep{reply("should not be requested"), reply("after")})
		sess := openTestSession(t, r)
		turnCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		unsubscribe := sess.inner.Subscribe(func(ev agent.AgentEvent) {
			if _, ok := ev.(agent.AgentStartEvent); ok {
				cancel()
			}
		})

		res, err := sess.Send(turnCtx, Prompt{}, []Notice{testNotice("t1")}, nil)
		unsubscribe()
		require.NoError(t, err)
		assert.Equal(t, event.Interrupted, res.Outcome.Status)
		assert.Zero(t, faux.CallCount())

		res, err = sess.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, event.Completed, res.Outcome.Status)
		assert.Equal(t, 1, faux.CallCount())
	})
}

func countOf(texts []string, text string) int {
	n := 0
	for _, s := range texts {
		if s == text {
			n++
		}
	}
	return n
}

// corruptPNG has a PNG signature, so it passes a content sniff, but cannot be
// decoded.
func corruptPNG() Image {
	return Image{MimeType: "image/png", Data: append([]byte("\x89PNG\r\n\x1a\n"), "not really a png"...)}
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return b.Bytes()
}

// widePNG is wider than PiG's default longest side of 2000 pixels.
func widePNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 6000, 40))))
	return b.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))
	return b.Bytes()
}

func TestReply(t *testing.T) {
	lookFirst := ai.FauxAssistantMessage(ai.FauxContentBlocks{
		ai.FauxText("Let me look."),
		ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"}),
	}, ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)})
	final := func(blocks ...ai.FauxContentBlock) ai.FauxResponseStep {
		return ai.FauxAssistantMessage(ai.FauxContentBlocks(blocks), ai.FauxAssistantMessageOptions{})
	}
	stopped := func(reason ai.StopReason, text, errorMessage string) ai.FauxResponseStep {
		return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{StopReason: string(reason), ErrorMessage: errorMessage})
	}
	for _, tt := range []struct {
		name   string
		steps  []ai.FauxResponseStep
		status event.Status
		want   string
		calls  int
		// held cancels the turn while the probe runs.
		held bool
	}{
		{name: "after a tool", steps: []ai.FauxResponseStep{lookFirst, reply("The answer.")}, status: event.Completed, want: "The answer.", calls: 2},
		{name: "joined text blocks", steps: []ai.FauxResponseStep{final(ai.FauxText("First."), ai.FauxThinking("Hmm."), ai.FauxText(""), ai.FauxText("Second."))}, status: event.Completed, want: "First.\n\nSecond.", calls: 1},
		{name: "thinking only after a tool", steps: []ai.FauxResponseStep{lookFirst, final(ai.FauxThinking("Considering."))}, status: event.Completed, calls: 2},
		{name: "empty content after a tool", steps: []ai.FauxResponseStep{lookFirst, final()}, status: event.Completed, calls: 2},
		{name: "length truncated", steps: []ai.FauxResponseStep{stopped(ai.StopReasonLength, "partial", "")}, status: event.Completed, want: "partial", calls: 1},
		{name: "after a retried failure", steps: []ai.FauxResponseStep{stopped(ai.StopReasonError, "", "overloaded"), reply("recovered")}, status: event.Completed, want: "recovered", calls: 2},
		{name: "failed", steps: []ai.FauxResponseStep{stopped(ai.StopReasonError, "partial", "provider exploded")}, status: event.Failed, calls: 1},
		{name: "aborted with partial text", steps: []ai.FauxResponseStep{stopped(ai.StopReasonAborted, "partial", "")}, status: event.Interrupted, calls: 1},
		{name: "interrupted", steps: []ai.FauxResponseStep{lookFirst, reply("unreached")}, status: event.Interrupted, calls: 1, held: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sb := newSandbox(t)
			r, faux := newTestRuntime(t, sb)
			var s coding.Settings
			require.NoError(t, json.Unmarshal([]byte(`{"retry":{"baseDelayMs":0}}`), &s))
			r.svcs.SettingsManager().ApplyOverrides(s)
			faux.SetResponses(tt.steps)
			probe := probeTool(okProbe)
			turnCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.held {
				started := make(chan struct{})
				probe = heldProbe(started, make(chan struct{}))
				go func() {
					<-started
					cancel()
				}()
			}
			sess := openTestSession(t, r, probe)

			res, err := sess.Send(turnCtx, Prompt{Text: "prompt"}, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.status, res.Outcome.Status)
			assert.Equal(t, tt.want, res.Reply)
			assert.Equal(t, tt.calls, faux.CallCount())
		})
	}
}

func TestSendQueuesSkillsAfterPromptBeforeNotices(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	n := testNotice("t1")

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt", Skills: []string{"S1", "S2"}}, []Notice{n}, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.Empty(t, res.UndeliveredNotices)
	assert.Equal(t, []string{"prompt", "S1", "S2", n.Text}, userTexts(<-requests))
	assert.Equal(t, []event.Event{n.Shown}, drain(evs))
}

func TestSendHandsOverSkillsWithPrompt(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	evs := recordDelivered(t, sess)
	more, _ := moreOnce(Prompt{Text: "x", Skills: []string{"S"}})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.Empty(t, res.Undelivered)
	assert.Equal(t, []string{"prompt", "x", "S"}, userTexts(<-requests))
	assert.Equal(t, []event.Event{event.UserMessage{Text: "x"}}, drain(evs))
}

func TestRootAndChildSessionsSendConcurrently(t *testing.T) {
	r, rootFaux := newTestRuntime(t, newSandbox(t))
	childFaux := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "kopi-faux-child", Model: "faux-1"})
	require.NoError(t, r.RegisterProvider(childFaux.Provider()))

	// Each turn blocks until the other has started, so the two overlap.
	rootStarted, childStarted := make(chan struct{}), make(chan struct{})
	overlap := func(own, other chan struct{}, text string) ai.FauxResponseStep {
		return ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			close(own)
			<-other
			return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{}), nil
		})
	}
	rootFaux.SetResponses([]ai.FauxResponseStep{overlap(rootStarted, childStarted, "root done")})
	childFaux.SetResponses([]ai.FauxResponseStep{overlap(childStarted, rootStarted, "child done")})

	root := openTestSession(t, r)
	child, err := r.Open(t.Context(), OpenOptions{Model: "kopi-faux-child/faux-1", Parent: root, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, child.Close(context.Background())) })

	results := make(chan event.Outcome, 2)
	for _, s := range []*Session{root, child} {
		go func() {
			res, err := s.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
			assert.NoError(t, err)
			results <- res.Outcome
		}()
	}
	for range 2 {
		assert.Equal(t, event.Outcome{Status: event.Completed}, <-results)
	}
}

func twoProbeCalls() ai.FauxResponseStep {
	return ai.FauxAssistantMessage(
		ai.FauxContentBlocks{
			ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"}),
			ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-2"}),
		},
		ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
	)
}

// Opening a child session from parallel tool calls must not touch state the
// root's sibling tool calls or other live sessions read.
func TestChildOpenedFromParallelToolCallsDoesNotRace(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	childFaux := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "kopi-faux-child", Model: "faux-1"})
	require.NoError(t, r.RegisterProvider(childFaux.Provider()))
	childFaux.SetResponses([]ai.FauxResponseStep{twoProbeCalls(), reply("child one"), reply("child two")})
	faux.SetResponses([]ai.FauxResponseStep{twoProbeCalls(), reply("root done")})

	var root *Session
	probe := probeTool(func(ctx context.Context, _ json.RawMessage) (string, error) {
		child, err := r.Open(ctx, OpenOptions{Model: "kopi-faux-child/faux-1", Parent: root, SystemPrompt: kopiPrompt, Tools: []tool.Tool{probeTool(okProbe)}, Authority: policy.NewAuthority()})
		if err != nil {
			return "", err
		}
		defer func() { assert.NoError(t, child.Close(context.Background())) }()
		res, err := child.Send(ctx, Prompt{Text: "go"}, nil, nil)
		assert.Equal(t, event.Completed, res.Outcome.Status)
		return toolOutput, err
	})
	root = openTestSession(t, r, probe)
	res, err := root.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
}
