package pigruntime

import (
	"cmp"
	"slices"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
)

// Tokens is what requests read and wrote, by kind.
type Tokens struct{ Input, Output, CacheRead, CacheWrite int }

// ModelUsage is what one model's requests cost. Model is "provider/id", the id
// the provider answered with when it names one; "" for the summaries a session
// writes when it compacts.
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
// compactions and branch summaries under.
const summariesKey = "Tools/summaries"

// modelKey is the key PiG's session accounting files an assistant message
// under: the provider and the model the reply names, else the one asked.
func modelKey(provider, asked, responded string) string {
	if responded != "" {
		return provider + "/" + responded
	}
	return provider + "/" + asked
}

// UsageByModel returns what the session's requests used and cost, by model.
// It reads PiG's session accounting, which PiG keeps per model and rebuilds
// from the log on resume; the accounting keeps one token total per model.
func (s *Session) UsageByModel() Usage {
	accounting := s.inner.SessionManager().Accounting()
	tokens := s.inner.GetSessionStats().Tokens
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
	st := s.inner.GetSessionStats()
	return st.UserMessages + st.AssistantMessages
}

// Cache is what a session's requests report about the provider's prompt cache.
type Cache struct {
	// Reported is false until a request reports cache tokens; Hit is the
	// percent of the last request's prompt read from the cache.
	Reported bool
	Hit      float64
	// A miss is prompt an earlier request made cacheable that a later one paid
	// for again; MissedCost is what that cost over the cache-read price.
	Misses, MissedTokens int
	MissedCost           float64
	// Expires is when the cache the last request left goes cold by time; zero
	// when there is no request yet or the model declares no cache lifetime.
	Expires time.Time
}

// Cache returns what the session's requests report about the prompt cache.
// Hit and the misses are PiG's accounting; Expires is estimated by time alone,
// so a model switch or a compaction still leaves it warm.
func (s *Session) Cache() Cache {
	accounting := s.inner.SessionManager().Accounting()
	tokens := s.inner.GetSessionStats().Tokens
	c := Cache{
		Misses:       accounting.CacheWaste.MissCount,
		MissedTokens: accounting.CacheWaste.MissedTokens,
		MissedCost:   accounting.CacheWaste.MissedCost,
	}
	// The footer rule of PiG: a share is shown once the session has cache tokens.
	if rate := accounting.LatestCacheHitRate; rate != nil && (tokens.CacheRead > 0 || tokens.CacheWrite > 0) {
		c.Reported, c.Hit = true, *rate
	}
	c.Expires = s.cacheExpires()
	return c
}

// cacheExpires dates the last request that sent a prompt. PiG's GetPromptCacheTtlMs
// is internal and picks the tier from a retention setting Kopi never sets, so the
// tier is read from what the request wrote: long after a one-hour cache write,
// else short. The lifetimes are the model's promptCache entries.
func (s *Session) cacheExpires() time.Time {
	model := s.inner.Model()
	if model == nil {
		return time.Time{}
	}
	messages := s.inner.Messages()
	for i := len(messages) - 1; i >= 0; i-- {
		last := messages[i].Assistant
		if last == nil || last.Usage == nil || last.Usage.Input+last.Usage.CacheRead+last.Usage.CacheWrite == 0 {
			continue
		}
		tier := "short"
		if last.Usage != nil && last.Usage.CacheWrite1h != nil && *last.Usage.CacheWrite1h > 0 {
			tier = "long"
		}
		seconds, ok := model.PromptCache[tier]
		if !ok || last.Timestamp <= 0 {
			return time.Time{}
		}
		return time.UnixMilli(last.Timestamp).Add(time.Duration(seconds * float64(time.Second)))
	}
	return time.Time{}
}

// usageOf is the one request msg made, as a row keyed the way PiG's session
// accounting keys it, so it merges with a session's rows.
func usageOf(msg *ai.AssistantMessage, asked *ai.Model) Usage {
	u := msg.Usage
	return Usage{
		Tokens: Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite},
		Models: []ModelUsage{{
			Model:  modelKey(cmp.Or(msg.Provider, asked.ProviderMeta.ProviderID), cmp.Or(msg.Model, asked.ID), msg.ResponseModel),
			Tokens: u.Input + u.Output + u.CacheRead + u.CacheWrite,
			Cost:   u.Cost.Total,
		}},
	}
}
