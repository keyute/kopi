package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/trust"
)

// dirHarness is a harness whose Controller can add directories: file tools
// over the sandbox's workspace with Kopi's home protected, a deny rule for
// .env, a trust file and a fake sandbox that records the directories it is given.
type dirHarness struct {
	*harness
	sb   pigruntimetest.Sandbox
	ws   *tool.Workspace
	gate *policy.Gate
	// sandboxed are the lists SandboxDirs was called with; it fails once with sandboxErr.
	sandboxed  [][]string
	sandboxErr error
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := trust.Canonical(t.TempDir())
	require.NoError(t, err)
	return dir
}

// dirConfig is cfg with what adding directories needs over sb's workspace.
func dirConfig(t *testing.T, sb pigruntimetest.Sandbox, cfg Config) (Config, *tool.Workspace, *policy.Gate) {
	t.Helper()
	ws, err := tool.NewWorkspace(sb.Workspace, []string{sb.KopiHome})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	read := tool.NewRead(ws)
	specs := tool.Specs([]tool.Tool{read})
	rules, err := policy.NewRules(sb.Workspace, sb.Home, specs, policy.Source{Name: "test", Deny: []string{"Read(.env)"}})
	require.NoError(t, err)
	cfg.Files, cfg.Rules, cfg.Tools = ws, rules, append(cfg.Tools, read)
	cfg.TrustFile, cfg.UserHome = filepath.Join(sb.KopiHome, "trust.json"), sb.Home
	cfg.Plans = tool.NewPlans(filepath.Join(sb.Home, "plans"))
	gate := policy.NewGate(policy.NewAuthority(), specs, rules, answer(policy.Allow, ""), nil)
	return cfg, ws, gate
}

func newDirHarness(t *testing.T, cfg Config, open bool) *dirHarness {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	cfg, ws, gate := dirConfig(t, sb, cfg)
	d := &dirHarness{sb: sb, ws: ws, gate: gate}
	cfg.SandboxDirs = func(dirs []string) error {
		d.sandboxed = append(d.sandboxed, slices.Clone(dirs))
		err := d.sandboxErr
		d.sandboxErr = nil
		return err
	}
	d.harness = newHarness(t, sb, cfg)
	if open {
		require.NoError(t, d.c.Open(t.Context(), OpenRequest{}))
	}
	return d
}

// decide saves a trust decision for dir.
func decide(t *testing.T, c *Controller, dir string, decision trust.Decision) {
	t.Helper()
	store, err := trust.Open(c.cfg.TrustFile)
	require.NoError(t, err)
	require.NoError(t, store.Set(dir, decision))
}

// saved is the decision trust.json holds for dir, and whether it holds one.
func saved(t *testing.T, c *Controller, dir string) (trust.Decision, bool) {
	t.Helper()
	store, err := trust.Open(c.cfg.TrustFile)
	require.NoError(t, err)
	d, ok, err := store.Lookup(dir)
	require.NoError(t, err)
	return d, ok
}

// otherDir is a new directory with an instruction file.
func otherDir(t *testing.T) string {
	t.Helper()
	dir := canonicalTemp(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("use tabs\n"), 0o600))
	return dir
}

// told is the text the model is sent for dirs, whose instruction files each
// hold "use tabs", in a session that started in workspace.
func told(workspace string, dirs ...string) string {
	text := "The workspace also includes " + strings.Join(dirs, ", ") + ". Give a path there as an absolute path; relative paths and Bash still start from " + workspace + "."
	var blocks []string
	for _, dir := range dirs {
		blocks = append(blocks, "Instructions from "+filepath.Join(dir, "AGENTS.md")+":\nuse tabs")
	}
	return text + "\n" + strings.Join(blocks, "\n\n")
}

// inJSON is text as it reads inside a JSON string.
func inJSON(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(text)
	require.NoError(t, err)
	return string(b[1 : len(b)-1])
}

// denied reports whether the gate refuses reading path.
func (d *dirHarness) denied(t *testing.T, path string) bool {
	t.Helper()
	args, err := json.Marshal(map[string]string{"file_path": path})
	require.NoError(t, err)
	err = d.gate.Before(t.Context(), "c1", "Read", args)
	return errors.Is(err, policy.ErrDenied)
}

