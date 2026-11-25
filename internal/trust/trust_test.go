package trust

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tempDir returns a canonical temporary directory; on macOS t.TempDir sits
// behind the /var -> /private/var symlink.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(parts...)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "absent", "trust.json"))
	require.NoError(t, err)
	_, ok, err := s.Lookup(t.TempDir())
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSetThenLookup(t *testing.T) {
	root := tempDir(t)
	s, err := Open(filepath.Join(root, "home", "trust.json"))
	require.NoError(t, err)
	project := mkdir(t, root, "project")

	require.NoError(t, s.Set(project, Untrusted))
	d, ok, err := s.Lookup(project)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Untrusted, d)

	require.NoError(t, s.Set(project, Trusted))
	d, ok, err = s.Lookup(project)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Trusted, d)
}

func TestLookupClosestDecisionApplies(t *testing.T) {
	root := tempDir(t)
	parent := mkdir(t, root, "parent")
	child := mkdir(t, parent, "child")
	inner := mkdir(t, child, "inner")
	sibling := mkdir(t, parent, "sibling")
	s, err := Open(filepath.Join(root, "trust.json"))
	require.NoError(t, err)
	require.NoError(t, s.Set(parent, Trusted))
	require.NoError(t, s.Set(child, Untrusted))

	for _, tc := range []struct {
		name string
		dir  string
		want Decision
	}{
		{"ancestor match", sibling, Trusted},
		{"closer decision wins", child, Untrusted},
		{"closer ancestor wins", inner, Untrusted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ok, err := s.Lookup(tc.dir)
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, tc.want, d)
		})
	}

	_, ok, err := s.Lookup(root)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestLookupMatchesAFolderTypedInAnotherCase(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("case-insensitive folders are checked on darwin")
	}
	root := tempDir(t)
	parent := mkdir(t, root, "parent")
	child := mkdir(t, parent, "evil")
	flipped := filepath.Join(parent, "EVIL")
	if _, err := os.Stat(flipped); err != nil {
		t.Skip("the temporary directory is on a case-sensitive volume")
	}
	s, err := Open(filepath.Join(root, "trust.json"))
	require.NoError(t, err)
	require.NoError(t, s.Set(parent, Trusted))
	require.NoError(t, s.Set(child, Untrusted))

	d, ok, err := s.Lookup(flipped)

	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Untrusted, d)
	got, err := Canonical(strings.ToUpper(child))
	require.NoError(t, err)
	assert.Equal(t, child, got)
}

func TestBroadIsHomeOrRoot(t *testing.T) {
	root := tempDir(t)
	home := mkdir(t, root, "home")
	project := mkdir(t, home, "project")
	slash := string(filepath.Separator)

	assert.True(t, Broad(home, home))
	assert.True(t, Broad(slash, home))
	assert.False(t, Broad(project, home))
}

func TestSymlinkedPathResolvesToSameEntry(t *testing.T) {
	root := tempDir(t)
	project := mkdir(t, root, "project")
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(project, link))
	path := filepath.Join(root, "trust.json")
	s, err := Open(path)
	require.NoError(t, err)

	require.NoError(t, s.Set(link, Trusted))
	d, ok, err := s.Lookup(project)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Trusted, d)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"version":1,"folders":{%q:"trusted"}}`, project), string(b))
}

func TestDecisionsPersistAcrossOpen(t *testing.T) {
	root := tempDir(t)
	home := filepath.Join(root, "home")
	path := filepath.Join(home, "trust.json")
	a := mkdir(t, root, "a")
	b := mkdir(t, root, "b")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Set(a, Trusted))
	require.NoError(t, s.Set(b, Untrusted))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	info, err = os.Stat(home)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	var entryNames []string
	for _, entry := range entries {
		entryNames = append(entryNames, entry.Name())
	}
	assert.ElementsMatch(t, []string{"trust.json", "trust.json.lock"}, entryNames, "no temporary file left behind")

	s, err = Open(path)
	require.NoError(t, err)
	for dir, want := range map[string]Decision{a: Trusted, b: Untrusted} {
		d, ok, err := s.Lookup(dir)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, want, d)
	}
}

func TestOpenRejectsInvalidFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    error
	}{
		{"bad version", `{"version":2,"folders":{}}`, ErrUnsupportedVersion},
		{"missing version", `{"folders":{}}`, ErrUnsupportedVersion},
		{"bad value", `{"version":1,"folders":{"/x":"maybe"}}`, ErrInvalidDecision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trust.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			_, err := Open(path)
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), path)
		})
	}
}

func TestOpenRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,`), 0o600))
	_, err := Open(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
}

