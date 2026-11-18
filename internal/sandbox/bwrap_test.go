package sandbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// mounts returns policy mounts, excluding the root and special mounts.
func mounts(t *testing.T, args []string) []string {
	t.Helper()
	i := slices.Index(args, "/")
	require.Positive(t, i)
	require.Equal(t, []string{"--ro-bind", "/", "/"}, args[i-1:i+2])
	require.Equal(t, bwrapTail, args[len(args)-len(bwrapTail):])
	policyMounts := args[i+2 : len(args)-len(bwrapTail)]
	j := slices.Index(policyMounts, "--dev")
	require.GreaterOrEqual(t, j, 0)
	require.Equal(t, bwrapSpecials, policyMounts[j:j+len(bwrapSpecials)])
	return concat(policyMounts[:j], policyMounts[j+len(bwrapSpecials):])
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
		bwrapSpecials,
		ro(filepath.Join(root, ".git", "hooks")),
		null(filepath.Join(root, ".mcp.json")),
		emptyDir(filepath.Join(root, "secret")),
		bwrapTail,
	), args)
	assert.Equal(t, []placeholderDestination{{path: filepath.Join(root, ".mcp.json")}}, made)
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
	assert.Equal(t, []placeholderDestination{{path: a, directory: true}, {path: file}}, made)
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

func TestBwrapArgsSymlinkComponents(t *testing.T) {
	for _, broken := range []bool{false, true} {
		for _, name := range []string{".mcp.json", ".git", ".git/hooks", ".git/config"} {
			for _, read := range []bool{false, true} {
				base := resolvedTemp(t)
				root := filepath.Join(base, "root")
				entry := filepath.Join(root, name)
				mkdirs(t, filepath.Dir(entry))
				target := filepath.Join(base, "target")
				if !broken {
					if name == ".mcp.json" || name == ".git/config" {
						writeFile(t, target)
					} else {
						mkdirs(t, target)
					}
				}
				require.NoError(t, os.Symlink(target, entry))
				alias := filepath.Join(base, "alias")
				require.NoError(t, os.Symlink(root, alias))
				for _, prefix := range []string{root, alias} {
					denied := filepath.Join(prefix, name)
					if name == ".git" {
						denied += "/hooks"
					}
					p := Policy{Write: []string{root}, DenyWrite: []string{denied}}
					if read {
						p.DenyRead, p.DenyWrite = p.DenyWrite, nil
					}
					_, _, err := bwrapArgs(p)
					require.ErrorIs(t, err, ErrDeniedSymlink, "broken=%v read=%v path=%s", broken, read, denied)
					assert.Contains(t, err.Error(), entry)
				}
			}
		}
	}
}

func TestBwrapArgsSafeSymlinkParents(t *testing.T) {
	base := resolvedTemp(t)
	real := filepath.Join(base, "real")
	root := filepath.Join(real, "ws")
	mkdirs(t, filepath.Join(root, ".git", "hooks"), filepath.Join(real, "child"))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(filepath.Join(real, "child"), link))
	ws := link + "/../ws"
	args, _, err := bwrapArgs(Policy{Write: []string{ws}, DenyWrite: []string{ws + "/.git/hooks"}})
	require.NoError(t, err)
	assert.Equal(t, concat(ro(filepath.Join(root, ".git")), rw(root), ro(filepath.Join(root, ".git", "hooks"))), mounts(t, args))

	// A safe alias can also hide an unsafe link in its target path.
	unsafe := filepath.Join(root, "redirect")
	require.NoError(t, os.Symlink(filepath.Join(real, "child"), unsafe))
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Symlink(unsafe, alias))
	_, _, err = bwrapArgs(Policy{Write: []string{root}, DenyWrite: []string{alias + "/../ws/.mcp.json"}})
	require.ErrorIs(t, err, ErrDeniedSymlink)
	assert.Contains(t, err.Error(), unsafe)
}

