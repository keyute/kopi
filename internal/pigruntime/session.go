package pigruntime

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// Session wraps a PiG session. PiG's Session.Events and Session.ListSessions
// are never used: an un-drained Events channel stalls the agent loop, and
// ListSessions resolves PiG's default agent dir, not the runtime's.
type Session struct {
	inner *coding.Session
	// gate admits the session's tool calls; joining is the runtime's extension.
	gate    *policy.Gate
	joining *extension.Extension
	// partsMu guards parts, the system prompt as it is now.
	partsMu        sync.Mutex
	parts          []PromptPart
	sendInProgress atomic.Bool
	// listenersMu guards listeners and nextID. notify holds it while calling
	// listeners, so no listener may call back into Session.
	listenersMu sync.Mutex
	listeners   []listener
	nextID      int
	// toolsMu serializes LoadTools' read-modify-write of the active tools.
	toolsMu sync.Mutex
	// effortMu guards asked, raised, restore and effortRevision.
	effortMu       sync.Mutex
	effortRevision uint64
	// asked is the level last asked for (SetEffort, or open), before PiG clamped
	// it to the model; empty when none was. A model switch applies it again; a
	// raise never changes it.
	asked ai.ModelThinkingLevel
	// raised is the Effort that lifted the level while a Send runs, and
	// DefaultEffort when none did; restore is the level to put back when it ends.
	raised  Effort
	restore ai.ModelThinkingLevel
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
	// Skills holds the texts of the skills the prompt loads. The model reads
	// them as messages right after Text; the transcript does not draw them.
	Skills []string
	// FromAgent marks a prompt another agent sent rather than the human typed;
	// it is carried only so input a run never read goes back to its sender.
	FromAgent bool
	// Mode is the hidden line Kopi tells the model with a prompt: the session's
	// mode and the directories added to the workspace. It is read after Text and
	// Skills; the transcript does not draw it. Empty sends none.
	Mode string
	// Tools names the deferred tools the prompt makes callable, from the
	// model request that carries it. Names the session does not have are
	// ignored.
	Tools []string
	// Effort is the level the run reads this prompt at and keeps until it
	// ends, when the model reasons; DefaultEffort leaves the session's level.
	Effort Effort
	// Continuation keeps an interrupted send's raise unless SetEffort has
	// replaced it. An explicit Effort takes precedence.
	Continuation *EffortContinuation
}

// EffortContinuation is opaque state passed from a SendResult to the prompt
// that continues its interrupted turn.
type EffortContinuation struct {
	revision uint64
	effort   Effort
}

// skillType is the PiG custom type of a skill text, stored in session history.
const skillType = "kopi.skill"

func skillMessage(text string) extension.CustomMessageRef {
	return extension.CustomMessageRef{CustomType: skillType, Content: text, Display: false}
}

// modeType is the PiG custom type of a mode line, stored in session history.
const modeType = "kopi.mode"

func modeMessage(text string) extension.CustomMessageRef {
	return extension.CustomMessageRef{CustomType: modeType, Content: text, Display: false}
}

// recallAfterCompaction sends text() as a mode line after every compaction that
// succeeded, for the session's lifetime.
//
// The message does not trigger a turn: PiG appends such a message at the end of
// the turn in progress, before an overflow retry, or at once when no run is
// active. A steered one would count as queued input after a final reply and
// make PiG request the model again.
func (s *Session) recallAfterCompaction(text func() string) {
	passive := false
	s.inner.Subscribe(func(ev agent.AgentEvent) {
		end, ok := ev.(agent.CompactionEndEvent)
		if !ok || end.Aborted || end.ErrorMessage != "" {
			return
		}
		line := text()
		if line == "" {
			return
		}
		if err := s.inner.SendCustomMessage(context.Background(), modeMessage(line), &extension.SendMessageOptions{TriggerTurn: &passive}); err != nil {
			slog.Warn("failed to recall after compaction", "error", err)
		}
	})
}

// Notice is input Kopi gives the model on its own behalf, not the human's. The
// model reads Text as user input; the transcript draws Shown where the model
// reads it, live and on resume. Shown is an event.TaskEnded or an
// event.UserCommand.
type Notice struct {
	Text  string
	Shown event.Event
}

