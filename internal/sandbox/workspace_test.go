package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type workspaceEnv struct {
	root, home, kopiHome, workspace string
}

func newWorkspaceEnv(t *testing.T) workspaceEnv {
	t.Helper()
	root := t.TempDir()
	e := workspaceEnv{
		root:      root,
		home:      filepath.Join(root, "home"),
		kopiHome:  filepath.Join(root, "kopi"),
		workspace: filepath.Join(root, "ws"),
	}
	for _, dir := range []string{e.home, e.kopiHome, e.workspace} {
		require.NoError(t, os.Mkdir(dir, 0o755))
	}
	return e
}

// open is OpenWorkspace for w in e's workspace, with the sandboxes' directory in Kopi's home.
func (e workspaceEnv) open(ctx context.Context, w Workspace) (execute, plan *Sandbox, setDirs, validateDirs func(dirs []string) error, err error) {
	w.CWD, w.UserHome, w.KopiHome = e.workspace, e.home, e.kopiHome
	return OpenWorkspace(ctx, w)
}

func withCWD(w Workspace, cwd string) Workspace {
	w.CWD = cwd
	return w
}

func canonical(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return p
}

// refused is the error text of a sandbox refusal: Seatbelt words them all
// alike, while on Linux it names what the refusing mount does.
func refused(linux string) string {
	if runtime.GOOS == "linux" {
		return linux
	}
	return "Operation not permitted"
}

const (
	readOnly = "Read-only file system" // a write under a read-only mount
	// The open of a /dev/null placeholder, which bubblewrap mounts nodev.
	permissionDenied = "Permission denied"
)

func TestSandboxPolicyProtectsHEADWithConcurrentLinuxPlaceholder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is the Linux sandbox")
	}
	newSandbox(t, Policy{})
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprintf("HEAD exists=%t", exists), func(t *testing.T) {
			e := newWorkspaceEnv(t)
			head := filepath.Join(e.workspace, "HEAD")
			const original = "ref: refs/heads/main\n"
			if exists {
				require.NoError(t, os.WriteFile(head, []byte(original), 0o600))
			}
			first, _, _, _, err := e.open(t.Context(), Workspace{})
			require.NoError(t, err)
			require.NotNil(t, first)
			cmd, done, err := first.Command([]string{"/bin/bash", "-c", `echo ready; read -r _ || true`})
			require.NoError(t, err)
			defer done()
			stdin, err := cmd.StdinPipe()
			require.NoError(t, err)
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			require.NoError(t, cmd.Start())
			defer func() {
				_ = stdin.Close()
				require.NoError(t, cmd.Wait(), stderr.String())
			}()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "ready\n", line)
			assert.DirExists(t, filepath.Join(e.workspace, ".git"))

			p := workspacePolicy(Workspace{CWD: e.workspace, UserHome: e.home, KopiHome: e.kopiHome})
			assert.Contains(t, p.DenyWrite, head)
			second, _, _, _, err := e.open(t.Context(), Workspace{})
			require.NoError(t, err)
			require.NotNil(t, second)
			attempt, cleanup, err := second.Command([]string{"/bin/bash", "-c", `/bin/mkdir -p objects refs && printf '[core]\n' > config && printf changed > HEAD`})
			require.NoError(t, err)
			attempt.Dir = e.workspace
			out, err := attempt.CombinedOutput()
			cleanup()
			require.Error(t, err, string(out))
			assert.Contains(t, string(out), readOnly)
			content, err := os.ReadFile(head)
			require.NoError(t, err)
			if exists {
				assert.Equal(t, original, string(content))
			} else {
				assert.Empty(t, content, "only the first command's empty placeholder remains")
			}
		})
	}
}

