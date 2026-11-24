package pigruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"slices"
	"strings"
	"sync/atomic"
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

// recordDelivered collects the UserMessage, TaskEnded and UserCommand events; all of a
// turn's have been sent once its Send returned.
func recordDelivered(t *testing.T, sess *Session) <-chan event.Event {
	t.Helper()
	evs := make(chan event.Event, 32)
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		switch ev.(type) {
		case event.UserMessage, event.TaskEnded, event.UserCommand:
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
func moreOnce(prompts ...Prompt) (more func() []Entry, calls *int) {
	return handOnce(prompts, nil)
}

// handOnce is moreOnce for prompts and notices, prompts first.
func handOnce(prompts []Prompt, notices []Notice) (more func() []Entry, calls *int) {
	var entries []Entry
	for _, p := range prompts {
		entries = append(entries, Entry{Prompt: p})
	}
	for _, n := range notices {
		entries = append(entries, Entry{Notice: &n})
	}
	return handEntries(entries...)
}

// handEntries hands over entries, in order, at its first call and nothing after.
func handEntries(entries ...Entry) (more func() []Entry, calls *int) {
	n := 0
	return func() []Entry {
		n++
		if n > 1 {
			return nil
		}
		return entries
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
	assert.Nil(t, res.Continuation)

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

func testCommand(command string) Notice {
	return Notice{
		Text:  "The user ran: " + command,
		Shown: event.UserCommand{Command: command, State: event.CommandExited, Elapsed: 2 * time.Second, Exit: "exit 1", Failed: true, Output: "out of " + command, TaskID: "t9"},
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

func TestSendHandsOverEntriesInOrder(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	evs := recordDelivered(t, sess)
	first, last := testCommand("ls"), testNotice("t1")
	more, _ := handEntries(Entry{Notice: &first}, Entry{Prompt: Prompt{Text: "x"}}, Entry{Notice: &last})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.Empty(t, res.Undelivered)
	assert.Empty(t, res.UndeliveredNotices)
	assert.Equal(t, []string{"prompt", first.Text, "x", last.Text}, userTexts(<-requests))
	assert.Equal(t, []event.Event{first.Shown, event.UserMessage{Text: "x"}, last.Shown}, drain(evs))
}

func TestSendUndeliversEntriesAfterNoticeFails(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	evs := recordDelivered(t, sess)
	failing, last := testCommand("ls"), testNotice("t1")
	handed, _ := handEntries(Entry{Notice: &failing}, Entry{Prompt: Prompt{Text: "x"}}, Entry{Notice: &last})
	// Closing the session is the only way a notice fails to queue; it ends the run.
	more := func() []Entry {
		require.NoError(t, sess.inner.Close())
		return handed()
	}

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	_ = err
	assert.Equal(t, []Prompt{{Text: "x"}}, res.Undelivered)
	assert.Equal(t, []Notice{failing, last}, res.UndeliveredNotices)
	assert.Empty(t, drain(evs))
}

func TestRecordNoticesAddsWithoutRequest(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	n1, n2 := testCommand("ls"), testNotice("t1")

	require.NoError(t, sess.RecordNotices([]Notice{n1, n2}))
	assert.Zero(t, faux.CallCount())
	assert.Equal(t, []event.Event{n1.Shown, n2.Shown}, drain(evs))
}

func TestRecordNoticesStopsAtFailureWithoutNotifying(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	require.NoError(t, sess.Close(t.Context()))

	err := sess.RecordNotices([]Notice{testCommand("ls"), testNotice("t1")})
	require.Error(t, err)
	assert.Empty(t, drain(evs))
}

func TestRecordNoticesStopsPartWayAtFailure(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	first := testCommand("ls")
	// The session closes once the first notice is recorded, so the second fails.
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.UserCommand); ok {
			assert.NoError(t, sess.inner.Close())
		}
	}))

	err := sess.RecordNotices([]Notice{first, testNotice("t1"), testNotice("t2")})
	require.Error(t, err)
	assert.Equal(t, []event.Event{first.Shown}, drain(evs))
}

func TestRecordNoticesThenSendReadsThemBeforePrompt(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	n := testCommand("ls")

	require.NoError(t, sess.RecordNotices([]Notice{n}))
	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.Equal(t, 1, faux.CallCount())
	assert.Equal(t, []string{n.Text, "prompt"}, userTexts(<-requests))
	assert.Equal(t, []event.Event{n.Shown}, drain(evs))
}

func TestRecordNoticesRefusedDuringSend(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))

	done := sendAsync(t, sess, "first")
	<-started
	err := sess.RecordNotices([]Notice{testCommand("ls")})
	close(release)
	require.NoError(t, <-done)

	require.ErrorIs(t, err, ErrSendInProgress)
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

// PiG answers a call "Operation aborted" when the turn was cancelled while the
// call's before hook ran. A sequential batch stops at the cancelled call and
// never reports the ones after it, so the calls here run in parallel.
func TestCancelledTurnMarksAbortedCallInterrupted(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	two := ai.FauxAssistantMessage(ai.FauxContentBlocks{
		ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"}),
		ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-2"}),
	}, ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)})
	faux.SetResponses([]ai.FauxResponseStep{two, reply("unreached")})
	hooked, cancelled := make(chan struct{}), make(chan struct{})
	var targets atomic.Int32
	probe := probeTool(okProbe)
	probe.Target = func(json.RawMessage) (policy.Target, error) {
		// The second call's before hook returns only once the turn is cancelled.
		if targets.Add(1) == 2 {
			close(hooked)
			<-cancelled
		}
		return policy.Target{}, nil
	}
	sess := openTestSession(t, r, probe)
	ends := make(chan event.ToolEnd, 2)
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		if end, ok := ev.(event.ToolEnd); ok {
			ends <- end
		}
	}))
	turnCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-hooked
		cancel()
		// Send forwards the cancel to PiG on its own goroutine (context.AfterFunc);
		// release the hook only once PiG's run context is cancelled.
		<-sess.inner.Agent().Signal().Done()
		close(cancelled)
	}()

	res, err := sess.Send(turnCtx, Prompt{Text: "prompt"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Interrupted, res.Outcome.Status)

	got := map[string]event.ToolEnd{}
	for range 2 {
		end := <-ends
		got[end.CallID] = end
	}
	assert.Equal(t, event.EndInterrupted, got["call-2"].Kind)
	assert.Equal(t, abortedCall, got["call-2"].Output)
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

func TestSendQueuesModeAfterSkillsBeforeNotices(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
	sess := openTestSession(t, r)
	evs := recordDelivered(t, sess)
	n := testNotice("t1")

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt", Skills: []string{"S"}, Mode: "M"}, []Notice{n}, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.Equal(t, []string{"prompt", "S", "M", n.Text}, userTexts(<-requests))
	assert.Equal(t, []event.Event{n.Shown}, drain(evs))
	require.NoError(t, sess.Close(t.Context()))
	assert.Equal(t, []event.Event{
		event.TurnStart{Prompt: "prompt"},
		n.Shown,
		event.TextDelta{Text: "done"},
	}, resume(t, r, sess.ID()).History())
}

func TestSendHandsOverModeWithPrompt(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	evs := recordDelivered(t, sess)
	more, _ := moreOnce(Prompt{Text: "x", Skills: []string{"S"}, Mode: "M"})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, nil, more)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	assert.Equal(t, []string{"prompt", "x", "S", "M"}, userTexts(<-requests))
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

func TestSetNameSurvivesResume(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	faux.SetResponses([]ai.FauxResponseStep{reply("ok")})
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	assert.Empty(t, sess.Name())

	require.NoError(t, sess.SetName("Fix the build"))
	assert.Equal(t, "Fix the build", sess.Name())
	require.Error(t, sess.SetName(" "))
	_, err = sess.Send(t.Context(), Prompt{Text: "hi"}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, sess.Close(t.Context()))

	assert.Equal(t, "Fix the build", resume(t, r, sess.ID()).Name())
}

// openThinker opens a session on a reasoning model at low effort.
func openThinker(t *testing.T, r *Runtime, tools ...tool.Tool) (*Session, *ai.FauxProviderHandle, OpenOptions) {
	t.Helper()
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "faux-thinker", Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, r.RegisterProvider(thinker.Provider()))
	opts := OpenOptions{Model: "faux-thinker/thinker", Effort: LowEffort, SystemPrompt: kopiPrompt, Tools: tools, Authority: policy.NewAuthority()}
	sess, err := r.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	return sess, thinker, opts
}