// Entry is one item of a hand-over: a prompt, or a notice when Notice is
// non-nil.
type Entry struct {
	Prompt Prompt
	Notice *Notice
}

// noticeType is the PiG custom type of a delivered Notice whose Shown is an
// event.TaskEnded, stored in session history.
const noticeType = "kopi.notice"

// commandType is the PiG custom type of a delivered Notice whose Shown is an
// event.UserCommand, stored in session history.
const commandType = "kopi.command"

// noticeDetails is what history keeps of a Notice's Shown TaskEnded.
type noticeDetails struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Agent     string `json:"agent,omitempty"`
	Status    string `json:"status"`
	ElapsedMs int64  `json:"elapsedMs"`
	Agents    int    `json:"agents,omitempty"`
	Failed    int    `json:"failed,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Result    string `json:"result,omitempty"`
}

// commandDetails is what history keeps of a Notice's Shown UserCommand.
type commandDetails struct {
	Command   string `json:"command"`
	State     string `json:"state"`
	Exit      string `json:"exit,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
	ElapsedMs int64  `json:"elapsedMs"`
	Task      string `json:"task,omitempty"`
	Output    string `json:"output,omitempty"`
}

func (n Notice) message() extension.CustomMessageRef {
	customType, details := noticeType, any(nil)
	if c, ok := n.Shown.(event.UserCommand); ok {
		customType, details = commandType, commandDetails{
			Command: c.Command, State: string(c.State), Exit: c.Exit, Failed: c.Failed,
			ElapsedMs: c.Elapsed.Milliseconds(), Task: c.TaskID, Output: c.Output,
		}
	} else {
		t, _ := n.Shown.(event.TaskEnded)
		details = noticeDetails{
			ID: t.ID, Command: t.Command, Agent: t.Agent, Status: t.Status, ElapsedMs: t.Elapsed.Milliseconds(),
			Agents: t.Agents, Failed: t.Failed, Reason: t.Reason, Result: t.Result,
		}
	}
	return extension.CustomMessageRef{CustomType: customType, Content: n.Text, Display: true, Details: details}
}

// label names the notice in logs.
func (n Notice) label() string {
	switch s := n.Shown.(type) {
	case event.TaskEnded:
		return s.ID
	case event.UserCommand:
		return s.Command
	}
	return ""
}

func isNotice(m agent.AgentMessage) bool {
	t, _ := m.Custom["customType"].(string)
	return t == noticeType || t == commandType
}

func toAI(images []Image) []ai.ImageContent {
	out := make([]ai.ImageContent, 0, len(images))
	for _, img := range images {
		out = append(out, ai.ImageContent{Data: base64.StdEncoding.EncodeToString(img.Data), MimeType: img.MimeType})
	}
	return out
}

func (s *Session) ID() string { return s.inner.ID() }

// SetPromptPart replaces the part of the system prompt that has p's name, or
// adds p after the others, from the session's next model request. A part with
// empty text removes the part of that name; the session may have none.
func (s *Session) SetPromptPart(p PromptPart) {
	s.partsMu.Lock()
	defer s.partsMu.Unlock()
	i := slices.IndexFunc(s.parts, func(q PromptPart) bool { return q.Name == p.Name })
	switch {
	case p.Text == "":
		if i < 0 {
			return
		}
		s.parts = slices.Delete(s.parts, i, i+1)
	case i < 0:
		s.parts = append(s.parts, p)
	case s.parts[i] == p:
		return
	default:
		s.parts[i] = p
	}
	s.inner.SetSystemPromptSections(promptSections(s.parts))
}

func (s *Session) sections() ai.OrderedSections {
	s.partsMu.Lock()
	defer s.partsMu.Unlock()
	return promptSections(s.parts)
}

// Name is the session's name; empty when it has none.
func (s *Session) Name() string { return s.inner.SessionName() }

// SetName gives the session a name, which PiG stores in the session file.
func (s *Session) SetName(name string) error {
	if err := s.inner.SetName(name); err != nil {
		return fmt.Errorf("failed to name session: %w", err)
	}
	return nil
}

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

