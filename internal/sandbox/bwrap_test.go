package sandbox

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvedTemp is a fresh directory named without symlinks, as the mounts
// name it.
func resolvedTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(d, 0o700))
	}
}

// mounts returns the mount section of args: between the root bind and the
// device and proc mounts.
func mounts(t *testing.T, args []string) []string {
	t.Helper()
	i := slices.Index(args, "/")
	require.Positive(t, i)
	require.Equal(t, []string{"--ro-bind", "/", "/"}, args[i-1:i+2])
	require.Equal(t, bwrapTail, args[len(args)-len(bwrapTail):])
	return args[i+2 : len(args)-len(bwrapTail)]
}

func ro(path string) []string           { return []string{"--ro-bind", path, path} }
func rw(path string) []string           { return []string{"--bind", path, path} }
func null(path string) []string         { return []string{"--ro-bind", "/dev/null", path} }
func concat(parts ...[]string) []string { return slices.Concat(parts...) }

func TestBwrapArgsOrder(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	mkdirs(t, filepath.Join(root, ".git", "hooks"), filepath.Join(root, "secret"))

	args, made, err := bwrapArgs(Policy{
		Write:     []string{root},
		DenyRead:  []string{filepath.Join(root, "secret")},
		DenyWrite: []string{filepath.Join(root, ".git", "hooks"), filepath.Join(root, ".mcp.json")},
	})

	require.NoError(t, err)
	assert.Equal(t, concat(
		[]string{"--die-with-parent", "--unshare-user", "--unshare-pid", "--cap-drop", "ALL", "--unshare-net"},
		ro("/"),
		ro(filepath.Join(root, ".git")),
		rw(root),
		ro(filepath.Join(root, ".git", "hooks")),
		null(filepath.Join(root, ".mcp.json")),
		emptyDir(filepath.Join(root, "secret")),
		[]string{"--dev", "/dev", "--proc", "/proc", "--seccomp", "3", "--"},
	), args)
	assert.Equal(t, []string{filepath.Join(root, ".mcp.json")}, made)
}

func TestBwrapArgsNetwork(t *testing.T) {
	for _, network := range []bool{false, true} {
		args, _, err := bwrapArgs(Policy{Network: network})
		require.NoError(t, err)
		assert.Equal(t, !network, slices.Contains(args, "--unshare-net"), network)
		assert.NotContains(t, args, "--new-session")
		assert.Contains(t, args, "--unshare-pid")
		assert.Contains(t, args, "--die-with-parent")
		assert.Equal(t, "ALL", args[slices.Index(args, "--cap-drop")+1])
	}
}

func TestBwrapArgsWriteRoots(t *testing.T) {
	base := resolvedTemp(t)
	outer, inner, missing := filepath.Join(base, "outer"), filepath.Join(base, "outer", "inner"), filepath.Join(base, "missing")
	mkdirs(t, inner)
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(outer, link))

	args, _, err := bwrapArgs(Policy{Write: []string{inner, missing, link, outer}})

	require.NoError(t, err)
	assert.Equal(t, concat(rw(outer), rw(inner)), mounts(t, args))
}

func TestBwrapArgsPinsBeforeWriteRoots(t *testing.T) {
	base := resolvedTemp(t)
	outer, inner := filepath.Join(base, "outer"), filepath.Join(base, "outer", "a", "inner")
	deep := filepath.Join(inner, "b", "c", "d")
	mkdirs(t, deep, filepath.Join(base, "elsewhere", "x"))

	args, _, err := bwrapArgs(Policy{
		Write:     []string{outer, inner},
		DenyWrite: []string{deep, filepath.Join(base, "elsewhere", "x")},
	})

	require.NoError(t, err)
	assert.Equal(t, concat(
		ro(filepath.Join(outer, "a")),
		ro(filepath.Join(inner, "b")),
		ro(filepath.Join(inner, "b", "c")),
		rw(outer), rw(inner),
		ro(deep),
	), mounts(t, args))
}

func TestBwrapArgsDenyAfterAllow(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	ws, nested := filepath.Join(root, "ws"), filepath.Join(root, "ws", "sub")
	mkdirs(t, nested)

	args, _, err := bwrapArgs(Policy{Write: []string{root, nested}, DenyWrite: []string{ws}})

	require.NoError(t, err)
	assert.Equal(t, concat(rw(root), rw(nested), ro(ws)), mounts(t, args))
}

func TestBwrapArgsPlaceholderKind(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	mkdirs(t, root)
	a, file := filepath.Join(root, "a"), filepath.Join(root, "file")

	args, made, err := bwrapArgs(Policy{
		Write:     []string{root},
		DenyRead:  []string{filepath.Join(a, "c")},
		DenyWrite: []string{filepath.Join(a, "b", "x"), file},
	})

	require.NoError(t, err)
	assert.Equal(t, concat(rw(root), emptyDir(a), null(file)), mounts(t, args))
	assert.Equal(t, []string{a, file}, made)
}

