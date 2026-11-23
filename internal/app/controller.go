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
	// Files lists the entries that @ references complete to; nil offers none.
	Files *tool.Workspace
	// Instructions are added to the system prompt in order: the global
	// file's group first, then each root's.
	Instructions []instruction.Group
	// Errors are reported once as Error events, after the first session
	// opens, before those about the MCP servers.
	Errors []error
	// Servers waits for the MCP servers to settle and returns their statuses;
	// Open or Start calls it once, before the first session. Open returns its
	// error unchanged; Start reports it as an Error event. Nil means there are
	// no servers.
	Servers func(ctx context.Context) ([]pigruntime.ServerStatus, error)
	// Tasks is the background task manager the Bash tool shares; nil means one
	// no tool shares, so there are never tasks.
	Tasks Tasks
	// Wake lets a task's end start a turn on its own; otherwise its notice
	// waits for a request a human prompt starts.
	Wake bool
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
	// tools; Agent, SendMessage, TaskOutput, TaskStop and ToolSearch are never
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
	// activity, for Status.Limits.
	Limits bool
	// Title names a session that has no name from its first prompt, using the
	// small model.
	Title bool
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
// call back into the Controller. Neither queue nor mu is held while calling Config.Tasks,
// whose callback takes both, except for starting an agent task and its
// AgentTask.Run and End, which a subagent's or a workflow's run makes with
// queue held, and Tasks.List, which Status calls with queue held. That is
// safe only because tool.Tasks calls nothing back while holding its own locks:
// OnChange and the stop function run without them, and the stop function takes
// queue. The mutex of limits is a leaf: nothing is called with it held.
type Controller struct {
	rt  *pigruntime.Runtime
	cfg Config
	// tools and prompt are what every session opens with; set before the
	// first session and not changed after.
	tools  []tool.Tool
	prompt []pigruntime.PromptPart
	// agents are the subagents' tools and prompts, set with tools; workflowSpecs
	// are the workflow agents', the default one first.
	agents        []agentSpec
	workflowSpecs []agentSpec
	// skills is what users can type; skillErrs reports the skills it left out.
	skills    *skill.Set
	skillErrs []error
	authority *policy.Authority
	relay     relay
	// limits is nil unless Config.Limits.
	limits *limits
	wg     sync.WaitGroup
	// titleCtx ends when Close starts, which stops a title request in flight.
	titleCtx  context.Context
	titleStop context.CancelFunc
	// titleCommit serialises title persistence and announcement with opening
	// and installing a replacement, including a resume of the same session.
	// It is never held across wg.Wait: title workers need it to finish.
	titleCommit sync.Mutex
	// startup is what the first open that succeeds reports, so a failed first
	// open does not lose it; life guards it once a session can be opened.
	startup []event.Event
	// attempted is set once the first open has been tried or its wait for the
	// servers has failed; life guards it.
	attempted bool

	// life serialises session start, switch and close.
	life sync.Mutex
	// stopAllDone retains a task cleanup across timed-out Close calls; life guards it.
	stopAllDone chan struct{}
	// queue makes queueing input atomic with taking it from pending and with
	// deciding to stop, continue or end the running Work; it guards the
	// runners' pending, notices and hold and the Work's queue fields. PiG calls back under its own lock to take
	// pending, so nothing may call into PiG with queue held.
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
	// spent is what the current root session has spent outside its open sessions,
	// in estimated dollars: side requests and ended workflow agents.
	spent float64
	// model is the session model: Config.Model, or the runtime's default once resolved.
	model  string
	sessID string
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
	// requestSeq numbers the requests of every kind, so a token names one
	// request for the Controller's lifetime; requests holds the end of each
	// request still open, by token. Both are guarded by mu.
	requestSeq uint64
	requests   map[uint64]func()
	// deferred is the first open that Start put off for a login; cleared once
	// any session opens.
	deferred *OpenRequest
}

// sessionSwitch retains work even after it deregisters while start waits for life.
type sessionSwitch struct {
	work *Work
	emit func(event.Event)
	// cancelWait is set only for the first open started by Start, during which
	// prompts are queued rather than refused; Close calls it to end the wait
	// for the servers.
	cancelWait context.CancelFunc
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
	c.setup(nil)
	cfg.Tasks.OnChange(c.taskChanged)
	cfg.Tasks.OnForeground(func() { c.relay.emit(event.TasksChanged{}) })
	if cfg.Limits {
		c.limits = newLimits(rt.Limits, c.modelProvider, c.relay.emit)
		c.relay.subscribe(c.limits.observe)
	}
	return c
}