func TestBwrapArgsSpecialMountsBeforeMasks(t *testing.T) {
	args, _, err := bwrapArgs(Policy{DenyRead: []string{"/dev"}})
	require.NoError(t, err)
	assert.Equal(t, concat(bwrapFlags(false), ro("/"), bwrapSpecials, emptyDir("/dev"), bwrapTail), args)
}

func TestDenyWriteBindSpecialOverlaps(t *testing.T) {
	for _, path := range []string{"/", "/proc", "/proc/1", "/dev", "/dev/null"} {
		t.Run(path, func(t *testing.T) {
			err := checkDenyWriteBind(path)
			require.ErrorContains(t, err, "unsupported sandbox policy")
			assert.Contains(t, err.Error(), path)
		})
	}
	for _, path := range []string{"/process", "/device", "/tmp/proc", "/tmp/dev"} {
		assert.NoError(t, checkDenyWriteBind(path))
	}
}

func TestBwrapArgsRefusesSpecialSelfBinds(t *testing.T) {
	for _, path := range []string{"/", "/dev", "/dev/null", "/dev/null/child", "/proc", "/proc/version", "/proc/version/child"} {
		t.Run(path, func(t *testing.T) {
			if within(path, "/proc") && runtime.GOOS != "linux" {
				t.Skip("proc mount paths require Linux")
			}
			root := resolvedTemp(t)
			missing := filepath.Join(root, "missing")
			p := Policy{Write: []string{"/"}, DenyWrite: []string{missing, path}}
			args, made, err := bwrapArgs(p)
			require.ErrorContains(t, err, "unsupported sandbox policy")
			assert.Nil(t, args)
			assert.Nil(t, made)
			pl := newPlaceholders(resolvedTemp(t))
			b := bwrap{policy: p, shared: pl}
			cmd, done, err := b.command([]string{"true"})
			require.ErrorContains(t, err, "unsupported sandbox policy")
			assert.Nil(t, cmd)
			assert.Nil(t, done)
			_, err = os.Lstat(missing)
			assert.ErrorIs(t, err, os.ErrNotExist)
			assert.NoFileExists(t, pl.recordPath)
		})
	}
	// No self-bind is emitted outside write roots, even for a special path.
	args, made, err := bwrapArgs(Policy{DenyWrite: []string{"/dev", "/dev/null/child"}})
	require.NoError(t, err)
	assert.Empty(t, mounts(t, args))
	assert.Empty(t, made)
	args, _, err = bwrapArgs(Policy{Write: []string{"/"}, DenyRead: []string{"/dev", "/dev/null"}})
	require.NoError(t, err)
	assert.Equal(t, concat(rw("/"), emptyDir("/dev")), mounts(t, args))
}

