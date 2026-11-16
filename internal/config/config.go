package config

import (
	encjson "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	"github.com/keyute/kopi/internal/policy"
)

const Version = 1

const delim = "."

// Config is the effective configuration. It holds no secrets and must never
// grow any: Entries prints every value verbatim.
type Config struct {
	Version int    `koanf:"version"`
	Model   string `koanf:"model"` // "provider/id"; empty means not configured (there is no default model)
	// Mode is the mode sessions start in, plan or execute; --mode overrides it
	// for the session the process opens.
	Mode string `koanf:"mode"`
	// Keys maps an action from KeyActions to the keys bound to it.
	Keys        map[string][]string `koanf:"keys"`
	StatusLine  StatusLine          `koanf:"statusline"`
	Permissions Permissions         `koanf:"permissions"`
}

// Permissions holds rule strings checked only for syntax here; a later layer's
// list replaces an earlier one's whole.
type Permissions struct {
	Allow []string `koanf:"allow"`
	Ask   []string `koanf:"ask"`
	Deny  []string `koanf:"deny"`
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
	values["permissions"] = map[string]any{"allow": []string{}, "ask": []string{}, "deny": []string{}}
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
		if c.Model != "" {
			if _, _, err := ParseModelRef(c.Model); err != nil {
				return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
			}
		}
		if k.Exists("mode") {
			if _, err := policy.ParseMode(c.Mode); err != nil {
				return nil, fmt.Errorf("config layer %s: %w: %w", l.Name, ErrInvalidValue, err)
			}
		}
		if err := validateUI(c); err != nil {
			return nil, fmt.Errorf("config layer %s: %w", l.Name, err)
		}
		if err := validatePermissions(c.Permissions); err != nil {
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
// key. DecodeNil routes JSON null here too, so null cannot wipe a block.
func rejectWrongShape(from, to reflect.Type, data any) (any, error) {
	v := reflect.ValueOf(data)
	switch to.Kind() {
	case reflect.Slice:
		if from.Kind() != reflect.Slice || v.IsNil() {
			return nil, fmt.Errorf("%w: %v is not a list", ErrInvalidValue, data)
		}
	case reflect.Map, reflect.Struct:
		if from.Kind() != reflect.Map || v.IsNil() {
			return nil, fmt.Errorf("%w: %v is not an object", ErrInvalidValue, data)
		}
	}
	return data, nil
}

// rejectFractionalInt exists because the JSON parser yields float64 and
// mapstructure truncates a float decoded into an int ("version": 1.9 -> 1).
func rejectFractionalInt(_, to reflect.Type, data any) (any, error) {
	if f, ok := data.(float64); ok && to.Kind() >= reflect.Int && to.Kind() <= reflect.Uint64 && f != math.Trunc(f) {
		return nil, fmt.Errorf("%w: %v is not an integer", ErrInvalidValue, f)
	}
	return data, nil
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

func originOf(origins map[string]string, key string) string {
	if o, ok := origins[key]; ok {
		return o
	}
	return "unset"
}

type Entry struct{ Key, Value, Origin string }

// Entries returns every schema key sorted by key, including unset ones (empty
// Value, Origin "unset"). Lists are shown as compact JSON.
func (r *Resolved) Entries() []Entry {
	c := r.Config
	values := map[string]any{
		"model":             c.Model,
		"mode":              c.Mode,
		"version":           c.Version,
		"statusline.left":   c.StatusLine.Left,
		"statusline.right":  c.StatusLine.Right,
		"permissions.allow": c.Permissions.Allow,
		"permissions.ask":   c.Permissions.Ask,
		"permissions.deny":  c.Permissions.Deny,
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
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Key, b.Key) })
	return entries
}

func formatValue(v any) string {
	if list, ok := v.([]string); ok {
		b, _ := encjson.Marshal(list) // a []string always marshals
		return string(b)
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