// setup sets the session tools and prompt from the built-in tools and
// statuses, and returns the events about the servers.
func (c *Controller) setup(statuses []pigruntime.ServerStatus) []event.Event {
	var events []event.Event
	var serverTools []tool.Tool
	var servers []mcpServer
	for _, st := range statuses {
		if st.Err != nil {
			events = append(events, event.Error{Err: fmt.Errorf("MCP server %s failed to start: %w", st.Name, st.Err)})
			continue
		}
		server := mcpServer{name: st.Name, instructions: st.Instructions}
		for _, t := range st.Tools {
			if c.cfg.Rules.Removes(t.Name) {
				continue
			}
			serverTools = append(serverTools, t)
			server.tools = append(server.tools, t.Name)
		}
		for _, skipped := range st.Skipped {
			events = append(events, event.Notice{Text: fmt.Sprintf("MCP server %s: skipped tool %s", st.Name, skipped)})
		}
		servers = append(servers, server)
	}
	tools := slices.Clone(c.cfg.Tools)
	if ask := tool.NewAskUserQuestion(c.askQuestion); !c.cfg.Unattended && !c.cfg.Rules.Removes(ask.Name) {
		tools = append(tools, ask)
	}
	if exit := tool.NewExitPlanMode(c.cfg.Plans, c.reviewPlan); c.cfg.Plans != nil && !c.cfg.Unattended && !c.cfg.Rules.Removes(exit.Name) {
		tools = append(tools, exit)
	}
	var search []tool.Tool
	if !c.cfg.Rules.Removes("WebSearch") {
		search = append(search, tool.NewWebSearch(func(ctx context.Context, system, query string) (tool.SearchResult, error) {
			return c.sideSearch(ctx, c.SmallModel(), system, query)
		}))
		tools = append(tools, search...)
	}
	if len(c.cfg.Agents) > 0 && !c.cfg.Rules.Removes("Agent") {
		launcher := agentLauncher{c}
		tools = append(tools, tool.NewAgent(c.cfg.Agents, launcher))
		if send := tool.NewSendMessage(launcher); !c.cfg.Rules.Removes(send.Name) {
			tools = append(tools, send)
		}
	}
	if len(serverTools) > 0 {
		toolSearch := tool.NewToolSearch(serverTools, pigruntime.RankTools, pigruntime.LoadTools)
		if c.cfg.Rules.Removes(toolSearch.Name) {
			// ToolSearch is the only way to load deferred tools.
			serverTools = nil
			for i := range servers {
				servers[i].tools = nil
			}
		} else {
			tools = append(tools, toolSearch)
		}
	}
	tools = append(tools, serverTools...)
	// Workflow never reaches a subagent's tools, which buildAgents builds from
	// search and serverTools.
	if c.workflowOffered() {
		tools = append(tools, tool.NewWorkflow(c.cfg.Agents, workflowLauncher{c}, c.cfg.MaxWorkflows, c.cfg.MaxAgents))
	}
	c.tools = tools
	c.prompt = buildPrompt(c.cfg.Workspace, c.tools, c.cfg.Instructions, servers, c.cfg.Classifier != nil, nil, false)
	c.agents, c.workflowSpecs = c.buildAgents(search, serverTools, servers)
	return events
}

// prepareStartup sets up the first session from statuses and keeps what it
// reports once open: Config.Errors, then the events about the servers.
func (c *Controller) prepareStartup(statuses []pigruntime.ServerStatus) {
	for _, err := range slices.Concat(c.cfg.Errors, c.skillErrs) {
		c.startup = append(c.startup, event.Error{Err: err})
	}
	c.startup = append(c.startup, c.setup(statuses)...)
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

// Open checks that req can be opened, waits for the MCP servers, opens the
// first session with their tools, then emits each of Config.Errors and the
// events about the servers.
func (c *Controller) Open(ctx context.Context, req OpenRequest) error {
	notices, err := c.check(ctx, req)
	if err != nil {
		return err
	}
	statuses, err := c.servers(ctx)
	if err != nil {
		return err
	}
	c.startup = append(c.startup, notices...)
	c.prepareStartup(statuses)
	return c.start(ctx, req)
}

// Start is Open without waiting: it returns the check's error, and otherwise
// returns at once and opens the session in the background, reporting failure
// as an Error event. Prompts given until the session opens are queued and
// start its first turn; /new and /resume are refused with ErrBusy meanwhile.
// The wait for the servers runs without life, so Close can end it.
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
		return c.loginFirst(ctx, req, c.withLoginHint(err))
	}
	return c.startFirst(ctx, req, notices)
}

// startFirst opens the first session in the background; notices are reported
// after it opens, ahead of the other startup events.
func (c *Controller) startFirst(ctx context.Context, req OpenRequest, notices []event.Event) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	op, err := c.beginSwitch(cancel)
	if err != nil {
		cancel()
		return err
	}
	if c.cfg.Servers != nil {
		c.relay.emit(event.Activity{Text: "Connecting to MCP servers…"})
	}
	go func() {
		defer cancel()
		statuses, err := c.servers(ctx)
		if c.cfg.Servers != nil {
			c.relay.emit(event.Activity{})
		}
		c.life.Lock()
		defer c.life.Unlock()
		if err == nil {
			c.startup = append(c.startup, notices...)
			c.prepareStartup(statuses)
			err = c.startSwitch(ctx, req, op)
		} else {
			c.attempted = true
			c.mu.Lock()
			c.switching = nil
			c.mu.Unlock()
			for _, ev := range notices {
				c.relay.emit(ev)
			}
		}
		if err != nil {
			c.relay.emit(event.Error{Err: err})
		}
	}()
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

