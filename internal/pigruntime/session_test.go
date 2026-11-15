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

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
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
		_, err := sess.Send(t.Context(), Prompt{Text: text}, nil)
		done <- err
	}()
	return done
}

// recordUserMessages collects the UserMessage events; all of a turn's have
// been sent once its Send returned.
func recordUserMessages(t *testing.T, sess *Session) <-chan event.Event {
	t.Helper()
	evs := make(chan event.Event, 32)
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.UserMessage); ok {
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
func moreOnce(prompts ...Prompt) (more func() []Prompt, calls *int) {
	n := 0
	return func() []Prompt {
		n++
		if n > 1 {
			return nil
		}
		return prompts
	}, &n
}

func okProbe(context.Context, json.RawMessage) (string, error) { return toolOutput, nil }

func TestSendHandsOverAtToolTurnEnd(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	sess := openTestSession(t, r, probeTool(okProbe))
	evs := recordUserMessages(t, sess)
	more, _ := moreOnce(Prompt{Text: "x"}, Prompt{Text: "y"})

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, more)
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

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, more)
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

		res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, more)
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

		res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, more)
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

		_, err := sess.Send(turnCtx, Prompt{Text: "prompt"}, more)
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

	_, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, more)
	require.NoError(t, err)

	msgs := <-requests
	assert.Equal(t, []string{"prompt", "x [image #1] [image #2]\n\n" + omittedImageNote}, userTexts(msgs))
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

	_, err := sess.Send(t.Context(), Prompt{Text: "look [image #1]", Images: []Image{sent}}, more)
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

	_, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, more)
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
	res, err := sess.Send(t.Context(), Prompt{Text: "second"}, nil)
	close(release)
	require.NoError(t, <-done)

	require.Error(t, err)
	assert.False(t, res.Sent)
	assert.Empty(t, res.Undelivered)
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

	res, err := sess.Send(t.Context(), Prompt{Text: "prompt"}, more)
	require.NoError(t, err)
	assert.True(t, res.Sent)
	assert.Equal(t, []Prompt{handed}, res.Undelivered)
	assert.Equal(t, []string{"prompt"}, userTexts(<-requests))
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

			res, err := sess.Send(turnCtx, Prompt{Text: "prompt"}, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.status, res.Outcome.Status)
			assert.Equal(t, tt.want, res.Reply)
			assert.Equal(t, tt.calls, faux.CallCount())
		})
	}
}
