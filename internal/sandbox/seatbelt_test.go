package sandbox

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// profileOf splits a seatbelt prefix into its profile and its -D parameters.
func profileOf(t *testing.T, prefix []string) (string, map[string]string) {
	t.Helper()
	require.GreaterOrEqual(t, len(prefix), 4)
	require.Equal(t, []string{sandboxExec, "-p"}, prefix[:2])
	require.Equal(t, "--", prefix[len(prefix)-1])
	defs := map[string]string{}
	for _, arg := range prefix[3 : len(prefix)-1] {
		kv, ok := strings.CutPrefix(arg, "-D")
		require.True(t, ok, arg)
		k, v, ok := strings.Cut(kv, "=")
		require.True(t, ok, arg)
		require.NotContains(t, defs, k)
		defs[k] = v
	}
	return prefix[2], defs
}

// values returns the parameters named prefix_n in order.
func values(defs map[string]string, prefix string) []string {
	var out []string
	for i := 0; ; i++ {
		v, ok := defs[prefix+"_"+strconv.Itoa(i)]
		if !ok {
			return out
		}
		out = append(out, v)
	}
}

func fullPolicy(t *testing.T) Policy {
	t.Helper()
	base := filepath.Join(filepath.VolumeName(t.TempDir())+string(filepath.Separator), "kopi-test")
	return Policy{
		Write:          []string{filepath.Join(base, "w")},
		DenyRead:       []string{filepath.Join(base, "w", "secret")},
		DenyWrite:      []string{filepath.Join(base, "w", ".git", "hooks")},
		DenyWriteNames: []string{".mcp.json"},
		Network:        true,
	}
}

func TestSeatbeltSectionOrder(t *testing.T) {
	profile, _ := profileOf(t, seatbeltPrefix(fullPolicy(t)))
	markers := []string{
		"\n(allow file-read*)\n",
		"(deny file-read*\n",
		"(allow file-write*\n",
		"(deny file-write*\n",
		"(deny file-write*\n  (regex ",
		networkPolicy,
		`(deny mach-lookup (xpc-service-name-prefix ""))`,
		"(deny system-fcntl (fcntl-command 80 110))",
		"(deny file-write-unlink file-write-create\n",
		"(deny file-write-unlink\n",
	}
	last := len(basePolicy) - 1
	for _, m := range markers {
		i := strings.Index(profile, m)
		require.Greater(t, i, last, m)
		last = i
	}
	assert.True(t, strings.HasPrefix(profile, basePolicy))
	_, rename, ok := strings.Cut(profile, "(deny file-write-unlink file-write-create\n")
	require.True(t, ok)
	for _, line := range strings.Split(strings.TrimSuffix(rename, "\n"), "\n") {
		assert.Regexp(t, `^(\(deny file-write-unlink|  \()`, line)
	}
}

func TestSeatbeltOmitsRulesWithoutFilters(t *testing.T) {
	empty, defs := profileOf(t, seatbeltPrefix(Policy{}))
	assert.Empty(t, defs)
	for _, head := range []string{"(allow file-write*", "(deny file-read*", "(deny file-write*", "(deny file-write-unlink"} {
		assert.NotContains(t, empty, head)
	}

	full, _ := profileOf(t, seatbeltPrefix(fullPolicy(t)))
	for _, profile := range []string{empty, full} {
		for _, bare := range []string{"(allow file-write*)", "(deny file-read*)", "(deny file-write*)", "(deny file-write-unlink)", "(deny file-write-unlink file-write-create)"} {
			assert.NotContains(t, profile, bare)
		}
	}

	onlyDenyWrite, _ := profileOf(t, seatbeltPrefix(Policy{DenyWrite: []string{t.TempDir()}}))
	assert.NotContains(t, onlyDenyWrite, "(allow file-write*")
	assert.NotContains(t, onlyDenyWrite, "(deny file-read*")
}

