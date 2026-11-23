package pigruntime

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

// Tokens is what requests read and wrote, by kind.
type Tokens struct{ Input, Output, CacheRead, CacheWrite int }

// ModelUsage is what one model's requests cost. Model is "provider/id", the id
// the provider answered with when it names one; "" for the summaries a session
// writes when it compacts and any usage a tool result reports.
type ModelUsage struct {
	Model  string
	Tokens int
	Cost   float64 // dollars, estimated from the model's price
}

// Usage is what a session's requests, or one request outside a session, cost.
type Usage struct {
	Tokens Tokens
	Models []ModelUsage // unordered
}

// Add returns u and more together: tokens summed, rows of one Model merged, a
// row of no tokens and no cost left out as PiG's breakdown does.
func (u Usage) Add(more Usage) Usage {
	sum := Usage{
		Tokens: Tokens{
			Input:      u.Tokens.Input + more.Tokens.Input,
			Output:     u.Tokens.Output + more.Tokens.Output,
			CacheRead:  u.Tokens.CacheRead + more.Tokens.CacheRead,
			CacheWrite: u.Tokens.CacheWrite + more.Tokens.CacheWrite,
		},
		Models: slices.Clone(u.Models),
	}
	for _, row := range more.Models {
		if row.Tokens == 0 && row.Cost == 0 {
			continue
		}
		i := slices.IndexFunc(sum.Models, func(m ModelUsage) bool { return m.Model == row.Model })
		if i < 0 {
			sum.Models = append(sum.Models, row)
			continue
		}
		sum.Models[i].Tokens += row.Tokens
		sum.Models[i].Cost += row.Cost
	}
	return sum
}

// Cost is the dollars of every row.
func (u Usage) Cost() float64 {
	var cost float64
	for _, row := range u.Models {
		cost += row.Cost
	}
	return cost
}

// summariesKey is the key PiG's session accounting files the requests of
// compactions and branch summaries under, and the usage of any tool result.
const summariesKey = "Tools/summaries"

// UsageByModel returns what the session's requests used and cost, by model.
// It reads PiG's session accounting, which PiG keeps per model and rebuilds
// from the log on resume; the accounting keeps one token total per model.
func (s *Session) UsageByModel() Usage {
	accounting := s.inner.SessionManager().Accounting()
	tokens := accounting.Tokens
	u := Usage{Tokens: Tokens{Input: tokens.Input, Output: tokens.Output, CacheRead: tokens.CacheRead, CacheWrite: tokens.CacheWrite}}
	for _, row := range accounting.UsageBreakdown {
		model := row.Key
		if model == summariesKey {
			model = ""
		}
		u.Models = append(u.Models, ModelUsage{Model: model, Tokens: row.Tokens, Cost: row.Cost})
	}
	return u
}

// LoggedMessages is how many user and assistant messages the session's log
// holds, which a compaction does not lower.
func (s *Session) LoggedMessages() int {
	accounting := s.inner.SessionManager().Accounting()
	return accounting.UserMessages + accounting.AssistantMessages
}

// Cache is what a session's requests report about the provider's prompt cache.
type Cache struct {
	// Reported is false until a request reports cache tokens; Hit is the
	// percent of the last request's prompt read from the cache.
	Reported bool
	Hit      float64
	// A miss is prompt an earlier request made cacheable that a later one paid
	// for again; MissedCost is what that cost over the cache-read price.
	Misses     int
	MissedCost float64
	// Expires is when the cache the last request left goes cold by time; zero
	// when there is no request yet, another model made the last one, or the
	// model declares no cache lifetime.
	Expires time.Time
}

// Record is what the session's whole log says beyond PiG's accounting.
type Record struct {
	Cache Cache
	// ModelTime and ToolTime are the summed durations of the session's model
	// requests and of its tool runs; a message logged without one adds nothing.
	ModelTime, ToolTime time.Duration
	Compactions         int
}

