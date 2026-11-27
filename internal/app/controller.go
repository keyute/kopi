// Package app is the controller that frontends drive: it opens and switches
// sessions, queues prompts and answers, and reports what happens as events.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/keyute/kopi/internal/classifier"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/history"
	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/skill"
	"github.com/keyute/kopi/internal/subagent"
	"github.com/keyute/kopi/internal/tool"
)

type Config struct {
	// Model is "provider/id"; empty means the runtime's default model of a
	// provider with a credential, chosen when the first session opens.
	Model string
	// SmallModel is "provider/id"; empty means the session model.
	SmallModel string
	// ConfigFile is the path the notice about a chosen model names.
	ConfigFile string
	Workspace  string
	// Tools are the built-in tools; the MCP servers' tools come from Servers.
	Tools []tool.Tool
	// JoinTimeout bounds how long a session switch or Close waits for running
	// work to stop.
	JoinTimeout time.Duration
	// History records human input; nil disables it.
	History *history.Store
	// Rules nil means no rules.
	Rules *policy.Rules
	// Unattended means no human can answer, so every call that would ask for
	// approval is denied.
	Unattended bool
	// Mode is the controller's initial mode, which the first session starts
	// in, and the mode every later session starts in.
	Mode policy.Mode
	// Effort is the level a new main session opens at; a resumed one keeps its own.
	Effort pigruntime.Effort
	// Files lists the entries that @ references complete to, and is the workspace
	// /add-dir adds to and completes in, so /add-dir needs it.
	Files *tool.Workspace
	// SandboxDirs sets the directories added to the workspace as the sandbox's
	// write roots; nil when there is no sandbox.
	SandboxDirs func(dirs []string) error
	// ValidateSandboxDirs checks prospective roots without changing the sandbox.
	ValidateSandboxDirs func(dirs []string) error
	// TrustFile is the trust store /add-dir reads and saves decisions in.
	TrustFile string
	// Home is the user's home: /add-dir expands ~ with its Given name and never
	// saves a decision for its Real one.
	Home tool.Dir
	// InstructionFallbacks are the file names read where AGENTS.md is missing,
	// in an added directory too.
	InstructionFallbacks []string
	// Instructions are added to the system prompt in order: the global
	// file's group first, then each root's.
	Instructions []instruction.Group
	// Errors are reported once as Error events, after the first session
	// opens, before those about the MCP servers.
	Errors []error
	// Servers are the MCP servers, which join the session as they settle; nil
	// means there are none.
	Servers Servers
	// Tasks is the background task manager the Bash tool shares; nil means one
	// no tool shares, so there are never tasks.
	Tasks Tasks
	// Wake lets a task's end start a turn on its own; otherwise its notice
	// waits for a request a human prompt starts.
	Wake bool
	// UserShell runs the commands the human types after "!"; nil means there are
	// none, and such input is a prompt.
	UserShell *tool.Shell
	// Skills are the loaded skills; users type one as /SkillPrefix:name, or
	// /name when SkillPrefix is empty.
	Skills      []skill.Skill
	SkillPrefix string
	// Classifier reviews the calls that would otherwise ask; nil means none
	// are reviewed.
	Classifier *classifier.Classifier
	// Agents are the subagents the model can start; none means there is no
	// Agent or SendMessage tool.
	Agents []subagent.Definition
	// AgentTools are the tools a subagent may have, besides the MCP servers'
	// tools; Agent, SendMessage, TaskStop, ToolSearch and Workflow are never
	// given to one.
	AgentTools []tool.Tool
	// Plans is the root session's plan file; nil means there is none, and no
	// ExitPlanMode tool.
	Plans *tool.Plans
	// Outputs saves the output too long to return whole in the root session's
	// folder; nil saves nothing.
	Outputs *tool.Outputs
	// WorkflowModel is "provider/id"; empty means the session model. A
	// definition's model comes first.
	WorkflowModel string
	// MaxWorkflows is how many workflows may run at once, and MaxAgents how
	// many agents of one workflow.
	MaxWorkflows, MaxAgents int
	// Limits reads the plan limits of the session model's provider on
	// activity, for Status.Limits. Without it they are read only once Usage has
	// been called.
	Limits bool
	// Title names a session that has no name from its first prompt, using the
	// small model.
	Title bool
}

// Servers are the MCP servers of a run, already starting.
type Servers interface {
	// Names lists the servers, and Connecting those that have not settled.
	Names() []string
	Connecting() []string
	// Watch calls fn with each server's status once it has settled, from one
	// goroutine.
	Watch(fn func(pigruntime.ServerStatus))
	// Wait returns once the servers have settled or have had the time a run
	// with one turn gives them, or when ctx is done.
	Wait(ctx context.Context) []pigruntime.ServerStatus
	// Ready returns once the servers have settled or have had the time a
	// prompt waits for them, or when ctx is done.
	Ready(ctx context.Context)
}

const maxReferenceEntries = 10000

// Main is the main session's id.
const Main = ""