func TestSeatbeltPreferences(t *testing.T) {
	restricted, _ := profileOf(t, seatbeltPrefix(fullPolicy(t)))
	unrestricted, _ := profileOf(t, seatbeltPrefix(Policy{}))
	for _, rule := range []string{
		`(allow ipc-posix-shm-read* (ipc-posix-name-prefix "apple.cfprefs."))`,
		`(global-name "com.apple.cfprefsd.daemon")`,
		`(global-name "com.apple.cfprefsd.agent")`,
		`(local-name "com.apple.cfprefsd.agent")`,
		`(allow user-preference-read)`,
	} {
		assert.NotContains(t, restricted, rule)
		assert.Contains(t, unrestricted, rule)
	}
	assert.NotContains(t, restricted, preferencesPolicy)
	assert.True(t, strings.HasPrefix(unrestricted, basePolicy+preferencesPolicy))
}

func TestSeatbeltNetwork(t *testing.T) {
	off, _ := profileOf(t, seatbeltPrefix(Policy{}))
	on, _ := profileOf(t, seatbeltPrefix(Policy{Network: true}))
	assert.NotContains(t, off, networkPolicy)
	assert.NotContains(t, off, "(allow network")
	assert.Contains(t, on, networkPolicy)

	allow := regexp.MustCompile(`\(allow network[^\s()]*[^\n]*`)
	rules := allow.FindAllString(on, -1)
	require.NotEmpty(t, rules)
	for _, r := range rules {
		ok := strings.Contains(r, `(local ip "*:*")`) || strings.Contains(r, `(remote ip "*:*")`) ||
			r == `(allow network-outbound (remote unix-socket (path-literal "/private/var/run/mDNSResponder")))`
		assert.True(t, ok, r)
	}
}

func TestSeatbeltParamsMatchDefinitions(t *testing.T) {
	p := fullPolicy(t)
	profile, defs := profileOf(t, seatbeltPrefix(p))
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`\(param "([^"]*)"\)`).FindAllStringSubmatch(profile, -1) {
		used[m[1]] = true
	}
	for k := range used {
		assert.Contains(t, defs, k)
	}
	for k, v := range defs {
		assert.True(t, used[k], k)
		assert.NotContains(t, profile, v)
	}
	for _, path := range slices.Concat(p.Write, p.DenyRead, p.DenyWrite) {
		assert.NotContains(t, profile, path)
	}
}

func TestSeatbeltPathForms(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o700))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))
	resolved, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)

	_, defs := profileOf(t, seatbeltPrefix(Policy{
		Write:     []string{link + "/"},
		DenyRead:  []string{filepath.Join(link, "missing")},
		DenyWrite: []string{filepath.Join(link, "x", "y")},
	}))
	assert.Equal(t, []string{link, resolved}, values(defs, "WRITE"))
	assert.Equal(t, []string{filepath.Join(link, "missing"), filepath.Join(resolved, "missing")}, values(defs, "DENY_READ"))
	assert.Equal(t, []string{
		filepath.Join(link, "missing"), filepath.Join(resolved, "missing"),
		filepath.Join(link, "x", "y"), filepath.Join(resolved, "x", "y"),
	}, values(defs, "DENY_WRITE"))
}

func TestSeatbeltAncestors(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	_, defs := profileOf(t, seatbeltPrefix(Policy{
		DenyRead:  []string{filepath.Join(base, "a", "x")},
		DenyWrite: []string{filepath.Join(base, "a", "b", "c"), filepath.Join(base, "a", "b", "d")},
	}))
	want := []string{filepath.Join(base, "a")}
	for d := base; d != filepath.Dir(d); d = filepath.Dir(d) {
		want = append(want, d)
	}
	want = append(want, filepath.Join(base, "a", "b"))
	assert.Equal(t, want, values(defs, "ANCESTOR"))
}

func TestSeatbeltDenyReadIsAlsoDenyWrite(t *testing.T) {
	path, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	profile, defs := profileOf(t, seatbeltPrefix(Policy{DenyRead: []string{path}}))
	assert.Equal(t, []string{path}, values(defs, "DENY_WRITE"))
	assert.Contains(t, profile, `(deny file-write*
  (literal (param "DENY_WRITE_0")) (subpath (param "DENY_WRITE_0")))`)
}

