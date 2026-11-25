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
	"github.com/keyute/kopi/internal/trust"
)

// maxDirCandidates bounds the directories /add-dir offers.
const maxDirCandidates = 50

// addedDir is a directory added to the workspace: its canonical path and the
// instruction files the model is sent with it.
type addedDir struct {
	path  string
	files []instruction.File
}

// pendingDir is an add waiting for the human's answer. arg is the argument as
// written and typed its absolute spelling, which the workspace keeps; dir is
// canonical. files are every instruction file trusting dir would send.
type pendingDir struct {
	arg, typed, dir string
	files           []instruction.File
	// saved is false when the answer is never written to the trust store.
	saved bool
}

// AddDir adds the directory arg names to the workspace until Kopi exits: for
// /add-dir and --add-dir, so it works before a session is open. A directory no
// trust decision covers is not added until AnswerDir says so, unless the
// Controller is unattended, which adds it without trust. The error wraps
// ErrMissingArgument for an empty arg, and otherwise the reason the directory
// cannot join.
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
	typed := c.typedDir(arg)
	if err := c.cfg.Files.CanAdd(typed); err != nil {
		return fmt.Errorf("failed to add %s: %w", arg, err)
	}
	dir, err := trust.Canonical(typed)
	if err != nil {
		return fmt.Errorf("failed to add %s: %w", arg, err)
	}
	store, err := trust.Open(c.cfg.TrustFile)
	if err != nil {
		return fmt.Errorf("failed to add %s: %w", arg, err)
	}
	decision, decided, err := store.Lookup(dir)
	if err != nil {
		return fmt.Errorf("failed to add %s: %w", arg, err)
	}
	p := &pendingDir{arg: arg, typed: typed, dir: dir, files: c.newInstructionFiles(dir)}
	if decided || c.cfg.Unattended {
		return c.applyDir(p, decided && decision == trust.Trusted)
	}
	broad, err := trust.Broad(dir, c.cfg.UserHome)
	if err != nil {
		return fmt.Errorf("failed to add %s: %w", arg, err)
	}
	p.saved = !broad
	req := event.DirRequested{Dir: dir, Saved: p.saved}
	for _, f := range p.files {
		req.Instructions = append(req.Instructions, f.Path)
	}
	if c.cfg.Covers != nil {
		req.Servers, req.Skills = c.cfg.Covers(dir)
	}
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

// AnswerDir resolves the add pending for token and reports whether one was
// pending. With add set it adds the directory, with its instruction files when
// trusted; the decision is saved first, except for a directory whose answer
// holds for this run only.
func (c *Controller) AnswerDir(token uint64, add, trusted bool) bool {
	c.mu.Lock()
	p := c.dirRequests[token]
	delete(c.dirRequests, token)
	c.mu.Unlock()
	if p == nil {
		return false
	}
	c.relay.emit(event.DirEnded{Token: token})
	if !add {
		return true
	}
	if p.saved {
		decision := trust.Untrusted
		if trusted {
			decision = trust.Trusted
		}
		store, err := trust.Open(c.cfg.TrustFile)
		if err == nil {
			err = store.Set(p.dir, decision)
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
	c.mu.Lock()
	tokens := slices.Sorted(maps.Keys(c.dirRequests))
	c.dirRequests = nil
	c.mu.Unlock()
	for _, token := range tokens {
		c.relay.emit(event.DirEnded{Token: token})
	}
}

// applyDir makes p's directory reachable and reports it. The rules take it
// first, so no deny or ask rule lags behind the reach; a failure puts the
// previous directories back.
func (c *Controller) applyDir(p *pendingDir, trusted bool) error {
	var files []instruction.File
	if trusted {
		files = p.files
	}
	c.dirsMu.Lock()
	err := c.reach(p, files)
	c.dirsMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to add %s: %w", p.arg, err)
	}
	text := fmt.Sprintf("Added %s to the workspace.", p.dir)
	switch {
	case len(p.files) == 0:
	case trusted:
		text = fmt.Sprintf("Added %s to the workspace, with its instruction files.", p.dir)
	default:
		text += " Its instruction files were not loaded: the folder is not trusted."
	}
	c.report(event.Notice{Text: text})
	return nil
}

// reach is applyDir's work under dirsMu; files are the instruction files the
// model is sent.
func (c *Controller) reach(p *pendingDir, files []instruction.File) error {
	c.mu.Lock()
	prev := make([]string, len(c.dirs))
	for i, d := range c.dirs {
		prev[i] = d.path
	}
	c.mu.Unlock()
	next := append(slices.Clone(prev), p.dir)
	c.cfg.Rules.SetDirs(next)
	var err error
	if c.cfg.SandboxDirs != nil {
		err = c.cfg.SandboxDirs(next)
	}
	if err == nil {
		err = c.cfg.Files.Add(p.typed, p.dir)
	}
	if err != nil {
		c.cfg.Rules.SetDirs(prev)
		if c.cfg.SandboxDirs != nil {
			if restoreErr := c.cfg.SandboxDirs(prev); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("failed to restore the sandbox directories: %w", restoreErr))
			}
		}
		return err
	}
	c.mu.Lock()
	c.dirs = append(c.dirs, addedDir{path: p.dir, files: files})
	c.mu.Unlock()
	return nil
}