func TestSendRaisesEffortForTheRunAndRestoresIt(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	sess, thinker, opts := openThinker(t, r)
	levels := make(chan ai.ThinkingLevel, 1)
	thinker.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
			levels <- o.Thinking
			return ai.FauxAssistantMessage(ai.FauxContentString("ok"), ai.FauxAssistantMessageOptions{}), nil
		}),
	})
	var changes int
	sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.EffortChanged); ok {
			changes++
		}
	})
	_, err := sess.Send(t.Context(), Prompt{Text: "think", Effort: MaxEffort}, nil, nil)
	require.NoError(t, err)

	got := <-levels
	assert.Equal(t, ai.ThinkingLevelHigh, got, "PiG clamps max to the model's highest level")
	assert.Equal(t, "low", sess.Effort())
	assert.Equal(t, 2, changes, "raise and restore")

	opts.Effort, opts.ResumeID = DefaultEffort, sess.ID()
	resumed, err := r.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, resumed.Close(context.Background())) })
	assert.Equal(t, "low", resumed.Effort())
}

func TestSendRaisesEffortForAHandedOverPrompt(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	sess, thinker, _ := openThinker(t, r, probeTool(okProbe))
	levels := make(chan ai.ThinkingLevel, 2)
	final := ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		levels <- o.Thinking
		return ai.FauxAssistantMessage(ai.FauxContentString("done"), ai.FauxAssistantMessageOptions{}), nil
	})
	call := ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		levels <- o.Thinking
		return ai.FauxAssistantMessage(
			ai.FauxContentBlocks{ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"})},
			ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
		), nil
	})
	thinker.SetResponses([]ai.FauxResponseStep{call, final})
	more, _ := moreOnce(Prompt{Text: "think hard", Effort: MaxEffort})
	_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, more)
	require.NoError(t, err)

	assert.Equal(t, ai.ThinkingLevelLow, <-levels, "the request before the hand-over")
	assert.Equal(t, ai.ThinkingLevelHigh, <-levels, "the request carrying the handed-over prompt")
	assert.Equal(t, "low", sess.Effort())
}

