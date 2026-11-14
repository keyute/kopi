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

	"github.com/MichaelKinsy/PiG/ai"
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

func sendAsync(t *testing.T, sess *Session, text string, images ...Image) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := sess.Send(t.Context(), text, images...)
		done <- err
	}()
	return done
}

func steer(sess *Session, text string, images ...Image) {
	sess.Steer(sess.PrepareSteer(Prompt{Text: text, Images: images}))
}

// recordQueue collects the QueueChanged and UserMessage events; all of a
// turn's have been sent once its Send returned.
func recordQueue(t *testing.T, sess *Session) <-chan event.Event {
	t.Helper()
	evs := make(chan event.Event, 32)
	t.Cleanup(sess.Subscribe(func(ev event.Event) {
		switch ev.(type) {
		case event.QueueChanged, event.UserMessage:
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

// queueChanged normalises an empty Pending so event lists compare whether PiG
// reports an empty queue as nil or empty.
func queueChanged(evs []event.Event) []event.Event {
	out := make([]event.Event, 0, len(evs))
	for _, ev := range evs {
		if q, ok := ev.(event.QueueChanged); ok && len(q.Pending) == 0 {
			ev = event.QueueChanged{}
		}
		out = append(out, ev)
	}
	return out
}

func TestSteerDeliversAtNextBoundary(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))
	evs := recordQueue(t, sess)

	done := sendAsync(t, sess, "prompt")
	<-started
	steer(sess, "x")
	close(release)
	require.NoError(t, <-done)

	assert.Equal(t, []string{"prompt", "x"}, userTexts(<-requests))
	assert.Equal(t, []event.Event{
		event.QueueChanged{Pending: []string{"x"}},
		event.QueueChanged{},
		event.UserMessage{Text: "x"},
	}, queueChanged(drain(evs)))
	assert.Empty(t, sess.TakeQueued())
}

func TestSteersDeliveredInOneRequest(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))
	evs := recordQueue(t, sess)

	done := sendAsync(t, sess, "prompt")
	<-started
	steer(sess, "x")
	steer(sess, "y")
	close(release)
	require.NoError(t, <-done)

	assert.Equal(t, []string{"prompt", "x", "y"}, userTexts(<-requests))
	assert.Equal(t, 2, faux.CallCount())
	assert.Equal(t, []event.Event{
		event.QueueChanged{Pending: []string{"x"}},
		event.QueueChanged{Pending: []string{"x", "y"}},
		event.QueueChanged{Pending: []string{"y"}},
		event.UserMessage{Text: "x"},
		event.QueueChanged{},
		event.UserMessage{Text: "y"},
	}, queueChanged(drain(evs)))
}

func TestTakeQueuedReturnsUndeliveredPrompts(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	first, second := make(chan []ai.Message, 1), make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(first, "done"), captureRequest(second, "next done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))

	done := sendAsync(t, sess, "prompt")
	<-started
	img := Image{Path: "/shots/a.png", MimeType: "image/png", Data: pngBytes(t)}
	steer(sess, "x", img)
	steer(sess, "y")
	assert.Equal(t, []Prompt{{Text: "x", Images: []Image{img}}, {Text: "y"}}, sess.TakeQueued())
	close(release)
	require.NoError(t, <-done)
	assert.Equal(t, []string{"prompt"}, userTexts(<-first))

	_, err := sess.Send(t.Context(), "next")
	require.NoError(t, err)
	assert.Equal(t, []string{"prompt", "next"}, userTexts(<-second))
	assert.Empty(t, sess.TakeQueued())
}

func TestSendAndSteerCarryImages(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))

	sent := Image{MimeType: "image/png", Data: pngBytes(t)}
	done := sendAsync(t, sess, "look [image #1]", sent)
	<-started
	steered := Image{MimeType: "image/jpeg", Data: jpegBytes(t)}
	steer(sess, "and [image #2]", steered)
	close(release)
	require.NoError(t, <-done)

	assert.Equal(t, []ai.ImageContent{
		{Data: base64.StdEncoding.EncodeToString(sent.Data), MimeType: "image/png"},
		{Data: base64.StdEncoding.EncodeToString(steered.Data), MimeType: "image/jpeg"},
	}, userImages(<-requests))
}

func TestSteerResizesLargeImage(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))

	done := sendAsync(t, sess, "prompt")
	<-started
	steer(sess, "x [image #1]", Image{MimeType: "image/png", Data: widePNG(t)})
	close(release)
	require.NoError(t, <-done)

	images := userImages(<-requests)
	require.Len(t, images, 1)
	data, err := base64.StdEncoding.DecodeString(images[0].Data)
	require.NoError(t, err)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Less(t, cfg.Width, 6000)
	assert.Less(t, cfg.Height, 40)
}

func TestTakeQueuedReturnsOriginalLargeImage(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(make(chan []ai.Message, 1), "done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))

	done := sendAsync(t, sess, "prompt")
	<-started
	img := Image{Path: "/shots/wide.png", MimeType: "image/png", Data: widePNG(t)}
	steer(sess, "x", img)
	assert.Equal(t, []Prompt{{Text: "x", Images: []Image{img}}}, sess.TakeQueued())
	close(release)
	require.NoError(t, <-done)
}

func TestSteerOmitsUnprocessableImage(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))

	done := sendAsync(t, sess, "prompt")
	<-started
	good := Image{MimeType: "image/png", Data: pngBytes(t)}
	steer(sess, "x [image #1] [image #2] [image #3]", corruptPNG(), good, corruptPNG())
	close(release)
	require.NoError(t, <-done)

	msgs := <-requests
	want := "x [image #1] [image #2] [image #3]\n\n" + omittedImageNote + "\n" + omittedImageNote
	assert.Equal(t, []string{"prompt", want}, userTexts(msgs))
	images := userImages(msgs)
	require.Len(t, images, 1)
	assert.Equal(t, "image/png", images[0].MimeType)
	sess.queuedMu.Lock()
	assert.Empty(t, sess.queued, "the delivered message left the mirror")
	sess.queuedMu.Unlock()
}

func TestTakeQueuedReturnsOriginalOfOmittedImage(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(make(chan []ai.Message, 1), "done")})
	started, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, heldProbe(started, release))

	done := sendAsync(t, sess, "prompt")
	<-started
	img := corruptPNG()
	img.Path = "/shots/broken.png"
	steer(sess, "x [image #1]", img)
	assert.Equal(t, []Prompt{{Text: "x [image #1]", Images: []Image{img}}}, sess.TakeQueued())
	close(release)
	require.NoError(t, <-done)
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
