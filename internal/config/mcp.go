package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"strings"
)

// ProjectServers reads the mcpServers of a standard .mcp.json; a missing file
// is no servers. Unknown keys are ignored, since other clients add their own,
// and entries are decoded independently so one bad server does not hide the rest.
// Entry decode errors are returned by name, separately from file errors.
func ProjectServers(path string) (map[string]MCPServer, map[string]error, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read MCP config %s: %w", path, err)
	}
	var f struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, nil, fmt.Errorf("failed to parse MCP config %s: %w", path, err)
	}
	var servers map[string]MCPServer
	var invalid map[string]error
	if f.MCPServers != nil {
		servers = make(map[string]MCPServer, len(f.MCPServers))
	}
	for name, raw := range f.MCPServers {
		var server MCPServer
		if err := json.Unmarshal(raw, &server); err != nil {
			if invalid == nil {
				invalid = map[string]error{}
			}
			invalid[name] = fmt.Errorf("failed to decode MCP server: %w", err)
			continue
		}
		servers[name] = server
	}
	return servers, invalid, nil
}

var variable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// Expand returns a copy of s with ${VAR} and ${VAR:-default} expanded in its
// command, args, env values, url and header values.
func (s MCPServer) Expand(lookup func(string) (string, bool)) (MCPServer, error) {
	var unset error
	expand := func(v string) string {
		return variable.ReplaceAllStringFunc(v, func(m string) string {
			sub := variable.FindStringSubmatch(m)
			val, ok := lookup(sub[1])
			// As in the shell, :- also replaces a variable that is set but empty.
			if strings.Contains(m, ":-") && val == "" {
				return sub[2]
			}
			if ok {
				return val
			}
			if unset == nil {
				unset = fmt.Errorf("%w: %s", ErrUnsetVariable, sub[1])
			}
			return m
		})
	}
	expandMap := func(in map[string]string) map[string]string {
		if in == nil {
			return nil
		}
		out := maps.Clone(in)
		for k, v := range out {
			out[k] = expand(v)
		}
		return out
	}

	out := s
	out.Command = expand(s.Command)
	if s.Args != nil {
		out.Args = make([]string, len(s.Args))
		for i, a := range s.Args {
			out.Args[i] = expand(a)
		}
	}
	out.Env = expandMap(s.Env)
	out.URL = expand(s.URL)
	out.Headers = expandMap(s.Headers)
	if unset != nil {
		return MCPServer{}, unset
	}
	return out, nil
}