func TestSetEffortDuringARaisedRunKeepsTheUsersLevel(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	sess, thinker, _ := openThinker(t, r)
	thinker.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(_ ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
			assert.NoError(t, sess.SetEffort(MediumEffort))
			return ai.FauxAssistantMessage(ai.FauxContentString("ok"), ai.FauxAssistantMessageOptions{}), nil
		}),
	})
	res, err := sess.Send(t.Context(), Prompt{Text: "think", Effort: MaxEffort}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "medium", sess.Effort())
	assert.Nil(t, res.Continuation)
}

func TestSetEffortFailedSaveStillCountsAsAskedForALaterModelSwitch(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess, thinker, _ := openThinker(t, r)
	thinker.SetResponses([]ai.FauxResponseStep{ai.FauxAssistantMessage(ai.FauxContentString("ok"), ai.FauxAssistantMessageOptions{})})
	_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
	require.NoError(t, err)
	files := sessionFiles(t, sb.kopiHome)
	require.Len(t, files, 1)
	// The write is refused as the file is opened for appending.
	require.NoError(t, os.Chmod(files[0], 0o400))
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })
	if f, err := os.OpenFile(files[0], os.O_WRONLY|os.O_APPEND, 0); err == nil {
		_ = f.Close()
		t.Skip("file permissions do not stop this user from appending")
	}

	require.Error(t, sess.SetEffort(HighEffort))
	require.ErrorIs(t, r.SetModel(t.Context(), sess, fauxModel), ErrModelNotSaved)
	require.ErrorIs(t, r.SetModel(t.Context(), sess, "faux-thinker/thinker"), ErrModelNotSaved)

	assert.Equal(t, "high", sess.Effort())
}

