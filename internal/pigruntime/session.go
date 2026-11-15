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
	inner          *coding.Session
	sendInProgress atomic.Bool
	// listenersMu guards listeners and nextID. notify holds it while calling
	// listeners, so no listener may call back into Session.
	listenersMu sync.Mutex
	listeners   []listener
	nextID      int
}

type listener struct {
	id int
	fn func(event.Event)
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

// Subscribe registers fn to run synchronously on PiG's event executor. A slow
// fn stalls the turn, and fn must not call back into the Session. Every call
// for a turn has run when Send returns.
func (s *Session) Subscribe(fn func(event.Event)) func() {
	s.listenersMu.Lock()
	s.nextID++
	id := s.nextID
	s.listeners = append(s.listeners, listener{id: id, fn: fn})
	s.listenersMu.Unlock()
	var j textJoiner
	unsubscribe := s.inner.Subscribe(func(ev agent.AgentEvent) {
		if e, ok := j.translate(ev); ok {
			fn(e)
		}
	})
	return func() {
		unsubscribe()
		s.listenersMu.Lock()
		defer s.listenersMu.Unlock()
		s.listeners = slices.DeleteFunc(s.listeners, func(l listener) bool { return l.id == id })
	}
}

// SendResult is what one Send did.
type SendResult struct {
	Outcome event.Outcome
	// Reply is the text of the run's last assistant message when Outcome is
	// Completed, else "". Its text blocks are joined as History joins them.
	Reply string
	// Sent reports that p was delivered; it is false when ctx was already done
	// or PiG rejected the prompt.
	Sent bool
	// Undelivered are the handed-over prompts the model never read, as given.
	Undelivered []Prompt
}

// Send runs one turn. Cancelling ctx is the abort request for this turn
// only: a ctx already done returns Interrupted without calling the provider,
// and a turn Kopi aborted reports Interrupted unless its last assistant message
// stopped normally (PiG reports an abort during a tool as StopReason "error"
// with "context canceled"). Model failures are not
// errors: they surface as a Failed outcome carrying the provider's error text.
// An overlapping call returns ErrSendInProgress unless ctx is already done.
//
// At the end of each turn that did not fail and was not aborted, while ctx is
// not cancelled, Send calls more, if not nil, for prompts to hand over; the
// model reads them at the next boundary of the same run. more is called on
// PiG's event executor, with no Session lock held. Each handed-over prompt becomes a UserMessage when it is
// delivered; p does not, as the caller draws it.
func (s *Session) Send(ctx context.Context, p Prompt, more func() []Prompt) (SendResult, error) {
	if ctx.Err() != nil {
		return SendResult{Outcome: event.Outcome{Status: event.Interrupted}}, nil
	}
	if !s.sendInProgress.CompareAndSwap(false, true) {
		return SendResult{}, fmt.Errorf("failed to send prompt: %w", ErrSendInProgress)
	}
	defer s.sendInProgress.Store(false)
	var aborted atomic.Bool
	abort := func() {
		aborted.Store(true)
		s.inner.RequestAbort()
	}
	runStart := abortAtRunStart(ctx, abort)
	// handed is the handed-over prompts not yet delivered. Send is the only
	// producer into PiG's queue and PiG delivers in queue order, after p, so
	// each user message after p's is the head.
	var handed []Prompt
	var sent bool
	// last is the run's last assistant message. SendContent returns PiG's
	// whole transcript, so it cannot say which assistant message is this run's.
	var last *agent.AssistantMessage
	unsubscribe := s.inner.Subscribe(func(ev agent.AgentEvent) {
		runStart(ev)
		switch ev := ev.(type) {
		case agent.MessageStartEvent:
			if ev.Message.User == nil {
				return
			}
			if !sent {
				sent = true
				return
			}
			if len(handed) > 0 {
				handed = handed[1:]
			}
			s.notify(event.UserMessage{Text: userText(ev.Message.User.Content)})
		case agent.MessageEndEvent:
			if ev.Message.Assistant != nil {
				last = ev.Message.Assistant
			}
		case agent.TurnEndEvent:
			// A steer after a failed or aborted turn would continue a run
			// that should end.
			if more == nil || aborted.Load() || ctx.Err() != nil {
				return
			}
			if a := ev.Message.Assistant; a != nil && (a.StopReason == ai.StopReasonError || a.StopReason == ai.StopReasonAborted) {
				return
			}
			for _, q := range more() {
				text, images := s.steerContent(q)
				handed = append(handed, q)
				s.inner.QueueSteer(text, images)
			}
		}
	})
	defer unsubscribe()
	abortDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(abortDone)
		abort()
	})
	// PiG gets an uncancellable ctx: a cancelled one makes it return
	// ctx.Err() instead of the turn, so RequestAbort is the only abort path.
	_, err := s.inner.SendContent(context.WithoutCancel(ctx), coding.BuildUserContent(p.Text, toAI(p.Images)))
	// An abort already under way must finish before Send returns, or its
	// RequestAbort could land on the following turn.
	if !stop() {
		<-abortDone
	}
	// What PiG has not drained must not reach the next run.
	_, _ = s.inner.ClearQueue()
	res := SendResult{Sent: sent, Undelivered: handed}
	if err != nil {
		return res, fmt.Errorf("failed to send prompt: %w", err)
	}
	// A cancel can land after PiG finished the turn but before stop(); a
	// turn that ended normally is complete whatever the abort hook saw.
	if aborted.Load() && (last == nil || last.StopReason != ai.StopReasonStop) {
		res.Outcome = event.Outcome{Status: event.Interrupted}
		return res, nil
	}
	if last == nil {
		res.Outcome = event.Outcome{Status: event.Completed}
		return res, nil
	}
	switch last.StopReason {
	case ai.StopReasonAborted:
		res.Outcome = event.Outcome{Status: event.Interrupted}
	case ai.StopReasonError:
		res.Outcome = event.Outcome{Status: event.Failed, Message: last.ErrorMessage}
	default:
		res.Outcome = event.Outcome{Status: event.Completed}
		var parts []string
		for _, block := range last.Content {
			if b, ok := block.(ai.TextContent); ok && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		res.Reply = strings.Join(parts, "\n\n")
	}
	return res, nil
}

// notify sends e to every listener, in the order they subscribed.
func (s *Session) notify(e event.Event) {
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	for _, l := range s.listeners {
		l.fn(e)
	}
}

const omittedImageNote = "[Image omitted: it could not be processed.]"

// steerContent does the image work PiG's prompt path does before images enter
// history and QueueSteer does not: it resizes p's images to the model's limits
// and, as that path does, omits an image that cannot be processed and notes
// the omission in the text.
func (s *Session) steerContent(p Prompt) (string, []ai.ImageContent) {
	text := p.Text
	var images []ai.ImageContent
	var notes []string
	for _, img := range s.resize(toAI(p.Images)) {
		if img == nil {
			notes = append(notes, omittedImageNote)
			continue
		}
		images = append(images, *img)
	}
	if len(notes) > 0 {
		text += "\n\n" + strings.Join(notes, "\n")
	}
	return text, images
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
