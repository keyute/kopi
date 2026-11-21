package pigruntime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/tool"
)

const searchModel = "kopi-search/search-1"

type searchRequest struct {
	path   string
	header http.Header
	body   map[string]any
}

// newSearchRuntime serves a custom provider speaking api from an httptest
// server: serve writes each response and the requests arrive on the channel.
func newSearchRuntime(t *testing.T, api, key string, serve func(http.ResponseWriter)) (*Runtime, <-chan searchRequest) {
	t.Helper()
	sb := newSandbox(t)
	requests := make(chan searchRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		assert.NoError(t, json.Unmarshal(raw, &body))
		requests <- searchRequest{path: r.URL.Path, header: r.Header.Clone(), body: body}
		serve(w)
	}))
	t.Cleanup(server.Close)
	writeAgentFile(t, sb, "models.json", fmt.Sprintf(
		`{"providers":{"kopi-search":{"baseUrl":%q,"api":%q,"apiKey":%q,"models":[{"id":"search-1","reasoning":true,"cost":{"input":1000000,"output":3000000,"cacheRead":0,"cacheWrite":0}}]}}}`, server.URL, api, key,
	))
	r, err := New(t.Context(), Options{Home: sb.kopiHome, AgentDir: sb.agentDir, CWD: sb.ws})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	return r, requests
}

// sse writes one server-sent event per value, named by its "type" member.
func sse(w http.ResponseWriter, events ...map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
	}
}

func responsesStream(w http.ResponseWriter, text string, annotations []any, extra ...map[string]any) {
	message := map[string]any{
		"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": annotations}},
	}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_1", "status": "in_progress", "output": []any{}}},
	}
	events = append(events, extra...)
	events = append(events,
		map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "in_progress", "content": []any{}}},
		map[string]any{"type": "response.content_part.added", "item_id": "msg_1", "output_index": 1, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 1, "content_index": 0, "delta": text},
	)
	for _, a := range annotations {
		events = append(events, map[string]any{"type": "response.output_text.annotation.added", "item_id": "msg_1", "output_index": 1, "content_index": 0, "annotation": a})
	}
	events = append(events,
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": message},
		map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_1", "status": "completed", "output": []any{message},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		}},
	)
	sse(w, events...)
}

func urlCitation(url, title string) map[string]any {
	return map[string]any{"type": "url_citation", "url": url, "title": title, "start_index": 0, "end_index": 1}
}

func toolTypes(t *testing.T, body map[string]any) []any {
	t.Helper()
	tools, ok := body["tools"].([]any)
	require.True(t, ok, "request has no tools: %v", body)
	return tools
}

func TestSearchResponsesAddsHostedToolAndReturnsCitedSources(t *testing.T) {
	r, requests := newSearchRuntime(t, "openai-responses", "synthetic-key", func(w http.ResponseWriter) {
		responsesStream(w, "Go 1.27 is out.", []any{
			urlCitation("https://go.dev/doc/go1.27", "Go 1.27"),
			urlCitation("https://go.dev/doc/go1.27", "Go 1.27"),
			urlCitation("https://example.com/news", "News"),
		}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{
			"type": "web_search_call", "id": "ws_1", "status": "completed",
			"action": map[string]any{"type": "search", "query": "go", "sources": []any{map[string]any{"type": "url", "url": "https://consulted.example/"}}},
		}})
	})

	got, _, err := r.Search(t.Context(), searchModel, "system text", "latest Go release")

	require.NoError(t, err)
	assert.Equal(t, tool.SearchResult{Answer: "Go 1.27 is out.", Sources: []tool.Source{
		{Title: "Go 1.27", URL: "https://go.dev/doc/go1.27"},
		{Title: "News", URL: "https://example.com/news"},
	}}, got)
	req := <-requests
	assert.Equal(t, "/responses", req.path)
	assert.Equal(t, []any{map[string]any{"type": "web_search"}}, toolTypes(t, req.body))
	assert.Contains(t, req.body["include"], "web_search_call.action.sources")
	assert.NotContains(t, req.body, "tool_choice")
	assert.Contains(t, fmt.Sprint(req.body["input"]), "system text")
}

func TestSearchReturnsTheRequestsCost(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-responses", "synthetic-key", func(w http.ResponseWriter) {
		responsesStream(w, "An answer.", nil)
	})

	_, cost, err := r.Search(t.Context(), searchModel, "system", "q")

	require.NoError(t, err)
	assert.Equal(t, 4.0, cost)
}