func TestSendKeepsTheFirstRaiseWhenAHandedOverPromptRaisesAgain(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	sess, thinker, _ := openThinker(t, r, probeTool(okProbe))
	thinker.SetResponses([]ai.FauxResponseStep{
		ai.FauxAssistantMessage(
			ai.FauxContentBlocks{ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"})},
			ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
		),
		ai.FauxAssistantMessage(ai.FauxContentString("done"), ai.FauxAssistantMessageOptions{}),
	})
	var changes int
	sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.EffortChanged); ok {
			changes++
		}
	})
	more, _ := moreOnce(Prompt{Text: "again", Effort: MaxEffort})
	_, err := sess.Send(t.Context(), Prompt{Text: "go", Effort: MaxEffort}, nil, more)
	require.NoError(t, err)
	assert.Equal(t, "low", sess.Effort())
	assert.Equal(t, 2, changes, "one raise and one restore")
}

func TestSendLeavesEffortAloneWhenTheModelDoesNotReason(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	sess := openTestSession(t, r)
	faux.SetResponses([]ai.FauxResponseStep{reply("ok")})
	var changes int
	sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.EffortChanged); ok {
			changes++
		}
	})
	res, err := sess.Send(t.Context(), Prompt{Text: "think", Effort: MaxEffort}, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, res.Continuation)
	assert.Zero(t, changes)
	assert.Empty(t, sess.Effort())
}

func TestSendEffortContinuation(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		preCancelled, setEffort, explicit bool
		want                              ai.ThinkingLevel
	}{
		{name: "valid", want: ai.ThinkingLevelHigh},
		{name: "invalidated", setEffort: true, want: ai.ThinkingLevelLow},
		{name: "pre-cancelled", preCancelled: true, want: ai.ThinkingLevelHigh},
		{name: "pre-cancelled then invalidated", preCancelled: true, setEffort: true, want: ai.ThinkingLevelLow},
		{name: "explicit raise after invalidation", setEffort: true, explicit: true, want: ai.ThinkingLevelHigh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRuntime(t, newSandbox(t))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sess, thinker, _ := openThinker(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
				cancel()
				return toolOutput, nil
			}))
			levels := make(chan ai.ThinkingLevel, 1)
			thinker.SetResponses([]ai.FauxResponseStep{callProbe(), ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
				levels <- o.Thinking
				return ai.FauxAssistantMessage(ai.FauxContentString("ok"), ai.FauxAssistantMessageOptions{}), nil
			})})
			res, err := sess.Send(ctx, Prompt{Text: "think", Effort: MaxEffort}, nil, nil)
			require.NoError(t, err)
			require.Equal(t, event.Interrupted, res.Outcome.Status)
			require.NotNil(t, res.Continuation)
			assert.Equal(t, "low", sess.Effort())
			if tc.preCancelled {
				res, err = sess.Send(ctx, Prompt{Text: "instead", Continuation: res.Continuation}, nil, nil)
				require.NoError(t, err)
				require.False(t, res.Sent)
				require.NotNil(t, res.Continuation)
				assert.Equal(t, 1, thinker.CallCount())
			}
			if tc.setEffort {
				require.NoError(t, sess.SetEffort(LowEffort))
			}
			p := Prompt{Text: "continue", Continuation: res.Continuation}
			if tc.explicit {
				p.Effort = MaxEffort
			}
			res, err = sess.Send(t.Context(), p, nil, nil)
			require.NoError(t, err)
			assert.True(t, res.Sent)
			assert.Equal(t, tc.want, <-levels)
			assert.Equal(t, "low", sess.Effort())
		})
	}
}

func TestSetPromptPartReachesTheNextRequest(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	sess := openTestSession(t, r)
	prompt := func() string {
		faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
		_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
		require.NoError(t, err)
		return ai.GetCurrentSystemPrompt(<-requests)
	}
	require.Equal(t, kopiText, prompt())

	sess.SetPromptPart(PromptPart{Name: "mcp-servers", Text: "MCP server s tools: mcp__s__t."})
	assert.Equal(t, kopiText+"\n\nMCP server s tools: mcp__s__t.", prompt())

	sess.SetPromptPart(PromptPart{Name: "mcp-servers", Text: "MCP server s tools: mcp__s__t.\nMCP server u tools: mcp__u__v."})
	assert.Equal(t, kopiText+"\n\nMCP server s tools: mcp__s__t.\nMCP server u tools: mcp__u__v.", prompt())
}