func (c *Controller) servers(ctx context.Context) ([]pigruntime.ServerStatus, error) {
	if c.cfg.Servers == nil {
		return nil, nil
	}
	return c.cfg.Servers(ctx)
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
	op, err := c.beginSwitch(nil)
	if err != nil {
		return err
	}
	go func() {
		c.life.Lock()
		defer c.life.Unlock()
		if err := c.startSwitch(ctx, req, op); err != nil {
			c.relay.emit(event.Error{Err: err})
		}
	}()
	return nil
}

func (c *Controller) Sessions() ([]pigruntime.SessionInfo, error) {
	return c.rt.Sessions()
}

// Submit takes one message of human input with the paths of its images for
// session, Main or a subagent's task id; an unknown one fails with an error
// wrapping tool.ErrUnknownTask. A control command always runs on the main
// session, whatever session is, and is executed and returns (nil, nil), ErrQuit for /quit,
// ErrShowTasks for /tasks, ErrMissingArgument for /resume, /model, /login or /logout
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
// of session or, for Main, the first session. An unknown session has none.
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
	return inputs(r.takeHuman(emit))
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
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	login := c.login
	c.mu.Unlock()
	if login != nil {
		login.cancel()
		return true
	}
	r, err := c.runnerLocked(session)
	if err != nil {
		return false
	}
	if r != c.main {
		return c.interruptByHumanLocked(r) != nil
	}
	return r.interrupt()
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
	if !w.stopped || len(r.pending) > 0 {
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
	s := Status{SessionID: sessID, Mode: mode, Model: c.Model(), Busy: busy, Branch: gitBranch(c.cfg.Workspace), Workspace: c.cfg.Workspace, TotalCost: side}
	if sess != nil {
		_, cost := sess.Usage()
		s.TotalCost += cost
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
	for _, r := range children {
		_, cost := r.sess.Usage()
		s.TotalCost += cost
	}
	s.Foreground = c.cfg.Tasks.Foreground()
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
	c.main.notices = nil
	c.approvals = nil
	sess, unsubscribe, w, emit, login := c.main.sess, c.main.unsubscribe, c.main.work, c.main.emit, c.login
	if login != nil {
		login.cancel()
	}
	if c.switching != nil && c.switching.cancelWait != nil {
		c.switching.cancelWait()
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
	c.queue.Unlock()
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
	op, err := c.beginSwitch(nil)
	if err != nil {
		return err
	}
	return c.startSwitch(ctx, req, op)
}

// beginSwitch marks and stops in one queue section, before settlement can
// drain input queued after an earlier Interrupt. cancelWait is non-nil only
// for the first open started by Start, which a login's deferred open also
// starts; every other switch is refused with ErrLoggingIn while a login runs.
func (c *Controller) beginSwitch(cancelWait context.CancelFunc) (*sessionSwitch, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.login != nil && cancelWait == nil {
		c.mu.Unlock()
		return nil, ErrLoggingIn
	}
	if c.switching != nil || c.changingModel {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	op := &sessionSwitch{work: c.main.work, emit: c.main.emit, cancelWait: cancelWait}
	c.switching = op
	c.mu.Unlock()
	if op.work != nil {
		op.work.switchHold = true
		c.main.stop(op.work, op.emit)
	}
	return op, nil
}

// startSwitch runs with life held and resolves the captured operation, not
// the current turn, which may already have deregistered. Once the session is
// open, input still pending, which only the first open started by Start or a
// later open after it failed can leave, starts the session's first turn in the
// same queue section that ends the switch, ahead of any input given later.
func (c *Controller) startSwitch(ctx context.Context, req OpenRequest, op *sessionSwitch) (err error) {
	defer func() {
		c.queue.Lock()
		defer c.queue.Unlock()
		var parts []pigruntime.Prompt
		if err == nil {
			c.mu.Lock()
			emit := c.main.emit
			c.mu.Unlock()
			parts = c.main.take(emit)
		}
		c.mu.Lock()
		c.switching = nil
		if len(parts) == 0 {
			c.mu.Unlock()
			return
		}
		_, _ = c.main.startTurn(ctx, parts)
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
		c.startup = append(c.startup, notices...)
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
	sess, err := c.rt.Open(ctx, pigruntime.OpenOptions{
		Model:        c.Model(),
		ResumeID:     req.ResumeID,
		Continue:     req.Continue,
		SystemPrompt: c.prompt,
		Tools:        c.tools,
		Authority:    c.authority,
		Rules:        c.cfg.Rules,
		Approve:      c.approver(d, c.main),
		Review:       c.reviewer(rec),
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
	unsubscribe := sess.Subscribe(emit)
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
	if old != nil {
		c.main.notices = nil
		c.main.hold = false
	}
	c.main.sess, c.sessID, c.main.emit, c.main.bound, c.main.unsubscribe = sess, id, emit, bound, unsubscribe
	c.main.told = ""
	c.titled = sess.Name() != ""
	c.spent = 0
	c.deferred = nil
	c.mu.Unlock()
	c.queue.Unlock()
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
