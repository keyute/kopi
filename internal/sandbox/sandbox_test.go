package sandbox

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsRelativePath(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("only darwin and linux build a sandbox from the paths")
	}
	for _, p := range []Policy{{Write: []string{"rel"}}, {DenyRead: []string{"a/b"}}, {DenyWrite: []string{""}}} {
		_, err := New(t.Context(), p)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrUnavailable)
		assert.Contains(t, err.Error(), "is not absolute")
	}
}

func TestNewUnavailableElsewhere(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("darwin and linux have a sandbox")
	}
	_, err := New(t.Context(), Policy{Write: []string{t.TempDir()}})
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), runtime.GOOS)
}

func TestCommandDoesNotAliasPrefix(t *testing.T) {
	s := &Sandbox{prefix: seatbeltPrefix(Policy{Write: []string{t.TempDir()}})}
	want := append([]string(nil), s.prefix...)

	a, doneA, err := s.Command([]string{"a"})
	require.NoError(t, err)
	defer doneA()
	b, doneB, err := s.Command([]string{"b"})
	require.NoError(t, err)
	defer doneB()
	assert.Equal(t, append(append([]string(nil), want...), "a"), a.Args)
	assert.Equal(t, append(append([]string(nil), want...), "b"), b.Args)

	a.Args[0] = "x"
	empty, doneEmpty, err := s.Command(nil)
	require.NoError(t, err)
	defer doneEmpty()
	empty.Args[0] = "z"
	assert.Equal(t, want, s.prefix)
	assert.Equal(t, want[0], b.Args[0])
}

// newSandbox skips where the platform sandbox cannot run: off macOS and
// Linux, when the test itself already runs inside a Seatbelt sandbox, or
// where bubblewrap is missing or cannot make its namespaces.
func newSandbox(t *testing.T, p Policy) *Sandbox {
	t.Helper()
	sbs, err := New(t.Context(), p)
	if errors.Is(err, ErrUnavailable) {
		t.Skip(err)
	}
	require.NoError(t, err)
	return sbs[0]
}

// bash runs script in sb with env added to the test's environment.
func bash(t *testing.T, sb *Sandbox, script string, env ...string) error {
	t.Helper()
	cmd, done, err := sb.Command([]string{"/bin/bash", "-c", script})
	require.NoError(t, err)
	defer done()
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	t.Logf("%s: %s", script, out)
	return err
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
}

func TestWriteRoots(t *testing.T) {
	base := t.TempDir()
	root, out := filepath.Join(base, "root"), filepath.Join(base, "out")
	writeFile(t, filepath.Join(out, "f"))
	require.NoError(t, os.Mkdir(root, 0o700))
	sb := newSandbox(t, Policy{Write: []string{root}})
	env := []string{"ROOT=" + root, "OUT=" + out}

	require.NoError(t, bash(t, sb, `echo x >"$ROOT/f"`, env...))
	assert.FileExists(t, filepath.Join(root, "f"))
	require.Error(t, bash(t, sb, `echo x >"$OUT/new"`, env...))
	assert.NoFileExists(t, filepath.Join(out, "new"))
	require.NoError(t, bash(t, sb, `cat "$OUT/f"`, env...))
}

func TestDenyReadInsideWriteRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	secret := filepath.Join(root, "secret")
	writeFile(t, filepath.Join(secret, "f"))
	sb := newSandbox(t, Policy{Write: []string{root}, DenyRead: []string{secret}})
	env := []string{"ROOT=" + root}

	for _, script := range []string{
		`cat "$ROOT/secret/f"`,
		`echo y >"$ROOT/secret/f"`,
		`touch "$ROOT/secret/new"`,
		`mv "$ROOT/secret" "$ROOT/moved"`,
	} {
		assert.Error(t, bash(t, sb, script, env...), script)
	}
	got, err := os.ReadFile(filepath.Join(secret, "f"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
	assert.NoFileExists(t, filepath.Join(secret, "new"))
	assert.NoDirExists(t, filepath.Join(root, "moved"))
}

func TestDenyWriteGit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	git := filepath.Join(root, ".git")
	writeFile(t, filepath.Join(git, "config"))
	require.NoError(t, os.MkdirAll(filepath.Join(git, "hooks"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(git, "objects"), 0o700))
	sb := newSandbox(t, Policy{
		Write:     []string{root},
		DenyWrite: []string{filepath.Join(git, "hooks"), filepath.Join(git, "config")},
	})
	env := []string{"ROOT=" + root}

	assert.Error(t, bash(t, sb, `echo x >"$ROOT/.git/hooks/pre-commit"`, env...))
	assert.Error(t, bash(t, sb, `echo y >>"$ROOT/.git/config"`, env...))
	require.NoError(t, bash(t, sb, `touch "$ROOT/.git/objects/x"`, env...))
	assert.Error(t, bash(t, sb, `mv "$ROOT/.git" "$ROOT/g"`, env...))
	assert.NoFileExists(t, filepath.Join(git, "hooks", "pre-commit"))
	assert.DirExists(t, git)
}

func TestDenyWriteSymlinkProtectsTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	writeFile(t, filepath.Join(root, "servers.json"))
	writeFile(t, filepath.Join(root, "other"))
	require.NoError(t, os.Symlink("servers.json", filepath.Join(root, "link")))
	sb := newSandbox(t, Policy{Write: []string{root}, DenyWrite: []string{filepath.Join(root, "link")}})
	env := []string{"ROOT=" + root}

	scripts := []string{
		`echo y >"$ROOT/servers.json"`,
		`echo y >"$ROOT/link"`,
		`mv "$ROOT/other" "$ROOT/servers.json"`,
		`rm "$ROOT/servers.json"`,
	}
	if runtime.GOOS == "darwin" {
		// APFS folds case, so this names the same file.
		scripts = append(scripts, `echo y >"$ROOT/SERVERS.JSON"`)
	}
	for _, script := range scripts {
		assert.Error(t, bash(t, sb, script, env...), script)
	}
	got, err := os.ReadFile(filepath.Join(root, "servers.json"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
	require.NoError(t, bash(t, sb, `echo y >"$ROOT/sibling"`, env...))
}

func TestWorkspaceThroughSymlinkedParentStaysWritable(t *testing.T) {
	base := t.TempDir()
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "ws", ".git", "hooks"), 0o700))
	require.NoError(t, os.Symlink(real, link))
	ws := filepath.Join(link, "ws")
	sb := newSandbox(t, Policy{Write: []string{ws}, DenyWrite: []string{filepath.Join(ws, ".git", "hooks")}})
	env := []string{"WS=" + ws}

	for _, script := range []string{
		`echo x >"$WS/f"`,
		`mkdir "$WS/d" && echo x >"$WS/d/f"`,
		`rm "$WS/f"`,
	} {
		require.NoError(t, bash(t, sb, script, env...), script)
	}
	assert.Error(t, bash(t, sb, `echo x >"$WS/.git/hooks/pre-commit"`, env...))
	assert.FileExists(t, filepath.Join(real, "ws", "d", "f"))
	assert.NoFileExists(t, filepath.Join(real, "ws", ".git", "hooks", "pre-commit"))
}

func TestDenyWriteInsideWriteRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	ws := filepath.Join(root, "ws")
	writeFile(t, filepath.Join(ws, "f"))
	require.NoError(t, os.Mkdir(filepath.Join(root, "scratch"), 0o700))
	sb := newSandbox(t, Policy{Write: []string{root}, DenyWrite: []string{ws}})
	env := []string{"ROOT=" + root, "WS=" + ws}

	for _, script := range []string{
		`cat "$WS/f"`,
		`ls "$WS"`,
		`echo x >"$ROOT/scratch/new"`,
	} {
		require.NoError(t, bash(t, sb, script, env...), script)
	}
	for _, script := range []string{
		`echo y >"$WS/f"`,
		`rm "$WS/f"`,
		`touch "$WS/new"`,
		`mkdir "$WS/d"`,
		`mv "$WS/f" "$ROOT/scratch/moved"`,
		`mv "$WS" "$ROOT/moved"`,
	} {
		assert.Error(t, bash(t, sb, script, env...), script)
	}
	// Either the link or the write through it must fail.
	_ = bash(t, sb, `ln "$WS/f" "$ROOT/scratch/link" && echo y >"$ROOT/scratch/link"`, env...)
	got, err := os.ReadFile(filepath.Join(ws, "f"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
	assert.NoFileExists(t, filepath.Join(ws, "new"))
	assert.NoDirExists(t, filepath.Join(ws, "d"))
	assert.NoDirExists(t, filepath.Join(root, "moved"))
}

func TestWriteRootCannotBeRenamed(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "outer")
	inner := filepath.Join(outer, "inner")
	require.NoError(t, os.MkdirAll(inner, 0o700))
	sb := newSandbox(t, Policy{Write: []string{outer, inner}})

	assert.Error(t, bash(t, sb, `mv "$OUTER/inner" "$OUTER/moved"`, "OUTER="+outer))
	assert.DirExists(t, inner)
}

func TestWriteRootThroughSymlink(t *testing.T) {
	base := t.TempDir()
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	require.NoError(t, os.Mkdir(real, 0o700))
	require.NoError(t, os.Symlink(real, link))
	sb := newSandbox(t, Policy{Write: []string{link}})

	require.NoError(t, bash(t, sb, `echo x >"$LINK/f"`, "LINK="+link))
	assert.FileExists(t, filepath.Join(real, "f"))
}