func TestBwrapArgsNoPlaceholderWhereReadOnly(t *testing.T) {
	base := resolvedTemp(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	ws, hidden := filepath.Join(root, "ws"), filepath.Join(root, "hidden")
	mkdirs(t, ws, hidden, outside)

	args, made, err := bwrapArgs(Policy{
		Write:    []string{root},
		DenyRead: []string{hidden},
		DenyWrite: []string{
			filepath.Join(outside, "missing"),
			filepath.Join(ws, "missing"),
			ws,
			filepath.Join(hidden, "missing"),
		},
	})

	require.NoError(t, err)
	assert.Equal(t, concat(rw(root), ro(ws), emptyDir(hidden)), mounts(t, args))
	assert.Empty(t, made)
}

func TestBwrapArgsFileOnTheWay(t *testing.T) {
	base := resolvedTemp(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	mkdirs(t, root, outside)
	git := filepath.Join(root, ".git")
	writeFile(t, git)
	writeFile(t, filepath.Join(outside, "file"))

	for _, tc := range []struct {
		name string
		deny []string
		want []string
	}{
		{"held", []string{filepath.Join(git, "hooks"), filepath.Join(git, "config")}, ro(git)},
		{"denied itself", []string{filepath.Join(git, "hooks"), git}, ro(git)},
		{"read-only", []string{filepath.Join(outside, "file", "x")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, made, err := bwrapArgs(Policy{Write: []string{root}, DenyWrite: tc.deny})

			require.NoError(t, err)
			assert.Equal(t, concat(rw(root), tc.want), mounts(t, args))
			assert.Empty(t, made)
		})
	}
}

func TestBwrapArgsRefusesSymlinkInWriteRoot(t *testing.T) {
	base := resolvedTemp(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	mkdirs(t, root, outside)
	require.NoError(t, os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(root, ".mcp.json")))
	require.NoError(t, os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(outside, "link")))

	_, _, err := bwrapArgs(Policy{Write: []string{root}, DenyWrite: []string{filepath.Join(root, ".mcp.json")}})
	require.ErrorIs(t, err, ErrDeniedSymlink)
	assert.Contains(t, err.Error(), filepath.Join(root, ".mcp.json"))

	args, made, err := bwrapArgs(Policy{Write: []string{root}, DenyRead: []string{filepath.Join(outside, "link")}})
	require.NoError(t, err)
	assert.Equal(t, rw(root), mounts(t, args))
	assert.Empty(t, made)
}

func TestBwrapArgsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	base := resolvedTemp(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	mkdirs(t, filepath.Join(root, "locked"), filepath.Join(outside, "locked"))
	for _, d := range []string{filepath.Join(root, "locked"), filepath.Join(outside, "locked")} {
		require.NoError(t, os.Chmod(d, 0))
		t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
	}

	_, _, err := bwrapArgs(Policy{Write: []string{root}, DenyWrite: []string{filepath.Join(root, "locked", "x")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to inspect denied path")

	args, _, err := bwrapArgs(Policy{Write: []string{root}, DenyRead: []string{filepath.Join(outside, "locked", "x")}})
	require.NoError(t, err)
	assert.Equal(t, rw(root), mounts(t, args))
}

func TestBwrapArgsDenyRead(t *testing.T) {
	base := resolvedTemp(t)
	root, home := filepath.Join(base, "root"), filepath.Join(base, "home")
	mkdirs(t, root, filepath.Join(home, "inner"))
	writeFile(t, filepath.Join(root, "key"))

	args, _, err := bwrapArgs(Policy{
		Write:     []string{root},
		DenyRead:  []string{home, filepath.Join(home, "inner"), filepath.Join(root, "key")},
		DenyWrite: []string{filepath.Join(root, "key"), filepath.Join(home, "inner")},
	})

	require.NoError(t, err)
	assert.Equal(t, concat(rw(root), emptyDir(home), null(filepath.Join(root, "key"))), mounts(t, args))
}

func TestBwrapArgsPlanProfile(t *testing.T) {
	tmp := resolvedTemp(t)
	ws := filepath.Join(tmp, "ws")
	home := filepath.Join(ws, "home")
	mkdirs(t, filepath.Join(ws, ".git", "hooks"), home)
	p := Policy{
		Write:     []string{ws, tmp},
		DenyRead:  []string{home},
		DenyWrite: []string{filepath.Join(ws, ".git", "hooks"), filepath.Join(ws, ".git", "config"), filepath.Join(ws, ".mcp.json")},
	}
	p.DenyWrite = append(p.DenyWrite, ws)

	args, made, err := bwrapArgs(p)

	require.NoError(t, err)
	assert.Equal(t, concat(
		ro(filepath.Join(ws, ".git")),
		rw(tmp), rw(ws),
		ro(filepath.Join(ws, ".git", "hooks")),
		ro(ws),
		emptyDir(home),
	), mounts(t, args))
	assert.Empty(t, made)
}

func TestPlaceholdersRemovedAfterLastCommand(t *testing.T) {
	dir := resolvedTemp(t)
	empty, full := filepath.Join(dir, "empty"), filepath.Join(dir, "full")
	parent, child := filepath.Join(dir, "parent"), filepath.Join(dir, "parent", "child")
	kept := filepath.Join(dir, "kept")
	mkdirs(t, child, kept)
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	writeFile(t, full)
	writeFile(t, filepath.Join(kept, "f"))
	pl := &placeholders{}

	pl.start()
	pl.start()
	pl.add([]string{empty, full, parent, kept})
	pl.add([]string{child})
	pl.finish()
	for _, p := range []string{empty, full, child, kept} {
		_, err := os.Lstat(p)
		assert.NoError(t, err, p)
	}

	pl.finish()
	for _, p := range []string{empty, child, parent} {
		_, err := os.Lstat(p)
		assert.ErrorIs(t, err, os.ErrNotExist, p)
	}
	assert.FileExists(t, full)
	assert.DirExists(t, kept)
	assert.Empty(t, pl.made)
}

// newBwrap skips off Linux and where bubblewrap cannot run.
func newBwrap(t *testing.T, policies ...Policy) []*Sandbox {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is the Linux sandbox")
	}
	sbs, err := New(t.Context(), policies...)
	if err != nil {
		require.ErrorIs(t, err, ErrUnavailable)
		t.Skip(err)
	}
	return sbs
}

func gitEnv() []string {
	return []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=k", "GIT_AUTHOR_EMAIL=k@example.com", "GIT_COMMITTER_NAME=k", "GIT_COMMITTER_EMAIL=k@example.com"}
}

func TestBwrapGitCommit(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	git := filepath.Join(root, ".git")
	// Any git init writes .git/config, which the policy denies, so the
	// repository is made outside the sandbox.
	create := exec.CommandContext(t.Context(), "git", "init", "-q", root)
	create.Env = append(os.Environ(), gitEnv()...)
	out, err := create.CombinedOutput()
	require.NoError(t, err, string(out))
	config, err := os.ReadFile(filepath.Join(git, "config"))
	require.NoError(t, err)
	var deny []string
	for _, name := range []string{"hooks", "config", "config.worktree", "worktrees", "modules"} {
		deny = append(deny, filepath.Join(git, name))
	}
	sb := newBwrap(t, Policy{Write: []string{root}, DenyWrite: append(deny, filepath.Join(root, ".mcp.json"))})[0]
	env := append(gitEnv(), "ROOT="+root)

	require.NoError(t, bash(t, sb, `cd "$ROOT" && echo x >f && git add f && git commit -q -m m && git log --oneline`, env...))
	for _, script := range []string{
		`echo x >"$ROOT/.git/hooks/pre-commit"`,
		`echo y >>"$ROOT/.git/config"`,
		`git -C "$ROOT" config core.hooksPath "$ROOT/h"`,
		`mv "$ROOT/.git" "$ROOT/g"`,
		`mv "$ROOT/.git/hooks" "$ROOT/h"`,
	} {
		assert.Error(t, bash(t, sb, script, env...), script)
	}
	rev := exec.CommandContext(t.Context(), "git", "-C", root, "rev-parse", "HEAD")
	rev.Env = append(os.Environ(), gitEnv()...)
	out, err = rev.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.NoFileExists(t, filepath.Join(git, "hooks", "pre-commit"))
	got, err := os.ReadFile(filepath.Join(git, "config"))
	require.NoError(t, err)
	assert.Equal(t, string(config), string(got))
	for _, name := range []string{"config.worktree", "worktrees", "modules"} {
		_, err := os.Lstat(filepath.Join(git, name))
		assert.ErrorIs(t, err, os.ErrNotExist, name)
	}
}

func TestBwrapMissingDeniedDirectoryCannotBeCreated(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	mkdirs(t, root)
	sb := newBwrap(t, Policy{Write: []string{root}, DenyWrite: []string{filepath.Join(root, "a", "b")}})[0]
	env := []string{"ROOT=" + root}

	require.NoError(t, bash(t, sb, `test -d "$ROOT/a" && ls -A "$ROOT/a"`, env...))
	for _, script := range []string{
		`mkdir -p "$ROOT/a/b"`,
		`touch "$ROOT/a/x"`,
		`rmdir "$ROOT/a"`,
		`mv "$ROOT/a" "$ROOT/moved"`,
	} {
		assert.Error(t, bash(t, sb, script, env...), script)
	}
	_, err := os.Lstat(filepath.Join(root, "a"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.NoDirExists(t, filepath.Join(root, "moved"))
}

func TestBwrapExistingEmptyDeniedPathStays(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	mkdirs(t, root)
	kept := filepath.Join(root, "kept")
	require.NoError(t, os.WriteFile(kept, nil, 0o600))
	sb := newBwrap(t, Policy{Write: []string{root}, DenyWrite: []string{kept}})[0]

	assert.Error(t, bash(t, sb, `echo x >"$KEPT"`, "KEPT="+kept))
	assert.FileExists(t, kept)
}

// held is a sandboxed command waiting for its input to close.
type held struct {
	cmd    *exec.Cmd
	done   func()
	stdin  io.WriteCloser
	stderr bytes.Buffer
}

// hold starts a command in sb and returns once it is running, with its
// mounts in place.
func hold(t *testing.T, sb *Sandbox) *held {
	t.Helper()
	cmd, done, err := sb.Command([]string{"/bin/bash", "-c", `echo ready; read -r _ || true`})
	require.NoError(t, err)
	h := &held{cmd: cmd, done: done}
	h.stdin, err = cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = &h.stderr
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = cmd.Wait()
		done()
		t.Fatalf("failed to start held command: %v: %s", err, h.stderr.String())
	}
	require.Equal(t, "ready\n", line)
	return h
}

func (h *held) release(t *testing.T) {
	t.Helper()
	require.NoError(t, h.stdin.Close())
	err := h.cmd.Wait()
	h.done()
	require.NoError(t, err, h.stderr.String())
}

func TestBwrapPlaceholderOutlivesConcurrentCommand(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	mkdirs(t, root)
	protected := filepath.Join(root, "protected")
	p := Policy{Write: []string{root}, DenyWrite: []string{protected}}
	sbs := newBwrap(t, p, p)

	first := hold(t, sbs[0])
	assert.FileExists(t, protected)
	second := hold(t, sbs[1])

	first.release(t)
	assert.FileExists(t, protected)
	second.release(t)
	_, err := os.Lstat(protected)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestBwrapBackgroundProcessEndsWithCommand(t *testing.T) {
	sb := newBwrap(t, Policy{})[0]
	cmd, done, err := sb.Command([]string{"/bin/bash", "-c", `sleep 600 </dev/null >/dev/null 2>&1 &`})
	require.NoError(t, err)
	defer done()
	// Every process the command starts inherits w as fd 4, so r reaches its
	// end once they have all gone.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	cmd.ExtraFiles = append(cmd.ExtraFiles, w)
	err = cmd.Run()
	require.NoError(t, w.Close())
	require.NoError(t, err)
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		_, _ = io.Copy(io.Discard, r)
	}()

	select {
	case <-dead:
	case <-time.After(30 * time.Second):
		t.Fatal("the background process outlived its command")
	}
}

func TestBwrapUserNamespaceRefused(t *testing.T) {
	sb := newBwrap(t, Policy{})[0]
	control := exec.CommandContext(t.Context(), "unshare", "--user", "true")
	if out, err := control.CombinedOutput(); err != nil {
		t.Skipf("unshare --user fails outside the sandbox: %v: %s", err, out)
	}

	assert.Error(t, bash(t, sb, `unshare --user true`))
	assert.Error(t, bash(t, sb, `unshare --user --map-root-user --mount true`))
}

func TestBwrapPlanProfile(t *testing.T) {
	tmp := resolvedTemp(t)
	ws := filepath.Join(tmp, "ws")
	home := filepath.Join(ws, "home")
	mkdirs(t, filepath.Join(ws, ".git", "hooks"))
	writeFile(t, filepath.Join(home, "secret"))
	sb := newBwrap(t, Policy{
		Write:     []string{ws, tmp},
		DenyRead:  []string{home},
		DenyWrite: []string{filepath.Join(ws, ".git", "hooks"), filepath.Join(ws, ".mcp.json"), ws},
	})[0]
	env := []string{"WS=" + ws, "TMP=" + tmp}

	require.NoError(t, bash(t, sb, `echo x >"$TMP/f" && test ! -e "$WS/.mcp.json"`, env...))
	assert.FileExists(t, filepath.Join(tmp, "f"))
	for _, script := range []string{
		`echo x >"$WS/f"`,
		`cat "$WS/home/secret"`,
		`mv "$WS" "$TMP/moved"`,
	} {
		assert.Error(t, bash(t, sb, script, env...), script)
	}
	assert.NoFileExists(t, filepath.Join(ws, "f"))
	assert.DirExists(t, ws)
}
