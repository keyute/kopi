package sandbox

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinuxSandboxSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is the Linux sandbox")
	}
	sbs, err := New(t.Context(), t.TempDir(), Policy{})
	if errors.Is(err, ErrUnavailable) && os.Getenv("KOPI_TEST_REQUIRE_LINUX_SANDBOX") != "1" {
		t.Skip(err)
	}
	require.NoError(t, err)
	require.NoError(t, bash(t, sbs[0], `echo ready >/dev/null; test -r /proc/self/status`))
}

func TestNewUnusableBwrap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is the Linux sandbox")
	}
	bin := t.TempDir()
	path := filepath.Join(bin, "bwrap")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700))
	t.Setenv("PATH", bin)
	dir := filepath.Join(t.TempDir(), "sandbox")

	sbs, err := New(t.Context(), dir, Policy{})

	require.ErrorIs(t, err, ErrUnavailable)
	assert.NotContains(t, err.Error(), "failed to find bwrap")
	assert.Nil(t, sbs)
	assert.NoDirExists(t, dir)
}

func TestNewRejectsRelativePath(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("only darwin and linux build a sandbox from the paths")
	}
	for _, p := range []Policy{{Write: []string{"rel"}}, {DenyRead: []string{"a/b"}}, {DenyWrite: []string{""}}} {
		_, err := New(t.Context(), t.TempDir(), p)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrUnavailable)
		assert.Contains(t, err.Error(), "is not absolute")
	}
}

func TestNewRejectsNetworkWithProxy(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("only darwin and linux build a sandbox from the policy")
	}
	proxy, err := NewProxy([]string{"example.com"}, nil, []string{"/helper"})
	require.NoError(t, err)

	_, err = New(t.Context(), t.TempDir(), Policy{Network: true, Proxy: proxy})

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnavailable)
}

func TestCheckHelper(t *testing.T) {
	base := resolvedTemp(t)
	secret := filepath.Join(base, "secret")
	require.NoError(t, os.Mkdir(secret, 0o700))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(secret, link))
	for _, tc := range []struct {
		name   string
		helper []string
		deny   []string
		fails  string
	}{
		{"outside", []string{"/usr/bin/kopi"}, []string{secret}, ""},
		{"under", []string{filepath.Join(secret, "kopi")}, []string{secret}, secret},
		{"through a link", []string{filepath.Join(link, "kopi")}, []string{secret}, secret},
		{"denied by a link", []string{filepath.Join(secret, "kopi")}, []string{link}, secret},
		{"none", nil, nil, "no helper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, err := NewProxy([]string{"example.com"}, nil, tc.helper)
			require.NoError(t, err)

			err = checkHelper([]Policy{{}, {Proxy: proxy, DenyRead: tc.deny}})

			if tc.fails == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrUnavailable)
			assert.Contains(t, err.Error(), tc.fails)
		})
	}
}

func TestFiltersNetwork(t *testing.T) {
	proxy, err := NewProxy([]string{"example.com"}, nil, nil)
	require.NoError(t, err)

	assert.False(t, (&Sandbox{}).FiltersNetwork())
	assert.False(t, (&Sandbox{prefix: seatbeltPrefix(Policy{Network: true})}).FiltersNetwork())
	assert.True(t, (&Sandbox{prefix: seatbeltPrefix(Policy{Proxy: proxy}), proxy: proxy}).FiltersNetwork())
}

func TestCommandWithProxy(t *testing.T) {
	proxy, err := NewProxy([]string{"example.com"}, nil, nil)
	require.NoError(t, err)
	s := &Sandbox{prefix: seatbeltPrefix(Policy{Write: []string{t.TempDir()}, Proxy: proxy}), proxy: proxy}
	want := append([]string(nil), s.prefix...)

	cmd, done, err := s.Command([]string{"a"})
	require.NoError(t, err)
	var defs []string
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "-DPROXY=") {
			defs = append(defs, arg)
		}
	}
	require.Len(t, defs, 1)
	host, port, ok := strings.Cut(strings.TrimPrefix(defs[0], "-DPROXY="), ":")
	require.True(t, ok)
	assert.Equal(t, "localhost", host)
	assert.Equal(t, append(append([]string(nil), want[:len(want)-1]...), defs[0], "--", "a"), cmd.Args)
	for _, v := range proxyEnv(mustAtoi(t, port)) {
		assert.Contains(t, cmd.Env, v)
	}
	conn, err := net.Dial("tcp", "127.0.0.1:"+port)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	assert.Empty(t, done())

	_, err = net.Dial("tcp", "127.0.0.1:"+port)
	assert.Error(t, err)
	assert.Equal(t, want, s.prefix)
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

func TestNewUnavailableElsewhere(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("darwin and linux have a sandbox")
	}
	_, err := New(t.Context(), t.TempDir(), Policy{Write: []string{t.TempDir()}})
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
	sbs, err := New(t.Context(), t.TempDir(), p)
	if errors.Is(err, ErrUnavailable) {
		t.Skip(err)
	}
	require.NoError(t, err)
	return sbs[0]
}

// bash runs script in sb with env added to the command's environment.
func bash(t *testing.T, sb *Sandbox, script string, env ...string) error {
	t.Helper()
	_, err := bashRefused(t, sb, script, env...)
	return err
}

// bashRefused is bash that also returns what done reports.
func bashRefused(t *testing.T, sb *Sandbox, script string, env ...string) (refused []string, err error) {
	t.Helper()
	cmd, done, err := sb.Command([]string{"/bin/bash", "-c", script})
	require.NoError(t, err)
	cmd.Env = append(cmd.Environ(), env...)
	out, err := cmd.CombinedOutput()
	refused = done()
	t.Logf("%s: %s", script, out)
	return refused, err
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
	if runtime.GOOS == "linux" {
		_, _, err := sb.Command([]string{"true"})
		require.ErrorIs(t, err, ErrDeniedSymlink)
		return
	}
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

// listenHelperEnv gates TestListenHelper, the helper a filtered Linux command
// runs in the sandbox in place of Kopi.
const listenHelperEnv = "KOPI_TEST_LISTEN_HELPER"

func TestNetworkProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "from upstream")
	}))
	t.Cleanup(upstream.Close)
	exe, err := os.Executable()
	require.NoError(t, err)
	proxy, err := newProxy([]string{"allowed.test"}, nil, []string{exe, "-test.run=^TestListenHelper$", "--"}, dialTo(upstream.Listener.Addr().String()))
	require.NoError(t, err)
	sb := newSandbox(t, Policy{Write: []string{filepath.Join(t.TempDir(), "root")}, Proxy: proxy})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	accepted := listen(ln)

	refused, err := bashRefused(t, sb, `set -e
port=${HTTP_PROXY##*:}
exec 3<>/dev/tcp/127.0.0.1/$port
printf 'GET http://allowed.test/ HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n' >&3
reply=$(cat <&3)
exec 3>&-
[[ $reply == *"from upstream"* ]]
exec 4<>/dev/tcp/127.0.0.1/$port
printf 'CONNECT blocked.test:443 HTTP/1.1\r\nHost: blocked.test:443\r\n\r\n' >&4
read -r status <&4
[[ $status == *" 403 "* ]]
if (exec 5<>/dev/tcp/127.0.0.1/$OTHER) 2>/dev/null; then exit 3; fi`,
		"OTHER="+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port), listenHelperEnv+"=1")

	require.NoError(t, err)
	assert.Equal(t, []string{"blocked.test:443"}, refused)
	require.NoError(t, ln.Close())
	_, ok := <-accepted
	assert.False(t, ok)
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