func TestResumedSessionTakesAPartSetAfterItOpened(t *testing.T) {
	for _, when := range []string{"before its first prompt", "after its first prompt"} {
		t.Run(when, func(t *testing.T) {
			sb := newSandbox(t)
			r, faux := newTestRuntime(t, sb)
			servers := PromptPart{Name: "mcp-servers", Text: "MCP tools."}
			saved, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: append(slices.Clone(kopiPrompt), servers), Authority: policy.NewAuthority()})
			require.NoError(t, err)
			faux.SetResponses([]ai.FauxResponseStep{reply("saved")})
			_, err = saved.Send(t.Context(), Prompt{Text: "save"}, nil, nil)
			require.NoError(t, err)
			require.NoError(t, saved.Close(t.Context()))

			// The servers have not settled when the session is opened again.
			sess := resume(t, r, saved.ID())
			requests := make(chan []ai.Message, 1)
			prompt := func() string {
				faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
				_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
				require.NoError(t, err)
				return ai.GetCurrentSystemPrompt(<-requests)
			}
			if when == "after its first prompt" {
				require.Equal(t, kopiText, prompt())
			}
			sess.SetPromptPart(servers)
			assert.Equal(t, kopiText+"\n\nMCP tools.", prompt())
			assert.Equal(t, kopiText+"\n\nMCP tools.", prompt())
		})
	}
}

// systemUpdates counts the system messages that carry sections in a transcript.
func systemUpdates(sess *Session) int {
	n := 0
	for _, m := range sess.inner.Agent().MessagesSnapshot() {
		if m.System != nil && len(m.System.Sections) > 0 {
			n++
		}
	}
	return n
}

func TestResumedSessionKeepsALaterPartUntilItIsSet(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	servers := PromptPart{Name: "mcp-servers", Text: "MCP tools."}
	saved, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: append(slices.Clone(kopiPrompt), servers), Authority: policy.NewAuthority()})
	require.NoError(t, err)
	faux.SetResponses([]ai.FauxResponseStep{reply("saved")})
	_, err = saved.Send(t.Context(), Prompt{Text: "save"}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, saved.Close(t.Context()))

	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, ResumeID: saved.ID(), SystemPrompt: kopiPrompt, Later: []string{"mcp-servers"}, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	requests := make(chan []ai.Message, 1)
	prompt := func() string {
		faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
		_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
		require.NoError(t, err)
		return ai.GetCurrentSystemPrompt(<-requests)
	}
	stored := systemUpdates(sess)

	assert.Equal(t, kopiText+"\n\nMCP tools.", prompt())
	assert.Equal(t, stored, systemUpdates(sess), "keeping the stored part adds no system message")

	sess.SetPromptPart(servers)
	assert.Equal(t, kopiText+"\n\nMCP tools.", prompt())
	assert.Equal(t, stored, systemUpdates(sess), "the same text adds none")

	sess.SetPromptPart(PromptPart{Name: "mcp-servers", Text: "Other tools."})
	assert.Equal(t, kopiText+"\n\nOther tools.", prompt())

	sess.SetPromptPart(PromptPart{Name: "mcp-servers"})
	assert.Equal(t, kopiText, prompt())

	sess.SetPromptPart(PromptPart{Name: "mcp-servers"})
	assert.Equal(t, kopiText, prompt(), "removing a part the session has not is a no-op")
}

func TestNewSessionIgnoresLaterParts(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: append(slices.Clone(kopiPrompt), PromptPart{Name: "mcp-servers", Text: "MCP tools."}), Later: []string{"mcp-servers"}, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
	_, err = sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, kopiText+"\n\nMCP tools.", ai.GetCurrentSystemPrompt(<-requests))
}

func TestResumedSessionWithoutALaterPartDropsThePassedOne(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	saved := openTestSession(t, r)
	faux.SetResponses([]ai.FauxResponseStep{reply("saved")})
	_, err := saved.Send(t.Context(), Prompt{Text: "save"}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, saved.Close(t.Context()))

	servers := PromptPart{Name: "mcp-servers", Text: "MCP tools."}
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, ResumeID: saved.ID(), SystemPrompt: append(slices.Clone(kopiPrompt), servers), Later: []string{"mcp-servers"}, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	requests := make(chan []ai.Message, 1)
	prompt := func() string {
		faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
		_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
		require.NoError(t, err)
		return ai.GetCurrentSystemPrompt(<-requests)
	}
	stored := systemUpdates(sess)

	assert.Equal(t, kopiText, prompt())
	assert.Equal(t, stored, systemUpdates(sess))

	sess.SetPromptPart(servers)
	assert.Equal(t, kopiText+"\n\nMCP tools.", prompt())
	assert.Equal(t, stored+1, systemUpdates(sess))
}

