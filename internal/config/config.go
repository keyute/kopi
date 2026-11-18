package config

import (
	encjson "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

const Version = 1

const delim = "."

// Config is the effective configuration. MCP server env and header values may
// be secrets; Entries never prints them, and prints every other value verbatim.
type Config struct {
	Version    int    `koanf:"version"`
	Model      string `koanf:"model"`      // "provider/id"; empty means not configured (there is no default model)
	SmallModel string `koanf:"smallModel"` // "provider/id"; empty means model
	// Mode is the mode sessions start in, plan or execute.
	Mode string `koanf:"mode"`
	// InstructionFallbacks are file names tried in order in a directory
	// without an AGENTS.md.
	InstructionFallbacks []string `koanf:"instructionFallbacks"`
	// Keys maps an action from KeyActions to the keys bound to it.
	Keys        map[string][]string  `koanf:"keys"`
	StatusLine  StatusLine           `koanf:"statusline"`
	Permissions Permissions          `koanf:"permissions"`
	Sandbox     Sandbox              `koanf:"sandbox"`
	MCPServers  map[string]MCPServer `koanf:"mcpServers"`
}

// Permissions holds rule strings checked only for syntax here; a later layer's
// list replaces an earlier one's whole.
type Permissions struct {
	Allow []string `koanf:"allow"`
	Ask   []string `koanf:"ask"`
	Deny  []string `koanf:"deny"`
}

type Sandbox struct {
	Enabled    *bool    `koanf:"enabled"` // nil: use the sandbox when it is available
	AllowWrite []string `koanf:"allowWrite"`
	DenyRead   []string `koanf:"denyRead"`
	Network    bool     `koanf:"network"`
}

// MCPServer is one entry of the standard mcpServers object.
type MCPServer struct {
	Type    string            `koanf:"type" json:"type"` // "", "stdio" or "http"
	Command string            `koanf:"command" json:"command"`
	Args    []string          `koanf:"args" json:"args"`
	Env     map[string]string `koanf:"env" json:"env"`
	URL     string            `koanf:"url" json:"url"`
	Headers map[string]string `koanf:"headers" json:"headers"`
}

func (s MCPServer) Validate() error {
	switch {
	case s.Command == "" && s.URL == "":
		return fmt.Errorf("%w: want command or url", ErrInvalidValue)
	case s.Command != "" && s.URL != "":
		return fmt.Errorf("%w: want command or url, not both", ErrInvalidValue)
	case s.Type != "" && s.Type != "stdio" && s.Type != "http":
		return fmt.Errorf("%w: unsupported transport type %q", ErrInvalidValue, s.Type)
	case s.Type == "stdio" && s.Command == "":
		return fmt.Errorf("%w: type stdio needs command", ErrInvalidValue)
	case s.Type == "http" && s.URL == "":
		return fmt.Errorf("%w: type http needs url", ErrInvalidValue)
	case s.Command == "" && (len(s.Args) > 0 || len(s.Env) > 0):
		return fmt.Errorf("%w: args and env need command", ErrInvalidValue)
	case s.URL == "" && len(s.Headers) > 0:
		return fmt.Errorf("%w: headers need url", ErrInvalidValue)
	}
	return nil
}

type Layer struct {
	Name     string
	Provider koanf.Provider
	Parser   koanf.Parser // nil for map-backed layers
}

func Defaults() Layer {
	values := uiDefaults()
	values["version"] = Version
	values["mode"] = policy.Plan.String()
	values["instructionFallbacks"] = []string{"CLAUDE.md"}
	values["permissions"] = map[string]any{"allow": []string{}, "ask": []string{}, "deny": []string{}}
	values["sandbox"] = map[string]any{"allowWrite": []string{}, "denyRead": []string{}, "network": false}
	values["mcpServers"] = map[string]any{}
	return Map("default", values)
}

// GlobalFile returns the layer for the config file at path; a missing file is
// an empty layer.
func GlobalFile(path string) (Layer, error) {
	name := fmt.Sprintf("global (%s)", path)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Map(name, nil), nil
	}
	if err != nil {
		return Layer{}, fmt.Errorf("failed to read config file %s: %w", path, err)
	}
	return Layer{Name: name, Provider: rawbytes.Provider(b), Parser: json.Parser()}, nil
}

func Map(name string, values map[string]any) Layer {
	if values == nil {
		values = map[string]any{}
	}
	return Layer{Name: name, Provider: confmap.Provider(values, delim)}
}

type Resolved struct {
	Config  Config
	Origins map[string]string // key -> name of the last layer that set it
}

