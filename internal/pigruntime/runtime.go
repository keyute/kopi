package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/pkg/browser"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// pigSettings is unmarshalled rather than built because the nested settings
// types live in a PiG internal package and cannot be named here. Install
// telemetry off is what stops PiG attributing requests to itself. The default
// thinking level is PiG's own at open; set, it is also what PiG's
// thinkingLevelForModelSwitch picks, which otherwise carries over the level
// the old model clamped (PiG v0.7.0; recheck on a bump). Session.setModel
// applies the same default when PiG returned before setting a level.
const pigSettings = `{"retry":{"enabled":true},"compaction":{"enabled":true},"cacheWarming":"off","defaultThinkingLevel":"medium","steeringMode":"all","enableInstallTelemetry":false}`

type Options struct {
	// Home is Kopi's home; PiG's own home is pinned beneath it.
	Home, AgentDir, CWD string
	// catalogURL is the remote model catalog's base URL; empty selects PiG's
	// default host. Tests point it at a local server.
	catalogURL string
}

type Runtime struct {
	svcs             *coding.AgentSessionServices
	rt               *coding.Runtime
	installationFile string
	refreshMu        sync.Mutex
	modelsPath       string
	// refreshed holds the outcome of each provider's catalog fetch; a
	// present key means it ran. fetching holds a channel closed when the
	// provider's fetch in flight ends.
	refreshed map[string]error
	fetching  map[string]chan struct{}
	// used holds the providers whose models this process resolved. stopRefresh
	// is set once StartModelListRefresh has run.
	used        map[string]struct{}
	stopRefresh context.CancelFunc
	// refreshCtx lives until stopRefresh; refreshLateLocked derives from it.
	// refreshes counts the startup refresh and the refreshes of providers first
	// used after start.
	refreshCtx context.Context
	refreshes  sync.WaitGroup
	// openURL opens a sign-in URL in the user's browser.
	openURL func(string) error
	// limitsClient sends the plan-limit requests.
	limitsClient *http.Client
}

// New builds a PiG runtime whose every input is explicit: in-memory settings,
// untrusted project, and (in Open) no resources, no built-in tools. Leaving
// any of these nil makes PiG read ~/.pig, project settings or AGENTS.md.
// New also pins PiG's environment process-wide (see isolate) and creates the
// agent dir; Home and AgentDir must be absolute.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if err := isolate(opts.Home, opts.AgentDir); err != nil {
		return nil, err
	}
	var s coding.Settings
	if err := json.Unmarshal([]byte(pigSettings), &s); err != nil {
		return nil, fmt.Errorf("failed to decode pig settings: %w", err)
	}
	// RefreshOnCreate and AllowModelNetwork stay unset: creating the runtime
	// only restores the stored catalog and never reaches the network.
	modelsPath := filepath.Join(opts.AgentDir, "models.json")
	// CreateModelRuntime always calls PiG's internal AgentDir(), which reads
	// PIG_CODING_AGENT_DIR and PIG_HOME and panics when the user home cannot be
	// resolved, even though every path is passed explicitly.
	models, err := coding.CreateModelRuntime(ctx, coding.CreateModelRuntimeOptions{
		AuthPath:       filepath.Join(opts.AgentDir, "auth.json"),
		ModelsPath:     new(&modelsPath),
		CatalogBaseURL: opts.catalogURL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create pig model runtime: %w", err)
	}
	svcs, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{
		CWD:             opts.CWD,
		AgentDir:        opts.AgentDir,
		ProjectTrusted:  new(false),
		SettingsManager: coding.NewInMemorySettingsManager(s),
		ModelRuntime:    models,
	})
	if err != nil {
		models.Close()
		return nil, fmt.Errorf("failed to create pig services: %w", err)
	}
	rt, err := coding.NewRuntime(coding.RuntimeOptions{Services: svcs})
	if err != nil {
		svcs.Close()
		models.Close()
		return nil, fmt.Errorf("failed to create pig runtime: %w", err)
	}
	// The browser's own output would land in the terminal mid-prompt.
	browser.Stdout, browser.Stderr = io.Discard, io.Discard
	return &Runtime{
		svcs:             svcs,
		rt:               rt,
		installationFile: filepath.Join(opts.Home, "installation_id"),
		openURL:          browser.OpenURL,
		limitsClient:     &http.Client{},
		modelsPath:       modelsPath,
		refreshed:        map[string]error{},
		fetching:         map[string]chan struct{}{},
		used:             map[string]struct{}{},
	}, nil
}