// Two prompts of this size pass PiG's compaction threshold on the faux model's
// window, and one alone is more than PiG keeps verbatim, so a compaction
// summarises the first and keeps the second.
var (
	oldPrompt  = strings.Repeat("a", 240_000)
	keptPrompt = strings.Repeat("b", 240_000)
)

// afterSummary names the user messages a request carries after the compaction
// summary, which must be its first.
func afterSummary(t *testing.T, msgs []ai.Message, summary string) []string {
	t.Helper()
	texts := userTexts(msgs)
	require.NotEmpty(t, texts)
	require.Contains(t, texts[0], summary)
	for i, text := range texts {
		if text == keptPrompt {
			texts[i] = "kept prompt"
		}
	}
	return texts[1:]
}

func recordNotices(t *testing.T, sess *Session) <-chan event.Event {
	t.Helper()
	evs := make(chan event.Event, 32)
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.Notice); ok {
			evs <- ev
		}
	}))
	return evs
}

var compacted = []event.Event{event.Notice{Text: "Compacting context (threshold)"}, event.Notice{Text: "Compaction done"}}

func TestRecallFollowsCompactionWithoutAnotherRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		recall func() string
		want   []string
	}{
		{"recall", func() string { return "recalled" }, []string{"kept prompt", "recalled", "next"}},
		{"empty recall", func() string { return "" }, []string{"kept prompt", "next"}},
		{"no recall", nil, []string{"kept prompt", "next"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, faux := newTestRuntime(t, newSandbox(t))
			requests := make(chan []ai.Message, 1)
			faux.SetResponses([]ai.FauxResponseStep{reply("first"), reply("second"), reply("the summary"), captureRequest(requests, "third")})
			sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority(), Recall: tc.recall})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
			notices := recordNotices(t, sess)

			for _, prompt := range []string{oldPrompt, keptPrompt} {
				_, err = sess.Send(t.Context(), Prompt{Text: prompt}, nil, nil)
				require.NoError(t, err)
			}
			assert.Equal(t, compacted, drain(notices))
			assert.Equal(t, 3, faux.CallCount(), "two replies and the summary: the recalled line starts no request")

			_, err = sess.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, afterSummary(t, <-requests, "the summary"))
		})
	}
}

func TestRecallFollowsCompactionInsideRun(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	requests := make(chan []ai.Message, 2)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), callProbe(), reply("the summary"), captureRequest(requests, "second"), captureRequest(requests, "third")})
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, SystemPrompt: kopiPrompt, Tools: []tool.Tool{probeTool(okProbe)}, Authority: policy.NewAuthority(),
		Recall: func() string { return "recalled" },
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	notices := recordNotices(t, sess)

	for _, prompt := range []string{oldPrompt, keptPrompt} {
		_, err = sess.Send(t.Context(), Prompt{Text: prompt}, nil, nil)
		require.NoError(t, err)
	}
	assert.Equal(t, compacted, drain(notices))
	assert.Equal(t, 4, faux.CallCount())
	assert.Equal(t, []string{"kept prompt"}, afterSummary(t, <-requests, "the summary"), "the run's next request follows the compaction without the recalled line")

	_, err = sess.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"kept prompt", "recalled", "next"}, afterSummary(t, <-requests, "the summary"))
}

func TestRecallSkipsFailedCompaction(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	faux.SetResponses([]ai.FauxResponseStep{
		reply("first"), reply("second"),
		ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "invalid request: bad thing"}),
	})
	var recalls atomic.Int32
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority(),
		Recall: func() string { recalls.Add(1); return "recalled" },
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	notices := recordNotices(t, sess)

	for _, prompt := range []string{oldPrompt, keptPrompt} {
		_, err = sess.Send(t.Context(), Prompt{Text: prompt}, nil, nil)
		require.NoError(t, err)
	}
	evs := drain(notices)
	require.Len(t, evs, 2)
	assert.Equal(t, compacted[0], evs[0])
	assert.Contains(t, evs[1].(event.Notice).Text, "Compaction failed")
	assert.Zero(t, recalls.Load())
}
