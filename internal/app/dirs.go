package app

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/trust"
)

// maxDirCandidates bounds the directories /add-dir offers.
const maxDirCandidates = 50

// addedDir is a directory added to the workspace and the instruction files the
// model is sent with it: every one its walk finds that the system prompt does
// not already carry. removed marks the entry that takes dir out of the
// workspace again.
type addedDir struct {
	dir     tool.Dir
	files   []instruction.File
	removed bool
}

// pendingDir is an add waiting for the human's answer. arg is the argument as
// written. files are every instruction file trusting dir would send.
type pendingDir struct {
	arg   string
	dir   tool.Dir
	files []instruction.File
	info  os.FileInfo
	// saved is false when the answer is never written to the trust store.
	saved bool
}

// AddDir adds the directory arg names to the workspace until RemoveDir removes
// it or Kopi exits: for /add-dir and --add-dir, so it works before a session is
// open. A directory no trust decision covers is not added until AnswerDir says
// so, unless the Controller is unattended, which adds it without trust. The
// error wraps ErrMissingArgument for an empty arg, and otherwise the reason the
// directory cannot join.
func (c *Controller) AddDir(arg string) error {
	if arg == "" {
		return fmt.Errorf("%w: /add-dir needs a directory", ErrMissingArgument)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrNoSession
	}
	p, decision, decided, err := c.prepareDir(arg)
	if err != nil {
		return fmt.Errorf("failed to add %s: %w", arg, err)
	}
	if decided || c.cfg.Unattended {
		return c.applyDir(p, decided && decision == trust.Trusted)
	}
	req := event.DirRequested{Dir: p.dir.Real, Saved: p.saved}
	c.mu.Lock()
	sent := c.sentFilesLocked()
	c.mu.Unlock()
	for _, f := range p.files {
		if !sent[f.Path] {
			req.Instructions = append(req.Instructions, f.Path)
		}
	}
	if c.cfg.Covers != nil {
		req.Servers, req.Skills = c.cfg.Covers(p.dir.Real)
	}
	// The registration and its event stay ordered against the end of the
	// request, or a dialog could open after its DirEnded.
	c.dirEventsMu.Lock()
	defer c.dirEventsMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrNoSession
	}
	c.requestSeq++
	req.Token = c.requestSeq
	if c.dirRequests == nil {
		c.dirRequests = map[uint64]*pendingDir{}
	}
	c.dirRequests[req.Token] = p
	c.mu.Unlock()
	c.relay.emit(req)
	return nil
}

// AddDirs adds each of the --add-dir args once: a folder named twice, or
// through a symlink, joins once and asks once. Shells leave ~ literal after =,
// as typedDir expands it. An error stops at the arg that caused it.
func (c *Controller) AddDirs(args []string) error {
	added := map[string]bool{}
	for _, arg := range args {
		if real, err := trust.Canonical(c.typedDir(arg)); err == nil {
			if added[real] {
				continue
			}
			added[real] = true
		}
		if err := c.AddDir(arg); err != nil {
			return err
		}
	}
	return nil
}

// prepareDir checks that arg can join the workspace and looks up the trust
// decision for it.
func (c *Controller) prepareDir(arg string) (p *pendingDir, decision trust.Decision, decided bool, err error) {
	given := c.typedDir(arg)
	if err := c.cfg.Files.CanAdd(given); err != nil {
		return nil, 0, false, err
	}
	real, err := trust.Canonical(given)
	if err != nil {
		return nil, 0, false, err
	}
	dir := tool.Dir{Given: given, Real: real}
	if c.cfg.ValidateSandboxDirs != nil {
		c.dirsMu.Lock()
		c.mu.Lock()
		_, roots := dirNames(liveDirs(c.dirs))
		c.mu.Unlock()
		roots = append(roots, real)
		err := c.cfg.ValidateSandboxDirs(roots)
		c.dirsMu.Unlock()
		if err != nil {
			return nil, 0, false, err
		}
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, 0, false, err
	}
	store, err := trust.Open(c.cfg.TrustFile)
	if err != nil {
		return nil, 0, false, err
	}
	decision, decided, err = store.Lookup(dir.Real)
	if err != nil {
		return nil, 0, false, err
	}
	p = &pendingDir{arg: arg, dir: dir, info: info, files: c.newInstructionFiles(dir.Real), saved: !trust.Broad(dir.Real, c.cfg.Home.Real)}
	return p, decision, decided, nil
}