func TestAddDirRefusals(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	require.NoError(t, os.WriteFile(filepath.Join(d.sb.Workspace, "a.txt"), nil, 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(d.sb.Workspace, "sub"), 0o700))
	requests := subscribeTo[event.DirRequested](d.c)

	for _, tc := range []struct {
		arg, want string
		is        error
	}{
		{"", "missing argument: /add-dir needs a directory", ErrMissingArgument},
		{"nope", "failed to add nope: file does not exist", fs.ErrNotExist},
		{"a.txt", "failed to add a.txt: path is not a directory", tool.ErrNotDirectory},
		{".", "failed to add .: directory is already in the workspace", tool.ErrInWorkspace},
		{"sub", "failed to add sub: directory is already in the workspace", tool.ErrInWorkspace},
		{d.sb.KopiHome, "failed to add " + d.sb.KopiHome + ": path is protected", tool.ErrProtected},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			err := d.c.AddDir(tc.arg)
			assert.EqualError(t, err, tc.want)
			assert.ErrorIs(t, err, tc.is)
		})
	}
	assert.Empty(t, requests)
	assert.Empty(t, d.sandboxed)
}

func TestAddDirAsksWhenNoDecisionCoversTheDirectory(t *testing.T) {
	for _, tc := range []struct {
		name         string
		add, trusted bool
		notice       string
		decision     trust.Decision
	}{
		{"trust", true, true, ", with its instruction files.", trust.Trusted},
		{"without trust", true, false, ". Its instruction files were not loaded: the folder is not trusted.", trust.Untrusted},
		{"decline", false, false, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDirHarness(t, Config{Covers: func(string) ([]string, []string) { return []string{"docs: npx docs"}, []string{"skills in x"} }}, true)
			dir := otherDir(t)
			requests, ended, notices := subscribeTo[event.DirRequested](d.c), subscribeTo[event.DirEnded](d.c), subscribeTo[event.Notice](d.c)

			require.NoError(t, d.c.AddDir(dir))
			req := receiveEvent(t, requests)
			assert.Equal(t, event.DirRequested{
				Token: req.Token, Dir: dir, Instructions: []string{filepath.Join(dir, "AGENTS.md")},
				Servers: []string{"docs: npx docs"}, Skills: []string{"skills in x"}, Saved: true,
			}, req)
			assert.NotZero(t, req.Token)
			assert.Empty(t, d.sandboxed, "nothing is added before the answer")
			assert.NoError(t, d.ws.CanAdd(dir))

			assert.True(t, d.c.AnswerDir(req.Token, tc.add, tc.trusted))
			assert.Equal(t, event.DirEnded{Token: req.Token}, receiveEvent(t, ended))
			assert.False(t, d.c.AnswerDir(req.Token, tc.add, tc.trusted), "the request is over")

			got, ok := saved(t, d.c, dir)
			assert.Equal(t, tc.decision, got)
			assert.Equal(t, tc.add, ok)
			if !tc.add {
				assert.NoError(t, d.ws.CanAdd(dir))
				assert.Empty(t, notices)
				return
			}
			assert.Equal(t, event.Notice{Text: "Added " + dir + " to the workspace" + tc.notice}, receiveEvent(t, notices))
			assert.ErrorIs(t, d.ws.CanAdd(dir), tool.ErrInWorkspace)
			assert.Equal(t, [][]string{{dir}}, d.sandboxed)
		})
	}
}

func TestAddDirSendsAnInstructionFileSharedByTwoPendingAddsOnce(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	root := canonicalTemp(t)
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("SHARED_RULE\n"), 0o600))
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	require.NoError(t, os.Mkdir(a, 0o700))
	require.NoError(t, os.Mkdir(b, 0o700))
	requests := subscribeTo[event.DirRequested](d.c)

	require.NoError(t, d.c.AddDir(a))
	require.NoError(t, d.c.AddDir(b))
	first, second := receiveEvent(t, requests), receiveEvent(t, requests)
	assert.Equal(t, []string{filepath.Join(root, "AGENTS.md")}, first.Instructions)
	assert.Equal(t, first.Instructions, second.Instructions)
	assert.True(t, d.c.AnswerDir(first.Token, true, true))
	assert.True(t, d.c.AnswerDir(second.Token, true, true))

	assert.Equal(t, 1, strings.Count(d.c.dirsText(0, d.c.dirCount(), true), "SHARED_RULE"))
}