func TestBwrapArgsRefusesUnresolvedSymlinkTargetsInWriteRoot(t *testing.T) {
	for _, read := range []bool{false, true} {
		for _, intermediate := range []bool{false, true} {
			for _, relative := range []bool{false, true} {
				t.Run(fmt.Sprintf("read=%v/intermediate=%v/relative=%v", read, intermediate, relative), func(t *testing.T) {
					base := resolvedTemp(t)
					root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
					mkdirs(t, root, outside, filepath.Join(outside, "child"))
					missing := filepath.Join(root, "missing")
					target := missing
					if relative {
						target = "child/../../root/missing"
					}
					alias := filepath.Join(outside, "alias")
					require.NoError(t, os.Symlink(target, alias))
					denied := alias
					if intermediate {
						denied += "/leaf"
					}
					ordinary := filepath.Join(root, "ordinary")
					p := Policy{Write: []string{root}, DenyWrite: []string{ordinary, denied}}
					if read {
						p.DenyRead, p.DenyWrite = []string{denied}, []string{ordinary}
					}
					args, made, err := bwrapArgs(p)
					require.ErrorIs(t, err, ErrDeniedSymlink)
					assert.Contains(t, err.Error(), missing)
					assert.Nil(t, args)
					assert.Nil(t, made)
					pl := newPlaceholders(resolvedTemp(t))
					b := bwrap{policy: p, shared: pl}
					cmd, done, err := b.command([]string{"true"})
					require.ErrorIs(t, err, ErrDeniedSymlink)
					assert.Nil(t, cmd)
					assert.Nil(t, done)
					for _, path := range []string{missing, ordinary, pl.recordPath} {
						_, err := os.Lstat(path)
						assert.ErrorIs(t, err, os.ErrNotExist)
					}
					link, err := os.Readlink(alias)
					require.NoError(t, err)
					assert.Equal(t, target, link)
				})
			}
		}
	}
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

	pl := newPlaceholders(resolvedTemp(t))

	first, err := pl.start()
	require.NoError(t, err)
	second, err := pl.start()
	require.NoError(t, err)
	require.NoError(t, pl.prepare([]placeholderDestination{
		{path: empty}, {path: full}, {path: parent, directory: true}, {path: kept, directory: true},
	}))
	require.NoError(t, pl.prepare([]placeholderDestination{{path: child, directory: true}}))
	writeFile(t, full)
	writeFile(t, filepath.Join(kept, "f"))
	require.NoError(t, pl.record(nil))
	pl.finish(first)
	for _, p := range []string{empty, full, child, kept} {
		_, err := os.Lstat(p)
		assert.NoError(t, err, p)
	}

	pl.finish(second)
	for _, p := range []string{empty, child, parent} {
		_, err := os.Lstat(p)
		assert.ErrorIs(t, err, os.ErrNotExist, p)
	}
	assert.FileExists(t, full)
	assert.DirExists(t, kept)
	assert.NoFileExists(t, empty)
	info, err := os.Stat(pl.recordPath)
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

func TestPlaceholdersLeftByKilledKopiSwept(t *testing.T) {
	dir := resolvedTemp(t)
	stale, nested := filepath.Join(dir, "stale"), filepath.Join(dir, "a", "b")
	pl := newPlaceholders(resolvedTemp(t))
	require.NoError(t, pl.prepare([]placeholderDestination{{path: stale}, {path: filepath.Dir(nested), directory: true}}))
	// A killed append leaves a truncated tail; a later append stands alone.
	f, err := os.OpenFile(pl.recordPath, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`[{"path":`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, pl.prepare([]placeholderDestination{{path: nested, directory: true}}))

	lock, err := pl.start()
	require.NoError(t, err)
	pl.finish(lock)

	assert.NoFileExists(t, stale)
	assert.NoDirExists(t, filepath.Dir(nested))
	assert.DirExists(t, dir)
	info, err := os.Stat(pl.recordPath)
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

func TestPlaceholdersPreserveChangedEntries(t *testing.T) {
	for _, directory := range []bool{false, true} {
		for _, replacement := range []string{"same kind", "other kind", "symlink", "nonempty"} {
			t.Run(fmt.Sprintf("directory=%v/%s", directory, replacement), func(t *testing.T) {
				path := filepath.Join(resolvedTemp(t), "entry")
				pl := newPlaceholders(resolvedTemp(t))
				lock, err := pl.start()
				require.NoError(t, err)
				require.NoError(t, pl.prepare([]placeholderDestination{{path: path, directory: directory}}))
				// Pin the original inode so a replacement cannot reuse it.
				original, err := os.Open(path)
				require.NoError(t, err)
				defer original.Close()
				if replacement != "nonempty" {
					require.NoError(t, os.Remove(path))
				}
				switch replacement {
				case "symlink":
					require.NoError(t, os.Symlink(filepath.Join(filepath.Dir(path), "absent"), path))
				case "nonempty":
					if directory {
						writeFile(t, filepath.Join(path, "child"))
					} else {
						writeFile(t, path)
					}
				default:
					if directory == (replacement == "same kind") {
						require.NoError(t, os.Mkdir(path, 0o700))
					} else {
						require.NoError(t, os.WriteFile(path, nil, 0o600))
					}
				}
				pl.finish(lock)
				_, err = os.Lstat(path)
				assert.NoError(t, err)
			})
		}
	}
}

func TestPlaceholdersRejectSymlinkParents(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%v", directory), func(t *testing.T) {
			base := resolvedTemp(t)
			parent, relocated := filepath.Join(base, "parent"), filepath.Join(base, "relocated")
			mkdirs(t, relocated)
			require.NoError(t, os.Symlink("relocated", parent))
			path := filepath.Join(parent, "entry")
			pl := newPlaceholders(resolvedTemp(t))
			lock, err := pl.start()
			require.NoError(t, err)
			err = pl.prepare([]placeholderDestination{{path: path, directory: directory}})
			require.Error(t, err)
			_, err = os.Lstat(filepath.Join(relocated, "entry"))
			assert.ErrorIs(t, err, os.ErrNotExist)
			pl.finish(lock)

			require.NoError(t, os.Remove(parent))
			require.NoError(t, os.Remove(relocated))
			mkdirs(t, parent)
			lock, err = pl.start()
			require.NoError(t, err)
			require.NoError(t, pl.prepare([]placeholderDestination{{path: path, directory: directory}}))
			original, err := os.Lstat(path)
			require.NoError(t, err)
			require.NoError(t, os.Rename(parent, relocated))
			require.NoError(t, os.Symlink("relocated", parent))
			pl.finish(lock)
			kept, err := os.Lstat(filepath.Join(relocated, "entry"))
			require.NoError(t, err)
			assert.True(t, os.SameFile(original, kept))
			link, err := os.Readlink(parent)
			require.NoError(t, err)
			assert.Equal(t, "relocated", link)
		})
	}
}