func TestSearchFallsBackToConsultedSources(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-responses", "synthetic-key", func(w http.ResponseWriter) {
		responsesStream(w, "An answer.", nil, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{
			"type": "web_search_call", "id": "ws_1", "status": "completed",
			"action": map[string]any{"type": "search", "sources": []any{
				map[string]any{"type": "url", "url": "https://one.example/"},
				map[string]any{"type": "url", "url": "https://two.example/"},
				map[string]any{"type": "url", "url": "https://one.example/"},
			}},
		}})
	})

	got, _, err := r.Search(t.Context(), searchModel, "system", "q")

	require.NoError(t, err)
	assert.Equal(t, []tool.Source{{URL: "https://one.example/"}, {URL: "https://two.example/"}}, got.Sources)
}

func anthropicStream(w http.ResponseWriter, blocks ...map[string]any) {
	events := []map[string]any{
		{"type": "message_start", "message": map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "search-1", "content": []any{},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
		}},
	}
	for i, block := range blocks {
		events = append(events, map[string]any{"type": "content_block_start", "index": i, "content_block": block["start"]})
		for _, delta := range block["deltas"].([]any) {
			events = append(events, map[string]any{"type": "content_block_delta", "index": i, "delta": delta})
		}
		events = append(events, map[string]any{"type": "content_block_stop", "index": i})
	}
	events = append(events,
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 1}},
		map[string]any{"type": "message_stop"},
	)
	sse(w, events...)
}

func citedText(text, url, title string) map[string]any {
	return map[string]any{
		"start": map[string]any{"type": "text", "text": ""},
		"deltas": []any{
			map[string]any{"type": "text_delta", "text": text},
			map[string]any{"type": "citations_delta", "citation": map[string]any{"type": "web_search_result_location", "url": url, "title": title, "cited_text": "x"}},
		},
	}
}