// Controller holds all frontend-neutral behaviour: frontends pass input in
// and render the events they subscribe to.
//
// Lock order is life, then titleCommit, then queue, then mu. mu is never held
// while calling into PiG or delivering events; life is held across both and
// queue across delivering events, which is safe because subscribers must not
// call back into the Controller. A session's listeners run on PiG's event
// executor with neither queue nor mu held, and the one a runner wraps for its
// session (runner.reading) takes queue on the model reading a prompt; so nothing
// may call into a session or wait for its send with queue held, except
// Subscribe on a session that has not sent yet. Neither queue nor mu is held while calling Config.Tasks,
// whose callback takes both, except for starting an agent task and its
// AgentTask.Run and End, which a subagent's or a workflow's run makes with
// queue held, and Tasks.List, which Status calls with queue held. That is
// safe only because tool.Tasks calls nothing back while holding its own locks:
// OnChange and the stop function run without them, and the stop function takes
// queue. The mutex of limits is a leaf: nothing is called with it held.
type Controller struct {
	rt  *pigruntime.Runtime
	cfg Config
	// builtin are the tools every session opens with ahead of the MCP servers'
	// tools, and workflow the one after them; search is WebSearch. All are set
	// once. searchable reports that ToolSearch can load the servers' tools.
	builtin, workflow, search []tool.Tool
	searchable                bool
	// settled are the statuses of the MCP servers that have settled, sorted by
	// name; life guards it. joined is closed once every server is among them.
	settled []pigruntime.ServerStatus
	joined  chan struct{}
	// tools and prompt are what a session opens with now: builtin, the tools of
	// the settled servers, then workflow. agents are the subagents' tools and
	// prompts and workflowSpecs the workflow agents', the default one first.
	// serverTools are the servers' tools among tools. All five are written
	// with life and mu held, when a server settles.
	tools       []tool.Tool
	prompt      []pigruntime.PromptPart
	serverTools []tool.Tool
	// connecting are the configured servers not among settled; it is written
	// with serverTools, so a server is in one of them at every moment.
	connecting    []string
	agents        []agentSpec
	workflowSpecs []agentSpec
	// skills is what users can type; skillErrs reports the skills it left out.
	skills    *skill.Set
	skillErrs []error
	authority *policy.Authority
	relay     relay
	limits    *limits
	wg        sync.WaitGroup
	// titleCtx ends when Close starts, which stops a title request in flight.
	titleCtx  context.Context
	titleStop context.CancelFunc
	// sideSeq numbers the side questions; sideStop cancels the latest, nil when
	// none was asked or it was stopped. mu guards both.
	sideSeq  int
	sideStop context.CancelFunc
	// titleCommit serialises title persistence and announcement with opening
	// and installing a replacement, including a resume of the same session.
	// It is never held across wg.Wait: title workers need it to finish.
	titleCommit sync.Mutex
	// startup is what the next open that succeeds reports after its
	// SessionChanged, which clears the transcript: Config.Errors, and what the
	// servers that settled with no session open reported. life guards it.
	startup []event.Event
	// attempted is set once the first open has been tried; life guards it.
	attempted bool

	// life serialises session start, switch and close.
	life sync.Mutex
	// stopAllDone retains a task cleanup across timed-out Close calls; life guards it.
	stopAllDone chan struct{}
	// queue makes queueing input atomic with taking it from the queue and with
	// deciding to stop, continue or end the running Work; it guards the
	// runners' queued, notices and hold and the Work's queue fields. PiG calls
	// back under its own lock to hand over queued input, and on a session's
	// listeners when the model reads it, so nothing may call into PiG with queue
	// held.
	queue sync.Mutex
	// children are the subagents of the current session by task id, and live
	// counts the running ones; both are guarded by queue.
	children map[string]*runner
	live     int
	// turnSeq numbers the Works; queue guards it.
	turnSeq int
	// changed closes, and is replaced, whenever a turn starts or ends or a
	// subagent's run ends; queue guards it.
	changed chan struct{}
	// workflows are the runs of the current session by task id, running and
	// ended; queue guards the map and each run's ended.
	workflows map[string]*workflowRun
	// followers retain turns only for frontends that consume their results.
	followers map[*WorkQueue]struct{}

	mu sync.Mutex
	// spent is what the current root session has used outside its open sessions:
	// side requests and ended workflow agents.
	spent pigruntime.Usage
	// model is the session model: Config.Model, or the runtime's default once resolved.
	model  string
	sessID string
	// dirsToldBy is how many entries of dirs each session of this run has
	// read, so a session resumed later is not told them again; queue guards it.
	dirsToldBy map[string]int
	// titled is set once the current main session's title has been decided on,
	// whether or not it was asked for.
	titled bool
	// main drives the main session; its sess, emit, unsubscribe and work are
	// guarded by mu.
	main *runner
	// switching refuses prompts while a session transition is under way, so
	// no work can start on a session that is about to be replaced.
	switching *sessionSwitch
	// changingModel is set from the checks of a /model until its model has
	// changed. Main starts no turn, takes no prompt and begins no switch or
	// login meanwhile; subagents and workflow agents are untouched.
	changingModel bool
	// closed is set by Close; afterwards the Controller refuses all input,
	// leaves the mode alone and delivers no events.
	closed bool
	// approvals are the calls waiting for Answer, oldest first.
	approvals []*pendingApproval
	// question is the AskUserQuestion call waiting for AnswerQuestion; nil when none is.
	question *pendingQuestion
	// plan is the ExitPlanMode call waiting for AnswerPlan; nil when none is.
	plan *pendingPlan
	// login is the running login; nil when none is. A login and a turn or a
	// running subagent or workflow never run together: each is refused while the other is
	// registered. The login's questions and an approval would otherwise share
	// the one dialog the frontend has.
	login *loginRun
	// loginActivity is the Activity text of the running login; "" when it
	// shows none.
	loginActivity string
	// activity serialises showActivity; it is taken before mu and by nothing
	// else.
	activity sync.Mutex
	// requestSeq numbers the requests of every kind, so a token names one
	// request for the Controller's lifetime; requests holds the end of each
	// request still open, by token. Both are guarded by mu.
	requestSeq uint64
	requests   map[uint64]func()
	// deferred is the first open that Start put off for a login; cleared once
	// any session opens.
	deferred *OpenRequest
	// dirs is the log of adds and removals, in order, and dirRequests the adds
	// waiting for AnswerDir by token; mu guards both. Entries are only appended:
	// a removal is an entry of its own, and the directories now added are what
	// the log folds to. Neither is tied to a session, so a switch keeps them.
	dirs        []addedDir
	dirRequests map[uint64]*pendingDir
	// reports are the events of adds and removals waiting to be shown, in order; mu guards it.
	reports []event.Event
	// dirsMu serialises applying an add or a removal and takes mu inside.
	dirsMu sync.Mutex
	// dirEventsMu orders a request's DirRequested before its DirEnded; it takes
	// mu inside.
	dirEventsMu sync.Mutex
}

// sessionSwitch retains work even after it deregisters while start waits for life.
type sessionSwitch struct {
	work *Work
	emit func(event.Event)
}

func NewController(rt *pigruntime.Runtime, cfg Config) *Controller {
	cfg.Tools = slices.DeleteFunc(slices.Clone(cfg.Tools), func(t tool.Tool) bool { return cfg.Rules.Removes(t.Name) })
	if cfg.Tasks == nil {
		cfg.Tasks = tool.NewTasks()
	}
	c := &Controller{rt: rt, cfg: cfg, model: cfg.Model, authority: policy.NewAuthority()}
	c.titleCtx, c.titleStop = context.WithCancel(context.Background())
	c.main = &runner{c: c, role: mainRole{c}}
	var reserved []string
	for _, cmd := range commandTable() {
		reserved = append(reserved, cmd.Name)
	}
	var dropped []string
	c.skills, dropped = skill.NewSet(cfg.SkillPrefix, cfg.Skills, reserved)
	for _, n := range dropped {
		c.skillErrs = append(c.skillErrs, fmt.Errorf("skill %s cannot be typed: /%s is a control command", n, n))
	}
	if cfg.Mode == policy.Execute {
		c.authority.EnterExecute()
	}
	for _, err := range slices.Concat(cfg.Errors, c.skillErrs) {
		c.startup = append(c.startup, event.Error{Err: err})
	}
	c.setup()
	cfg.Tasks.OnChange(c.taskChanged)
	cfg.Tasks.OnForeground(func() { c.relay.emit(event.TasksChanged{}) })
	c.limits = newLimits(rt.Limits, c.modelProvider, c.relay.emit)
	if cfg.Limits {
		c.relay.subscribe(c.limits.observe)
	}
	if cfg.Servers != nil {
		c.joined = make(chan struct{})
		if len(cfg.Servers.Names()) == 0 {
			close(c.joined)
		}
		cfg.Servers.Watch(c.serverSettled)
	}
	return c
}