// typedDir is arg as the absolute path the human spelled: ~ is the user's home,
// and a relative path starts at the start directory.
func (c *Controller) typedDir(arg string) string {
	switch {
	case arg == "~":
		arg = c.cfg.UserHome
	case strings.HasPrefix(arg, "~/"):
		arg = filepath.Join(c.cfg.UserHome, arg[2:])
	case !filepath.IsAbs(arg):
		arg = filepath.Join(c.cfg.Workspace, arg)
	}
	return filepath.Clean(arg)
}

// newInstructionFiles reads the instruction files of dir, from the git root
// down, that the model has not been sent yet. They are held in memory until the
// human trusts dir.
func (c *Controller) newInstructionFiles(dir string) []instruction.File {
	sent := map[string]bool{}
	for _, g := range c.cfg.Instructions {
		for _, f := range g.Files {
			sent[f.Path] = true
		}
	}
	c.mu.Lock()
	for _, d := range c.dirs {
		for _, f := range d.files {
			sent[f.Path] = true
		}
	}
	c.mu.Unlock()
	groups, errs := instruction.Load("", []instruction.Root{{Dir: dir, Trusted: true}}, c.cfg.InstructionFallbacks)
	for _, err := range errs {
		c.report(event.Error{Err: err})
	}
	var files []instruction.File
	for _, g := range groups {
		for _, f := range g.Files {
			if !sent[f.Path] {
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
	evs, open, closed := c.reports, c.main.sess != nil && c.switching == nil, c.closed
	c.reports = nil
	c.mu.Unlock()
	switch {
	case closed:
	case open:
		for _, ev := range evs {
			c.relay.emit(ev)
		}
	default:
		c.startup = append(c.startup, evs...)
	}
}

// dirCount is how many directories have been added.
func (c *Controller) dirCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.dirs)
}

// dirsText is what the model is told of the added directories from..to, "" for
// none: the directories and, for those added with their instruction files, the
// files.
func (c *Controller) dirsText(from, to int) string {
	c.mu.Lock()
	dirs := c.dirs
	c.mu.Unlock()
	if from >= to {
		return ""
	}
	var paths, blocks []string
	for _, d := range dirs[from:to] {
		paths = append(paths, d.path)
		for _, f := range d.files {
			blocks = append(blocks, instructionText(f))
		}
	}
	text := "The workspace also includes " + strings.Join(paths, ", ") + ". Give a path there as an absolute path; relative paths and Bash still start from " + c.cfg.Workspace + "."
	if len(blocks) > 0 {
		text += "\n" + strings.Join(blocks, "\n\n")
	}
	return text
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
	if last == "" && strings.HasSuffix(typed, "/") && c.cfg.Files.CanAdd(c.typedDir(head)) == nil {
		out = append(out, Candidate{Value: typed})
	}
	dir := c.typedDir(head)
	entries, err := os.ReadDir(dir)
	if err != nil {
		// The human is still typing a path that may not exist yet.
		return out, nil
	}
	for _, e := range entries {
		name := e.Name()
		if len(out) == maxDirCandidates {
			break
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(last, ".") || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(last)) {
			continue
		}
		path := filepath.Join(dir, name)
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
