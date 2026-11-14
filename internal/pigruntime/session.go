package pigruntime

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"

	"github.com/keyute/kopi/internal/event"
)

// Session wraps a PiG session. PiG's Session.Events and Session.ListSessions
// are never used: an un-drained Events channel stalls the agent loop, and
// ListSessions reads ~/.pig.
type Session struct {
	inner *coding.Session
	// sends counts the prompts handed to PiG, so translation can tell a
	// prompt's own user message from a steered one.
	sends atomic.Uint64
	// steerMu keeps queued and PiG's steering queue changing together. It is
	// held across PiG calls that notify listeners on the caller's goroutine,
	// so no listener may take it.
	steerMu sync.Mutex
	// queuedMu guards queued; a listener takes it, so it is never held across
	// a PiG call.
	queuedMu sync.Mutex
	// queued mirrors PiG's steering queue, matched by queued text, with the
	// prompts as written: PiG's ClearQueue returns only the queued texts.
	queued []Steering
}

type Image struct {
	// Path is carried only so unsent queued input can be handed back.
	Path, MimeType string
	Data           []byte
}

type Prompt struct {
	Text   string
	Images []Image
}

func toAI(images []Image) []ai.ImageContent {
	out := make([]ai.ImageContent, 0, len(images))
	for _, img := range images {
		out = append(out, ai.ImageContent{Data: base64.StdEncoding.EncodeToString(img.Data), MimeType: img.MimeType})
	}
	return out
}

func (s *Session) ID() string { return s.inner.ID() }

// MessageCount counts user, assistant and tool-result messages; PiG also keeps
// the system prompt and tool set as system messages in the transcript.
func (s *Session) MessageCount() int {
	n := 0
	for _, m := range s.inner.Messages() {
		if m.User != nil || m.Assistant != nil || m.ToolResult != nil {
			n++
		}
	}
	return n
}

// ContextUsage reports the session's projected context tokens and the
// model's window. ok is false when the model declares no window or the
// tokens are unknown, as they are right after a compaction.
func (s *Session) ContextUsage() (tokens, window int, ok bool) {
	u := s.inner.ContextUsage()
	if u == nil || u.Tokens == nil {
		return 0, 0, false
	}
	return *u.Tokens, u.ContextWindow, true
}

// Subscribe registers fn to run synchronously on PiG's event executor; a slow
// fn stalls the turn. Every call for a turn has run when Send returns.
func (s *Session) Subscribe(fn func(event.Event)) func() {
	j := textJoiner{sends: &s.sends}
	return s.inner.Subscribe(func(ev agent.AgentEvent) {
		if e, ok := j.translate(ev); ok {
			fn(e)
		}
	})
}

// Send runs one turn. Cancelling ctx is the abort request for this turn
// only: a ctx already done returns Interrupted without calling the provider,
// and a turn Kopi aborted reports Interrupted unless its last assistant message
// stopped normally (PiG reports an abort during a tool as StopReason "error"
// with "context canceled"). Model failures are not
// errors: they surface as a Failed outcome carrying the provider's error text.
func (s *Session) Send(ctx context.Context, text string, images ...Image) (event.Outcome, error) {
	if ctx.Err() != nil {
		return event.Outcome{Status: event.Interrupted}, nil
	}
	var aborted atomic.Bool
	abort := func() {
		aborted.Store(true)
		s.inner.RequestAbort()
	}
	unsubscribe := s.inner.Subscribe(abortAtRunStart(ctx, abort))
	defer unsubscribe()
	defer s.inner.Subscribe(s.dropDelivered)()
	abortDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(abortDone)
		abort()
	})
	// PiG gets an uncancellable ctx: a cancelled one makes it return
	// ctx.Err() instead of the turn, so RequestAbort is the only abort path.
	s.sends.Add(1)
	msgs, err := s.inner.SendContent(context.WithoutCancel(ctx), coding.BuildUserContent(text, toAI(images)))
	// An abort already under way must finish before Send returns, or its
	// RequestAbort could land on the following turn.
	if !stop() {
		<-abortDone
	}
	if err != nil {
		return event.Outcome{}, fmt.Errorf("failed to send prompt: %w", err)
	}
	last := lastAssistant(msgs)
	// A cancel can land after PiG finished the turn but before stop(); a
	// turn that ended normally is complete whatever the abort hook saw.
	if aborted.Load() && (last == nil || last.StopReason != ai.StopReasonStop) {
		return event.Outcome{Status: event.Interrupted}, nil
	}
	if last != nil {
		switch last.StopReason {
		case ai.StopReasonAborted:
			return event.Outcome{Status: event.Interrupted}, nil
		case ai.StopReasonError:
			return event.Outcome{Status: event.Failed, Message: last.ErrorMessage}, nil
		}
	}
	return event.Outcome{Status: event.Completed}, nil
}

// Steering is a message prepared by PrepareSteer for Steer.
type Steering struct {
	// Prompt is what was written; TakeQueued hands it back.
	Prompt Prompt
	// text is what is queued: Prompt.Text plus a note per omitted image.
	text   string
	images []ai.ImageContent
}