func TestPlaceholdersDoNotAdoptUnownedEntries(t *testing.T) {
	pl := newPlaceholders(resolvedTemp(t))
	lock, err := pl.start()
	require.NoError(t, err)
	dir := resolvedTemp(t)
	existing, never := filepath.Join(dir, "existing"), filepath.Join(dir, "never-created")
	existingDir := filepath.Join(dir, "existing-directory")
	require.NoError(t, os.WriteFile(existing, nil, 0o600))
	require.NoError(t, os.Mkdir(existingDir, 0o700))
	// EEXIST is not ownership, even when the destination was planned absent.
	require.NoError(t, pl.prepare([]placeholderDestination{{path: existing}, {path: existingDir, directory: true}}))
	assert.NoFileExists(t, pl.recordPath)
	require.NoError(t, os.WriteFile(never, nil, 0o600))
	legacy, err := json.Marshal([]string{existing, never})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pl.recordPath, append(legacy, '\n'), 0o600))
	pl.finish(lock)
	assert.FileExists(t, existing)
	assert.DirExists(t, existingDir)
	assert.FileExists(t, never)
}

func TestPlaceholdersIgnoreIncompleteAndUnknownRecords(t *testing.T) {
	for _, record := range []string{"unknown kind", "wrong kind", "missing identity", "malformed", "unterminated"} {
		t.Run(record, func(t *testing.T) {
			path := filepath.Join(resolvedTemp(t), "entry")
			owned, err := createPlaceholder(placeholderDestination{path: path})
			require.NoError(t, err)
			require.NotNil(t, owned)
			if record == "unknown kind" {
				owned.Kind = "unknown"
			}
			if record == "wrong kind" {
				owned.Kind = "directory"
			}
			if record == "missing identity" {
				owned.Inode = nil
			}
			data, err := json.Marshal([]placeholderOwnership{*owned})
			require.NoError(t, err)
			if record == "malformed" {
				data = data[:len(data)-2]
			}
			if record != "unterminated" {
				data = append(data, '\n')
			}
			pl := newPlaceholders(resolvedTemp(t))
			require.NoError(t, os.WriteFile(pl.recordPath, data, 0o600))
			lock, err := pl.start()
			require.NoError(t, err)
			pl.finish(lock)
			assert.FileExists(t, path)
		})
	}
}