// setup sets the tools every session opens with besides the MCP servers'.
func (c *Controller) setup() {
	tools := slices.Clone(c.cfg.Tools)
	if ask := tool.NewAskUserQuestion(c.askQuestion); !c.cfg.Unattended && !c.cfg.Rules.Removes(ask.Name) {
		tools = append(tools, ask)
	}
	if c.planApproval() {
		tools = append(tools, tool.NewExitPlanMode(c.cfg.Plans, c.reviewPlan))
	}
	if !c.cfg.Rules.Removes("WebSearch") {
		c.search = []tool.Tool{tool.NewWebSearch(func(ctx context.Context, system, query string) (tool.SearchResult, error) {
			return c.sideSearch(ctx, c.SmallModel(), system, query)
		})}
		tools = append(tools, c.search...)
	}
	agentOffered := len(c.cfg.Agents) > 0 && !c.cfg.Rules.Removes("Agent")
	if agentOffered {
		launcher := agentLauncher{c}
		tools = append(tools, tool.NewAgent(c.cfg.Agents, launcher))
		if send := tool.NewSendMessage(launcher); !c.cfg.Rules.Removes(send.Name) {
			tools = append(tools, send)
		}
	}
	if c.cfg.Servers != nil {
		toolSearch := tool.NewToolSearch(c.serverToolsNow, c.connectingNow, pigruntime.RankTools, pigruntime.LoadTools)
		// ToolSearch is the only way to load deferred tools.
		if c.searchable = !c.cfg.Rules.Removes(toolSearch.Name); c.searchable {
			tools = append(tools, toolSearch)
		}
	}
	c.builtin = tools
	// Workflow never reaches a subagent's tools, which buildAgents builds from
	// search and the servers' tools.
	if c.workflowOffered() {
		c.workflow = []tool.Tool{tool.NewWorkflow(c.cfg.Agents, workflowLauncher{c}, c.cfg.MaxWorkflows, c.cfg.MaxAgents, agentOffered)}
	}
	c.rebuild()
}

// rebuild sets what a session opens with from the settled servers. life is
// held, or the Controller is not yet shared.
func (c *Controller) rebuild() {
	var serverTools []tool.Tool
	var servers []mcpServer
	var connecting []string
	if c.cfg.Servers != nil {
		connecting = slices.DeleteFunc(slices.Clone(c.cfg.Servers.Names()), func(name string) bool {
			_, ok := slices.BinarySearchFunc(c.settled, name, func(s pigruntime.ServerStatus, name string) int { return strings.Compare(s.Name, name) })
			return ok
		})
	}
	for _, st := range c.settled {
		if st.Err != nil {
			continue
		}
		server := mcpServer{name: st.Name, instructions: st.Instructions}
		for _, t := range c.offered(st) {
			serverTools = append(serverTools, t)
			server.tools = append(server.tools, t.Name)
		}
		servers = append(servers, server)
	}
	tools := slices.Concat(c.builtin, serverTools, c.workflow)
	prompt := buildPrompt(c.cfg.Workspace, tools, c.cfg.Instructions, servers, nil, false, c.cfg.Servers != nil, c.cfg.UserShell != nil, false)
	agents, workflowSpecs := c.buildAgents(c.search, serverTools, servers)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tools, c.prompt, c.serverTools, c.connecting, c.agents, c.workflowSpecs = tools, prompt, serverTools, connecting, agents, workflowSpecs
}

// offered are the tools of the settled server st that sessions get: those the
// rules leave in, and none when nothing can load them.
func (c *Controller) offered(st pigruntime.ServerStatus) []tool.Tool {
	if !c.searchable {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(st.Tools), func(t tool.Tool) bool { return c.cfg.Rules.Removes(t.Name) })
}

// serverToolsNow are the tools of the servers settled so far.
func (c *Controller) serverToolsNow() []tool.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverTools
}

// connectingNow are the servers not yet settled and joined, sorted.
func (c *Controller) connectingNow() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connecting
}

// emitOrQueue emits evs when a session is open and no switch is pending, and
// otherwise queues them for the next open that succeeds, since a pending
// switch's SessionChanged wipes what is emitted now. It must be called with life
// held.
func (c *Controller) emitOrQueue(evs []event.Event) {
	c.mu.Lock()
	open := c.main.sess != nil && c.switching == nil
	c.mu.Unlock()
	if !open {
		c.startup = append(c.startup, evs...)
		return
	}
	for _, ev := range evs {
		c.relay.emit(ev)
	}
}

// serverSettled gives the tools of st's server to the sessions opened from now
// on and to the open main session at once, and the servers part to the open
// main session once the last server has settled. A subagent or workflow agent
// keeps what it opened with.
func (c *Controller) serverSettled(st pigruntime.ServerStatus) {
	c.life.Lock()
	defer c.life.Unlock()
	closed := c.isClosed()
	c.mu.Lock()
	sess := c.main.sess
	c.mu.Unlock()
	// The session has the tools before ToolSearch can find them.
	if !closed && sess != nil && st.Err == nil {
		if err := sess.AddTools(c.offered(st)); err != nil {
			st = pigruntime.ServerStatus{Name: st.Name, Err: err}
		}
	}
	i, _ := slices.BinarySearchFunc(c.settled, st.Name, func(s pigruntime.ServerStatus, name string) int { return strings.Compare(s.Name, name) })
	c.settled = slices.Insert(c.settled, i, st)
	last := len(c.settled) == len(c.cfg.Servers.Names())
	if last {
		defer close(c.joined)
	}
	if closed {
		return
	}
	c.rebuild()
	var events []event.Event
	if st.Err != nil {
		events = append(events, event.Error{Err: fmt.Errorf("MCP server %s failed to start: %w", st.Name, st.Err)})
	}
	for _, skipped := range st.Skipped {
		events = append(events, event.Notice{Text: fmt.Sprintf("MCP server %s: skipped tool %s", st.Name, skipped)})
	}
	if sess != nil && last {
		// The part the session kept, if any, goes when no server has one.
		part := pigruntime.PromptPart{Name: serversPart}
		if p := c.prompt[len(c.prompt)-1]; p.Name == serversPart {
			part = p
		}
		sess.SetPromptPart(part)
	}
	c.emitOrQueue(events)
	if last {
		c.showActivity()
	}
}