func TestSeatbeltDenyWriteNamesMatchAnyCase(t *testing.T) {
	profile, defs := profileOf(t, seatbeltPrefix(Policy{DenyWriteNames: []string{".mcp.json"}}))
	assert.Equal(t, []string{`/\.(m|M)(c|C)(p|P)\.(j|J)(s|ſ|S)(o|O)(n|N)$`}, values(defs, "DENY_WRITE_NAME"))
	assert.Contains(t, profile, `(deny file-write*
  (regex (param "DENY_WRITE_NAME_0")))`)
	re := regexp.MustCompile(defs["DENY_WRITE_NAME_0"])
	for _, p := range []string{"/a/.mcp.json", "/a/.MCP.JSON", "/a/.mcp.jſon"} {
		assert.True(t, re.MatchString(p), p)
	}
	for _, p := range []string{"/a/not.mcp.json", "/a/.mcp.json.bak"} {
		assert.False(t, re.MatchString(p), p)
	}
}

// newSandbox skips where Seatbelt cannot run: off macOS, or when the test
// itself already runs inside a Seatbelt sandbox.
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
	argv := sb.Wrap([]string{"/bin/bash", "-c", script})
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
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

func TestSeatbeltWriteRoots(t *testing.T) {
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

func TestSeatbeltDenyReadInsideWriteRoot(t *testing.T) {
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

func TestSeatbeltDenyWriteGit(t *testing.T) {
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

func TestSeatbeltDenyWriteNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	writeFile(t, filepath.Join(root, ".mcp.json"))
	writeFile(t, filepath.Join(root, "other"))
	require.NoError(t, os.Symlink(".mcp.json", filepath.Join(root, "alias")))
	sb := newSandbox(t, Policy{Write: []string{root}, DenyWriteNames: []string{".mcp.json"}})
	env := []string{"ROOT=" + root}

	for _, script := range []string{
		`echo y >"$ROOT/.mcp.json"`,
		`echo y >>"$ROOT/.mcp.json"`,
		`echo y >"$ROOT/.MCP.JSON"`,
		`mv "$ROOT/other" "$ROOT/.mcp.json"`,
		`rm "$ROOT/.mcp.json"`,
		`echo y >"$ROOT/alias"`,
	} {
		assert.Error(t, bash(t, sb, script, env...), script)
	}
	got, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
	require.NoError(t, bash(t, sb, `echo y >"$ROOT/not.mcp.json"`, env...))
	require.NoError(t, bash(t, sb, `echo y >"$ROOT/.mcp.json.bak"`, env...))
}

func TestSeatbeltDenyWriteNamesCannotBeCreated(t *testing.T) {
	base := t.TempDir()
	empty, temp := filepath.Join(base, "empty"), filepath.Join(base, "temp")
	require.NoError(t, os.Mkdir(empty, 0o700))
	require.NoError(t, os.Mkdir(temp, 0o700))
	sb := newSandbox(t, Policy{Write: []string{empty, temp}, DenyWriteNames: []string{".mcp.json"}})
	env := []string{"EMPTY=" + empty, "TEMP=" + temp}

	for _, script := range []string{
		`echo y >"$EMPTY/.mcp.json"`,
		`mkdir -p "$EMPTY/new/dir" && echo y >"$EMPTY/new/dir/.mcp.json"`,
		`echo y >"$EMPTY/.MCP.JSON"`,
		`echo y >"$EMPTY/.mcp.jſon"`,
		`mkdir "$TEMP/p" && echo y >"$TEMP/p/.mcp.json" && mv "$TEMP/p" "$EMPTY/p"`,
		`printf x >"$EMPTY/a" && mv "$EMPTY/a" "$EMPTY/.mcp.json"`,
		`ln -s servers.json "$EMPTY/.mcp.json"`,
	} {
		assert.Error(t, bash(t, sb, script, env...), script)
		_, err := os.Lstat(filepath.Join(empty, ".mcp.json"))
		assert.ErrorIs(t, err, os.ErrNotExist, script)
	}
	for _, p := range []string{
		filepath.Join(empty, ".mcp.json"),
		filepath.Join(empty, "new", "dir", ".mcp.json"),
		filepath.Join(empty, ".MCP.JSON"),
		filepath.Join(empty, ".mcp.jſon"),
		filepath.Join(temp, "p", ".mcp.json"),
		filepath.Join(empty, "p", ".mcp.json"),
	} {
		assert.NoFileExists(t, p)
	}
}

func TestSeatbeltWorkspaceThroughSymlinkedParentStaysWritable(t *testing.T) {
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

func TestSeatbeltDenyWriteNamesHardLink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	writeFile(t, filepath.Join(root, ".mcp.json"))
	sb := newSandbox(t, Policy{Write: []string{root}, DenyWriteNames: []string{".mcp.json"}})

	assert.Error(t, bash(t, sb, `ln "$ROOT/.mcp.json" "$ROOT/hard" && echo y >"$ROOT/hard"`, "ROOT="+root))
	got, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
}

func TestSeatbeltDenyWriteInsideWriteRoot(t *testing.T) {
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

func TestSeatbeltWriteRootCannotBeRenamed(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "outer")
	inner := filepath.Join(outer, "inner")
	require.NoError(t, os.MkdirAll(inner, 0o700))
	sb := newSandbox(t, Policy{Write: []string{outer, inner}})

	assert.Error(t, bash(t, sb, `mv "$OUTER/inner" "$OUTER/moved"`, "OUTER="+outer))
	assert.DirExists(t, inner)
}

func TestSeatbeltWriteRootThroughSymlink(t *testing.T) {
	base := t.TempDir()
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	require.NoError(t, os.Mkdir(real, 0o700))
	require.NoError(t, os.Symlink(real, link))
	sb := newSandbox(t, Policy{Write: []string{link}})

	require.NoError(t, bash(t, sb, `echo x >"$LINK/f"`, "LINK="+link))
	assert.FileExists(t, filepath.Join(real, "f"))
}

func TestSeatbeltMissingDenyWriteCannotBeCreated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Mkdir(root, 0o700))
	sb := newSandbox(t, Policy{Write: []string{root}, DenyWrite: []string{filepath.Join(root, "protected")}})
	env := []string{"ROOT=" + root}

	assert.Error(t, bash(t, sb, `mkdir "$ROOT/protected"`, env...))
	assert.Error(t, bash(t, sb, `touch "$ROOT/protected"`, env...))
	assert.NoFileExists(t, filepath.Join(root, "protected"))
	assert.NoDirExists(t, filepath.Join(root, "protected"))
}