// Record reads the session's whole log once. The cache's Hit and misses are
// PiG's accounting; its Expires is estimated by time alone, so a compaction
// still leaves it warm.
func (s *Session) Record() Record {
	manager := s.inner.SessionManager()
	accounting := manager.Accounting()
	tokens := accounting.Tokens
	rec := Record{Cache: Cache{
		Misses:     accounting.CacheWaste.MissCount,
		MissedCost: accounting.CacheWaste.MissedCost,
	}}
	// The footer rule of PiG: a share is shown once the session has cache tokens.
	if rate := accounting.LatestCacheHitRate; rate != nil && (tokens.CacheRead > 0 || tokens.CacheWrite > 0) {
		rec.Cache.Reported, rec.Cache.Hit = true, *rate
	}
	entries := manager.GetBranch()
	for _, entry := range entries {
		if entry.Base().Type == "compaction" {
			rec.Compactions++
		}
	}
	var model, tools int64
	// PiG recovery omits failed attempts with context edits; their time still
	// belongs to the session even though history no longer displays them.
	for _, entry := range entries {
		if entry.Base().Type != "message" {
			continue
		}
		var record struct {
			Message struct {
				Role       string `json:"role"`
				DurationMs *int64 `json:"durationMs"`
			} `json:"message"`
		}
		if err := json.Unmarshal(entry.Raw(), &record); err != nil {
			return rec
		}
		m := record.Message
		if m.DurationMs == nil {
			continue
		}
		switch m.Role {
		case "assistant":
			model += *m.DurationMs
		case "toolResult":
			tools += *m.DurationMs
		}
	}
	rec.ModelTime, rec.ToolTime = time.Duration(model)*time.Millisecond, time.Duration(tools)*time.Millisecond
	messages, err := branchMessages(entries)
	if err != nil {
		return rec
	}
	rec.Cache.Expires = s.cacheExpires(messages)
	return rec
}

// cacheExpires dates the last request in messages that sent a prompt, when the
// session's model made it. PiG's GetPromptCacheTtlMs is internal; Kopi sets no
// explicit retention, so its tier comes from the provider/process environment
// unless a cache write reports its tier. OpenAI Responses never reports
// CacheWrite1h. messages is the full branch history, which keeps that evidence
// when compaction drops active messages.
func (s *Session) cacheExpires(messages []agent.AgentMessage) time.Time {
	model := s.inner.Model()
	if model == nil || len(model.PromptCache) == 0 {
		return time.Time{}
	}
	var last *agent.AssistantMessage
	var tier string
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i].Assistant
		if m == nil || m.Usage == nil || m.Usage.Input+m.Usage.CacheRead+m.Usage.CacheWrite == 0 {
			continue
		}
		if last == nil {
			last = m
		}
		if m.Provider != last.Provider || m.ModelID != last.ModelID || m.Usage.CacheWrite == 0 {
			continue
		}
		if m.Usage.CacheWrite1h != nil {
			tier = "short"
			if *m.Usage.CacheWrite1h > 0 {
				tier = "long"
			}
		}
		break
	}
	// Another model's cache is not one the session's model can hit.
	if last == nil || last.Provider != model.ProviderMeta.ProviderID || last.ModelID != model.ID || last.Timestamp <= 0 {
		return time.Time{}
	}
	if tier == "" {
		env, err := ai.CredentialStoreProviderEnv(context.Background(), s.inner.Services().Credentials(), model.ProviderMeta.ProviderID)
		if err != nil {
			return time.Time{}
		}
		tier = "short"
		if ai.GetProviderEnvValue("PI_CACHE_RETENTION", env) == "long" {
			tier = "long"
		}
	}
	seconds, ok := model.PromptCache[tier]
	if !ok {
		return time.Time{}
	}
	return time.UnixMilli(last.Timestamp).Add(time.Duration(seconds * float64(time.Second)))
}

// Size is the estimated tokens of one named prompt part or tool definition.
type Size struct {
	Name   string
	Tokens int
}

// ContextSizes estimates what the prompt parts and the loaded tool definitions
// hold of the session's context, in the order PiG sends them. The figures are
// PiG's character estimate, not the provider's count ContextUsage starts from.
func (s *Session) ContextSizes() (parts, tools []Size) {
	current := ai.GetCurrentSystemMessage(s.inner.Messages())
	if current == nil {
		return nil, nil
	}
	for _, section := range current.Sections {
		parts = append(parts, Size{Name: section.Name, Tokens: ai.EstimateTextTokens(*section.Value)})
	}
	for _, schema := range current.ToolsAdded {
		tools = append(tools, Size{Name: schema.Name, Tokens: ai.EstimateMessageTokens(ai.SystemMessage{ToolsAdded: []ai.ToolSchema{schema}})})
	}
	return parts, tools
}

// usageOf is the one request msg made, as a row keyed the way PiG's session
// accounting keys an assistant message (the provider and the model the reply
// names, else the one asked), so it merges with a session's rows.
func usageOf(msg *ai.AssistantMessage, asked *ai.Model) Usage {
	u := msg.Usage
	return Usage{
		Tokens: Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite},
		Models: []ModelUsage{{
			Model:  cmp.Or(msg.Provider, asked.ProviderMeta.ProviderID) + "/" + cmp.Or(msg.ResponseModel, msg.Model, asked.ID),
			Tokens: u.Input + u.Output + u.CacheRead + u.CacheWrite,
			Cost:   u.Cost.Total,
		}},
	}
}