func TestSearchAnthropicJoinsSplitTextAndReadsCitations(t *testing.T) {
	r, requests := newSearchRuntime(t, "anthropic-messages", "sk-ant-oat-synthetic", func(w http.ResponseWriter) {
		anthropicStream(w,
			citedText("Go 1.27", "https://go.dev/doc/go1.27", "Go 1.27"),
			citedText(" is out.", "https://go.dev/doc/go1.27", "Go 1.27"),
			citedText(" Really.", "https://example.com/news", ""),
			map[string]any{
				"start":  map[string]any{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": []any{map[string]any{"type": "web_search_result", "url": "https://consulted.example/", "title": "C"}}},
				"deltas": []any{},
			},
		)
	})

	got, _, err := r.Search(t.Context(), searchModel, "system", "latest Go release")

	require.NoError(t, err)
	assert.Equal(t, tool.SearchResult{Answer: "Go 1.27 is out. Really.", Sources: []tool.Source{
		{Title: "Go 1.27", URL: "https://go.dev/doc/go1.27"},
		{URL: "https://example.com/news"},
	}}, got)
	req := <-requests
	assert.Contains(t, req.header.Get("anthropic-beta"), "oauth-2025-04-20", "PiG's beta header survives the payload edit")
	assert.Equal(t, []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": float64(5)}}, toolTypes(t, req.body))
	assert.NotContains(t, req.body, "betas")
}

func TestSearchAnthropicBreaksTheAnswerAfterASearch(t *testing.T) {
	r, _ := newSearchRuntime(t, "anthropic-messages", "synthetic-key", func(w http.ResponseWriter) {
		anthropicStream(w,
			map[string]any{"start": map[string]any{"type": "text", "text": ""}, "deltas": []any{map[string]any{"type": "text_delta", "text": "preamble"}}},
			map[string]any{"start": map[string]any{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": map[string]any{}}, "deltas": []any{}},
			map[string]any{"start": map[string]any{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": []any{}}, "deltas": []any{}},
			citedText("Go 1.27", "https://go.dev/doc/go1.27", "Go 1.27"),
			citedText(" is out.", "https://go.dev/doc/go1.27", "Go 1.27"),
		)
	})

	got, _, err := r.Search(t.Context(), searchModel, "system", "q")

	require.NoError(t, err)
	assert.Equal(t, "preamble\n\nGo 1.27 is out.", got.Answer)
}

func TestSearchResponsesJoinsMessagesWithNewline(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-responses", "synthetic-key", func(w http.ResponseWriter) {
		item := func(id, text string) map[string]any {
			return map[string]any{
				"type": "message", "id": id, "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			}
		}
		first, second := item("msg_1", "I'll search."), item("msg_2", "Answer.")
		sse(w,
			map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1", "status": "in_progress", "output": []any{}}},
			map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "in_progress", "content": []any{}}},
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": first},
			map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_2", "role": "assistant", "status": "in_progress", "content": []any{}}},
			map[string]any{"type": "response.output_item.done", "output_index": 1, "item": second},
			map[string]any{"type": "response.completed", "response": map[string]any{
				"id": "resp_1", "status": "completed", "output": []any{first, second},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			}},
		)
	})

	got, _, err := r.Search(t.Context(), searchModel, "system", "q")

	require.NoError(t, err)
	assert.Equal(t, "I'll search.\nAnswer.", got.Answer)
}

func TestSearchAnthropicReadsConsultedSourcesAndSkipsErrorResults(t *testing.T) {
	r, _ := newSearchRuntime(t, "anthropic-messages", "synthetic-key", func(w http.ResponseWriter) {
		anthropicStream(w,
			map[string]any{
				"start":  map[string]any{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}},
				"deltas": []any{},
			},
			map[string]any{
				"start":  map[string]any{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_2", "content": []any{map[string]any{"type": "web_search_result", "url": "https://one.example/", "title": "One"}}},
				"deltas": []any{},
			},
			map[string]any{"start": map[string]any{"type": "text", "text": ""}, "deltas": []any{map[string]any{"type": "text_delta", "text": "Answer."}}},
		)
	})

	got, _, err := r.Search(t.Context(), searchModel, "system", "q")

	require.NoError(t, err)
	assert.Equal(t, tool.SearchResult{Answer: "Answer.", Sources: []tool.Source{{Title: "One", URL: "https://one.example/"}}}, got)
}

func TestSearchGoogleEnablesGroundingAndTheLastMetadataWins(t *testing.T) {
	chunk := func(text, finish string, webs ...map[string]any) map[string]any {
		var chunks []any
		for _, web := range webs {
			chunks = append(chunks, map[string]any{"web": web})
		}
		candidate := map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}}
		if finish != "" {
			candidate["finishReason"] = finish
		}
		if chunks != nil {
			candidate["groundingMetadata"] = map[string]any{"groundingChunks": chunks}
		}
		return map[string]any{"candidates": []any{candidate}}
	}
	r, requests := newSearchRuntime(t, "google-generative-ai", "synthetic-key", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []map[string]any{
			chunk("Go 1.27 ", "", map[string]any{"uri": "https://early.example/", "title": "Early"}),
			chunk("is out.", "", map[string]any{"uri": "https://vertexaisearch.example/a", "title": "A"}, map[string]any{"uri": "https://vertexaisearch.example/b", "title": "B"}),
			chunk("", "STOP"),
		} {

			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
	})

	got, _, err := r.Search(t.Context(), searchModel, "system", "q")

	require.NoError(t, err)
	assert.Equal(t, tool.SearchResult{Answer: "Go 1.27 is out.", Sources: []tool.Source{
		{Title: "A", URL: "https://vertexaisearch.example/a"},
		{Title: "B", URL: "https://vertexaisearch.example/b"},
	}}, got)
	req := <-requests
	assert.Equal(t, []any{map[string]any{"google_search": map[string]any{}}}, toolTypes(t, req.body))
}

func TestSearchGoogleReturnsOnlySupportedChunks(t *testing.T) {
	r, _ := newSearchRuntime(t, "google-generative-ai", "synthetic-key", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Answer."}]},"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://a.example/","title":"A"}},{"web":{"uri":"https://b.example/","title":"B"}}],"groundingSupports":[{"groundingChunkIndices":[1]}]}}]}

data: {"candidates":[{"finishReason":"STOP"}]}

`)
	})

	got, _, err := r.Search(t.Context(), searchModel, "system", "q")

	require.NoError(t, err)
	assert.Equal(t, tool.SearchResult{Answer: "Answer.", Sources: []tool.Source{{Title: "B", URL: "https://b.example/"}}}, got)
}

func TestReadGoogleSourcesSupportIndices(t *testing.T) {
	for _, tt := range []struct {
		name     string
		supports string
		cited    []tool.Source
	}{
		{name: "absent supports"},
		{name: "empty supports", supports: `,"groundingSupports":[]`},
		{name: "only B", supports: `,"groundingSupports":[{"groundingChunkIndices":[1]}]`, cited: []tool.Source{{Title: "B", URL: "https://b.example/"}}},
		{name: "invalid indices", supports: `,"groundingSupports":[{"groundingChunkIndices":[-1,0.5,"0",null,{},true,5,1e100]}]`},
		{name: "duplicate indices and URLs in chunk order", supports: `,"groundingSupports":[{"groundingChunkIndices":[4,1,1,3]},{"groundingChunkIndices":[0]}]`, cited: []tool.Source{{Title: "A", URL: "https://a.example/"}, {Title: "B", URL: "https://b.example/"}}},
		{name: "nonweb chunks", supports: `,"groundingSupports":[{"groundingChunkIndices":[2,3]}]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var event map[string]any
			require.NoError(t, json.Unmarshal([]byte(`{"candidates":[{"groundingMetadata":{"groundingChunks":[
				{"web":{"uri":"https://a.example/","title":"A"}},
				{"web":{"uri":"https://b.example/","title":"B"}},
				{"retrievedContext":{"uri":"https://context.example/"}},
				{"web":{"title":"No URI"}},
				{"web":{"uri":"https://a.example/","title":"Duplicate"}}
			]`+tt.supports+`}}]}`), &event))
			var f found

			readGoogleSources(event, &f)

			consulted := []tool.Source{{Title: "A", URL: "https://a.example/"}, {Title: "B", URL: "https://b.example/"}}
			assert.Equal(t, consulted, f.consulted)
			assert.Equal(t, tt.cited, f.cited)
			if len(tt.cited) > 0 {
				assert.Equal(t, tt.cited, f.sources())
			} else {
				assert.Equal(t, consulted, f.sources())
			}
		})
	}
}

