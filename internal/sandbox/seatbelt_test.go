package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
		networkPolicy + openNetwork,
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
	assert.Contains(t, on, networkPolicy+openNetwork)
	assert.NotContains(t, on, proxyNetwork)

	allow := regexp.MustCompile(`\(allow network[^\s()]*[^\n]*`)
	rules := allow.FindAllString(on, -1)
	require.NotEmpty(t, rules)
	for _, r := range rules {
		ok := strings.Contains(r, `(local ip "*:*")`) || strings.Contains(r, `(remote ip "*:*")`) ||
			r == `(allow network-outbound (remote unix-socket (path-literal "/private/var/run/mDNSResponder")))`
		assert.True(t, ok, r)
	}
}

func TestSeatbeltNetworkFiltered(t *testing.T) {
	proxy, err := NewProxy([]string{"example.com"}, nil, nil)
	require.NoError(t, err)
	filtered, defs := profileOf(t, seatbeltPrefix(Policy{Proxy: proxy}))

	assert.Contains(t, filtered, networkPolicy+proxyNetwork)
	assert.NotContains(t, filtered, openNetwork)
	assert.Equal(t, []string{`(allow network-outbound (remote ip (param "PROXY")))`},
		regexp.MustCompile(`\(allow network[^\n]*`).FindAllString(filtered, -1))
	assert.NotContains(t, filtered, "unix-socket")
	// Command defines PROXY once it has a port.
	assert.NotContains(t, defs, "PROXY")
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

func TestSeatbeltDenyWriteNames(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only Seatbelt denies file names")
	}
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
	if runtime.GOOS != "darwin" {
		t.Skip("only Seatbelt denies file names")
	}
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

func TestSeatbeltDenyWriteNamesHardLink(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only Seatbelt denies file names")
	}
	root := filepath.Join(t.TempDir(), "root")
	writeFile(t, filepath.Join(root, ".mcp.json"))
	sb := newSandbox(t, Policy{Write: []string{root}, DenyWriteNames: []string{".mcp.json"}})

	assert.Error(t, bash(t, sb, `ln "$ROOT/.mcp.json" "$ROOT/hard" && echo y >"$ROOT/hard"`, "ROOT="+root))
	got, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
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