func TestSetKeepsDecisionSavedByAnotherStore(t *testing.T) {
	root := tempDir(t)
	a := mkdir(t, root, "a")
	b := mkdir(t, root, "b")
	path := filepath.Join(root, "trust.json")
	first, err := Open(path)
	require.NoError(t, err)
	second, err := Open(path)
	require.NoError(t, err)

	require.NoError(t, first.Set(a, Trusted))
	require.NoError(t, second.Set(b, Untrusted))

	d, ok, err := second.Lookup(a)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, Trusted, d)

	s, err := Open(path)
	require.NoError(t, err)
	for dir, want := range map[string]Decision{a: Trusted, b: Untrusted} {
		d, ok, err := s.Lookup(dir)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, want, d)
	}
}

func TestSetFailsWithoutWritingOnBadFile(t *testing.T) {
	root := tempDir(t)
	path := filepath.Join(root, "trust.json")
	s, err := Open(path)
	require.NoError(t, err)
	bad := `{"version":2,"folders":{}}`
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o600))

	require.ErrorIs(t, s.Set(mkdir(t, root, "a"), Trusted), ErrUnsupportedVersion)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, bad, string(b))

	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked, "Set must release the lock on reload failure")
	require.NoError(t, lock.Unlock())
}

func TestSetFailsWithoutWritingWhenLockCannotOpen(t *testing.T) {
	root := tempDir(t)
	path := filepath.Join(root, "trust.json")
	project := mkdir(t, root, "project")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(filepath.Join(root, "absent", "lock"), path+".lock"))

	err = s.Set(project, Trusted)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to lock trust file")
	var pathErr *os.PathError
	assert.ErrorAs(t, err, &pathErr)
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, ok, err := s.Lookup(project)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSetKeepsCompetingProcessDecisions(t *testing.T) {
	root := tempDir(t)
	parent := mkdir(t, root, "parent")
	child := mkdir(t, parent, "child")
	path := filepath.Join(root, "trust.json")
	lock := flock.New(path + ".lock")
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { assert.NoError(t, lock.Unlock()) })

	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var commands []*exec.Cmd
	var starts []io.WriteCloser
	var outputs []*bufio.Reader
	var diagnostics []*bytes.Buffer
	for _, writer := range []struct {
		dir      string
		decision string
	}{{parent, "trusted"}, {child, "untrusted"}} {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestTrustWriterProcess$", "--", path, writer.dir, writer.decision)
		cmd.Env = append(os.Environ(), "KOPI_TRUST_TEST_WRITER=1")
		stdin, err := cmd.StdinPipe()
		require.NoError(t, err)
		stdout, err := cmd.StdoutPipe()
		require.NoError(t, err)
		stderr := new(bytes.Buffer)
		cmd.Stderr = stderr
		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		commands = append(commands, cmd)
		starts = append(starts, stdin)
		outputs = append(outputs, bufio.NewReader(stdout))
		diagnostics = append(diagnostics, stderr)
	}
	// Both processes open the same stale snapshot before either may Set.
	for _, output := range outputs {
		line, err := output.ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "ready\n", line)
	}
	for _, start := range starts {
		_, err := io.WriteString(start, "set\n")
		require.NoError(t, err)
		require.NoError(t, start.Close())
	}
	for _, output := range outputs {
		line, err := output.ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "setting\n", line)
	}
	require.NoError(t, lock.Unlock())
	for i, cmd := range commands {
		require.NoError(t, cmd.Wait(), diagnostics[i].String())
	}

	s, err := Open(path)
	require.NoError(t, err)
	for dir, want := range map[string]Decision{parent: Trusted, child: Untrusted} {
		d, ok, err := s.Lookup(dir)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, want, d)
	}
}

func TestTrustWriterProcess(t *testing.T) {
	if os.Getenv("KOPI_TRUST_TEST_WRITER") != "1" {
		return
	}
	args := os.Args[len(os.Args)-3:]
	s, err := Open(args[0])
	require.NoError(t, err)
	d, ok := parse(args[2])
	require.True(t, ok)
	_, err = fmt.Fprintln(os.Stdout, "ready")
	require.NoError(t, err)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "set\n", line)
	_, err = fmt.Fprintln(os.Stdout, "setting")
	require.NoError(t, err)
	require.NoError(t, s.Set(args[1], d))
}