// showActivity reports what the Controller is busy with outside a turn: a
// login, else the MCP servers still connecting.
func (c *Controller) showActivity() {
	// Held across the read and the emit, so the last report is of the latest state.
	c.activity.Lock()
	defer c.activity.Unlock()
	c.mu.Lock()
	text := c.loginActivity
	c.mu.Unlock()
	if text == "" && len(c.connectingNow()) > 0 {
		text = "Connecting to MCP servers…"
	}
	c.relay.emit(event.Activity{Text: text})
}

// awaitServers gives the servers still connecting the time a prompt waits for
// them, and those that have settled the time to join.
func (c *Controller) awaitServers(ctx context.Context) {
	if c.cfg.Servers == nil {
		return
	}
	c.cfg.Servers.Ready(ctx)
	if len(c.cfg.Servers.Connecting()) > 0 {
		return
	}
	select {
	case <-c.joined:
	case <-ctx.Done():
	}
}

// Subscribe registers fn for every event of the current session plus
// controller events. fn runs synchronously; it must not block and must not
// call back into the Controller.
func (c *Controller) Subscribe(fn func(event.Event)) func() {
	return c.relay.subscribe(fn)
}

type OpenRequest struct {
	ResumeID string
	Continue bool
}

// Model is the session model, empty until resolved when none is configured.
func (c *Controller) Model() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model
}

// SmallModel is the model for side questions: Config.SmallModel, else Model.
func (c *Controller) SmallModel() string {
	if c.cfg.SmallModel != "" {
		return c.cfg.SmallModel
	}
	return c.Model()
}

// resolveModel chooses the runtime's default model when none is configured and
// returns the notice naming it, again on a retry. It does nothing when a model
// is configured.
func (c *Controller) resolveModel(ctx context.Context) ([]event.Event, error) {
	if c.cfg.Model != "" {
		return nil, nil
	}
	ref := c.Model()
	if ref == "" {
		var err error
		if ref, err = c.rt.DefaultModel(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.model = ref
		c.mu.Unlock()
	}
	return []event.Event{event.Notice{Text: fmt.Sprintf("Using %s. Set \"model\" in %s to choose another.", ref, c.cfg.ConfigFile)}}, nil
}

// Open checks that req can be opened, waits for the MCP servers as long as a
// run with one turn does, opens the first session with the tools of those that
// settled, then emits each of Config.Errors and the events about the servers.
func (c *Controller) Open(ctx context.Context, req OpenRequest) error {
	notices, err := c.check(ctx, req)
	if err != nil {
		return err
	}
	if c.cfg.Servers != nil {
		c.cfg.Servers.Wait(ctx)
		select {
		case <-c.joined:
		case <-ctx.Done():
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrStartupInterrupted, err)
		}
	}
	return c.first(ctx, req, notices)
}

// Start is Open without the wait: the session opens at once, and each MCP
// server joins it when it settles.
//
// When the check fails only because the model's provider has no credential,
// an attended Start instead returns nil and runs that provider's login, as
// /login does; the open follows once it succeeds. With no model configured and
// no provider holding a credential, the login first asks which provider to log
// in to. If the login is cancelled or fails, no session opens and a later
// /login that succeeds retries the open.
func (c *Controller) Start(ctx context.Context, req OpenRequest) error {
	notices, err := c.check(ctx, req)
	if err != nil {
		if !c.canLoginFirst(ctx, err) {
			return err
		}
		if c.cfg.Servers != nil {
			c.showActivity()
		}
		return c.loginFirst(ctx, req, c.withLoginHint(err))
	}
	if c.cfg.Servers != nil {
		c.showActivity()
	}
	return c.first(ctx, req, notices)
}

// first opens the session Open or Start owes.
func (c *Controller) first(ctx context.Context, req OpenRequest, notices []event.Event) error {
	c.life.Lock()
	defer c.life.Unlock()
	op, err := c.beginSwitch(false)
	if err != nil {
		return err
	}
	return c.startFirst(ctx, req, op, notices)
}

// startFirst is startSwitch for the open Open, Start or a login owes; notices
// are reported after it opens, ahead of the other startup events. It runs with
// life held.
func (c *Controller) startFirst(ctx context.Context, req OpenRequest, op *sessionSwitch, notices []event.Event) error {
	kept := c.startup
	c.startup = slices.Concat(notices, kept)
	if err := c.startSwitch(ctx, req, op); err != nil {
		c.startup = kept
		return err
	}
	return nil
}

// check resolves the model, then checks that req can be opened; it returns the
// notices.
func (c *Controller) check(ctx context.Context, req OpenRequest) ([]event.Event, error) {
	notices, err := c.resolveModel(ctx)
	if err != nil {
		return nil, err
	}
	return notices, c.rt.Check(ctx, pigruntime.OpenOptions{Model: c.Model(), ResumeID: req.ResumeID, Continue: req.Continue})
}

func (c *Controller) NewSession(ctx context.Context) error {
	return c.start(ctx, OpenRequest{})
}

func (c *Controller) Resume(ctx context.Context, id string) error {
	return c.start(ctx, OpenRequest{ResumeID: id})
}

// switchTo starts the switch to req in the background; its failure arrives as
// an Error event. It marks the switch before returning, so input given
// meanwhile is refused rather than landing on the session being replaced. The
// goroutine is not in wg: Close holds life across wg.Wait, and start needs
// life. start refuses once closed, and Close shuts the relay before releasing
// life, so nothing reaches the runtime or a subscriber after Close.
func (c *Controller) switchTo(ctx context.Context, req OpenRequest) error {
	op, err := c.beginSwitch(false)
	if err != nil {
		return err
	}
	go func() {
		c.life.Lock()
		defer c.life.Unlock()
		c.finishSwitch(ctx, req, op)
	}()
	return nil
}