// Resolve applies layers in order, later ones overriding earlier ones per key.
// Each layer is validated alone first so errors name the offending layer.
func Resolve(layers ...Layer) (*Resolved, error) {
	merged := koanf.New(delim)
	origins := map[string]string{}
	for _, l := range layers {
		k := koanf.New(delim)
		if err := k.Load(l.Provider, l.Parser); err != nil {
			return nil, fmt.Errorf("config layer %s: %w: %w", l.Name, ErrInvalidValue, err)
		}
		var c Config
		if err := decode(k, &c); err != nil {
			return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
		}
		for _, ref := range []string{c.Model, c.SmallModel} {
			if ref == "" {
				continue
			}
			if _, _, err := ParseModelRef(ref); err != nil {
				return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
			}
		}
		if k.Exists("mode") {
			if _, err := policy.ParseMode(c.Mode); err != nil {
				return nil, fmt.Errorf("config layer %s: %w: %w", l.Name, ErrInvalidValue, err)
			}
		}
		if err := validateInstructionFallbacks(c.InstructionFallbacks); err != nil {
			return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
		}
		if err := validateUI(c); err != nil {
			return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
		}
		if err := validatePermissions(c.Permissions); err != nil {
			return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
		}
		if err := validateSandbox(c.Sandbox); err != nil {
			return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
		}
		if err := validateMCPServers(c.MCPServers); err != nil {
			return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
		}
		for _, key := range k.Keys() {
			origins[key] = l.Name
		}
		if err := merged.Merge(k); err != nil {
			return nil, fmt.Errorf("failed to merge config layer %s: %w", l.Name, err)
		}
	}

	var c Config
	if err := decode(merged, &c); err != nil {
		return nil, err
	}
	if c.Version != Version {
		return nil, fmt.Errorf("%w: %d (from %s), want %d", ErrUnsupportedVersion, c.Version, originOf(origins, "version"), Version)
	}
	return &Resolved{Config: c, Origins: origins}, nil
}

// decode rejects unknown keys and refuses weak typing ("version": "1").
func decode(k *koanf.Koanf, c *Config) error {
	err := k.UnmarshalWithConf("", c, koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			ErrorUnused: true,
			DecodeNil:   true,
			DecodeHook:  mapstructure.ComposeDecodeHookFunc(rejectWrongShape, rejectFractionalInt),
		},
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrInvalidValue) {
		return err
	}
	// mapstructure reports unused keys only as an untyped message, so anything
	// that is not a typed conversion failure is the unknown-key case.
	var unconvertible *mapstructure.UnconvertibleTypeError
	var parse *mapstructure.ParseError
	if errors.As(err, &unconvertible) || errors.As(err, &parse) {
		return fmt.Errorf("%w: %w", ErrInvalidValue, err)
	}
	return fmt.Errorf("%w: %w", ErrUnknownKey, err)
}

// rejectWrongShape exists because mapstructure reports a scalar where a list or
// object belongs as an untyped error, which decode would misfile as an unknown
// key. DecodeNil routes JSON null here too, so null cannot wipe a block or
// reset an optional value to unset. Its errors name kinds, never the data,
// because a value under an MCP server's env or headers may be a secret.
func rejectWrongShape(from, to reflect.Type, data any) (any, error) {
	v := reflect.ValueOf(data)
	switch to.Kind() {
	case reflect.Slice:
		if from.Kind() != reflect.Slice || v.IsNil() {
			return nil, fmt.Errorf("%w: want a list, got %s", ErrInvalidValue, kindOf(v))
		}
	case reflect.Map, reflect.Struct:
		if from.Kind() != reflect.Map || v.IsNil() {
			return nil, fmt.Errorf("%w: want an object, got %s", ErrInvalidValue, kindOf(v))
		}
	case reflect.String:
		if v.Kind() != reflect.String {
			return nil, fmt.Errorf("%w: want a string, got %s", ErrInvalidValue, kindOf(v))
		}
	case reflect.Pointer:
		if from.Kind() == reflect.Pointer && v.IsNil() {
			return nil, fmt.Errorf("%w: null is not a value", ErrInvalidValue)
		}
	}
	return data, nil
}

func kindOf(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Invalid:
		return "null"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Slice:
		if v.IsNil() {
			return "null"
		}
		return "a list"
	case reflect.Map:
		if v.IsNil() {
			return "null"
		}
		return "an object"
	case reflect.Pointer:
		if v.IsNil() {
			return "null"
		}
		return kindOf(v.Elem())
	default:
		return "a number"
	}
}