const omittedImageNote = "[Image omitted: it could not be processed.]"

// PrepareSteer does the image work PiG's prompt path does before images enter
// history and QueueSteer does not: it resizes p's images to the model's limits
// and, as that path does, omits an image that cannot be processed and notes
// the omission in the text. It takes no lock and can be slow.
func (s *Session) PrepareSteer(p Prompt) Steering {
	st := Steering{Prompt: p, text: p.Text}
	var notes []string
	for _, img := range s.resize(toAI(p.Images)) {
		if img == nil {
			notes = append(notes, omittedImageNote)
			continue
		}
		st.images = append(st.images, *img)
	}
	if len(notes) > 0 {
		st.text += "\n\n" + strings.Join(notes, "\n")
	}
	return st
}

// Steer queues st for the model to read at the running turn's next boundary.
func (s *Session) Steer(st Steering) {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	s.queuedMu.Lock()
	s.queued = append(s.queued, st)
	s.queuedMu.Unlock()
	s.inner.QueueSteer(st.text, st.images)
}

// resize returns each image resized, or nil for one that cannot be processed.
// The limits model is PiG's: under a virtual selection, the physical model
// that answered last.
func (s *Session) resize(images []ai.ImageContent) []*ai.ImageContent {
	model := s.inner.Model()
	if routed := s.inner.RoutedModel(); routed != nil {
		model = routed.Model
	}
	var args subprocess.ResizeImageArgs
	if model != nil && model.InputLimits != nil && model.InputLimits.Images != nil && model.InputLimits.Images.Resize != nil {
		o := model.InputLimits.Images.Resize
		args.Options = &struct {
			MaxWidth    int `json:"maxWidth,omitempty"`
			MaxHeight   int `json:"maxHeight,omitempty"`
			MaxBytes    int `json:"maxBytes,omitempty"`
			JPEGQuality int `json:"jpegQuality,omitempty"`
		}{MaxWidth: o.MaxWidth, MaxHeight: o.MaxHeight, MaxBytes: o.MaxBytes, JPEGQuality: o.JPEGQuality}
	}
	out := make([]*ai.ImageContent, 0, len(images))
	for _, img := range images {
		args.Data, args.MimeType = img.Data, img.MimeType
		res, err := subprocess.ResizeImage(args)
		if err != nil || res.Image == nil {
			slog.Warn("failed to resize steered image; omitting it", "mime", img.MimeType, "error", err)
			out = append(out, nil)
			continue
		}
		out = append(out, &ai.ImageContent{Data: res.Image.Data, MimeType: res.Image.MimeType})
	}
	return out
}

// TakeQueued removes and returns the messages not yet delivered, in queue
// order. A message PiG has already drained for delivery is returned and is
// delivered too.
func (s *Session) TakeQueued() []Prompt {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	steering, _ := s.inner.ClearQueue()
	s.queuedMu.Lock()
	mirror := s.queued
	s.queued = nil
	s.queuedMu.Unlock()
	var out []Prompt
	for _, text := range steering {
		i := slices.IndexFunc(mirror, func(st Steering) bool { return st.text == text })
		if i < 0 {
			out = append(out, Prompt{Text: text})
			continue
		}
		out = append(out, mirror[i].Prompt)
		mirror = slices.Delete(mirror, i, i+1)
	}
	return out
}

// dropDelivered removes a delivered message from the mirror as PiG removes it
// from its queue: the first queued entry with the user message's non-empty
// text.
func (s *Session) dropDelivered(ev agent.AgentEvent) {
	start, ok := ev.(agent.MessageStartEvent)
	if !ok || start.Message.User == nil {
		return
	}
	text := userText(start.Message.User.Content)
	if text == "" {
		return
	}
	s.queuedMu.Lock()
	defer s.queuedMu.Unlock()
	if i := slices.IndexFunc(s.queued, func(st Steering) bool { return st.text == text }); i >= 0 {
		s.queued = slices.Delete(s.queued, i, i+1)
	}
}

// abortAtRunStart re-issues the abort at agent_start when ctx is already
// cancelled. PiG drops an abort requested before the run is marked busy
// (RequestAbort only records it while a run is active) and clears pending
// requests when the run starts; agent_start is emitted after that clear and
// before the first provider request, so aborting there stops the run as PiG's
// own agent_start check does.
func abortAtRunStart(ctx context.Context, abort func()) func(agent.AgentEvent) {
	return func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.AgentStartEvent); ok && ctx.Err() != nil {
			abort()
		}
	}
}

func lastAssistant(msgs []agent.AgentMessage) *agent.AssistantMessage {
	for _, m := range slices.Backward(msgs) {
		if m.Assistant != nil {
			return m.Assistant
		}
	}
	return nil
}

// Close aborts any running turn, waits for it within ctx, and always releases
// the session.
func (s *Session) Close(ctx context.Context) error {
	abortErr := s.inner.Abort(ctx)
	closeErr := s.inner.Close()
	if abortErr != nil {
		return fmt.Errorf("failed to stop session run: %w: %w", ErrJoinTimeout, abortErr)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to close session: %w", closeErr)
	}
	return nil
}