// finishSwitch runs the switch switchTo began, with life held. When it fails
// and the session it was to replace stays open, the events of servers that
// settled meanwhile are reported there.
func (c *Controller) finishSwitch(ctx context.Context, req OpenRequest, op *sessionSwitch) {
	err := c.startSwitch(ctx, req, op)
	if err == nil {
		return
	}
	c.relay.emit(event.Error{Err: err})
	c.mu.Lock()
	open := c.main.sess != nil
	c.mu.Unlock()
	if open {
		for _, ev := range c.startup {
			c.relay.emit(ev)
		}
		c.startup = nil
	}
}

func (c *Controller) Sessions() ([]pigruntime.SessionInfo, error) {
	return c.rt.Sessions()
}

// Submit takes one message of human input with the paths of its images for
// session, Main or a subagent's task id; an unknown one fails with an error
// wrapping tool.ErrUnknownTask. A control command always runs on the main
// session, whatever session is, and is executed and returns (nil, nil), ErrQuit for /quit,
// ErrShowTasks for /tasks, ErrShowUsage for /usage, ErrCopy for /copy, ErrMissingArgument for /resume, /model, /login or /logout
// without an argument, or ErrUnknownCommand. /new, /resume with an id, /model and
// /login with a provider return at once and report their result through
// events; while a login runs, /new and /resume return ErrLoggingIn.
// Anything else is a prompt: it starts a turn of session or, while one runs, is
// queued for that turn and returns (nil, nil); while a login runs it returns
// ErrLoggingIn. After Close every input but a
// blank line returns ErrNoSession.
func (c *Controller) Submit(ctx context.Context, session, text string, images ...string) (*Work, error) {
	return c.input(ctx, session, text, images, false)
}

// SendNow is Submit, except that while a turn of session runs it stops the turn's
// current send and continues the turn with everything queued plus text. With
// nothing queued and a blank text it does nothing.
func (c *Controller) SendNow(ctx context.Context, session, text string, images ...string) error {
	_, err := c.input(ctx, session, text, images, true)
	return err
}

// Prompt sends text verbatim as a prompt, even when it reads as a control
// command, for input that must never change mode. It records no history. Like
// Submit, it returns a nil Work when the input is queued behind a running turn.
func (c *Controller) Prompt(ctx context.Context, text string) (*Work, error) {
	p := pigruntime.Prompt{Text: text, Skills: c.skillTexts(text)}
	if len(keywordSpans(text, c.skills, ultrathinkWord)) > 0 {
		p.Effort = pigruntime.MaxEffort
	}
	return c.main.submit(ctx, p, false)
}

// ultrathinks reports whether the main session's model reasons, so the
// ultrathink keyword takes effect.
func (c *Controller) ultrathinks() bool {
	c.mu.Lock()
	sess := c.main.sess
	c.mu.Unlock()
	return sess != nil && sess.Effort() != ""
}

// skillTexts returns the texts of the skills text names, in first-seen order.
func (c *Controller) skillTexts(text string) []string {
	var out []string
	for _, sk := range c.skills.Named(text) {
		out = append(out, skill.Text(sk))
	}
	return out
}

func (c *Controller) input(ctx context.Context, session, text string, paths []string, now bool) (*Work, error) {
	blank := strings.TrimSpace(text) == ""
	if blank && !now {
		return nil, nil
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, ErrNoSession
	}
	if command, ok := shellInput(text); ok && c.cfg.UserShell != nil {
		return nil, c.runShell(command, text)
	}
	if name, _, isCommand := parse(text); isCommand && isControl(name) {
		session = Main
	}
	if _, err := c.runnerOf(session); err != nil {
		return nil, err
	}
	images, err := loadImages(paths)
	if err != nil {
		return nil, err
	}
	if !blank {
		c.record(text)
	}
	if name, args, isCommand := parse(text); isCommand {
		cmds := commandTable()
		if i := slices.IndexFunc(cmds, func(cmd command) bool { return cmd.Name == name }); i >= 0 {
			return nil, cmds[i].run(c, ctx, args)
		}
		if _, ok := c.skills.Lookup(leadingWord(text)); !ok {
			return nil, fmt.Errorf("%w: /%s", ErrUnknownCommand, name)
		}
	}
	prompt := strings.TrimSpace(text)
	if strings.HasPrefix(prompt, "//") {
		prompt = prompt[1:]
	}
	skills := c.skillTexts(text)
	c.queue.Lock()
	defer c.queue.Unlock()
	target, err := c.runnerLocked(session)
	if err != nil {
		return nil, err
	}
	p := pigruntime.Prompt{Text: prompt, Images: images, Skills: skills}
	if target == c.main && c.workflowOffered() && len(keywordSpans(text, c.skills, ultracodeWord)) > 0 {
		p.Tools = []string{"Workflow"}
	}
	if target == c.main && len(keywordSpans(text, c.skills, ultrathinkWord)) > 0 {
		p.Effort = pigruntime.MaxEffort
	}
	return target.submitLocked(ctx, p, now)
}

// runnerOf returns the runner of session, Main or a subagent's task id.
func (c *Controller) runnerOf(session string) (*runner, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	return c.runnerLocked(session)
}

// runnerLocked is runnerOf with queue held.
func (c *Controller) runnerLocked(session string) (*runner, error) {
	if session == Main {
		return c.main, nil
	}
	r := c.children[session]
	if r == nil {
		return nil, fmt.Errorf("failed to find session %q: %w", session, tool.ErrUnknownTask)
	}
	return r, nil
}

func (c *Controller) record(text string) {
	if c.cfg.History == nil {
		return
	}
	if err := c.cfg.History.Add(text); err != nil {
		slog.Warn("failed to record prompt history", "err", err)
	}
}

// History returns the recorded inputs, oldest first; nil when disabled.
func (c *Controller) History() []string {
	if c.cfg.History == nil {
		return nil
	}
	return c.cfg.History.Texts()
}

// Files returns the workspace entries @ references complete to; nil when disabled.
func (c *Controller) Files() ([]Candidate, error) {
	if c.cfg.Files == nil {
		return nil, nil
	}
	paths, err := c.cfg.Files.List(maxReferenceEntries)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}
	out := make([]Candidate, 0, len(paths))
	for _, p := range paths {
		out = append(out, Candidate{Value: p})
	}
	return out, nil
}

// TakeQueued removes and returns the human's input queued for the running turn
// of session. An unknown session has none.
func (c *Controller) TakeQueued(session string) []event.Input {
	c.queue.Lock()
	defer c.queue.Unlock()
	r, err := c.runnerLocked(session)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	emit := r.emit
	c.mu.Unlock()
	if emit == nil {
		emit = c.relay.emit
	}
	return c.inputs(r.takeHuman(emit))
}