func TestAnswerDirSavesNothingForAFolderAnotherAnswerAlreadyAdded(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := otherDir(t)
	requests, errs := subscribeTo[event.DirRequested](d.c), subscribeTo[event.Error](d.c)
	require.NoError(t, d.c.AddDir(dir))
	require.NoError(t, d.c.AddDir(dir))
	first, second := receiveEvent(t, requests), receiveEvent(t, requests)
	assert.True(t, d.c.AnswerDir(first.Token, true, true))
	sandboxed := slices.Clone(d.sandboxed)

	assert.True(t, d.c.AnswerDir(second.Token, true, false))

	err := receiveEvent(t, errs).Err
	assert.EqualError(t, err, "failed to add "+dir+": directory is already in the workspace")
	assert.ErrorIs(t, err, tool.ErrInWorkspace)
	got, ok := saved(t, d.c, dir)
	assert.True(t, ok)
	assert.Equal(t, trust.Trusted, got)
	assert.Equal(t, sandboxed, d.sandboxed)
	assert.Equal(t, 1, d.c.dirCount())
}

func TestAddDirAddsAtOnceWhenADecisionCoversIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision trust.Decision
		files    bool
		notice   string
	}{
		{"trusted", trust.Trusted, true, ", with its instruction files."},
		{"untrusted", trust.Untrusted, true, ". Its instruction files were not loaded: the folder is not trusted."},
		{"nothing to load", trust.Untrusted, false, "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDirHarness(t, Config{}, true)
			dir := canonicalTemp(t)
			if tc.files {
				dir = otherDir(t)
			}
			decide(t, d.c, filepath.Dir(dir), tc.decision)
			requests, notices := subscribeTo[event.DirRequested](d.c), subscribeTo[event.Notice](d.c)

			require.NoError(t, d.c.AddDir(dir))

			assert.Empty(t, requests)
			assert.Equal(t, event.Notice{Text: "Added " + dir + " to the workspace" + tc.notice}, receiveEvent(t, notices))
			assert.ErrorIs(t, d.ws.CanAdd(dir), tool.ErrInWorkspace)
			assert.Equal(t, [][]string{{dir}}, d.sandboxed)
		})
	}
}

func TestAddDirNeverSavesAnAnswerForTheUsersHome(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	requests, notices := subscribeTo[event.DirRequested](d.c), subscribeTo[event.Notice](d.c)
	home, err := trust.Canonical(d.sb.Home)
	require.NoError(t, err)

	require.NoError(t, d.c.AddDir("~"))
	req := receiveEvent(t, requests)
	assert.Equal(t, home, req.Dir)
	assert.False(t, req.Saved)

	assert.True(t, d.c.AnswerDir(req.Token, true, true))
	assert.Equal(t, event.Notice{Text: "Added " + home + " to the workspace."}, receiveEvent(t, notices))
	assert.NoFileExists(t, d.c.cfg.TrustFile)
}

func TestAddDirUnattendedAddsWithoutTrustAndSavesNothing(t *testing.T) {
	d := newDirHarness(t, Config{Unattended: true}, true)
	dir := otherDir(t)
	requests, notices := subscribeTo[event.DirRequested](d.c), subscribeTo[event.Notice](d.c)

	require.NoError(t, d.c.AddDir(dir))

	assert.Empty(t, requests)
	assert.Equal(t, event.Notice{Text: "Added " + dir + " to the workspace. Its instruction files were not loaded: the folder is not trusted."}, receiveEvent(t, notices))
	assert.NoFileExists(t, d.c.cfg.TrustFile)
	text := d.c.dirsText(0, d.c.dirCount(), true)
	assert.Equal(t, "The workspace also includes "+dir+". Give a path there as an absolute path; relative paths and Bash still start from "+d.sb.Workspace+".", text)
}

func TestAddDirUnattendedHonoursADecision(t *testing.T) {
	d := newDirHarness(t, Config{Unattended: true}, true)
	dir := otherDir(t)
	decide(t, d.c, dir, trust.Trusted)
	notices := subscribeTo[event.Notice](d.c)

	require.NoError(t, d.c.AddDir(dir))

	assert.Equal(t, event.Notice{Text: "Added " + dir + " to the workspace, with its instruction files."}, receiveEvent(t, notices))
}