// CompactAt reports the context size above which the session compacts: PiG
// compacts once the context exceeds the model's window less its reserve.
// ok is false when the model declares no window.
func (s *Session) CompactAt() (tokens int, ok bool) {
	u := s.inner.ContextUsage()
	if u == nil {
		return 0, false
	}
	reserve, err := s.inner.SettingsManager().GetCompactionReserveTokens(s.inner.Model())
	if err != nil {
		return 0, false
	}
	return u.ContextWindow - reserve, true
}

// Effort reports the reasoning level the session runs at, in the names
// ParseEffort takes. It is "" when the model does not reason.
func (s *Session) Effort() string {
	if !reasons(s.inner.AvailableThinkingLevels()) {
		return ""
	}
	return string(s.inner.ThinkingLevel())
}

// SetEffort changes the level the session runs at, from the next prompt, for
// this session only. e is not DefaultEffort. PiG clamps a level the model lacks
// to the nearest it has; Effort reports the level in effect.
func (s *Session) SetEffort(e Effort) error {
	s.effortMu.Lock()
	defer s.effortMu.Unlock()
	s.raised = DefaultEffort
	s.effortRevision++
	s.asked = e.thinkingLevel()
	if err := s.inner.SetThinkingLevel(e.thinkingLevel()); err != nil {
		return fmt.Errorf("failed to set effort: %w", err)
	}
	return nil
}

// setModel switches the model, then asks again for the level the session was
// given, which the old model may have clamped; with nothing asked the level is
// the default PiG applies on a switch. PiG sets the agent's model before it
// appends the change and then the level to the session file (PiG v0.7.0
// coding/session.go beginModelChangeState; recheck on a bump), so a failed
// append of either leaves the session on model: that error wraps ErrModelNotSaved. A
// failure that left the model as it was is returned as it is.
func (s *Session) setModel(model *ai.Model) error {
	s.effortMu.Lock()
	defer s.effortMu.Unlock()
	was := s.inner.Model()
	err := s.inner.SetModel(model)
	if err != nil {
		now := s.inner.Model()
		if sameModel(was, model) || !sameModel(now, model) {
			return err
		}
		err = fmt.Errorf("%w: %w", ErrModelNotSaved, err)
	}
	if lerr := s.inner.SetThinkingLevel(cmp.Or(s.asked, MediumEffort.thinkingLevel())); lerr != nil {
		err = errors.Join(err, fmt.Errorf("failed to restore the effort after the model change: %w: %w", ErrModelNotSaved, lerr))
	}
	return err
}

func sameModel(a, b *ai.Model) bool {
	return a.ProviderMeta.ProviderID == b.ProviderMeta.ProviderID && a.ID == b.ID
}

// raise lifts the level to e for the rest of the Send, remembering the level
// to restore. PiG re-reads the level before every provider request, so a raise
// between turns reaches the next request. Every level change is a
// thinking_level_change entry in PiG's session file, so the restore is what a
// resume reads; a crash mid-run leaves the raised level saved.
func (s *Session) raise(e Effort, continuation *EffortContinuation) {
	s.effortMu.Lock()
	defer s.effortMu.Unlock()
	if e == DefaultEffort && continuation != nil && continuation.revision == s.effortRevision {
		e = continuation.effort
	}
	if e == DefaultEffort || s.raised != DefaultEffort || !reasons(s.inner.AvailableThinkingLevels()) {
		return
	}
	prev := s.inner.ThinkingLevel()
	// PiG sets the agent's level before it records the change, so a failed call
	// may still have raised it; the raise is kept so restoreEffort puts it back.
	if err := s.inner.SetThinkingLevel(e.thinkingLevel()); err != nil {
		slog.Warn("failed to raise effort", "error", err)
	}
	s.raised, s.restore = e, prev
	if s.inner.ThinkingLevel() != prev {
		s.notify(event.EffortChanged{})
	}
}

// restoreEffort puts back the level raise replaced, unless SetEffort ran since.
func (s *Session) restoreEffort() {
	s.effortMu.Lock()
	defer s.effortMu.Unlock()
	if s.raised == DefaultEffort {
		return
	}
	s.raised = DefaultEffort
	raisedTo := s.inner.ThinkingLevel()
	// PiG sets the agent's level before it records the change, so a failed call
	// still restores the level in memory.
	if err := s.inner.SetThinkingLevel(s.restore); err != nil {
		slog.Warn("failed to restore effort", "error", err)
	}
	if s.inner.ThinkingLevel() != raisedTo {
		s.notify(event.EffortChanged{})
	}
}