func TestMissingDenyWriteCannotBeCreated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Mkdir(root, 0o700))
	sb := newSandbox(t, Policy{Write: []string{root}, DenyWrite: []string{filepath.Join(root, "protected")}})
	env := []string{"ROOT=" + root}

	assert.Error(t, bash(t, sb, `mkdir "$ROOT/protected"`, env...))
	assert.Error(t, bash(t, sb, `touch "$ROOT/protected"`, env...))
	assert.NoFileExists(t, filepath.Join(root, "protected"))
	assert.NoDirExists(t, filepath.Join(root, "protected"))
}

func TestMissingDeniedAncestorCannotBeSymlink(t *testing.T) {
	base := t.TempDir()
	root, target := filepath.Join(base, "root"), filepath.Join(base, "target")
	require.NoError(t, os.Mkdir(root, 0o700))
	secret := filepath.Join(target, "secret")
	writeFile(t, secret)
	missing := filepath.Join(root, "missing")
	denied := filepath.Join(missing, "secret")

	for _, tc := range []struct {
		name   string
		policy Policy
	}{
		{"DenyRead", Policy{Write: []string{root}, DenyRead: []string{denied}}},
		{"DenyWrite", Policy{Write: []string{root}, DenyWrite: []string{denied}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t, tc.policy)
			assert.Error(t, bash(t, sb, `ln -s "$TARGET" "$ROOT/missing"`, "TARGET="+target, "ROOT="+root))
			_, err := os.Lstat(missing)
			assert.ErrorIs(t, err, os.ErrNotExist)
			got, err := os.ReadFile(secret)
			require.NoError(t, err)
			assert.Equal(t, "x", string(got))
		})
	}
}

func TestShellBasics(t *testing.T) {
	sb := newSandbox(t, Policy{Write: []string{filepath.Join(t.TempDir(), "root")}})
	for _, script := range []string{
		`echo x >/dev/null`,
		`echo x >/dev/stderr`,
		`true | cat`,
		`sleep 60 & kill $!; wait`,
	} {
		assert.NoError(t, bash(t, sb, script), script)
	}
}

// listen accepts connections on ln until it is closed, reporting each on the
// returned channel, which is closed once accepting stops.
func listen(ln net.Listener) <-chan struct{} {
	accepted := make(chan struct{}, 8)
	go func() {
		defer close(accepted)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			accepted <- struct{}{}
		}
	}()
	return accepted
}

func TestNetworkTCP(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	for _, network := range []bool{false, true} {
		sb := newSandbox(t, Policy{Write: []string{root}, Network: network})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		accepted := listen(ln)
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

		err = bash(t, sb, `exec 3<>/dev/tcp/127.0.0.1/$PORT`, "PORT="+port)
		if network {
			require.NoError(t, err)
			_, ok := <-accepted
			assert.True(t, ok)
			require.NoError(t, ln.Close())
		} else {
			require.Error(t, err)
			require.NoError(t, ln.Close())
			_, ok := <-accepted
			assert.False(t, ok)
		}
	}
}

func TestUnixSocketUnreachable(t *testing.T) {
	connect := `/usr/bin/nc -U "$SOCK" </dev/null`
	if runtime.GOOS == "linux" {
		connect = `python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).connect(sys.argv[1])' "$SOCK"`
	}
	root := filepath.Join(t.TempDir(), "root")
	for _, network := range []bool{false, true} {
		sb := newSandbox(t, Policy{Write: []string{root}, Network: network})
		// macOS limits socket paths to 104 bytes, too short for t.TempDir().
		dir, err := os.MkdirTemp("/tmp", "kopi")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sock := filepath.Join(dir, "s")
		ln, err := net.Listen("unix", sock)
		require.NoError(t, err)
		accepted := listen(ln)

		// Outside the sandbox the same client connects, so a failure inside
		// is the sandbox's doing.
		control := exec.CommandContext(t.Context(), "/bin/bash", "-c", connect)
		control.Env = append(os.Environ(), "SOCK="+sock)
		out, err := control.CombinedOutput()
		require.NoError(t, err, string(out))
		_, ok := <-accepted
		require.True(t, ok)

		assert.Error(t, bash(t, sb, connect, "SOCK="+sock), network)
		require.NoError(t, ln.Close())
		_, ok = <-accepted
		assert.False(t, ok, network)
	}
}

func TestSymlinkParentWriteEnforcement(t *testing.T) {
	base := t.TempDir()
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "child"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(real, "target"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(base, "target"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(real, "child"), link))
	sb := newSandbox(t, Policy{Write: []string{link + "/../target"}})
	require.NoError(t, bash(t, sb, `touch "$REAL/target/f"`, "REAL="+real))
	assert.Error(t, bash(t, sb, `touch "$BASE/target/f"`, "BASE="+base))
	assert.FileExists(t, filepath.Join(real, "target", "f"))
	assert.NoFileExists(t, filepath.Join(base, "target", "f"))
}