func TestAddDirKeepsNothingWhenSavingTheDecisionFails(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := otherDir(t)
	requests, errs := subscribeTo[event.DirRequested](d.c), subscribeTo[event.Error](d.c)
	require.NoError(t, d.c.AddDir(dir))
	req := receiveEvent(t, requests)
	// A directory where the trust file belongs makes the save fail.
	require.NoError(t, os.MkdirAll(d.c.cfg.TrustFile, 0o700))

	assert.True(t, d.c.AnswerDir(req.Token, true, true))

	assert.ErrorContains(t, receiveEvent(t, errs).Err, "failed to save the trust decision")
	assert.NoError(t, d.ws.CanAdd(dir))
	assert.Empty(t, d.sandboxed)
}

func TestAddDirRollsBackWhenTheSandboxRefuses(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), nil, 0o600))
	d.sandboxErr = errors.New("boom")

	err := d.c.AddDir(dir)

	assert.EqualError(t, err, "failed to add "+dir+": boom")
	assert.Equal(t, [][]string{{dir}, {}}, d.sandboxed, "the previous list is put back")
	assert.False(t, d.denied(t, filepath.Join(dir, ".env")), "the rules no longer reach it")
	assert.NoError(t, d.ws.CanAdd(dir), "the directory is not in the workspace")
	text := d.c.dirsText(0, d.c.dirCount(), true)
	assert.Empty(t, text)

	require.NoError(t, d.c.AddDir(dir))
	assert.True(t, d.denied(t, filepath.Join(dir, ".env")))
}

func TestAddDirAppliesRulesBeforeTheSandbox(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)
	var deniedAtSandbox bool
	inner := d.c.cfg.SandboxDirs
	d.c.cfg.SandboxDirs = func(dirs []string) error {
		deniedAtSandbox = d.denied(t, filepath.Join(dir, ".env"))
		return inner(dirs)
	}

	require.NoError(t, d.c.AddDir(dir))

	assert.True(t, deniedAtSandbox, "no deny rule lags behind the reach")
}

func TestAddDirKeepsBareRulesThroughASymlinkedAlias(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := canonicalTemp(t)
	alias := filepath.Join(canonicalTemp(t), "alias")
	require.NoError(t, os.Symlink(dir, alias))
	decide(t, d.c, dir, trust.Untrusted)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), nil, 0o600))

	require.NoError(t, d.c.AddDir(alias))

	assert.True(t, d.denied(t, filepath.Join(dir, ".env")), "written through the real path")
	assert.True(t, d.denied(t, filepath.Join(alias, ".env")), "written through the alias")
	assert.ErrorIs(t, d.ws.CanAdd(dir), tool.ErrInWorkspace, "both spellings are one directory")
	assert.Equal(t, [][]string{{dir}}, d.sandboxed, "the sandbox takes the resolved path")
}

func TestAddDirIsAllowedWhileATurnRuns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	d := newDirHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}}, true)
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)
	d.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	notices := subscribeTo[event.Notice](d.c)

	w, err := d.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = d.c.Submit(t.Context(), Main, "/add-dir "+dir)

	require.NoError(t, err)
	assert.Equal(t, event.Notice{Text: "Added " + dir + " to the workspace."}, receiveEvent(t, notices))
	close(release)
	wait(t, w.Done())
}

func TestAddDirCommandNeedsAnArgument(t *testing.T) {
	d := newDirHarness(t, Config{}, true)

	_, err := d.c.Submit(t.Context(), Main, "/add-dir")

	assert.ErrorIs(t, err, ErrMissingArgument)
}

func TestAddDirNoticeBeforeTheFirstSessionFollowsItsSessionChanged(t *testing.T) {
	d := newDirHarness(t, Config{}, false)
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)

	require.NoError(t, d.c.AddDir(dir))
	d.c.life.Lock()
	queued := slices.Contains(d.c.startup, event.Event(event.Notice{Text: "Added " + dir + " to the workspace."}))
	d.c.life.Unlock()
	assert.True(t, queued, "already waiting for the first open when AddDir returns")
	require.NoError(t, d.c.Open(t.Context(), OpenRequest{}))

	notice := event.Notice{Text: "Added " + dir + " to the workspace."}
	evs := d.rec.all()
	at := slices.Index(evs, event.Event(notice))
	changed := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	require.GreaterOrEqual(t, at, 0)
	assert.Less(t, changed, at)
	assert.Len(t, ofType[event.Notice](evs), 1)
}