func (c *Controller) Commands() []Command {
	cmds := commandTable()
	out := make([]Command, 0, len(cmds))
	for _, cmd := range cmds {
		cm := cmd.Command
		cm.Argument = cmd.args != nil
		out = append(out, cm)
	}
	for _, sk := range c.skills.Skills() {
		out = append(out, Command{Name: c.skills.Command(sk), Summary: sk.Description, Skill: true})
	}
	return out
}

// PromptSpans returns the highlighted runs of a prompt as sent to session; a
// sent prompt is never a command.
func (c *Controller) PromptSpans(session, text string) []Span {
	return promptSpans(text, c.skills, session == Main && c.workflowOffered(), session == Main && c.ultrathinks())
}

// Spans returns the input's highlighted runs when it is typed to session.
func (c *Controller) Spans(session, text string) []Span {
	return Spans(text, c.skills, session == Main && c.workflowOffered(), session == Main && c.ultrathinks())
}

// signalLocked must be called with queue held.
func (c *Controller) signalLocked() {
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
}

// Interrupt requests abort of a running login, whatever session is named, else
// of the running turn of session, without waiting for it, and reports whether
// there was one; an unknown session has none. Input still queued is handed back in the turn's
// TurnEnd.Unsent; task notices are not, and wait for the next human prompt
// without waking the model. A subagent stopped this way is reported to the main
// session as stopped by the user.
func (c *Controller) Interrupt(session string) bool {
	stop, ok := c.interruptLocked(session)
	if stop != nil {
		stop()
	}
	return ok
}

// interruptLocked is Interrupt's work under queue; stop kills the command the
// human's Esc stopped, which the caller does once queue is released.
func (c *Controller) interruptLocked(session string) (stop func(), ok bool) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	login := c.login
	emit := c.main.emit
	c.mu.Unlock()
	if login != nil {
		login.cancel()
		return nil, true
	}
	r, err := c.runnerLocked(session)
	if err != nil {
		return nil, false
	}
	if r != c.main {
		return nil, c.interruptByHumanLocked(r) != nil
	}
	// A turn's stop never touches the commands; with no turn, the stop is the
	// running command's.
	if r.interrupt() {
		return nil, true
	}
	if emit == nil {
		return nil, false
	}
	return r.interruptCommand(emit)
}

// interruptByHumanLocked stops r's running Work as the human's stop and returns
// it; nil when none runs. A Work already stopped by someone else stays theirs,
// unless input is pending: the stop then hands that input back, so the human's
// stop must report it. It must be called with queue held.
func (c *Controller) interruptByHumanLocked(r *runner) *Work {
	c.mu.Lock()
	w := r.work
	c.mu.Unlock()
	if w == nil {
		return nil
	}
	if !w.stopped || len(r.waiting()) > 0 {
		w.byHuman = true
	}
	r.interrupt()
	return w
}

// ToggleMode switches between plan and execute mode.
func (c *Controller) ToggleMode() {
	c.changeMode(func() uint64 {
		if mode, _ := c.authority.Snapshot(); mode == policy.Plan {
			return c.authority.EnterExecute()
		}
		return c.authority.EnterPlan()
	})
}

func (c *Controller) EnterPlan() {
	c.changeMode(c.authority.EnterPlan)
}

func (c *Controller) EnterExecute() {
	c.changeMode(c.authority.EnterExecute)
}

// changeMode holds mu across the read and the change so that only the caller
// that actually changed the mode emits ModeChanged.
func (c *Controller) changeMode(enter func() uint64) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	_, before := c.authority.Snapshot()
	after := enter()
	mode, _ := c.authority.Snapshot()
	c.mu.Unlock()
	if after != before {
		c.relay.emit(event.ModeChanged{Mode: mode})
	}
}

func (c *Controller) Status() Status {
	// Busy is read under queue: the final TurnEnd and the Work's deregistration
	// are one queue section, so a frontend refreshing on a TurnEnd sees Busy
	// false for a finished Work and true for one that continues.
	c.queue.Lock()
	children := maps.Clone(c.children)
	c.mu.Lock()
	sess, sessID := c.main.sess, c.sessID
	busy := c.main.work != nil
	side := c.spent
	c.mu.Unlock()
	c.queue.Unlock()

	mode, _ := c.authority.Snapshot()
	s := Status{SessionID: sessID, Mode: mode, Model: c.Model(), Busy: busy, Branch: gitBranch(c.cfg.Workspace), Workspace: c.cfg.Workspace, TotalCost: totalUsage(side, sess, children).Cost()}
	if sess != nil {
		s.MessageCount = sess.MessageCount()
		s.Ultracode = sess.Callable("Workflow")
		if tokens, window, ok := sess.ContextUsage(); ok {
			s.ContextTokens, s.ContextWindow = tokens, window
		}
	}
	for _, t := range c.cfg.Tools {
		if t.Capability == policy.Exec {
			s.Shell, s.Sandboxed = true, t.Sandboxed
			break
		}
	}
	// queue stays held so a workflow that starts meanwhile is not listed as a
	// subagent; List takes only the registry's lock.
	c.queue.Lock()
	for _, info := range c.cfg.Tasks.List() {
		if info.Status != tool.TaskRunning {
			continue
		}
		_, workflow := c.workflows[info.ID]
		switch {
		case workflow:
			s.Workflows++
		case info.Agent != "":
			s.Agents++
		default:
			s.Shells++
		}
	}
	c.queue.Unlock()
	s.ForegroundSince = c.cfg.Tasks.ForegroundSince()
	s.Limits = c.limits.get()
	return s
}

// Agents describes the main session, then each subagent of the current session
// in task order, including those whose run ended.
func (c *Controller) Agents() []AgentStatus {
	c.queue.Lock()
	children := maps.Clone(c.children)
	c.mu.Lock()
	main := c.main.sess
	c.mu.Unlock()
	c.queue.Unlock()

	out := []AgentStatus{c.agentStatus(AgentStatus{ID: Main, Model: c.Model()}, main)}
	for _, info := range c.cfg.Tasks.List() {
		r := children[info.ID]
		if r == nil {
			continue
		}
		out = append(out, c.agentStatus(AgentStatus{
			ID: info.ID, Agent: info.Agent, Description: info.Command, Model: r.model, State: info.Status,
		}, r.sess))
	}
	return out
}

// agentStatus adds the context usage, effort and usage of sess to a.
func (c *Controller) agentStatus(a AgentStatus, sess *pigruntime.Session) AgentStatus {
	if sess != nil {
		if tokens, window, ok := sess.ContextUsage(); ok {
			a.ContextTokens, a.ContextWindow = tokens, window
		}
		a.Effort = sess.Effort()
		a.ToolCalls, a.Cost = sess.Usage()
	}
	return a
}