func TestPlaceholdersConcurrentCreation(t *testing.T) {
	pl := newPlaceholders(resolvedTemp(t))
	path := filepath.Join(resolvedTemp(t), "entry")
	first, err := pl.start()
	require.NoError(t, err)
	second, err := pl.start()
	require.NoError(t, err)
	start, done := make(chan struct{}), make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			done <- pl.prepare([]placeholderDestination{{path: path}})
		}()
	}
	close(start)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	data, err := os.ReadFile(pl.recordPath)
	require.NoError(t, err)
	var records []placeholderOwnership
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &records))
	require.Len(t, records, 1)
	pl.finish(first)
	assert.FileExists(t, path)
	pl.finish(second)
	assert.NoFileExists(t, path)
}

func TestPlaceholdersProcessHelper(t *testing.T) {
	dir := os.Getenv("KOPI_PLACEHOLDER_TEST_DIR")
	if dir == "" {
		return
	}
	pl := newPlaceholders(dir)
	lock, err := pl.start()
	require.NoError(t, err)
	require.NoError(t, pl.prepare([]placeholderDestination{{path: os.Getenv("KOPI_PLACEHOLDER_TEST_PATH")}}))
	fmt.Println("ready")
	_, err = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)
	runtime.KeepAlive(lock)
	// Exit without finish, like a killed Kopi with committed records.
	os.Exit(0)
}

func TestPlaceholdersCommittedByAnotherProcess(t *testing.T) {
	pl := newPlaceholders(resolvedTemp(t))
	path := filepath.Join(resolvedTemp(t), "entry")
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestPlaceholdersProcessHelper$")
	cmd.Env = append(os.Environ(), "KOPI_PLACEHOLDER_TEST_DIR="+filepath.Dir(pl.recordPath), "KOPI_PLACEHOLDER_TEST_PATH="+path)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	lock, err := pl.start()
	require.NoError(t, err)
	pl.finish(lock)
	assert.FileExists(t, path)
	lock, err = pl.start()
	require.NoError(t, err)
	require.NoError(t, stdin.Close())
	require.NoError(t, cmd.Wait(), stderr.String())
	assert.FileExists(t, path)
	pl.finish(lock)
	assert.NoFileExists(t, path)
}

// newBwrap skips off Linux and where bubblewrap cannot run.
func newBwrap(t *testing.T, policies ...Policy) []*Sandbox {
	t.Helper()
	return newBwrapIn(t, t.TempDir(), policies...)
}

// newBwrapIn is newBwrap with the shared directory named, as for Kopi
// processes using one home.
func newBwrapIn(t *testing.T, dir string, policies ...Policy) []*Sandbox {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is the Linux sandbox")
	}
	sbs, err := New(t.Context(), dir, policies...)
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