func TestAddDirNoticeDuringASessionSwitchFollowsTheNewSessionChanged(t *testing.T) {
	d, release := switching(t)
	defer release()
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)
	sessions, notices := subscribeTo[event.SessionChanged](d.c), subscribeTo[event.Notice](d.c)

	require.NoError(t, d.c.AddDir(dir))
	release()

	assertNoticeAfterSwitch(t, d, sessions, notices, "Added "+dir+" to the workspace.")
}

// switching is a harness whose /new is in flight: the switch waits for a
// running tool with the old session open, and release lets it go on.
func switching(t *testing.T) (d *dirHarness, release func()) {
	t.Helper()
	started, gate := make(chan struct{}), make(chan struct{})
	d = newDirHarness(t, Config{Tools: []tool.Tool{releaseTool(started, gate)}}, true)
	d.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	_, err := d.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = d.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	return d, sync.OnceFunc(func() { close(gate) })
}

// returns runs f and fails the test if it does not return before release is
// called.
func returns(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	wait(t, done)
}

// assertNoticeAfterSwitch checks the notice came once, after the new session's
// SessionChanged.
func assertNoticeAfterSwitch(t *testing.T, d *dirHarness, sessions <-chan event.SessionChanged, notices <-chan event.Notice, want string) {
	t.Helper()
	changed := receiveEvent(t, sessions)
	notice := receiveEvent(t, notices)
	assert.Equal(t, event.Notice{Text: want}, notice)
	evs := d.rec.all()
	at := slices.Index(evs, event.Event(notice))
	last := slices.IndexFunc(evs, func(ev event.Event) bool {
		c, ok := ev.(event.SessionChanged)
		return ok && c.SessionID == changed.SessionID
	})
	require.GreaterOrEqual(t, at, 0)
	assert.Less(t, last, at)
	assert.Len(t, ofType[event.Notice](evs), 1)
}

func TestAddDirReturnsWhileASwitchRunsAndReportsAfterIt(t *testing.T) {
	d, release := switching(t)
	defer release()
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)
	sessions, notices := subscribeTo[event.SessionChanged](d.c), subscribeTo[event.Notice](d.c)

	var err error
	returns(t, func() { err = d.c.AddDir(dir) })

	require.NoError(t, err)
	assert.ErrorIs(t, d.ws.CanAdd(dir), tool.ErrInWorkspace, "the add itself is done")
	assert.Empty(t, notices, "the notice waits for the switch")
	release()
	assertNoticeAfterSwitch(t, d, sessions, notices, "Added "+dir+" to the workspace.")
}

func TestAnswerDirReturnsWhileASwitchRunsAndReportsAfterIt(t *testing.T) {
	d, release := switching(t)
	defer release()
	dir := canonicalTemp(t)
	requests := subscribeTo[event.DirRequested](d.c)
	sessions, notices := subscribeTo[event.SessionChanged](d.c), subscribeTo[event.Notice](d.c)
	returns(t, func() { assert.NoError(t, d.c.AddDir(dir)) })
	req := receiveEvent(t, requests)

	var ok bool
	returns(t, func() { ok = d.c.AnswerDir(req.Token, true, false) })

	assert.True(t, ok)
	assert.Empty(t, notices)
	release()
	assertNoticeAfterSwitch(t, d, sessions, notices, "Added "+dir+" to the workspace.")
}

func TestAddDirNoticeWhileASwitchIsRegisteredButNotStarted(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)
	sessions, notices := subscribeTo[event.SessionChanged](d.c), subscribeTo[event.Notice](d.c)

	// The switch is registered, and has not taken life yet, so the old session is open and life is free.
	op, err := d.c.beginSwitch(false)
	require.NoError(t, err)
	require.NoError(t, d.c.AddDir(dir))
	assert.Empty(t, notices, "the old session's transcript is about to be cleared")
	d.c.life.Lock()
	err = d.c.startSwitch(t.Context(), OpenRequest{}, op)
	d.c.life.Unlock()
	require.NoError(t, err)

	assertNoticeAfterSwitch(t, d, sessions, notices, "Added "+dir+" to the workspace.")
}

func TestAddDirKeepsNoRequestOnceTheControllerClosed(t *testing.T) {
	d := newDirHarness(t, Config{Covers: nil}, true)
	d.c.cfg.Covers = func(string) ([]string, []string) {
		// Between AddDir's first check and registering the request.
		assert.NoError(t, d.c.Close(t.Context()))
		return nil, nil
	}

	err := d.c.AddDir(canonicalTemp(t))

	assert.ErrorIs(t, err, ErrNoSession)
	d.c.mu.Lock()
	defer d.c.mu.Unlock()
	assert.Empty(t, d.c.dirRequests, "a request that no close would end")
}

