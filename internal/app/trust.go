package app

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/skill"
	"github.com/keyute/kopi/internal/trust"
)

// TrustRequest is what the trust prompt shows. Every field may come from an
// untrusted file.
type TrustRequest struct {
	Folder string
	// Parent is offered as a choice unless it is empty.
	Parent string
	// Servers has one line per server, such as "name: command args".
	Servers []string
	// Instructions are the paths of the folder's instruction files.
	Instructions []string
	// Skills are the paths of the folder's skill folders.
	Skills []string
}

type TrustChoice int

const (
	TrustCancelled TrustChoice = iota
	TrustFolder
	TrustParent
	TrustDeny
)

// ServerSet is the MCP server set of a run in one folder, and whether the
// folder is trusted.
type ServerSet struct {
	// Servers are the servers to start: the global ones and, in a trusted
	// folder, the project ones, which replace global ones of the same name.
	Servers map[string]config.MCPServer
	// Project names the servers in Servers that the project declares.
	Project map[string]bool
	// Untrusted are the project's servers, skipped as the folder is not trusted.
	Untrusted map[string]config.MCPServer
	// Undecided means no trust decision applies and none was asked for.
	Undecided bool
	// Trusted means the folder is trusted, so its servers and instruction
	// files and skills are used.
	Trusted bool
	Invalid map[string]error
	Errs    []error
}

// AskTrust asks the human whether to trust a folder; nil means no human can
// be asked.
type AskTrust func(TrustRequest) (TrustChoice, error)

// ResolveServers decides whether the folder is trusted and merges the
// project's servers over global ones when it is. The trust file is opened
// only when the project declares a server or has instruction files or skill
// folders, which instructions and skills list; an undecided folder is asked
// about through ask and the answer saved, and without ask it is untrusted for
// this run.
func ResolveServers(global map[string]config.MCPServer, trustFile, cwd, userHome string, instructions, skills []string, ask AskTrust) (ServerSet, error) {
	s := ServerSet{Servers: maps.Clone(global), Project: map[string]bool{}}
	if s.Servers == nil {
		s.Servers = map[string]config.MCPServer{}
	}
	project, invalid, err := config.ProjectServers(filepath.Join(cwd, policy.ProjectMCPFile))
	if err != nil {
		s.Errs = append(s.Errs, fmt.Errorf("skipped this folder's MCP servers: %w", err))
		project, invalid = nil, nil
	}
	for _, name := range slices.Sorted(maps.Keys(invalid)) {
		s.Errs = append(s.Errs, fmt.Errorf("MCP server %s: %w", name, invalid[name]))
		project[name] = config.MCPServer{}
	}
	if len(project) == 0 && len(instructions) == 0 && len(skills) == 0 {
		return s, nil
	}
	store, err := trust.Open(trustFile)
	if err != nil {
		return ServerSet{}, err
	}
	d, ok, err := store.Lookup(cwd)
	if err != nil {
		return ServerSet{}, err
	}
	switch {
	case ok:
	case ask == nil:
		s.Undecided = true
	default:
		if d, err = decideTrust(store, cwd, userHome, project, invalid, instructions, skills, ask); err != nil {
			return ServerSet{}, err
		}
	}
	if d != trust.Trusted {
		s.Untrusted = project
		return s, nil
	}
	s.Trusted = true
	s.Invalid = invalid
	for name, p := range project {
		s.Project[name] = true
		if invalid[name] != nil {
			delete(s.Servers, name)
			continue
		}
		s.Servers[name] = p
	}
	return s, nil
}

// decideTrust asks about cwd and saves the answer; a cancelled prompt saves
// nothing and returns no decision. The folder and parent are resolved first,
// so what is shown is what is saved, and the home check sees through
// symlinks and the case a name was typed in. An answer saved for the user's
// home or the root would cover almost every folder below it, so there either
// answer holds for this run only.
func decideTrust(store *trust.Store, cwd, userHome string, project map[string]config.MCPServer, invalid map[string]error, instructions, skills []string, ask AskTrust) (trust.Decision, error) {
	cwd, err := trust.Canonical(cwd)
	if err != nil {
		return 0, err
	}
	userHome, err = trust.Canonical(userHome)
	if err != nil {
		return 0, err
	}
	broad := trust.Broad(cwd, userHome)
	req := TrustRequest{Folder: cwd, Instructions: instructions, Skills: skills}
	if parent := filepath.Dir(cwd); !broad && parent != filepath.Dir(parent) && parent != userHome {
		req.Parent = parent
	}
	req.Servers = serverLines(project, invalid)
	choice, err := ask(req)
	if err != nil {
		return 0, err
	}
	dir, d := cwd, trust.Trusted
	switch choice {
	case TrustFolder:
		if broad {
			return trust.Trusted, nil
		}
	case TrustParent:
		dir = req.Parent
	case TrustDeny:
		if broad {
			return trust.Untrusted, nil
		}
		d = trust.Untrusted
	default:
		return 0, nil
	}
	if err := store.Set(dir, d); err != nil {
		return 0, fmt.Errorf("failed to save the trust decision: %w", err)
	}
	return d, nil
}

// serverLines lists project's servers and the invalid ones by name as the trust
// question shows them.
func serverLines(project map[string]config.MCPServer, invalid map[string]error) []string {
	names := map[string]bool{}
	for name := range project {
		names[name] = true
	}
	for name := range invalid {
		names[name] = true
	}
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(names)) {
		target := serverTarget(project[name])
		if invalid[name] != nil {
			target = "invalid, will not start"
		}
		lines = append(lines, name+": "+target)
	}
	return lines
}

// covers is what a trust answer saved for dir would cover when Kopi starts
// there: its .mcp.json servers and project skill folders. An unreadable
// .mcp.json lists no servers.
func covers(dir, canonicalHome string) (servers, skills []string) {
	project, invalid, err := config.ProjectServers(filepath.Join(dir, policy.ProjectMCPFile))
	if err == nil {
		servers = serverLines(project, invalid)
	}
	return servers, skill.Find(dir, filepath.Join(canonicalHome, ".agents", "skills"))
}

// serverTarget is what a server runs or connects to, as configured: the
// command and its arguments, or the URL, then the names, never the values, of
// the environment variables and headers it sets.
func serverTarget(s config.MCPServer) string {
	target := s.URL
	if s.Command != "" {
		target = strings.Join(append([]string{s.Command}, s.Args...), " ")
	}
	if len(s.Env) > 0 {
		target += " (env: " + strings.Join(slices.Sorted(maps.Keys(s.Env)), ", ") + ")"
	}
	if len(s.Headers) > 0 {
		target += " (headers: " + strings.Join(slices.Sorted(maps.Keys(s.Headers)), ", ") + ")"
	}
	return target
}