// Efforts lists the levels the model supports, in the names ParseEffort takes.
// It is nil when the model does not reason.
func (s *Session) Efforts() []string {
	levels := s.inner.AvailableThinkingLevels()
	if !reasons(levels) {
		return nil
	}
	var out []string
	for _, l := range levels {
		out = append(out, string(l))
	}
	return out
}

func reasons(levels []ai.ModelThinkingLevel) bool {
	return slices.ContainsFunc(levels, func(l ai.ModelThinkingLevel) bool { return l != ai.ThinkingOff })
}

// Usage reports the tool calls the session has made and the cost of its
// requests in dollars, computed from the model's price.
func (s *Session) Usage() (toolCalls int, cost float64) {
	st := s.inner.GetSessionStats()
	return st.ToolCalls, st.Cost
}

// Subscribe registers fn to run synchronously on PiG's event executor, and for
// event.ToolsChanged on the goroutine that changed the active tools, so fn may
// run concurrently with executor events. A slow fn stalls the turn, and fn must
// not call back into the Session. Every call for a turn has run when Send
// returns.
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
	// Sent reports that p was delivered or, when p is empty, the first notice
	// was; it is false when ctx was already done or PiG rejected the input.
	Sent bool
	// Undelivered are the handed-over prompts the model never read, as given.
	Undelivered []Prompt
	// UndeliveredNotices are the notices, given or handed over, the model
	// never read, in delivery order.
	UndeliveredNotices []Notice
	// ModeUndelivered are the handed-over prompts whose mode line failed to
	// reach the model, as given; the caller must offer it again.
	ModeUndelivered []Prompt
	// Continuation keeps this send's raise for a continuation of the turn.
	// SetEffort invalidates it, including after Send returns. A pre-cancelled
	// send passes its prompt's continuation through without applying it.
	Continuation *EffortContinuation
}

type (
	turnInterruptKey struct{}
	turnSessionKey   struct{}
)

type turnInterrupt struct {
	mu    sync.Mutex
	abort func()
}

func (i *turnInterrupt) interrupt() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.abort == nil {
		return false
	}
	i.abort()
	return true
}

func (i *turnInterrupt) retire() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.abort = nil
}

// InterruptTurn synchronously requests interruption of the Send that supplied
// ctx to a tool or approval hook. It returns false outside a Send or after
// that Send retires; a retained context cannot interrupt a later turn.
func InterruptTurn(ctx context.Context) bool {
	i, ok := ctx.Value(turnInterruptKey{}).(*turnInterrupt)
	return ok && i.interrupt()
}