func TestAddDirNamesTheOnDiskSpelling(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only a case-insensitive file system accepts another spelling")
	}
	d := newDirHarness(t, Config{}, true)
	dir := filepath.Join(canonicalTemp(t), "Other")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello"), 0o600))
	typed := filepath.Join(filepath.Dir(dir), "other")
	if _, err := os.Stat(typed); err != nil {
		t.Skip("the file system is case-sensitive")
	}
	decide(t, d.c, dir, trust.Untrusted)
	notices := subscribeTo[event.Notice](d.c)

	require.NoError(t, d.c.AddDir(typed))

	assert.Equal(t, event.Notice{Text: "Added " + dir + " to the workspace."}, receiveEvent(t, notices))
	assert.Contains(t, d.c.dirsText(0, d.c.dirCount(), true), "includes "+dir+".")
	args, err := json.Marshal(map[string]string{"file_path": filepath.Join(dir, "f.txt")})
	require.NoError(t, err)
	require.NoError(t, d.gate.Before(t.Context(), "c1", "Read", args))
	res, err := tool.NewRead(d.ws).Run(t.Context(), args, func(target policy.Target) (policy.Mode, error) {
		return d.gate.Admit("c1", "Read", args, &target)
	})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "hello")
}

func TestAddDirRequestIsEndedWhenTheControllerCloses(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	requests, ended := subscribeTo[event.DirRequested](d.c), subscribeTo[event.DirEnded](d.c)
	require.NoError(t, d.c.AddDir(canonicalTemp(t)))
	req := receiveEvent(t, requests)

	require.NoError(t, d.c.Close(t.Context()))

	assert.Equal(t, event.DirEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.False(t, d.c.AnswerDir(req.Token, true, true))
}

func TestDirCandidates(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	root := filepath.Dir(d.sb.Workspace)
	for _, name := range []string{"alpha/sub1", "alpha/sub2", "alpha/.git", "Beta", ".hidden", "many"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "zfile"), nil, 0o600))
	require.NoError(t, os.Symlink(filepath.Join(root, "alpha"), filepath.Join(root, "link")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "alpha", "file"), nil, 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(d.sb.Home, "proj"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(d.sb.Workspace, "inside"), 0o700))
	for i := range 60 {
		require.NoError(t, os.Mkdir(filepath.Join(root, "many", fmt.Sprintf("d%02d", i)), 0o700))
	}
	values := func(typed string) []string {
		t.Helper()
		cs, err := d.c.Arguments("add-dir", typed)
		require.NoError(t, err)
		var out []string
		for _, c := range cs {
			out = append(out, c.Value)
		}
		return out
	}

	assert.Equal(t, []string{"../Beta/", "../alpha/", "../home/", "../link/", "../many/"}, values(""), "siblings, without the workspace and Kopi's home, files and hidden ones")
	assert.Equal(t, []string{"../alpha/"}, values("../AL"), "a case-insensitive prefix")
	assert.Equal(t, []string{"../.hidden/"}, values("../."), "hidden ones once a dot is typed")
	assert.Equal(t, []string{"../.hidden/"}, values("."), "no slash is read as a sibling's name")
	assert.Equal(t, []string{root + "/alpha/"}, values(root+"/al"), "absolute")
	assert.Equal(t, []string{"../alpha/", "../alpha/sub1/", "../alpha/sub2/"}, values("../alpha/"), "the typed directory first")
	assert.Contains(t, values("~"), "~/proj/")
	assert.Equal(t, "~/", values("~")[0], "the home itself, as typed")
	assert.Equal(t, []string{"~/proj/"}, values("~/pr"))
	assert.Equal(t, []string{"../alpha/sub1/", "../alpha/sub2/"}, values("../alpha/s"))
	assert.Equal(t, []string{"../alpha/.git/"}, values("../alpha/."))
	assert.Empty(t, values("../nope/x"), "a missing directory lists nothing")
	assert.Empty(t, values("./"), "what is already in the workspace is refused")
	assert.Empty(t, values("../ws/"))

	many := values("../many/")
	require.Len(t, many, maxDirCandidates)
	assert.Equal(t, "../many/", many[0])
	assert.Equal(t, "../many/d48/", many[maxDirCandidates-1])
}