func (r *Runtime) RegisterProvider(p *ai.ModelsProvider) error {
	if err := r.svcs.ModelRuntime().RegisterNativeProvider(p); err != nil {
		return fmt.Errorf("failed to register provider: %w", err)
	}
	return nil
}

type SessionInfo struct {
	ID           string
	Modified     time.Time
	FirstMessage string
}

// Sessions lists the sessions of the runtime's workspace, newest first.
func (r *Runtime) Sessions() ([]SessionInfo, error) {
	infos, err := r.rt.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	out := make([]SessionInfo, 0, len(infos))
	for _, info := range infos {
		out = append(out, SessionInfo{ID: info.ID, Modified: info.Modified, FirstMessage: info.FirstMessage})
	}
	return out, nil
}

type OpenOptions struct {
	// Model is "provider/id". It is used for a resumed session too, whatever
	// model the session was saved with.
	Model string
	// ResumeID resumes the session with exactly this id.
	ResumeID string
	// Continue resumes the most recent session of the workspace.
	Continue bool
	// Effort is how much the model reasons; DefaultEffort is medium, clamped to the model.
	Effort Effort
	// Parent is the session this one works for; the transcript is saved beside the parent's, where Sessions does not list it.
	Parent       *Session
	SystemPrompt []PromptPart
	Tools        []tool.Tool
	Authority    *policy.Authority
	// Rules nil means no rules.
	Rules *policy.Rules
	// Approve nil means nobody can answer, so a call needing approval is denied.
	Approve policy.Approver
	// Review nil means a call needing approval goes straight to Approve.
	Review policy.Reviewer
}

// PromptPart is one named piece of the system prompt. Parts are sent in
// order, separated by a blank line. Resumes record changed parts by name,
// with suffix removals and additions when needed to restore that order.
type PromptPart struct{ Name, Text string }

func (r *Runtime) Open(ctx context.Context, o OpenOptions) (*Session, error) {
	return r.open(ctx, o, policy.NewGate(o.Authority, tool.Specs(o.Tools), o.Rules, o.Approve, o.Review))
}

// Check reports, without creating a session, the failures Open would report
// that do not depend on o's tools.
func (r *Runtime) Check(ctx context.Context, o OpenOptions) error {
	_, err := r.check(ctx, o)
	return err
}

// check returns the model o names once every tool-independent precondition
// of open holds.
func (r *Runtime) check(ctx context.Context, o OpenOptions) (*ai.Model, error) {
	if o.ResumeID != "" && o.Continue {
		return nil, errors.New("failed to open session: resume id and continue are mutually exclusive")
	}
	if o.Parent != nil && (o.ResumeID != "" || o.Continue) {
		return nil, fmt.Errorf("failed to open session: %w", ErrParentResume)
	}
	model, err := r.usableModel(ctx, o.Model)
	switch {
	case errors.Is(err, ErrUnknownModel):
		return nil, fmt.Errorf("failed to open session with model %q: %w", o.Model, err)
	case errors.Is(err, ErrNoCredential):
		return nil, fmt.Errorf("failed to open session: %w", err)
	case err != nil:
		return nil, err
	}
	if o.ResumeID == "" && !o.Continue {
		return model, nil
	}
	// The listing is checked here because PiG reports a missing session only
	// as unwrapped error text.
	infos, err := r.Sessions()
	if err != nil {
		return nil, fmt.Errorf("failed to open session: %w", err)
	}
	switch {
	case o.ResumeID != "" && !slices.ContainsFunc(infos, func(info SessionInfo) bool { return info.ID == o.ResumeID }):
		return nil, fmt.Errorf("failed to open session: session %q: %w", o.ResumeID, ErrSessionNotFound)
	case o.Continue && len(infos) == 0:
		return nil, fmt.Errorf("failed to open session: no session to continue: %w", ErrSessionNotFound)
	}
	return model, nil
}