// Send runs one turn. Cancelling ctx is the abort request for this turn
// only: a ctx already done returns Interrupted without calling the provider,
// and a turn Kopi aborted reports Interrupted unless its last assistant message
// stopped normally (PiG reports an abort during a tool as StopReason "error"
// with "context canceled"). Model failures are not
// errors: they surface as a Failed outcome carrying the provider's error text.
// An overlapping call returns ErrSendInProgress unless ctx is already done.
//
// The model reads p's skills, then its mode line, then the notices, after p; when p is empty, the
// notices alone start the run.
//
// At the end of each turn that did not fail and was not aborted, while ctx is
// not cancelled, Send calls more, if not nil, for entries to hand over; the
// model reads them in the order given, each prompt followed by its skills, at
// the next boundary of the same run. more is called on PiG's event executor,
// with no Session lock held. Each handed-over prompt becomes a UserMessage when
// it is delivered; p does not, as the caller draws it. Skills never become
// events. Every notice, given or handed over, becomes its Shown when it is
// delivered.
func (s *Session) Send(ctx context.Context, p Prompt, notices []Notice, more func() []Entry) (SendResult, error) {
	if ctx.Err() != nil {
		return SendResult{Outcome: event.Outcome{Status: event.Interrupted}, UndeliveredNotices: notices, Continuation: p.Continuation}, nil
	}
	if !s.sendInProgress.CompareAndSwap(false, true) {
		return SendResult{UndeliveredNotices: notices}, fmt.Errorf("failed to send prompt: %w", ErrSendInProgress)
	}
	defer s.sendInProgress.Store(false)
	var aborted atomic.Bool
	interrupt := &turnInterrupt{abort: func() {
		aborted.Store(true)
		s.inner.RequestAbort()
	}}
	// Retirement is serialized with interruption and precedes releasing send
	// ownership, so a hook retaining ctx cannot abort the next Send.
	defer interrupt.retire()
	toolCtx := context.WithValue(context.WithValue(ctx, turnInterruptKey{}, interrupt), turnSessionKey{}, s)
	// PiG gets an uncancellable ctx: a cancelled one makes it return
	// ctx.Err() instead of the turn, so RequestAbort is the only abort path.
	pigCtx := context.WithoutCancel(toolCtx)
	abort := func() { interrupt.interrupt() }
	abortInLoop := abortOnEvent(ctx, abort)
	// handed is the handed-over prompts not yet delivered. Send is the only
	// producer into PiG's queue and PiG delivers in queue order, after p, so
	// each user message after p's is the head.
	var handed []Prompt
	var modeUndelivered []Prompt
	// pending is the notices not yet delivered. PiG delivers them in the
	// order Send gives them, so each notice message is the head.
	pending := slices.Clone(notices)
	// failed is set once a notice fails to queue, for the rest of the run.
	failed := false
	var sent bool
	// last is the run's last assistant message. SendContent returns PiG's
	// whole transcript, so it cannot say which assistant message is this run's.
	var last *agent.AssistantMessage
	unsubscribe := s.inner.Subscribe(func(ev agent.AgentEvent) {
		abortInLoop(ev)
		switch ev := ev.(type) {
		case agent.MessageStartEvent:
			if isNotice(ev.Message) {
				sent = true
				if len(pending) > 0 {
					s.notify(pending[0].Shown)
					pending = pending[1:]
				}
				return
			}
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
			// Prompts and notices share PiG's steering queue, so one pass in the
			// order given keeps that order. A notice's TriggerTurn stays nil, so
			// on an idle session it is only appended and never starts a run on
			// the event executor. After a notice fails to queue every later entry
			// of the run is only recorded as undelivered, so none overtakes it.
			for _, e := range more() {
				if n := e.Notice; n != nil {
					pending = append(pending, *n)
					if failed {
						continue
					}
					if err := s.inner.SendCustomMessage(pigCtx, n.message(), &extension.SendMessageOptions{DeliverAs: extension.DeliverAsSteer}); err != nil {
						slog.Warn("failed to hand over notice", "notice", n.label(), "error", err)
						failed = true
					}
					continue
				}
				q := e.Prompt
				handed = append(handed, q)
				if failed {
					continue
				}
				text, images := s.steerContent(q)
				s.loadTools(q.Tools)
				s.raise(q.Effort, nil)
				s.inner.QueueSteer(text, images)
				for _, sk := range q.Skills {
					if err := s.inner.SendCustomMessage(pigCtx, skillMessage(sk), &extension.SendMessageOptions{DeliverAs: extension.DeliverAsSteer}); err != nil {
						slog.Warn("failed to hand over skill", "error", err)
						break
					}
				}
				if q.Mode != "" {
					if err := s.inner.SendCustomMessage(pigCtx, modeMessage(q.Mode), &extension.SendMessageOptions{DeliverAs: extension.DeliverAsSteer}); err != nil {
						slog.Warn("failed to hand over mode line", "error", err)
						modeUndelivered = append(modeUndelivered, q)
					}
				}
			}
		}
	})
	defer unsubscribe()
	abortDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(abortDone)
		abort()
	})
	s.loadTools(p.Tools)
	defer s.restoreEffort()
	s.raise(p.Effort, p.Continuation)
	err := s.deliver(pigCtx, p, notices)
	// An abort already under way must finish before Send returns, or its
	// RequestAbort could land on the following turn.
	if !stop() {
		<-abortDone
	}
	// What PiG has not drained must not reach the next run. ClearQueue drops
	// only queued user messages; Send is the only producer of the rest.
	_, _ = s.inner.ClearQueue()
	s.inner.Agent().ClearSteeringQueue()
	s.inner.Agent().TakePendingNextTurn()
	res := SendResult{Sent: sent, Undelivered: handed, UndeliveredNotices: pending, ModeUndelivered: modeUndelivered}
	s.effortMu.Lock()
	if s.raised != DefaultEffort {
		res.Continuation = &EffortContinuation{revision: s.effortRevision, effort: s.raised}
	}
	s.effortMu.Unlock()
	if err != nil {
		return res, err
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
		res.Outcome = event.Outcome{Status: event.Failed, Message: pigTexts.Replace(last.ErrorMessage)}
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

// RecordNotices adds notices to the session, in order, with no request to the model;
// each one's Shown is sent to the listeners once its message is in. It stops at
// the first failure, and the listeners have seen exactly the notices recorded.
// It excludes Send.
//
// This rests on PiG's SendCustomMessage for a session that is not streaming:
// with no DeliverAs and no TriggerTurn the message is appended at once and its
// message start and end events are emitted before the call returns; no run
// starts.
func (s *Session) RecordNotices(notices []Notice) error {
	if !s.sendInProgress.CompareAndSwap(false, true) {
		return fmt.Errorf("failed to record notices: %w", ErrSendInProgress)
	}
	defer s.sendInProgress.Store(false)
	for _, n := range notices {
		if err := s.inner.SendCustomMessage(context.Background(), n.message(), nil); err != nil {
			return fmt.Errorf("failed to record notice: %w", reword(err))
		}
		s.notify(n.Shown)
	}
	return nil
}

// deliver runs p with notices queued as next-turn context after it, or, when p
// is empty, a run started by the first notice with the rest after it. Either
// call blocks until the run settles.
func (s *Session) deliver(ctx context.Context, p Prompt, notices []Notice) error {
	wake := p.Text == "" && len(p.Images) == 0 && len(notices) > 0
	queued := notices
	if wake {
		queued = notices[1:]
	}
	if !wake {
		for _, sk := range p.Skills {
			if err := s.inner.SendCustomMessage(ctx, skillMessage(sk), &extension.SendMessageOptions{DeliverAs: extension.DeliverAsNextTurn}); err != nil {
				return fmt.Errorf("failed to send skill: %w", reword(err))
			}
		}
		if p.Mode != "" {
			if err := s.inner.SendCustomMessage(ctx, modeMessage(p.Mode), &extension.SendMessageOptions{DeliverAs: extension.DeliverAsNextTurn}); err != nil {
				return fmt.Errorf("failed to send mode line: %w", reword(err))
			}
		}
	}
	for _, n := range queued {
		if err := s.inner.SendCustomMessage(ctx, n.message(), &extension.SendMessageOptions{DeliverAs: extension.DeliverAsNextTurn}); err != nil {
			return fmt.Errorf("failed to send notice: %w", reword(err))
		}
	}
	if wake {
		trigger := true
		if err := s.inner.SendCustomMessage(ctx, notices[0].message(), &extension.SendMessageOptions{TriggerTurn: &trigger}); err != nil {
			return fmt.Errorf("failed to send notice: %w", reword(err))
		}
		return nil
	}
	if _, err := s.inner.SendContent(ctx, coding.BuildUserContent(p.Text, toAI(p.Images))); err != nil {
		return fmt.Errorf("failed to send prompt: %w", reword(err))
	}
	return nil
}

// notify sends e to every listener, in the order they subscribed.
func (s *Session) notify(e event.Event) {
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	for _, l := range s.listeners {
		l.fn(e)
	}
}

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
			notes = append(notes, tool.OmittedImageNote)
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

// abortOnEvent re-issues the abort at every PiG event while ctx is cancelled.
// The AfterFunc abort runs on its own goroutine and can land after PiG sends
// the next provider request; PiG's loop waits on the listeners of its own
// events, so no provider request starts after the next loop event. PiG drops
// an abort requested before the run is marked busy (RequestAbort only records
// it while a run is active) and clears pending requests when the run starts;
// agent_start is emitted after that clear and before the first provider
// request, so aborting there stops the run as PiG's own agent_start check
// does.
func abortOnEvent(ctx context.Context, abort func()) func(agent.AgentEvent) {
	return func(agent.AgentEvent) {
		if ctx.Err() != nil {
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