// Close stops running work and every task, and closes the session within
// min(ctx, JoinTimeout). Before that it waits for any session start or listing
// already in progress, which that bound does not cover. It does not close the Runtime. If work or a
// login does not stop, the session is left open, the work stays tracked and the
// error wraps pigruntime.ErrJoinTimeout; a later Close tries again. No event is delivered
// after Close returns.
func (c *Controller) Close(ctx context.Context) error {
	c.life.Lock()
	defer c.life.Unlock()
	defer c.relay.shut()
	defer c.limits.close()
	c.titleStop()

	c.queue.Lock()
	c.mu.Lock()
	c.closed = true
	c.stopSideLocked()
	c.main.notices = nil
	c.approvals = nil
	sess, unsubscribe, w, emit, login := c.main.sess, c.main.unsubscribe, c.main.work, c.main.emit, c.login
	if login != nil {
		login.cancel()
	}
	if c.switching != nil && c.switching.work != nil {
		w = c.switching.work
	}
	c.mu.Unlock()
	if w != nil {
		// Permanently, even if the join below times out.
		w.discard = true
		w.switchHold = false
		w.terminal = nil
		c.main.stop(w, emit)
	}
	stopCommand := c.main.clearQueueLocked()
	c.queue.Unlock()
	stopCommand()
	c.endDirRequests()
	ctx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
	defer cancel()
	// closed is set, so no login starts now. The login needs neither life nor
	// queue, and it does not end before its last emit, so after this nothing of
	// it reaches the runtime or a subscriber.
	if login != nil {
		select {
		case <-login.done:
		case <-ctx.Done():
			return fmt.Errorf("failed to close controller: login did not stop: %w", pigruntime.ErrJoinTimeout)
		}
	}
	if err := c.stopTasks(ctx); err != nil {
		return err
	}
	if sess == nil {
		return nil
	}

	if w != nil {
		select {
		case <-w.done:
		case <-ctx.Done():
			return fmt.Errorf("failed to close controller: work %s did not stop: %w", w.id, pigruntime.ErrJoinTimeout)
		}
	}
	c.wg.Wait()
	// Abort reaches tools asynchronously: adoption can follow the first sweep.
	if len(c.cfg.Tasks.List()) > 0 {
		if err := c.stopTasks(ctx); err != nil {
			return err
		}
	}
	c.closeChildren()
	unsubscribe()
	c.mu.Lock()
	c.main.sess, c.sessID, c.main.emit, c.main.unsubscribe = nil, "", nil, nil
	c.mu.Unlock()
	if err := sess.Close(ctx); err != nil {
		return fmt.Errorf("failed to close controller: %w", err)
	}
	return nil
}

// stopTasks runs with life held. A retry joins unfinished cleanup rather than
// overlapping it, even when StopAll has already emptied the visible task list.
func (c *Controller) stopTasks(ctx context.Context) error {
	if c.stopAllDone == nil {
		done := make(chan struct{})
		c.stopAllDone = done
		go func() {
			c.cfg.Tasks.StopAll()
			close(done)
		}()
	}
	select {
	case <-c.stopAllDone:
		c.stopAllDone = nil
		return nil
	case <-ctx.Done():
		return fmt.Errorf("failed to close controller: tasks did not stop: %w", pigruntime.ErrJoinTimeout)
	}
}

// start opens a session for req and makes it current, then emits events.
// Running work is stopped and joined first; if it will not stop the switch is
// refused. The new session is opened before the old one is closed, so a failed
// open leaves the old session in place.
func (c *Controller) start(ctx context.Context, req OpenRequest) error {
	c.life.Lock()
	defer c.life.Unlock()
	op, err := c.beginSwitch(false)
	if err != nil {
		return err
	}
	return c.startSwitch(ctx, req, op)
}