// open takes the gate separately so a test can install one that does not
// know every tool.
func (r *Runtime) open(ctx context.Context, o OpenOptions, gate *policy.Gate) (*Session, error) {
	model, err := r.check(ctx, o)
	if err != nil {
		return nil, err
	}
	tools := make([]agent.AgentTool, 0, len(o.Tools))
	names := make([]string, 0, len(o.Tools))
	allowed := make(map[string]struct{}, len(o.Tools))
	for _, t := range o.Tools {
		tools = append(tools, agentTool{tool: t, gate: gate})
		if !t.Deferred {
			names = append(names, t.Name)
		}
		allowed[t.Name] = struct{}{}
	}
	// Sections must be non-nil even when empty: nil with no SystemPrompt
	// selects PiG's stock prompt.
	sections := make(ai.OrderedSections, 0, len(o.SystemPrompt))
	for _, p := range o.SystemPrompt {
		sections = append(sections, ai.PromptSection{Name: p.Name, Value: &p.Text})
	}
	// InitialActiveToolNames must be set too: nil falls back to PiG's
	// read/bash/edit/write defaults. An explicit Model also stops a resumed
	// session from restoring the model it was saved with.
	start := coding.SessionStartOptions{
		Model:                  model,
		SystemPromptSections:   sections,
		ResourceLoader:         coding.NoResources,
		SkipBuiltinTools:       true,
		ExtraTools:             tools,
		AllowedTools:           allowed,
		InitialActiveToolNames: names,
		BeforeToolCall:         []agent.BeforeToolCallHook{beforeToolCall(gate)},
		ThinkingLevel:          o.Effort.thinkingLevel(),
	}
	if o.Parent != nil {
		// PiG lists, resumes and continues only the *.jsonl files directly in a
		// session directory, so a transcript in a subdirectory is never listed,
		// and PiG's public API cannot record a parent on a new session: the
		// directory is the link.
		parentFile := o.Parent.inner.Path()
		start.SessionDir = filepath.Join(filepath.Dir(parentFile), o.Parent.ID())
	}
	var sess *coding.Session
	switch {
	case o.Parent != nil:
		sess, err = r.newChild(start)
	case o.ResumeID != "":
		sess, err = r.rt.Resume(o.ResumeID, start)
	case o.Continue:
		sess, err = r.rt.Continue(start)
	default:
		sess, err = r.rt.New(start)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open session: %w", reword(err))
	}
	// With SkipBuiltinTools PiG activates every ExtraTool whatever
	// InitialActiveToolNames says, and a resumed session restores the tools
	// its transcript last declared, so deferred tools are deactivated here.
	sess.SetActiveToolsByName(names)
	if o.ResumeID != "" || o.Continue {
		a := sess.Agent()
		previous := a.PreparePromptHook()
		a.SetPreparePrompt(func(ctx context.Context, messages []agent.AgentMessage) ([]agent.AgentMessage, error) {
			if previous != nil {
				var err error
				messages, err = previous(ctx, messages)
				if err != nil {
					return nil, err
				}
			}
			history := a.MessagesSnapshot()
			current := ai.GetCurrentSystemMessage(history)
			var committed ai.OrderedSections
			if current != nil {
				committed = current.Sections
			}
			// Prepared repairs can be abandoned before the run commits them.
			// Retire only after the transcript itself has the opening order.
			if slices.EqualFunc(committed, sections, func(a, b ai.PromptSection) bool { return a.Name == b.Name }) {
				a.SetPreparePrompt(previous)
				return messages, nil
			}
			current = ai.GetCurrentSystemMessage(slices.Concat(history, messages))
			var replayed ai.OrderedSections
			if current != nil {
				replayed = current.Sections
			}
			prefix := 0
			for prefix < len(replayed) && prefix < len(sections) && replayed[prefix].Name == sections[prefix].Name {
				prefix++
			}
			if prefix == len(replayed) && prefix == len(sections) {
				return messages, nil
			}
			// PiG updates names in place and appends additions. Remove the whole
			// mismatched suffix first so replay can append it in the desired order.
			removed := make(ai.OrderedSections, 0, len(replayed)-prefix)
			for _, section := range replayed[prefix:] {
				removed = append(removed, ai.PromptSection{Name: section.Name})
			}
			repair := []agent.AgentMessage{
				{System: &ai.SystemMessage{Content: ai.SystemText(""), Sections: removed, Timestamp: time.Now().UnixMilli()}},
				{System: &ai.SystemMessage{Content: ai.SystemText(""), Sections: slices.Clone(sections[prefix:]), Timestamp: time.Now().UnixMilli()}},
			}
			// Preserve PiG's prepared system updates ahead of the repair and the
			// user's starting messages after it, without changing either slice.
			at := 0
			for at < len(messages) && messages[at].System != nil {
				at++
			}
			return slices.Concat(messages[:at], repair, messages[at:]), nil
		})
	}
	s := &Session{inner: sess}
	// On resume asked is PiG's restored level, already clamped to the model the
	// session is reopened on, so a more capable model later keeps that level.
	switch {
	case o.Effort != DefaultEffort:
		s.asked = o.Effort.thinkingLevel()
	case (o.ResumeID != "" || o.Continue) && reasons(sess.AvailableThinkingLevels()):
		s.asked = sess.ThinkingLevel()
	}
	return s, nil
}