func TestBwrapPlaceholderOutlivesAnotherKopi(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	mkdirs(t, root)
	protected := filepath.Join(root, "protected")
	p := Policy{Write: []string{root}, DenyWrite: []string{protected}}
	home := t.TempDir()
	for _, firstEnds := range []int{0, 1} {
		a, b := newBwrapIn(t, home, p)[0], newBwrapIn(t, home, p)[0]
		held := []*held{hold(t, a), hold(t, b)}

		held[firstEnds].release(t)
		assert.FileExists(t, protected)
		held[1-firstEnds].release(t)
		_, err := os.Lstat(protected)
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestBwrapReadOnlyGit(t *testing.T) {
	root := filepath.Join(resolvedTemp(t), "root")
	git := filepath.Join(root, ".git")
	env := append(os.Environ(), gitEnv()...)
	host := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	mkdirs(t, root)
	host("init", "-q")
	writeFile(t, filepath.Join(root, "a"))
	host("add", "a")
	host("commit", "-q", "-m", "first")
	head := host("rev-parse", "HEAD")
	writeFile(t, filepath.Join(root, "f"))
	var deny []string
	for _, name := range []string{"hooks", "config", "commondir", "config.worktree", "worktrees", "modules"} {
		deny = append(deny, filepath.Join(git, name))
	}
	sb := newBwrap(t, Policy{Write: []string{root}, DenyWrite: append(deny, git, filepath.Join(root, ".mcp.json"))})[0]
	sandboxEnv := append(gitEnv(), "ROOT="+root)

	for _, script := range []string{
		`printf '../evil\n' >"$ROOT/.git/commondir"`,
		`mv "$ROOT/f" "$ROOT/.git/commondir"`,
		`ln -s ../evil "$ROOT/.git/commondir"`,
		`mv "$ROOT/.git" "$ROOT/g"`,
		`rm "$ROOT/.git/config"`,
		`rm -r "$ROOT/.git/hooks"`,
		`git -C "$ROOT" add f`,
		`git -C "$ROOT" commit --allow-empty -m m`,
	} {
		assert.Error(t, bash(t, sb, script, sandboxEnv...), script)
	}
	for _, script := range []string{
		`git -C "$ROOT" status`,
		`git -C "$ROOT" log --oneline`,
		`git -C "$ROOT" diff`,
	} {
		assert.NoError(t, bash(t, sb, script, sandboxEnv...), script)
	}

	h := hold(t, sb)
	for _, name := range []string{"commondir", "config.worktree", "worktrees", "modules"} {
		_, err := os.Lstat(filepath.Join(git, name))
		assert.ErrorIs(t, err, os.ErrNotExist, name)
	}
	host("status")
	h.release(t)

	assert.NoFileExists(t, filepath.Join(git, "commondir"))
	assert.FileExists(t, filepath.Join(git, "config"))
	assert.DirExists(t, filepath.Join(git, "hooks"))
	assert.Equal(t, head, host("rev-parse", "HEAD"))
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

func TestBwrapSpecialDirectoryDenies(t *testing.T) {
	for _, path := range []string{"/proc", "/dev"} {
		t.Run(path, func(t *testing.T) {
			secret := filepath.Join(resolvedTemp(t), "secret")
			writeFile(t, secret)
			sb := newBwrap(t, Policy{DenyRead: []string{path, secret}})[0]
			require.NoError(t, bash(t, sb, `test -d "$DENIED" && test -z "$(ls -A "$DENIED")"`, "DENIED="+path))
			require.Error(t, bash(t, sb, `cat "$SECRET"`, "SECRET="+secret))
		})
	}
}

func TestBwrapUnixDatagramSocketpairRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is the Linux sandbox")
	}
	for _, network := range []bool{false, true} {
		sb := newBwrap(t, Policy{Network: network})[0]
		dir := t.TempDir()
		path := filepath.Join(dir, "s")
		ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
		require.NoError(t, err)
		defer ln.Close()
		script := `import socket, sys; a, b = socket.socketpair(socket.AF_UNIX, socket.SOCK_DGRAM); a.sendto(b"probe", sys.argv[1])`
		control := exec.CommandContext(t.Context(), "python3", "-c", script, path)
		out, err := control.CombinedOutput()
		require.NoError(t, err, string(out))
		buf := make([]byte, 16)
		n, _, err := ln.ReadFromUnix(buf)
		require.NoError(t, err)
		require.Equal(t, "probe", string(buf[:n]))

		probe := `import errno, socket, sys
try:
    a, b = socket.socketpair(socket.AF_UNIX, socket.SOCK_DGRAM)
except OSError as e:
    assert e.errno == errno.EPERM, e
else:
    a.sendto(b"escaped", sys.argv[1])
    sys.exit("Unix datagram socketpair was allowed")`
		cmd, done, err := sb.Command([]string{"python3", "-c", probe, path})
		require.NoError(t, err)
		out, err = cmd.CombinedOutput()
		done()
		require.NoError(t, err, "network=%v: %s", network, out)
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