func TestSandboxPolicy(t *testing.T) {
	home, kopiHome, absolute := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := Workspace{
		UserHome:   home,
		KopiHome:   kopiHome,
		AllowWrite: []string{absolute, "~/x", "~", "rel"},
		DenyRead:   []string{absolute, "~/x", "~", "rel"},
		Network:    true,
	}
	gitFiles := func(cwd string) []string {
		var out []string
		for _, name := range []string{"hooks", "config", "commondir", "config.worktree", "worktrees", "modules"} {
			out = append(out, filepath.Join(cwd, ".git", name))
		}
		return out
	}
	// Linux always denies HEAD and the whole .git, after the names.
	linuxGit := func(cwd string) []string {
		if runtime.GOOS != "linux" {
			return nil
		}
		return []string{filepath.Join(cwd, "HEAD"), filepath.Join(cwd, ".git")}
	}

	t.Run("git directory", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(cwd, ".git"), 0o755))
		expanded := []string{absolute, filepath.Join(home, "x"), home, filepath.Join(cwd, "rel")}
		write := []string{cwd, "/tmp", os.TempDir()}

		p := workspacePolicy(withCWD(cfg, cwd))

		assert.Equal(t, append(write, expanded...), p.Write)
		for _, path := range append(p.Write, p.DenyRead...) {
			assert.True(t, filepath.IsAbs(path), path)
		}
		assert.Equal(t, append([]string{kopiHome}, expanded...), p.DenyRead)
		assert.Equal(t, slices.Concat(gitFiles(cwd), linuxGit(cwd), []string{filepath.Join(cwd, ".mcp.json")}), p.DenyWrite)
		assert.Equal(t, []string{".mcp.json"}, p.DenyWriteNames)
		assert.True(t, p.Network)
	})

	t.Run("git file", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cwd, ".git"), []byte("gitdir: /elsewhere\n"), 0o600))

		p := workspacePolicy(withCWD(cfg, cwd))

		git := linuxGit(cwd)
		if runtime.GOOS != "linux" {
			git = []string{filepath.Join(cwd, ".git")}
		}
		assert.Equal(t, slices.Concat(gitFiles(cwd), git, []string{filepath.Join(cwd, ".mcp.json")}), p.DenyWrite)
		assert.Equal(t, []string{".mcp.json"}, p.DenyWriteNames)
	})

	t.Run("no git", func(t *testing.T) {
		cwd := t.TempDir()

		p := workspacePolicy(withCWD(cfg, cwd))

		git := linuxGit(cwd)
		if runtime.GOOS != "linux" {
			git = []string{filepath.Join(cwd, "HEAD")}
		}
		assert.Equal(t, slices.Concat(gitFiles(cwd), git, []string{filepath.Join(cwd, ".mcp.json")}), p.DenyWrite)
		assert.Equal(t, []string{".mcp.json"}, p.DenyWriteNames)
	})

	t.Run("git symlink to a directory", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(cwd, ".git")))

		p := workspacePolicy(withCWD(cfg, cwd))

		assert.Equal(t, slices.Concat(gitFiles(cwd), linuxGit(cwd), []string{filepath.Join(cwd, ".mcp.json")}), p.DenyWrite)
		assert.Equal(t, []string{".mcp.json"}, p.DenyWriteNames)
	})

	t.Run("plan", func(t *testing.T) {
		cwd := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(cwd, ".git"), 0o755))
		p := workspacePolicy(withCWD(cfg, cwd))

		plan := planPolicy(p, cwd, nil)

		assert.Equal(t, slices.Concat(gitFiles(cwd), linuxGit(cwd), []string{filepath.Join(cwd, ".mcp.json"), cwd}), plan.DenyWrite)
		assert.Equal(t, p.DenyWriteNames, plan.DenyWriteNames)
		assert.Equal(t, p.Write, plan.Write)
		assert.Equal(t, p.DenyRead, plan.DenyRead)
		assert.Equal(t, p.Network, plan.Network)
		plan.DenyWrite[0] = "changed"
		assert.Equal(t, slices.Concat(gitFiles(cwd), linuxGit(cwd), []string{filepath.Join(cwd, ".mcp.json")}), p.DenyWrite)
	})
}

func TestPlanPolicyDeniesAddedDirectories(t *testing.T) {
	cwd, home, kopiHome := t.TempDir(), t.TempDir(), t.TempDir()
	first, second := t.TempDir(), t.TempDir()
	cfg := Workspace{CWD: cwd, UserHome: home, KopiHome: kopiHome}

	p := workspacePolicy(cfg)
	plan := planPolicy(p, cwd, []string{first, second})

	assert.Subset(t, plan.DenyWrite, []string{cwd, first, second})
	assert.Equal(t, p.Write, plan.Write)
}