// DefaultModel returns the "provider/id" a session uses when none is
// configured. It fails wrapping ErrNoCredential when no provider has a
// credential.
//
// PiG chooses its default model only when a session is created without one,
// and keeps its list internal, so a throwaway in-memory session with no model,
// no tools and no resources is created to read it. It is never prompted and
// writes nothing; the runtime's own services stay open.
func (r *Runtime) DefaultModel(ctx context.Context) (string, error) {
	// PiG chooses from the availability it last published, which may predate
	// the credentials; a full pass first makes the choice follow them.
	if _, err := r.svcs.ModelRuntime().GetAvailable(ctx); err != nil {
		return "", fmt.Errorf("failed to choose a model: %w", reword(err))
	}
	sm, err := coding.NewInMemorySessionManager(r.svcs.CWD())
	if err != nil {
		return "", fmt.Errorf("failed to choose a model: %w", reword(err))
	}
	res, err := coding.CreateAgentSession(coding.CreateAgentSessionOptions{
		CWD:             r.svcs.CWD(),
		AgentDir:        r.svcs.AgentDir(),
		ModelRuntime:    r.svcs.ModelRuntime(),
		SettingsManager: r.svcs.SettingsManager(),
		ResourceLoader:  coding.NoResources,
		SessionManager:  sm,
		NoTools:         "all",
	})
	if err != nil {
		return "", fmt.Errorf("failed to choose a model: %w", reword(err))
	}
	defer func() {
		if err := res.Session.Close(); err != nil {
			slog.Warn("failed to close the session that reads the default model", "error", err)
		}
	}()
	// PiG reports a fallback message exactly when it chose no model, leaving
	// a placeholder model in the session.
	if res.ModelFallbackMessage != "" {
		return "", fmt.Errorf("failed to choose a model: %w for any provider", ErrNoCredential)
	}
	model := res.Session.Model()
	return model.ProviderMeta.ProviderID + "/" + model.ID, nil
}

// Model is an available model in Kopi's terms.
type Model struct{ Ref, Name string }