// rejectFractionalInt exists because the JSON parser yields float64 and
// mapstructure truncates a float decoded into an int ("version": 1.9 -> 1).
func rejectFractionalInt(_, to reflect.Type, data any) (any, error) {
	if f, ok := data.(float64); ok && to.Kind() >= reflect.Int && to.Kind() <= reflect.Uint64 && f != math.Trunc(f) {
		return nil, fmt.Errorf("%w: %v is not an integer", ErrInvalidValue, f)
	}
	return data, nil
}

func validateInstructionFallbacks(names []string) error {
	for i, n := range names {
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, `/\`) || filepath.Base(n) != n {
			return fmt.Errorf("%w: instructionFallbacks[%d]: %q is not a file name", ErrInvalidValue, i, n)
		}
	}
	return nil
}

func validatePermissions(p Permissions) error {
	for _, list := range []struct {
		key   string
		rules []string
	}{{"allow", p.Allow}, {"ask", p.Ask}, {"deny", p.Deny}} {
		for i, r := range list.rules {
			if err := policy.ValidateRule(r); err != nil {
				return fmt.Errorf("%w: permissions.%s[%d]: %w", ErrInvalidValue, list.key, i, err)
			}
		}
	}
	return nil
}

func validateSandbox(s Sandbox) error {
	for _, list := range []struct {
		key   string
		paths []string
	}{{"allowWrite", s.AllowWrite}, {"denyRead", s.DenyRead}} {
		for i, p := range list.paths {
			if p == "" {
				return fmt.Errorf("%w: sandbox.%s[%d]: empty path", ErrInvalidValue, list.key, i)
			}
		}
	}
	return nil
}

func validateMCPServers(servers map[string]MCPServer) error {
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if err := tool.ValidateMCPServerName(name); err != nil {
			return fmt.Errorf("%w: mcpServers.%s: %w", ErrInvalidValue, name, err)
		}
		if err := servers[name].Validate(); err != nil {
			return fmt.Errorf("mcpServers.%s: %w", name, err)
		}
	}
	return nil
}

func originOf(origins map[string]string, key string) string {
	if o, ok := origins[key]; ok {
		return o
	}
	return "unset"
}

type Entry struct{ Key, Value, Origin string }

// Entries returns every schema key sorted by key, including unset ones (empty
// Value, Origin "unset"), plus one entry per MCP server showing only its
// transport and command or URL. Lists are shown as compact JSON.
func (r *Resolved) Entries() []Entry {
	c := r.Config
	values := map[string]any{
		"model":                c.Model,
		"smallModel":           c.SmallModel,
		"mode":                 c.Mode,
		"instructionFallbacks": c.InstructionFallbacks,
		"version":              c.Version,
		"statusline.left":      c.StatusLine.Left,
		"statusline.right":     c.StatusLine.Right,
		"permissions.allow":    c.Permissions.Allow,
		"permissions.ask":      c.Permissions.Ask,
		"permissions.deny":     c.Permissions.Deny,
		"sandbox.enabled":      c.Sandbox.Enabled,
		"sandbox.allowWrite":   c.Sandbox.AllowWrite,
		"sandbox.denyRead":     c.Sandbox.DenyRead,
		"sandbox.network":      c.Sandbox.Network,
	}
	for _, action := range KeyActions {
		values["keys."+action] = c.Keys[action]
	}
	entries := make([]Entry, 0, len(values))
	for key, v := range values {
		e := Entry{Key: key, Origin: originOf(r.Origins, key)}
		if _, ok := r.Origins[key]; ok {
			e.Value = formatValue(v)
		}
		entries = append(entries, e)
	}
	for name, s := range c.MCPServers {
		prefix := "mcpServers." + name + "."
		e := Entry{Key: "mcpServers." + name, Value: "http: " + s.URL, Origin: originOf(r.Origins, prefix+"url")}
		if s.Command != "" {
			e.Value = "stdio: " + strings.Join(append([]string{s.Command}, s.Args...), " ")
			e.Origin = originOf(r.Origins, prefix+"command")
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Key, b.Key) })
	return entries
}

func formatValue(v any) string {
	if list, ok := v.([]string); ok {
		b, _ := encjson.Marshal(list) // a []string always marshals
		return string(b)
	}
	if b, ok := v.(*bool); ok {
		return strconv.FormatBool(*b)
	}
	return fmt.Sprint(v)
}

// ParseModelRef splits "provider/id" on the first slash; ids may contain
// slashes.
func ParseModelRef(s string) (provider, id string, err error) {
	provider, id, ok := strings.Cut(s, "/")
	if !ok || provider == "" || id == "" {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidModel, s)
	}
	return provider, id, nil
}
