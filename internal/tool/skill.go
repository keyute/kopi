package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/skill"
)

type skillArgs struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	Offset *int   `json:"offset"`
	Limit  *int   `json:"limit"`
}

// NewSkill returns the tool that serves skills' instructions and files. It
// serves every skill given, but its description lists only those the model may
// invoke. home is Kopi's home, whose files other than those under skills/ it
// refuses to read.
func NewSkill(skills []skill.Skill, home string) Tool {
	// Files are read only from the directory a skill was loaded from; nil marks
	// one that could not be identified.
	loaded := make([]os.FileInfo, len(skills))
	for i, sk := range skills {
		if info, err := os.Stat(sk.Dir); err == nil {
			loaded[i] = info
		}
	}
	return Tool{
		Name:        "Skill",
		Label:       "Skill",
		Description: skillDescription(skills),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Name of the skill.",
				},
				"file": map[string]any{
					"type":        "string",
					"description": "Path of a file of the skill, relative to the skill's directory. Omit it to get the skill's instructions.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based line number to start reading file from.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Maximum number of lines to read from file (at most 2000).",
				},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		// It runs before the arguments are validated, so it decodes leniently.
		Target: func(raw json.RawMessage) (policy.Target, error) {
			var args skillArgs
			_ = json.Unmarshal(raw, &args)
			_, target, err := skillTarget(skills, args)
			return target, err
		},
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (string, error) {
			args, err := decodeSkillArgs(raw)
			if err != nil {
				return "", err
			}
			i, target, err := skillTarget(skills, args)
			if err != nil {
				return "", err
			}
			if _, err := admit(target); err != nil {
				return "", err
			}
			if args.File == "" {
				return skill.Text(skills[i]), nil
			}
			return readSkillFile(ctx, skills[i], loaded[i], home, args, target)
		},
		Display: Display{
			Title: func(raw json.RawMessage) string {
				var args skillArgs
				_ = json.Unmarshal(raw, &args)
				switch {
				case args.Name == "":
					return "Skill"
				case args.File == "":
					return "Skill " + args.Name
				}
				return "Skill " + args.Name + " " + args.File
			},
			Summary: func(output string) string {
				if strings.HasPrefix(output, "<skill ") {
					return "loaded"
				}
				return readSummary(output)
			},
			Action: Action{Verb: "Loaded", One: "skill", Many: "skills"},
		},
	}
}