// AnswerDir resolves the add pending for token and reports whether one was
// pending. With add set it adds the directory, with its instruction files when
// trusted; the decision is saved first, except for a directory whose answer
// holds for this run only.
func (c *Controller) AnswerDir(token uint64, add, trusted bool) bool {
	c.dirEventsMu.Lock()
	c.mu.Lock()
	p := c.dirRequests[token]
	delete(c.dirRequests, token)
	c.mu.Unlock()
	if p != nil {
		c.relay.emit(event.DirEnded{Token: token})
	}
	c.dirEventsMu.Unlock()
	if p == nil {
		return false
	}
	if !add {
		return true
	}
	// Another pending add of the folder may have been answered first.
	if err := c.cfg.Files.CanAdd(p.dir.Real); err != nil {
		c.report(event.Error{Err: fmt.Errorf("failed to add %s: %w", p.arg, err)})
		return true
	}
	real, err := trust.Canonical(p.dir.Given)
	if err == nil && real == p.dir.Real {
		var info os.FileInfo
		info, err = os.Stat(real)
		if err == nil && !os.SameFile(p.info, info) {
			err = errors.New("directory identity changed")
		}
	} else if err == nil {
		err = errors.New("directory identity changed")
	}
	if err != nil {
		c.report(event.Error{Err: fmt.Errorf("failed to add %s: %w", p.arg, err)})
		return true
	}
	if p.saved {
		decision := trust.Untrusted
		if trusted {
			decision = trust.Trusted
		}
		store, err := trust.Open(c.cfg.TrustFile)
		if err == nil {
			err = store.SetCanonical(p.dir.Real, decision)
		}
		if err != nil {
			c.report(event.Error{Err: fmt.Errorf("failed to save the trust decision: %w", err)})
			return true
		}
	}
	if err := c.applyDir(p, trusted); err != nil {
		c.report(event.Error{Err: err})
	}
	return true
}

// endDirRequests ends every add still waiting, as the Controller closes.
func (c *Controller) endDirRequests() {
	c.dirEventsMu.Lock()
	defer c.dirEventsMu.Unlock()
	c.mu.Lock()
	tokens := slices.Sorted(maps.Keys(c.dirRequests))
	c.dirRequests = nil
	c.mu.Unlock()
	for _, token := range tokens {
		c.relay.emit(event.DirEnded{Token: token})
	}
}

// applyDir makes p's directory reachable and reports it.
func (c *Controller) applyDir(p *pendingDir, trusted bool) error {
	c.dirsMu.Lock()
	defer c.dirsMu.Unlock()
	c.mu.Lock()
	prev := liveDirs(c.dirs)
	c.mu.Unlock()
	next := append(slices.Clone(prev), addedDir{dir: p.dir})
	// A failure puts the previous directories back.
	err := c.setReach(next)
	if err == nil {
		err = c.cfg.Files.Add(p.dir)
	}
	if err != nil {
		if restoreErr := c.setReach(prev); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to restore the sandbox directories: %w", restoreErr))
		}
		return fmt.Errorf("failed to add %s: %w", p.arg, err)
	}
	c.mu.Lock()
	// Another pending add under the same git root may have been applied since
	// files were listed.
	sent := c.sentFilesLocked()
	fresh := slices.DeleteFunc(slices.Clone(p.files), func(f instruction.File) bool { return sent[f.Path] })
	entry := addedDir{dir: p.dir}
	if trusted {
		entry.files = p.files
	}
	c.dirs = append(c.dirs, entry)
	c.mu.Unlock()
	text := fmt.Sprintf("Added %s to the workspace.", p.dir.Real)
	switch {
	case len(fresh) == 0:
	case trusted:
		text = fmt.Sprintf("Added %s to the workspace, with its instruction files.", p.dir.Real)
	default:
		text += " Its instruction files were not loaded: the folder is not trusted."
	}
	c.report(event.Notice{Text: text})
	return nil
}

// RemoveDir removes the added directory arg names from the workspace, for
// /remove-dir. arg matches the spelling a directory was added in, its real path,
// or the real path of what arg resolves to. The error wraps ErrMissingArgument
// for an empty arg, ErrNotAdded when no added directory matches, and otherwise
// the reason the sandbox refused the change.
func (c *Controller) RemoveDir(arg string) error {
	if arg == "" {
		return fmt.Errorf("%w: /remove-dir needs a directory", ErrMissingArgument)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrNoSession
	}
	c.dirsMu.Lock()
	defer c.dirsMu.Unlock()
	c.mu.Lock()
	prev := liveDirs(c.dirs)
	c.mu.Unlock()
	typed := c.typedDir(arg)
	i := slices.IndexFunc(prev, func(d addedDir) bool { return typed == d.dir.Given || typed == d.dir.Real })
	if i < 0 {
		if real, err := trust.Canonical(typed); err == nil {
			i = slices.IndexFunc(prev, func(d addedDir) bool { return real == d.dir.Real })
		}
	}
	if i < 0 {
		return fmt.Errorf("failed to remove %s: %w", arg, ErrNotAdded)
	}
	removed := prev[i]
	next := slices.Delete(slices.Clone(prev), i, i+1)
	// Reach goes before the rules, so no deny or ask rule is dropped while the
	// directory can still be reached.
	if c.cfg.SandboxDirs != nil {
		_, real := dirNames(next)
		if err := c.cfg.SandboxDirs(real); err != nil {
			_, prevReal := dirNames(prev)
			if restoreErr := c.cfg.SandboxDirs(prevReal); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("failed to restore the sandbox directories: %w", restoreErr))
			}
			return fmt.Errorf("failed to remove %s: %w", arg, err)
		}
	}
	c.cfg.Files.Remove(removed.dir)
	names, _ := dirNames(next)
	c.cfg.Rules.SetDirs(names)
	c.mu.Lock()
	c.dirs = append(c.dirs, addedDir{dir: removed.dir, removed: true})
	c.mu.Unlock()
	text := fmt.Sprintf("Removed %s from the workspace.", removed.dir.Real)
	for _, d := range next {
		if insideDir(removed.dir.Real, d.dir.Real) {
			text += fmt.Sprintf(" It is still reachable inside %s.", d.dir.Real)
			break
		}
	}
	c.report(event.Notice{Text: text})
	return nil
}

