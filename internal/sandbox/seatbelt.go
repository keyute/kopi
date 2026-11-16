package sandbox

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/policy"
)

const sandboxExec = "/usr/bin/sandbox-exec"

var (
	//go:embed codex/base.sbpl
	basePolicy string
	//go:embed codex/preferences.sbpl
	preferencesPolicy string
	//go:embed codex/network.sbpl
	networkPolicy string
	//go:embed codex/deny.sbpl
	denyPolicy string
)

// probe fails when sandbox-exec is missing or cannot apply a profile, as when
// Kopi itself runs under Seatbelt: profiles do not nest.
func probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, sandboxExec, "-p", "(version 1)(allow default)", "/usr/bin/true")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: failed to run %s: %w: %s", ErrUnavailable, sandboxExec, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// seatbeltPrefix returns the sandbox-exec argv that applies p. Paths reach the
// profile only as -D parameters, so the profile text never needs escaping.
// Seatbelt applies the last matching rule: each section may only narrow the
// ones before it, and the rename protection stays last.
func seatbeltPrefix(p Policy) []string {
	var defs []string
	define := func(prefix string, paths []string) []string {
		keys := make([]string, len(paths))
		for i, path := range paths {
			keys[i] = fmt.Sprintf("%s_%d", prefix, i)
			defs = append(defs, "-D"+keys[i]+"="+path)
		}
		return keys
	}
	protected := forms(p.DenyRead, p.DenyWrite)
	write := define("WRITE", forms(p.Write))
	denyRead := define("DENY_READ", forms(p.DenyRead))
	denyWrite := define("DENY_WRITE", protected)
	ancestors := define("ANCESTOR", ancestors(protected))

	literal := func(k string) string { return `(literal (param "` + k + `"))` }
	subpath := func(k string) string { return `(subpath (param "` + k + `"))` }
	exact := func(k string) string { return literal(k) + " " + subpath(k) }
	rootDir := func(k string) string { return "(require-all " + literal(k) + " (vnode-type DIRECTORY))" }

	var b strings.Builder
	b.WriteString(basePolicy)
	if len(p.DenyRead) == 0 {
		b.WriteString(preferencesPolicy)
	}
	b.WriteString("\n(allow file-read*)\n")
	rule(&b, "deny file-read*", denyRead, exact)
	rule(&b, "allow file-write*", write, subpath)
	// literal beside subpath also stops first-time creation of a missing path.
	rule(&b, "deny file-write*", denyWrite, exact)
	if p.Network {
		b.WriteString(networkPolicy)
	}
	b.WriteString(denyPolicy)
	// Renaming an ancestor would move a protected path out from under its
	// rule, and a missing ancestor could be planted as a symlink.
	rule(&b, "deny file-write-unlink file-write-create", ancestors, literal)
	// A write root must not be renamed away or replaced; a missing one may
	// still be created.
	rule(&b, "deny file-write-unlink", write, rootDir)

	return append([]string{sandboxExec, "-p", b.String()}, append(defs, "--")...)
}

// rule writes one rule with a filter per key, and nothing when keys is empty:
// a rule without filters would apply to every path.
func rule(b *strings.Builder, head string, keys []string, filter func(string) string) {
	if len(keys) == 0 {
		return
	}
	b.WriteString("(" + head)
	for _, k := range keys {
		b.WriteString("\n  " + filter(k))
	}
	b.WriteString(")\n")
}

// forms keeps a cleaned lexical alias only when it names the resolved target:
// cleaning link/.. first could introduce a rule for an unrelated path.
func forms(lists ...[]string) []string {
	var out []string
	for _, list := range lists {
		for _, p := range list {
			resolved := policy.Resolve(p)
			clean := filepath.Clean(p)
			if policy.Resolve(clean) == resolved {
				out = appendNew(out, clean)
			}
			out = appendNew(out, resolved)
		}
	}
	return out
}

// ancestors returns every directory above paths, excluding the root.
func ancestors(paths []string) []string {
	var out []string
	for _, p := range paths {
		for d := filepath.Dir(p); d != filepath.Dir(d); d = filepath.Dir(d) {
			out = appendNew(out, d)
		}
	}
	return out
}

func appendNew(list []string, items ...string) []string {
	for _, s := range items {
		if !slices.Contains(list, s) {
			list = append(list, s)
		}
	}
	return list
}
