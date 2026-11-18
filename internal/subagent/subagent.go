// Package subagent loads the subagent definitions users write as Markdown
// files with YAML frontmatter.
package subagent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

type Definition struct {
	Name, Description string
	// Model is "provider/id", or "" for the root session's model.
	Model string
	// Effort is a level name, or "" for the model's default.
	Effort string
	// Tools is nil for every tool a subagent can have.
	Tools []string
	// Plan is frontmatter `mode: plan`.
	Plan         bool
	Instructions bool
	// Prompt is the file's body.
	Prompt string
	// Path is the file the definition was read from.
	Path string
}

type frontmatter struct {
	Name         string    `yaml:"name"`
	Description  string    `yaml:"description"`
	Model        string    `yaml:"model"`
	Effort       string    `yaml:"effort"`
	Tools        *[]string `yaml:"tools"`
	Mode         string    `yaml:"mode"`
	Instructions *bool     `yaml:"instructions"`
}

// Load reads every *.md file directly in dir, sorted by name. A missing dir
// yields none. tools is the set of tool names a definition may list; names
// starting with mcp__ are always allowed.
func Load(dir string, tools []string) ([]Definition, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", dir, err)
	}
	var defs []Definition
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// Stat follows links so definitions symlinked in by dotfile managers load.
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		def, err := parse(path, tools)
		if err != nil {
			return nil, err
		}
		if i := slices.IndexFunc(defs, func(d Definition) bool { return d.Name == def.Name }); i >= 0 {
			return nil, fmt.Errorf("%w %q: %s and %s", ErrDuplicateName, def.Name, defs[i].Path, path)
		}
		defs = append(defs, def)
	}
	slices.SortFunc(defs, func(a, b Definition) int { return strings.Compare(a.Name, b.Name) })
	return defs, nil
}

func parse(path string, tools []string) (Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, fmt.Errorf("failed to read %s: %w", path, err)
	}
	head, body, err := split(string(data))
	if err != nil {
		return Definition{}, fmt.Errorf("%w: %s: %w", ErrInvalidDefinition, path, err)
	}
	var fm frontmatter
	dec := yaml.NewDecoder(strings.NewReader(head))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil {
		return Definition{}, fmt.Errorf("%w: %s: failed to decode frontmatter: %w", ErrInvalidDefinition, path, err)
	}
	def, err := build(fm, body, path, tools)
	if err != nil {
		return Definition{}, fmt.Errorf("%w: %s: %w", ErrInvalidDefinition, path, err)
	}
	return def, nil
}

// split returns the frontmatter between the leading and closing --- lines and
// the body after them.
func split(s string) (head, body string, err error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return "", "", fmt.Errorf("no opening --- line")
	}
	var lines []string
	for {
		line, after, found := strings.Cut(rest, "\n")
		if strings.TrimRight(line, " \t") == "---" {
			return strings.Join(lines, "\n"), strings.Trim(after, "\n"), nil
		}
		if !found {
			return "", "", fmt.Errorf("no closing --- line")
		}
		lines = append(lines, line)
		rest = after
	}
}

func build(fm frontmatter, body, path string, tools []string) (Definition, error) {
	if err := singleLine("name", fm.Name); err != nil {
		return Definition{}, err
	}
	if err := singleLine("description", fm.Description); err != nil {
		return Definition{}, err
	}
	def := Definition{
		Name:         fm.Name,
		Description:  fm.Description,
		Model:        strings.TrimSpace(fm.Model),
		Effort:       strings.TrimSpace(fm.Effort),
		Instructions: fm.Instructions == nil || *fm.Instructions,
		Prompt:       body,
		Path:         path,
	}
	switch fm.Mode {
	case "":
	case "plan":
		def.Plan = true
	default:
		return Definition{}, fmt.Errorf("mode %q: want plan or nothing", fm.Mode)
	}
	if fm.Tools != nil {
		if len(*fm.Tools) == 0 {
			return Definition{}, fmt.Errorf("tools is empty: omit it for the default")
		}
		for i, t := range *fm.Tools {
			if !slices.Contains(tools, t) && !strings.HasPrefix(t, "mcp__") {
				return Definition{}, fmt.Errorf("unknown tool %q", t)
			}
			if slices.Contains((*fm.Tools)[:i], t) {
				return Definition{}, fmt.Errorf("duplicate tool %q", t)
			}
		}
		def.Tools = *fm.Tools
	}
	return def, nil
}

func singleLine(field, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", field)
	}
	// U+2028 and U+2029 end a line without being control characters.
	if strings.ContainsFunc(v, func(r rune) bool { return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' }) {
		return fmt.Errorf("%s must be a single line without control characters", field)
	}
	return nil
}