// removeDirCandidates lists the added directories, by real path, in the order
// they were added.
func (c *Controller) removeDirCandidates(_ string) ([]Candidate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	live := liveDirs(c.dirs)
	out := make([]Candidate, len(live))
	for i, d := range live {
		out[i] = Candidate{Value: d.dir.Real}
	}
	return out, nil
}

// dirNames is the names of dirs the rules take, both of each, and the real
// paths the sandbox takes.
func dirNames(dirs []addedDir) (names, real []string) {
	real = make([]string, len(dirs))
	for i, d := range dirs {
		names = append(names, d.dir.Given)
		if d.dir.Real != d.dir.Given {
			names = append(names, d.dir.Real)
		}
		real[i] = d.dir.Real
	}
	return names, real
}

// setReach makes dirs the added directories of the rules and of the sandbox. The
// rules go first, so no deny or ask rule lags behind the reach.
func (c *Controller) setReach(dirs []addedDir) error {
	names, real := dirNames(dirs)
	c.cfg.Rules.SetDirs(names)
	if c.cfg.SandboxDirs == nil {
		return nil
	}
	return c.cfg.SandboxDirs(real)
}

// insideDir reports whether path is dir or lies inside it, by name.
func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

// typedDir is arg as the absolute path the human spelled: ~ is the user's home as
// spelled, and a relative path starts at the start directory.
func (c *Controller) typedDir(arg string) string {
	switch {
	case arg == "~":
		arg = c.cfg.Home.Given
	case strings.HasPrefix(arg, "~/"):
		arg = c.cfg.Home.Given + string(filepath.Separator) + arg[2:]
	case !filepath.IsAbs(arg):
		arg = c.cfg.Workspace + string(filepath.Separator) + arg
	}
	return arg
}

// promptFiles is the paths of the instruction files the system prompt carries.
func (c *Controller) promptFiles() map[string]bool {
	files := map[string]bool{}
	for _, g := range c.cfg.Instructions {
		for _, f := range g.Files {
			files[f.Path] = true
		}
	}
	return files
}

// sentFilesLocked is the paths of the instruction files the system prompt
// carries and those of the directories now added. It must be called with mu
// held.
func (c *Controller) sentFilesLocked() map[string]bool {
	sent := c.promptFiles()
	for _, d := range liveDirs(c.dirs) {
		for _, f := range d.files {
			sent[f.Path] = true
		}
	}
	return sent
}

// newInstructionFiles reads the instruction files of dir, from the git root
// down, that the system prompt does not already carry. They are held in memory
// until the human trusts dir.
func (c *Controller) newInstructionFiles(dir string) []instruction.File {
	held := c.promptFiles()
	groups, errs := instruction.Load("", []instruction.Root{{Dir: dir, Trusted: true}}, c.cfg.InstructionFallbacks)
	for _, err := range errs {
		c.report(event.Error{Err: err})
	}
	var files []instruction.File
	for _, g := range groups {
		for _, f := range g.Files {
			if !held[f.Path] {
				files = append(files, f)
			}
		}
	}
	return files
}

// report shows evs in order once no open is in flight, or after the first
// session opens when none is open, since that session's SessionChanged clears
// the transcript. The caller may be the frontend's event loop, which must not
// wait for a switch, so it flushes at once only when TryLock finds life free and
// otherwise hands the flush to a goroutine. As for switchTo, that goroutine is
// not in wg, since Close holds life across wg.Wait, and it shows nothing once
// closed.
func (c *Controller) report(evs ...event.Event) {
	c.mu.Lock()
	c.reports = append(c.reports, evs...)
	c.mu.Unlock()
	if c.life.TryLock() {
		defer c.life.Unlock()
		c.flushReports()
		return
	}
	go func() {
		c.life.Lock()
		defer c.life.Unlock()
		c.flushReports()
	}()
}