func TestReadGoogleSourcesReplacesCitationsAndPreservesMetadataLessStop(t *testing.T) {
	var f found
	for i, raw := range []string{
		`{"candidates":[{"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://early.example/"}}],"groundingSupports":[{"groundingChunkIndices":[0]}]}}]}`,
		`{"candidates":[{"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://later.example/"}}]}}]}`,
		`{"candidates":[{"groundingMetadata":{"groundingChunks":[{"web":{"title":"No URI"}}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}]}`,
	} {
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &event))
		readGoogleSources(event, &f)
		if i == 0 {
			assert.Equal(t, []tool.Source{{URL: "https://early.example/"}}, f.cited)
		} else {
			assert.Empty(t, f.cited)
			assert.Equal(t, []tool.Source{{URL: "https://later.example/"}}, f.consulted)
		}
	}
	assert.Empty(t, f.cited)
	assert.Equal(t, []tool.Source{{URL: "https://later.example/"}}, f.consulted)
}

func TestSearchEmptyAnswerFails(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-responses", "synthetic-key", func(w http.ResponseWriter) {
		responsesStream(w, " ", nil)
	})

	_, cost, err := r.Search(t.Context(), searchModel, "system", "q")

	require.ErrorIs(t, err, ErrNoAnswer)
	assert.Contains(t, err.Error(), searchModel)
	assert.Equal(t, 4.0, cost)
}

func TestSearchReportsProviderError(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-responses", "synthetic-key", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"web_search is not supported","type":"invalid_request_error"}}`)
	})

	_, _, err := r.Search(t.Context(), searchModel, "system", "q")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "web_search is not supported")
}

func TestSearchRefusesModelWithoutHostedSearch(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))

	assert.False(t, r.CanSearch(fauxModel))
	assert.False(t, r.CanSearch(fauxProvider+"/missing"))
	_, _, err := r.Search(t.Context(), fauxModel, "system", "q")
	require.ErrorIs(t, err, ErrNoSearch)
}

func TestCanSearchFollowsTheProtocol(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-responses", "synthetic-key", func(http.ResponseWriter) {})
	assert.True(t, r.CanSearch(searchModel))

	r, _ = newSearchRuntime(t, "openai-completions", "synthetic-key", func(http.ResponseWriter) {})
	assert.False(t, r.CanSearch(searchModel))
}

func TestCodexSearchRequiresTheHostedTool(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	models := r.svcs.ModelRuntime().GetAllModels("openai-codex")
	require.NotEmpty(t, models)
	codex, ok := models[0].(*ai.Model)
	require.True(t, ok)
	assert.True(t, r.CanSearch("openai-codex/"+codex.ID))

	for api, required := range map[ai.API]bool{
		ai.APIOpenAICodexResponses: true, ai.APIOpenAIResponses: false, ai.APIAzureOpenAIResponses: false,
	} {
		payload := map[string]any{}
		searchProtocols[api].enable(payload)

		assert.Equal(t, []any{map[string]any{"type": "web_search"}}, payload["tools"], api)
		assert.Equal(t, []any{"web_search_call.action.sources"}, payload["include"], api)
		if required {
			assert.Equal(t, "required", payload["tool_choice"], api)
		} else {
			assert.NotContains(t, payload, "tool_choice", api)
		}
	}
}

type marshaledPayload struct{}

func (marshaledPayload) MarshalJSON() ([]byte, error) {
	return []byte(`{"model":"m","max":12345678901234567890,"include":["reasoning.encrypted_content"],"tools":[{"type":"function"}]}`), nil
}

func TestEditPayloadAppliesEditToAMarshaledValue(t *testing.T) {
	got, err := editPayload(marshaledPayload{}, enableResponsesSearch)

	require.NoError(t, err)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"m","max":12345678901234567890,"tools":[{"type":"function"},{"type":"web_search"}],"include":["reasoning.encrypted_content","web_search_call.action.sources"]}`, string(raw))
	assert.Contains(t, string(raw), "12345678901234567890", "numbers keep their digits")
}