// skillDescription lists the skills the model may invoke, sorted by name.
func skillDescription(skills []skill.Skill) string {
	desc := "A skill is a set of instructions for a kind of task. " +
		"Called with a skill's name, returns its instructions; paths in them are relative to the skill's directory, and file reads one of its files. " +
		"The user can also load a skill by naming it."
	var listed []skill.Skill
	for _, s := range skills {
		if !s.DisableModelInvocation {
			listed = append(listed, s)
		}
	}
	if len(listed) == 0 {
		return desc
	}
	slices.SortFunc(listed, func(a, b skill.Skill) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	b.WriteString(desc + " Skills:")
	for _, s := range listed {
		b.WriteString("\n" + s.Name + ": " + strings.NewReplacer("\r\n", " ", "\n", " ").Replace(s.Description))
	}
	return b.String()
}

// skillTarget returns the index of the skill args names and the path the call
// reads, which is the skill's directory, or its file when args names one. Run
// and Target share it, so the gate judges what Run then admits.
func skillTarget(skills []skill.Skill, args skillArgs) (int, policy.Target, error) {
	i := slices.IndexFunc(skills, func(s skill.Skill) bool { return s.Name == args.Name })
	if i < 0 {
		return 0, policy.Target{}, fmt.Errorf("%w: %s", ErrUnknownSkill, args.Name)
	}
	if args.File != "" && !filepath.IsLocal(args.File) {
		return 0, policy.Target{}, fmt.Errorf("%w: file must be a path inside the skill's directory", ErrInvalidArgs)
	}
	path := skillPath(skills[i], args.File)
	target := policy.Target{Path: filepath.Clean(path), Real: filepath.Clean(path)}
	// A path that does not resolve is not an error here: the walk's failure
	// would tell what exists inside Kopi's home before the read refuses it.
	if resolved, _, err := walk(string(filepath.Separator), path, nil); err == nil {
		target.Real = resolved
	}
	return i, target, nil
}

// skillPath is not cleaned: walk expands symlinks before it applies "..".
func skillPath(s skill.Skill, file string) string {
	if file == "" {
		return s.Dir
	}
	return s.Dir + string(filepath.Separator) + file
}

func decodeSkillArgs(raw json.RawMessage) (skillArgs, error) {
	args, err := decodeArgs[skillArgs](raw)
	if err != nil {
		return args, err
	}
	if args.Name == "" {
		return args, fmt.Errorf("%w: name is required", ErrInvalidArgs)
	}
	if args.File == "" && (args.Offset != nil || args.Limit != nil) {
		return args, fmt.Errorf("%w: offset and limit need file", ErrInvalidArgs)
	}
	if args.File != "" && !filepath.IsLocal(args.File) {
		return args, fmt.Errorf("%w: file must be a path inside the skill's directory", ErrInvalidArgs)
	}
	if args.Offset != nil && *args.Offset < 1 {
		return args, fmt.Errorf("%w: offset must be at least 1", ErrInvalidArgs)
	}
	if args.Limit != nil && *args.Limit < 1 {
		return args, fmt.Errorf("%w: limit must be at least 1", ErrInvalidArgs)
	}
	return args, nil
}

func readSkillFile(ctx context.Context, s skill.Skill, loaded os.FileInfo, home string, args skillArgs, target policy.Target) (string, error) {
	if loaded == nil {
		return "", fmt.Errorf("%w: %s", ErrSkillChanged, s.Name)
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", fmt.Errorf("failed to open skill %s: %w", s.Name, err)
	}
	defer root.Close()
	// The path was resolved again by OpenRoot; only the opened root's identity
	// shows it is still the directory the skill was loaded from.
	now, err := root.Stat(".")
	if err != nil {
		return "", fmt.Errorf("failed to stat skill %s: %w", s.Name, err)
	}
	if !os.SameFile(loaded, now) {
		return "", fmt.Errorf("%w: %s", ErrSkillChanged, s.Name)
	}
	// O_NONBLOCK stops a FIFO open from waiting for a writer, which nothing
	// could interrupt.
	f, err := root.OpenFile(args.File, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	path := skillPath(s, args.File)
	if err != nil {
		// The home check runs before the failure is reported, so a path into the
		// home is refused alike whether or not it exists.
		_, protected, perr := walkSkillHome(path, home)
		if perr != nil {
			return "", perr
		}
		if protected {
			return "", fmt.Errorf("%w: %s", ErrProtected, args.File)
		}
		return "", fmt.Errorf("failed to open %s: %w", args.File, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat %s: %w", args.File, err)
	}
	if err := checkSkillHome(s, args.File, info, home, target); err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s", ErrNotRegular, args.File)
	}
	return read(ctx, f, readArgs{FilePath: args.File, Offset: args.Offset, Limit: args.Limit})
}

// checkSkillHome refuses the read of file, which was opened as opened, if
// resolving it enters Kopi's home outside its skills directory. It runs on the
// opened file, with the protected identities taken afresh on every read, so a
// skill directory that is or holds part of the home, or reaches it through a
// symlink, cannot expose the home's credentials or settings.
func checkSkillHome(s skill.Skill, file string, opened os.FileInfo, home string, admitted policy.Target) error {
	target, protected, err := walkSkillHome(skillPath(s, file), home)
	if err != nil {
		return err
	}
	if protected {
		return fmt.Errorf("%w: %s", ErrProtected, file)
	}
	if target != admitted.Real {
		return fmt.Errorf("%w: %s", ErrSkillChanged, s.Name)
	}
	return validateSkillTarget(target, opened)
}

// validateSkillTarget binds the opened file to the canonical admitted path
// without following symlinks installed after that path was resolved.
func validateSkillTarget(target string, opened os.FileInfo) error {
	parent, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return fmt.Errorf("failed to open parent of %s: %w", target, err)
	}
	defer func() { parent.Close() }()
	for _, name := range components(filepath.Dir(target)) {
		child, err := parent.OpenRoot(name)
		if err != nil {
			return fmt.Errorf("%w: failed to open parent of %s: %w", ErrSkillChanged, target, err)
		}
		before, lerr := parent.Lstat(name)
		now, serr := child.Stat(".")
		if lerr != nil || serr != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, now) {
			child.Close()
			return fmt.Errorf("%w: %s", ErrSkillChanged, target)
		}
		parent.Close()
		parent = child
	}
	resolved, err := parent.Lstat(filepath.Base(target))
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", target, err)
	}
	if resolved.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, resolved) {
		return fmt.Errorf("%w: %s", ErrSkillChanged, target)
	}
	return nil
}

// walkSkillHome walks path and reports whether it enters Kopi's home outside
// its skills directory. path must not be cleaned.
func walkSkillHome(path, home string) (string, bool, error) {
	entries, err := os.ReadDir(home)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return "", false, fmt.Errorf("failed to check protected path: %w", err)
	}
	skills, err := os.Lstat(filepath.Join(home, "skills"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return "", false, fmt.Errorf("failed to check protected path: %w", err)
	}
	var stores []os.FileInfo
	for _, e := range entries {
		entry := filepath.Join(home, e.Name())
		own, err := os.Lstat(entry)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("failed to check protected path: %w", err)
		}
		// By identity, not name: the filesystem may be case-insensitive. By the
		// entry, not what it points to: a skills symlink retargeted at another
		// home entry must not exempt that entry.
		if skills != nil && os.SameFile(own, skills) {
			continue
		}
		info, err := os.Stat(entry)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("failed to check protected path: %w", err)
		}
		stores = append(stores, info)
	}
	isProtected := func(info os.FileInfo) bool {
		return slices.ContainsFunc(stores, func(st os.FileInfo) bool { return os.SameFile(info, st) })
	}
	return walk(string(filepath.Separator), path, isProtected)
}