// beginSwitch marks and stops in one queue section, before settlement can
// drain input queued after an earlier Interrupt. login is set only for the
// open a login owes; every other switch is refused with ErrLoggingIn while a
// login runs.
func (c *Controller) beginSwitch(login bool) (*sessionSwitch, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.login != nil && !login {
		c.mu.Unlock()
		return nil, ErrLoggingIn
	}
	if c.busyLocked() {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	op := &sessionSwitch{work: c.main.work, emit: c.main.emit}
	c.switching = op
	c.mu.Unlock()
	if op.work != nil {
		op.work.switchHold = true
		c.main.stop(op.work, op.emit)
	}
	return op, nil
}

// startSwitch runs with life held and resolves the captured operation, not
// the current turn, which may already have deregistered.
func (c *Controller) startSwitch(ctx context.Context, req OpenRequest, op *sessionSwitch) error {
	defer func() {
		// A command that finished meanwhile, or input given before the switch
		// failed, goes on once the session is free.
		c.queue.Lock()
		defer c.queue.Unlock()
		c.mu.Lock()
		c.switching = nil
		c.mu.Unlock()
		c.mu.Lock()
		emit := c.main.emit
		c.mu.Unlock()
		if c.main.startNextLocked() && emit != nil {
			c.main.queueChanged(emit)
		}
		// Only a command's reply wakes the model: the notices that queued during a
		// failed switch wait for the next prompt, as they always did.
		c.mu.Lock()
		c.main.advanceLocked(c.main.replies(c.main.ready()))
	}()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrNoSession
	}
	old, oldUnsubscribe := c.main.sess, c.main.unsubscribe
	c.mu.Unlock()
	w, oldEmit := op.work, op.emit
	if w != nil {
		defer c.releaseSwitch(w, oldEmit, false)
		joinCtx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
		defer cancel()
		select {
		case <-w.done:
		case <-joinCtx.Done():
			return fmt.Errorf("failed to switch session: work %s did not stop: %w", w.id, ErrWorkStillRunning)
		}
	}

	// A first open that never got a model leaves it unset; its notice is
	// reported with the session's other startup events.
	if c.Model() == "" {
		notices, err := c.resolveModel(ctx)
		if errors.Is(err, pigruntime.ErrNoCredential) {
			err = c.withLoginHint(err)
		}
		if err != nil {
			return err
		}
		c.startup = slices.Concat(notices, c.startup)
	}

	later := c.attempted
	c.attempted = true
	d := newDisplay(c.tools)
	var rec *classifier.Record
	if c.cfg.Classifier != nil {
		rec = &classifier.Record{}
	}
	c.titleCommit.Lock()
	defer c.titleCommit.Unlock()
	effort := pigruntime.DefaultEffort
	if req.ResumeID == "" && !req.Continue {
		effort = c.cfg.Effort
	}
	sess, err := c.rt.Open(ctx, pigruntime.OpenOptions{
		Model:        c.Model(),
		Effort:       effort,
		ResumeID:     req.ResumeID,
		Continue:     req.Continue,
		SystemPrompt: c.prompt,
		Tools:        c.tools,
		Joining:      c.joining(),
		Later:        c.later(),
		Authority:    c.authority,
		Rules:        c.cfg.Rules,
		Approve:      c.approver(d, c.main),
		Review:       c.reviewer(rec),
		Recall:       c.recall,
	})
	if err != nil {
		return err
	}
	c.offerSearch(sess)

	if w != nil {
		c.releaseSwitch(w, oldEmit, true)
	}
	// A child's emitter stays bound to the old epoch, so its pending asks end
	// before the advance drops them, and an ended ask is no longer answerable.
	// One queue section keeps a child's ask from registering in between.
	c.queue.Lock()
	c.endRequests()
	c.mu.Lock()
	c.approvals = nil
	c.mu.Unlock()
	c.relay.advance()
	c.queue.Unlock()
	if old != nil {
		oldUnsubscribe()
		// The old session is released even if the caller's ctx has ended:
		// the new session is already open.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.JoinTimeout)
		if err := old.Close(closeCtx); err != nil {
			c.relay.emit(event.Error{Err: err})
		}
		cancel()
		// The tasks and their notices belong to the old session; a failed
		// open keeps both. Notices are cleared atomically with installation.
		c.cfg.Tasks.StopAll()
		c.closeChildren()
		c.relay.emit(event.TasksChanged{})
	}
	// A session opened after the first open was tried, even one that failed,
	// starts in the configured mode, never in the mode it was left in; the
	// first keeps the mode chosen while it was opening.
	if later {
		c.authority.ClearEdits()
		if c.cfg.Mode == policy.Execute {
			c.EnterExecute()
		} else {
			c.EnterPlan()
		}
	}

	bound := c.relay.bind()
	emit := func(ev event.Event) {
		rec.Observe(ev)
		bound(d.apply(ev))
	}
	unsubscribe := sess.Subscribe(c.main.reading(sess, emit))
	id := sess.ID()
	var planErr error
	if c.cfg.Plans != nil {
		if err := c.cfg.Plans.Select(id); err != nil {
			planErr = fmt.Errorf("failed to select the plan file: %w", err)
		}
	}
	var outputErr error
	if c.cfg.Outputs != nil {
		if err := c.cfg.Outputs.Select(id); err != nil {
			outputErr = fmt.Errorf("failed to select the output folder: %w", err)
		}
	}
	c.queue.Lock()
	c.mu.Lock()
	var stopCommand func()
	cleared := false
	if old != nil {
		c.main.notices = nil
		c.main.hold = false
		c.main.wakeDue = false
		cleared = len(c.main.queued) > 0
		stopCommand = c.main.clearQueueLocked()
	}
	c.main.sess, c.sessID, c.main.emit, c.main.bound, c.main.unsubscribe = sess, id, emit, bound, unsubscribe
	c.main.told, c.main.dirsTold = "", c.dirsToldBy[id]
	c.titled = sess.Name() != ""
	c.spent = pigruntime.Usage{}
	c.stopSideLocked()
	c.deferred = nil
	c.mu.Unlock()
	c.queue.Unlock()
	if stopCommand != nil {
		stopCommand()
	}
	changed := event.SessionChanged{
		SessionID:    id,
		Resumed:      req.ResumeID != "" || req.Continue,
		MessageCount: sess.MessageCount(),
		Name:         sess.Name(),
	}
	if changed.Resumed {
		for _, ev := range sess.History() {
			rec.Observe(ev)
			changed.History = append(changed.History, d.apply(ev))
		}
	}
	c.relay.emit(changed)
	if cleared {
		c.relay.emit(event.QueueChanged{Pending: []event.Queued{}})
	}
	if planErr != nil {
		c.relay.emit(event.Error{Err: planErr})
	}
	if outputErr != nil {
		c.relay.emit(event.Error{Err: outputErr})
	}
	for _, ev := range c.startup {
		c.relay.emit(ev)
	}
	c.startup = nil
	return nil
}

// releaseSwitch resolves the held terminal event through the old session's
// emitter before its relay epoch can be retired. It is safe to call again.
func (c *Controller) releaseSwitch(w *Work, emit func(event.Event), committed bool) {
	c.queue.Lock()
	defer c.queue.Unlock()
	w.switchHold = false
	if w.terminal != nil {
		end := *w.terminal
		w.terminal = nil
		if committed || w.discard {
			end.Unsent = nil
		}
		emit(end)
	}
}

// joining names the servers whose tools a session opened now may be given
// later.
func (c *Controller) joining() []string {
	if !c.searchable {
		return nil
	}
	return c.cfg.Servers.Names()
}

// later names the prompt parts a session opened now is given after the open:
// the servers part, while servers are still to settle. It runs with life held.
func (c *Controller) later() []string {
	if c.cfg.Servers == nil || len(c.settled) == len(c.cfg.Servers.Names()) {
		return nil
	}
	return []string{serversPart}
}

// workflowOffered reports that the main session has the Workflow tool.
func (c *Controller) workflowOffered() bool {
	return !c.cfg.Unattended && !c.cfg.Rules.Removes("Workflow")
}

// openRequestLocked numbers a request and registers end, which emits its
// *Ended event; the returned end runs it once, however often it is called and
// whoever calls first, and deregisters the request. It must be called with mu
// held.
func (c *Controller) openRequestLocked(end func(token uint64)) (uint64, func()) {
	c.requestSeq++
	token := c.requestSeq
	done := sync.OnceFunc(func() {
		end(token)
		c.mu.Lock()
		delete(c.requests, token)
		c.mu.Unlock()
	})
	if c.requests == nil {
		c.requests = map[uint64]func(){}
	}
	c.requests[token] = done
	return token, done
}

// endRequests ends every open request, oldest first. It runs the ends with mu
// released, since an end takes it, and with queue held, so no request registers
// meanwhile.
func (c *Controller) endRequests() {
	c.mu.Lock()
	tokens := slices.Sorted(maps.Keys(c.requests))
	ends := make([]func(), len(tokens))
	for i, token := range tokens {
		ends[i] = c.requests[token]
	}
	c.mu.Unlock()
	for _, end := range ends {
		end()
	}
}