func TestSeatbeltMissingDeniedAncestorCannotBeSymlink(t *testing.T) {
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

func TestSeatbeltShellBasics(t *testing.T) {
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

func TestSeatbeltNetworkTCP(t *testing.T) {
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

func TestSeatbeltUnixSocketUnreachable(t *testing.T) {
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

		assert.Error(t, bash(t, sb, `/usr/bin/nc -U "$SOCK" </dev/null`, "SOCK="+sock), network)
		require.NoError(t, ln.Close())
		_, ok := <-accepted
		assert.False(t, ok, network)
	}
}

func TestSeatbeltSymlinkParentForms(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "child"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(real, "existing"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(real, "child"), link))
	for _, suffix := range []string{"existing", "existing/", "missing/deep", "missing/deep/"} {
		t.Run(suffix, func(t *testing.T) {
			path := link + "/../" + suffix
			resolved := filepath.Join(real, suffix)
			_, defs := profileOf(t, seatbeltPrefix(Policy{
				Write: []string{path, path}, DenyRead: []string{path}, DenyWrite: []string{path},
			}))
			for _, prefix := range []string{"WRITE", "DENY_READ", "DENY_WRITE"} {
				assert.Equal(t, []string{resolved}, values(defs, prefix))
				assert.NotContains(t, values(defs, prefix), filepath.Clean(path))
			}
			assert.Contains(t, values(defs, "ANCESTOR"), real)
		})
	}
}

func TestSeatbeltSymlinkParentWriteEnforcement(t *testing.T) {
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