func TestSandboxPolicyPreservesSymlinkParent(t *testing.T) {
	cwd, home, kopiHome := t.TempDir(), t.TempDir(), t.TempDir()
	real := filepath.Join(t.TempDir(), "child")
	require.NoError(t, os.Mkdir(real, 0o700))
	for _, base := range []string{cwd, home} {
		require.NoError(t, os.Symlink(real, filepath.Join(base, "link")))
	}
	for _, tc := range []struct{ name, path, want string }{
		{"relative", "link/../target", cwd + string(filepath.Separator) + "link/../target"},
		{"home", "~/link/../target", home + string(filepath.Separator) + "link/../target"},
		{"absolute", cwd + string(filepath.Separator) + "link/../target", cwd + string(filepath.Separator) + "link/../target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := workspacePolicy(Workspace{CWD: cwd, UserHome: home, KopiHome: kopiHome, AllowWrite: []string{tc.path}, DenyRead: []string{tc.path}})
			assert.Equal(t, tc.want, p.Write[len(p.Write)-1])
			assert.Equal(t, tc.want, p.DenyRead[len(p.DenyRead)-1])
		})
	}
}

func TestOpenWorkspaceSetDirsMakesTheDirectoryWritableOnlyInExecuteMode(t *testing.T) {
	if runtime.GOOS == "linux" {
		// t.TempDir() is under /tmp there, which is always a write root.
		t.Setenv("TMPDIR", "/var/tmp")
	}
	// After the TMPDIR above, as the first t.TempDir() call fixes where the test's are.
	newSandbox(t, Policy{})
	e := newWorkspaceEnv(t)
	// The temporary directory is a write root, so it must not contain the added one.
	temp := filepath.Join(e.root, "tmp")
	require.NoError(t, os.Mkdir(temp, 0o700))
	t.Setenv("TMPDIR", temp)
	added := filepath.Join(e.root, "added")
	require.NoError(t, os.Mkdir(added, 0o700))
	added = canonical(t, added)
	execute, plan, setDirs, _, err := e.open(t.Context(), Workspace{})
	require.NoError(t, err)
	require.NotNil(t, setDirs)
	attempt := func(sb *Sandbox) (string, error) {
		cmd, cleanup, err := sb.Command([]string{"/bin/bash", "-c", "printf x > " + filepath.Join(added, "f")})
		require.NoError(t, err)
		defer cleanup()
		cmd.Dir = e.workspace
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	_, err = attempt(execute)
	require.Error(t, err, "before it is added the directory is outside the write roots")

	require.NoError(t, setDirs([]string{added}))

	out, err := attempt(plan)
	require.Error(t, err, out)
	assert.Contains(t, out, refused(readOnly))
	out, err = attempt(execute)
	require.NoError(t, err, out)
	assert.FileExists(t, filepath.Join(added, "f"))

	require.NoError(t, os.Mkdir(filepath.Join(added, ".git"), 0o700))
	for _, tc := range []struct{ name, linux string }{
		{".mcp.json", permissionDenied},
		{filepath.Join(".git", "config"), readOnly},
	} {
		cmd, cleanup, err := execute.Command([]string{"/bin/bash", "-c", "printf x > " + filepath.Join(added, tc.name)})
		require.NoError(t, err)
		cmd.Dir = e.workspace
		out, err := cmd.CombinedOutput()
		cleanup()
		require.Error(t, err, tc.name)
		assert.Contains(t, string(out), refused(tc.linux), tc.name)
		assert.NoFileExists(t, filepath.Join(added, tc.name))
	}
}

func TestOpenWorkspaceSetDirsAppliesAllRootProtections(t *testing.T) {
	newSandbox(t, Policy{})
	e := newWorkspaceEnv(t)
	first, second := filepath.Join(e.root, "first"), filepath.Join(e.root, "second")
	for _, dir := range []string{first, second} {
		require.NoError(t, os.Mkdir(dir, 0o700))
	}
	require.NoError(t, os.Mkdir(filepath.Join(first, ".git"), 0o700))
	write, read := filepath.Join(e.root, "allowed"), filepath.Join(e.root, "secret")
	execute, plan, setDirs, _, err := e.open(t.Context(), Workspace{AllowWrite: []string{write}, DenyRead: []string{read}})
	require.NoError(t, err)
	require.NoError(t, setDirs([]string{first, second}))

	for _, tc := range []struct {
		name string
		sb   *Sandbox
		plan bool
	}{{"execute", execute, false}, {"plan", plan, true}} {
		t.Run(tc.name, func(t *testing.T) {
			contains := func(kind, path string) {
				t.Helper()
				if tc.sb.bwrap != nil {
					p := tc.sb.bwrap.policy
					paths := map[string][]string{"WRITE": p.Write, "DENY_READ": p.DenyRead, "DENY_WRITE": p.DenyWrite}
					assert.Contains(t, paths[kind], path)
					return
				}
				var found bool
				for _, arg := range tc.sb.prefix {
					found = found || (strings.HasPrefix(arg, "-D"+kind+"_") && strings.HasSuffix(arg, "="+path))
				}
				assert.True(t, found, "%s %s", kind, path)
			}
			for _, root := range []string{e.workspace, first, second} {
				contains("WRITE", root)
				for _, name := range []string{"hooks", "config", "commondir", "config.worktree", "worktrees", "modules"} {
					contains("DENY_WRITE", filepath.Join(root, ".git", name))
				}
				contains("DENY_WRITE", filepath.Join(root, ".mcp.json"))
				if runtime.GOOS == "linux" {
					contains("DENY_WRITE", filepath.Join(root, ".git"))
				}
				if runtime.GOOS == "linux" || root != first {
					contains("DENY_WRITE", filepath.Join(root, "HEAD"))
				}
				if tc.plan {
					contains("DENY_WRITE", root)
				}
			}
			contains("WRITE", write)
			contains("DENY_READ", read)
			contains("DENY_READ", e.kopiHome)
			if tc.sb.bwrap != nil {
				assert.Contains(t, tc.sb.bwrap.policy.DenyWriteNames, ".mcp.json")
			} else {
				assert.Contains(t, strings.Join(tc.sb.prefix, "\n"), "-DDENY_WRITE_NAME_")
			}
		})
	}
}

func TestOpenWorkspaceSetDirsKeepsStartupProtections(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires Seatbelt")
	}
	newSandbox(t, Policy{})
	e := newWorkspaceEnv(t)
	old, next := filepath.Join(e.root, "old"), filepath.Join(e.root, "next")
	require.NoError(t, os.Mkdir(old, 0o700))
	require.NoError(t, os.Mkdir(next, 0o700))
	read, write := filepath.Join(e.root, "read"), filepath.Join(e.root, "write")
	mcp := filepath.Join(e.workspace, ".mcp.json")
	for _, link := range []string{read, write, mcp} {
		require.NoError(t, os.Symlink(old, link))
	}
	cfg := Workspace{DenyRead: []string{read}, AllowWrite: []string{write}}
	execute, plan, setDirs, _, err := e.open(t.Context(), cfg)
	require.NoError(t, err)
	for _, link := range []string{read, write, mcp} {
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(next, link))
	}
	added := filepath.Join(e.root, "added")
	require.NoError(t, os.Mkdir(added, 0o700))
	for _, dirs := range [][]string{{added}, nil} {
		require.NoError(t, setDirs(dirs))
		for _, sb := range []*Sandbox{execute, plan} {
			cmd, done, err := sb.Command([]string{"/bin/true"})
			require.NoError(t, err)
			done()
			args := strings.Join(cmd.Args, "\n")
			assert.Contains(t, args, "="+canonical(t, old))
			assert.NotContains(t, args, "="+canonical(t, next))
		}
	}
}