// Models lists the models of the providers that have a credential, sorted by
// Ref.
func (r *Runtime) Models(ctx context.Context) ([]Model, error) {
	available, err := r.svcs.ModelRuntime().GetAvailable(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", reword(err))
	}
	out := make([]Model, 0, len(available))
	for _, m := range available {
		out = append(out, Model{Ref: m.ProviderMeta.ProviderID + "/" + m.ID, Name: m.DisplayName})
	}
	slices.SortFunc(out, func(a, b Model) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// SetModel switches s to the model ref names, keeping its history. ref is
// checked as Open checks it. On ErrModelNotSaved the model changed and the
// effort was applied; any other error leaves the model. PiG sets the agent's
// model before it appends the change to the session file, and returns on a
// failed append before it sets the level (PiG v0.7.0 coding/session.go
// beginModelChangeState; recheck on a bump).
func (r *Runtime) SetModel(ctx context.Context, s *Session, ref string) error {
	model, err := r.usableModel(ctx, ref)
	if err != nil {
		return fmt.Errorf("failed to change model to %q: %w", ref, err)
	}
	if err := s.setModel(model); err != nil {
		return fmt.Errorf("failed to change model to %q: %w", ref, reword(err))
	}
	return nil
}

// newChild starts a new session that has an extension runner of its own.
// Runtime.New would hand it the runtime's single extension runner, which every
// session started through the runtime rebinds in NewSession; that write races
// with the reads of other live sessions' tool calls, and leaves the runner
// bound to the newest session. NewSession with a nil Runner builds a private
// one, and the explicit NoResources keeps it from discovering resources.
func (r *Runtime) newChild(start coding.SessionStartOptions) (*coding.Session, error) {
	manager, err := coding.SessionManagerFor(r.svcs, start)
	if err != nil {
		return nil, err
	}
	return coding.NewSession(r.svcs, coding.SessionOptions{
		SessionManager:         manager,
		Model:                  start.Model,
		ThinkingLevel:          start.ThinkingLevel,
		SystemPromptSections:   start.SystemPromptSections,
		ResourceLoader:         start.ResourceLoader,
		Tools:                  start.ExtraTools,
		AllowedTools:           start.AllowedTools,
		InitialActiveToolNames: start.InitialActiveToolNames,
		SkipBuiltinTools:       start.SkipBuiltinTools,
		BeforeToolCall:         start.BeforeToolCall,
		Transport:              ai.Transport(r.svcs.Settings().Transport),
	})
}

// usableModel returns the model ref ("provider/id") names once its provider
// has a credential.
func (r *Runtime) usableModel(ctx context.Context, ref string) (*ai.Model, error) {
	provider, id, _ := strings.Cut(ref, "/")
	models := r.svcs.ModelRuntime()
	model := models.GetModel(provider, id)
	if model == nil && models.GetProvider(provider) == nil {
		return nil, r.unknownModel(nil)
	}
	ok, err := r.hasCredential(ctx, provider)
	if err != nil {
		return nil, err
	}
	if !ok {
		// A known provider without a credential reports that, since a sign-in
		// would let the fetch run; ErrUnknownModel marks the id as still
		// unverified.
		if model == nil {
			return nil, fmt.Errorf("provider %q: %w (%w)", provider, ErrNoCredential, ErrUnknownModel)
		}
		return nil, fmt.Errorf("provider %q: %w", provider, ErrNoCredential)
	}
	r.refreshMu.Lock()
	_, was := r.used[provider]
	r.used[provider] = struct{}{}
	if !was && model != nil {
		r.refreshLateLocked(provider)
	}
	r.refreshMu.Unlock()
	if model != nil {
		return model, nil
	}
	refreshErr := r.refreshModels(ctx, provider)
	if refreshErr != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("failed to check model %q: %w", ref, ctx.Err())
	}
	if model = models.GetModel(provider, id); model == nil {
		return nil, r.unknownModel(refreshErr)
	}
	return model, nil
}

// unknownModel points at models.json, where the user can declare the model;
// refreshErr, when set, is the failure to refresh the remote list.
func (r *Runtime) unknownModel(refreshErr error) error {
	if refreshErr != nil {
		return fmt.Errorf("%w: %w; declare the model in %s", ErrUnknownModel, refreshErr, r.modelsPath)
	}
	return fmt.Errorf("%w: declare the model in %s", ErrUnknownModel, r.modelsPath)
}

// hasCredential relies on PiG's CheckAuth returning (nil, nil) when the
// provider has no usable credential.
func (r *Runtime) hasCredential(ctx context.Context, provider string) (bool, error) {
	check, err := r.svcs.ModelRuntime().CheckAuth(ctx, provider)
	if err != nil {
		return false, fmt.Errorf("failed to check credential for provider %q: %w", provider, reword(err))
	}
	return check != nil, nil
}

// Close must follow Close of every Session opened on r.
func (r *Runtime) Close() {
	// The refresh must have returned before the runtime it calls is closed; PiG's
	// close then joins any store write it left (PiG v0.7.0; recheck on a bump).
	r.refreshMu.Lock()
	if r.stopRefresh != nil {
		// Under refreshMu, so refreshLateLocked starts nothing after it.
		r.stopRefresh()
	}
	r.refreshMu.Unlock()
	r.refreshes.Wait()
	if err := r.rt.Close(); err != nil {
		slog.Warn("failed to close pig runtime", "error", err)
	}
	r.svcs.Close()
	// svcs does not close a model runtime it was handed.
	r.svcs.ModelRuntime().Close()
}
