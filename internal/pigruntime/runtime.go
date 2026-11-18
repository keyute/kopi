package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
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
// telemetry off is what stops PiG attributing requests to itself.
const pigSettings = `{"retry":{"enabled":true},"compaction":{"enabled":true},"cacheWarming":"off","steeringMode":"all","enableInstallTelemetry":false}`

type Options struct {
	// Home is Kopi's home; PiG's own home is pinned beneath it.
	Home, AgentDir, CWD string
}

type Runtime struct {
	svcs             *coding.AgentSessionServices
	rt               *coding.Runtime
	installationFile string
	// openURL opens a sign-in URL in the user's browser.
	openURL func(string) error
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
	svcs, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{
		CWD:                opts.CWD,
		AgentDir:           opts.AgentDir,
		ProjectTrusted:     new(false),
		SettingsManager:    coding.NewInMemorySettingsManager(s),
		ModelRuntimeSignal: ctx,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create pig services: %w", err)
	}
	rt, err := coding.NewRuntime(coding.RuntimeOptions{Services: svcs})
	if err != nil {
		svcs.Close()
		return nil, fmt.Errorf("failed to create pig runtime: %w", err)
	}
	// The browser's own output would land in the terminal mid-prompt.
	browser.Stdout, browser.Stderr = io.Discard, io.Discard
	return &Runtime{
		svcs:             svcs,
		rt:               rt,
		installationFile: filepath.Join(opts.Home, "installation_id"),
		openURL:          browser.OpenURL,
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
	Continue     bool
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
	}
	var sess *coding.Session
	switch {
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
	return &Session{inner: sess}, nil
}

// usableModel returns the model ref ("provider/id") names once its provider
// has a credential.
func (r *Runtime) usableModel(ctx context.Context, ref string) (*ai.Model, error) {
	provider, id, _ := strings.Cut(ref, "/")
	model := r.svcs.ModelRuntime().GetModel(provider, id)
	if model == nil {
		return nil, ErrUnknownModel
	}
	ok, err := r.hasCredential(ctx, provider)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("provider %q: %w", provider, ErrNoCredential)
	}
	return model, nil
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
	if err := r.rt.Close(); err != nil {
		slog.Warn("failed to close pig runtime", "error", err)
	}
	r.svcs.Close()
}