func TestOpenWorkspaceSetDirsKeepsBothPoliciesOnAnError(t *testing.T) {
	newSandbox(t, Policy{})
	e := newWorkspaceEnv(t)
	execute, plan, setDirs, _, err := e.open(t.Context(), Workspace{})
	require.NoError(t, err)
	require.NotNil(t, setDirs)
	argv := func(sb *Sandbox) []string {
		cmd, cleanup, err := sb.Command([]string{"/bin/true"})
		require.NoError(t, err)
		defer cleanup()
		return cmd.Args
	}
	wantExecute, wantPlan := argv(execute), argv(plan)

	err = setDirs([]string{"relative"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not absolute")
	assert.Equal(t, wantExecute, argv(execute))
	assert.Equal(t, wantPlan, argv(plan))
}

func TestOpenWorkspaceValidateDirsChangesNeitherSandbox(t *testing.T) {
	newSandbox(t, Policy{})
	e := newWorkspaceEnv(t)
	execute, plan, _, validateDirs, err := e.open(t.Context(), Workspace{})
	require.NoError(t, err)
	require.NotNil(t, validateDirs)
	argv := func(sb *Sandbox) []string {
		cmd, cleanup, err := sb.Command([]string{"/bin/true"})
		require.NoError(t, err)
		defer cleanup()
		return cmd.Args
	}
	wantExecute, wantPlan := argv(execute), argv(plan)
	added := filepath.Join(e.root, "added")
	require.NoError(t, os.Mkdir(added, 0o700))

	require.NoError(t, validateDirs([]string{canonical(t, added)}))

	assert.Equal(t, wantExecute, argv(execute))
	assert.Equal(t, wantPlan, argv(plan))
	if runtime.GOOS == "linux" {
		err := validateDirs([]string{"/"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "overlaps /proc")
		assert.Equal(t, wantExecute, argv(execute))
		assert.Equal(t, wantPlan, argv(plan))
	}
}

func TestOpenWorkspaceSetDirsKeepsAnAddedDirectorysProtections(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires Seatbelt")
	}
	newSandbox(t, Policy{})
	e := newWorkspaceEnv(t)
	execute, plan, setDirs, _, err := e.open(t.Context(), Workspace{})
	require.NoError(t, err)
	added, second := filepath.Join(e.root, "added"), filepath.Join(e.root, "second")
	require.NoError(t, os.Mkdir(added, 0o700))
	require.NoError(t, os.Mkdir(second, 0o700))
	added = canonical(t, added)
	require.NoError(t, setDirs([]string{added}))
	require.NoError(t, os.Mkdir(filepath.Join(added, ".git"), 0o700))

	require.NoError(t, setDirs([]string{added, canonical(t, second)}))

	for _, sb := range []*Sandbox{execute, plan} {
		cmd, done, err := sb.Command([]string{"/bin/true"})
		require.NoError(t, err)
		done()
		var denied bool
		for _, arg := range cmd.Args {
			denied = denied || (strings.HasPrefix(arg, "-DDENY_WRITE_") && strings.HasSuffix(arg, "="+filepath.Join(added, "HEAD")))
		}
		assert.True(t, denied, "HEAD of the added directory stays denied")
	}
}

func TestOpenWorkspaceSetDirsDropsARemovedDirectoryAsAWriteRoot(t *testing.T) {
	if runtime.GOOS == "linux" {
		// t.TempDir() is under /tmp there, which is always a write root.
		t.Setenv("TMPDIR", "/var/tmp")
	}
	// After the TMPDIR above, as the first t.TempDir() call fixes where the test's are.
	newSandbox(t, Policy{})
	e := newWorkspaceEnv(t)
	// The temporary directory is a write root, so it must not contain either directory.
	temp := filepath.Join(e.root, "tmp")
	require.NoError(t, os.Mkdir(temp, 0o700))
	t.Setenv("TMPDIR", temp)
	kept, removed := filepath.Join(e.root, "kept"), filepath.Join(e.root, "removed")
	require.NoError(t, os.Mkdir(kept, 0o700))
	require.NoError(t, os.Mkdir(removed, 0o700))
	kept, removed = canonical(t, kept), canonical(t, removed)
	execute, plan, setDirs, _, err := e.open(t.Context(), Workspace{})
	require.NoError(t, err)
	require.NotNil(t, setDirs)
	attempt := func(sb *Sandbox, dir string) (string, error) {
		cmd, cleanup, err := sb.Command([]string{"/bin/bash", "-c", "printf x > " + filepath.Join(dir, "f")})
		require.NoError(t, err)
		defer cleanup()
		cmd.Dir = e.workspace
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	require.NoError(t, setDirs([]string{kept, removed}))
	out, err := attempt(execute, removed)
	require.NoError(t, err, "the directory is a write root while it is added: "+out)
	require.NoError(t, os.Remove(filepath.Join(removed, "f")))

	require.NoError(t, setDirs([]string{kept}))

	_, err = attempt(execute, removed)
	assert.Error(t, err, "the removed directory is no longer a write root")
	_, err = attempt(plan, removed)
	assert.Error(t, err)
	out, err = attempt(execute, kept)
	require.NoError(t, err, out)
	assert.FileExists(t, filepath.Join(kept, "f"))
	_, err = attempt(plan, kept)
	assert.Error(t, err, "the kept directory is read-only in plan mode")
}