// flushReports shows what report queued. It runs with life held, so no open is
// in flight: an open session has already reported its SessionChanged and
// flushed startup, and a missing one will flush what is appended. A switch that
// is registered but has not taken life yet is in the same case: it has not
// replaced the open session, whose SessionChanged will clear what is shown now,
// and it flushes startup after its own.
func (c *Controller) flushReports() {
	c.mu.Lock()
	evs, closed := c.reports, c.closed
	c.reports = nil
	c.mu.Unlock()
	if !closed {
		c.emitOrQueue(evs)
	}
}

// dirCount is how many adds and removals have been logged.
func (c *Controller) dirCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.dirs)
}

// liveAt is the positions in log of the adds still in the workspace after its
// first n entries: a removal cancels the live entry with the same real path.
func liveAt(log []addedDir, n int) []int {
	var live []int
	for i, d := range log[:n] {
		if !d.removed {
			live = append(live, i)
			continue
		}
		live = slices.DeleteFunc(live, func(j int) bool { return log[j].dir.Real == d.dir.Real })
	}
	return live
}

// liveDirs is the directories now added to the workspace, in the order added.
func liveDirs(log []addedDir) []addedDir {
	var out []addedDir
	for _, i := range liveAt(log, len(log)) {
		out = append(out, log[i])
	}
	return out
}

// dirsText is what the model is told of the changes the log holds from..to, ""
// for none: the directories that left the workspace, and those added, with
// their instruction files unless files is false. A directory is told as it
// stands at to against at from, so an add undone inside the window says nothing,
// and a directory still inside another added one is not called gone. A file is
// not sent again while a directory live at from holds it.
func (c *Controller) dirsText(from, to int, files bool) string {
	if from >= to {
		return ""
	}
	c.mu.Lock()
	log := c.dirs[:to]
	c.mu.Unlock()
	before, after := liveAt(log, from), liveAt(log, to)
	var gone []string
	for _, b := range before {
		real := log[b].dir.Real
		if !slices.ContainsFunc(after, func(a int) bool {
			return insideDir(real, log[a].dir.Real)
		}) {
			gone = append(gone, real)
		}
	}
	var lines []string
	if len(gone) > 0 {
		lines = append(lines, "The workspace no longer includes "+strings.Join(gone, ", ")+".")
	}
	held := map[string]bool{}
	for _, b := range before {
		for _, f := range log[b].files {
			held[f.Path] = true
		}
	}
	var paths, blocks []string
	for _, a := range after {
		if slices.Contains(before, a) {
			continue
		}
		paths = append(paths, log[a].dir.Real)
		if !files {
			continue
		}
		for _, f := range log[a].files {
			if !held[f.Path] {
				held[f.Path] = true
				blocks = append(blocks, instructionText(f))
			}
		}
	}
	if len(paths) > 0 {
		text := "The workspace also includes " + strings.Join(paths, ", ") + ". Give a path there as an absolute path; relative paths and Bash still start from " + c.cfg.Workspace + "."
		if len(blocks) > 0 {
			text += "\n" + strings.Join(blocks, "\n\n")
		}
		lines = append(lines, text)
	}
	return strings.Join(lines, "\n")
}

// dirCandidates lists the directories typed continues: the entries of its last
// slash-separated directory that start with what follows the slash. Nothing
// typed lists the start directory's siblings. Every value keeps the typed
// spelling of the directory, so it round-trips through AddDir.
func (c *Controller) dirCandidates(typed string) ([]Candidate, error) {
	if typed == "~" {
		typed = "~/"
	}
	head, last := "../", typed
	if i := strings.LastIndex(typed, "/"); i >= 0 {
		head, last = typed[:i+1], typed[i+1:]
	}
	var out []Candidate
	dir := c.typedDir(head)
	if last == "" && strings.HasSuffix(typed, "/") && c.cfg.Files.CanAdd(dir) == nil {
		out = append(out, Candidate{Value: typed})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// The human is still typing a path that may not exist yet.
		return out, nil
	}
	// The exact name comes first, since the match ignores case.
	slices.SortStableFunc(entries, func(a, b os.DirEntry) int {
		switch {
		case a.Name() == last:
			return -1
		case b.Name() == last:
			return 1
		}
		return 0
	})
	for _, e := range entries {
		name := e.Name()
		if len(out) == maxDirCandidates {
			break
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(last, ".") || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(last)) {
			continue
		}
		path := dir + string(filepath.Separator) + name
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			info, err := os.Stat(path)
			isDir = err == nil && info.IsDir()
		}
		if isDir && c.cfg.Files.CanAdd(path) == nil {
			out = append(out, Candidate{Value: head + name + "/"})
		}
	}
	return out, nil
}
