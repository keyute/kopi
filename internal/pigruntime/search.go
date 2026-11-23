package pigruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/keyute/kopi/internal/tool"
)

// searchProtocol adds a provider's hosted search to a request and reads the
// pages it reports from the stream events.
type searchProtocol struct {
	enable func(payload map[string]any)
	read   func(event map[string]any, f *found)
	// sep joins the answer's text blocks. Anthropic splits one sentence across
	// citation blocks, so it joins with none.
	sep string
}

// searchProtocols is keyed by the wire protocol, never by provider: an
// endpoint that speaks one of these is offered search.
var searchProtocols = map[ai.API]searchProtocol{
	ai.APIOpenAIResponses:      {enable: enableResponsesSearch, read: readResponsesSources, sep: "\n"},
	ai.APIAzureOpenAIResponses: {enable: enableResponsesSearch, read: readResponsesSources, sep: "\n"},
	ai.APIOpenAICodexResponses: {
		enable: func(payload map[string]any) {
			enableResponsesSearch(payload)
			payload["tool_choice"] = "required"
		},
		read: readResponsesSources,
		sep:  "\n",
	},
	ai.APIAnthropicMessages: {
		enable: func(payload map[string]any) {
			payload["tools"] = append(asSlice(payload["tools"]), map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 5})
		},
		read: readAnthropicSources,
	},
	ai.APIGoogleGenerativeAI: {
		enable: func(payload map[string]any) {
			config, _ := payload["config"].(map[string]any)
			if config == nil {
				config = map[string]any{}
				payload["config"] = config
			}
			config["tools"] = []any{map[string]any{"google_search": map[string]any{}}}
		},
		read: readGoogleSources,
		sep:  "\n",
	},
}

// found collects the pages a provider reported. It is written only by the
// stream observer and read after the request returns.
type found struct {
	cited, consulted []tool.Source

	// texts counts the text blocks started so far, searched whether a search
	// block started since the last of them, and breaks holds the ordinals of
	// the text blocks that follow a search after other text.
	texts    int
	searched bool
	breaks   map[int]bool
}

// join joins the text blocks of content with sep, or with a paragraph break
// where the answer resumes after a search.
func (f *found) join(content []ai.AssistantContentBlock, sep string) string {
	var b strings.Builder
	n := 0
	for _, block := range content {
		text, ok := block.(ai.TextContent)
		if !ok {
			continue
		}
		switch {
		case n > 0 && f.breaks[n]:
			b.WriteString("\n\n")
		case n > 0:
			b.WriteString(sep)
		}
		b.WriteString(text.Text)
		n++
	}
	return b.String()
}

func (f *found) cite(url, title string)    { addSource(&f.cited, url, title) }
func (f *found) consult(url, title string) { addSource(&f.consulted, url, title) }

func addSource(list *[]tool.Source, url, title string) {
	if url == "" || slices.ContainsFunc(*list, func(s tool.Source) bool { return s.URL == url }) {
		return
	}
	*list = append(*list, tool.Source{Title: title, URL: url})
}

// sources prefers the pages the answer cites over those the provider consulted.
func (f *found) sources() []tool.Source {
	if len(f.cited) > 0 {
		return f.cited
	}
	return f.consulted
}

// CanSearch reports whether model ("provider/id") is on a protocol with hosted
// search. It checks no credential.
func (r *Runtime) CanSearch(model string) bool {
	provider, id, _ := strings.Cut(model, "/")
	m := r.svcs.ModelRuntime().GetModel(provider, id)
	if m == nil {
		return false
	}
	_, ok := searchProtocols[m.ProviderMeta.API]
	return ok
}

// Search has model ("provider/id") answer query under system with its
// provider's hosted web search, outside any session. It also returns what the
// request used, with its cost estimated from the model's price as
// Session.Usage estimates it.
func (r *Runtime) Search(ctx context.Context, model, system, query string) (tool.SearchResult, Usage, error) {
	m, err := r.usableModel(ctx, model)
	if err != nil {
		return tool.SearchResult{}, Usage{}, fmt.Errorf("failed to search with model %q: %w", model, err)
	}
	proto, ok := searchProtocols[m.ProviderMeta.API]
	if !ok {
		return tool.SearchResult{}, Usage{}, fmt.Errorf("failed to search with model %q: %w", model, ErrNoSearch)
	}
	var f found
	text, used, err := r.complete(ctx, m, system, query, func(content []ai.AssistantContentBlock) string {
		return f.join(content, proto.sep)
	}, ai.StreamOptions{
		OnPayload: func(payload any, _ *ai.Model) (any, error) {
			return editPayload(payload, proto.enable)
		},
		OnProviderStreamEvent: func(_ context.Context, data any, _ *ai.Model) error {
			if event, ok := data.(map[string]any); ok {
				proto.read(event, &f)
			}
			return nil
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			return tool.SearchResult{}, used, err
		}
		return tool.SearchResult{}, used, fmt.Errorf("failed to search with model %q: %w", model, err)
	}
	return tool.SearchResult{Answer: text, Sources: f.sources()}, used, nil
}

// editPayload returns payload as a JSON object with enable applied. PiG hands
// each protocol's request in a different Go form, so it goes through JSON.
func editPayload(payload any, enable func(map[string]any)) (any, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var object map[string]any
	if err := dec.Decode(&object); err != nil {
		return nil, fmt.Errorf("failed to decode request: %w", err)
	}
	enable(object)
	return object, nil
}

func enableResponsesSearch(payload map[string]any) {
	payload["tools"] = append(asSlice(payload["tools"]), map[string]any{"type": "web_search"})
	payload["include"] = append(asSlice(payload["include"]), "web_search_call.action.sources")
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// readResponsesSources reads the OpenAI Responses stream: url_citation
// annotations are cited, a search call's action sources are consulted.
func readResponsesSources(event map[string]any, f *found) {
	switch str(event, "type") {
	case "response.output_text.annotation.added":
		readResponsesAnnotation(asMap(event["annotation"]), f)
	case "response.output_item.added", "response.output_item.done":
		readResponsesItem(asMap(event["item"]), f)
	case "response.completed", "response.incomplete", "response.done":
		for _, item := range asSlice(asMap(event["response"])["output"]) {
			readResponsesItem(asMap(item), f)
		}
	}
}

func readResponsesItem(item map[string]any, f *found) {
	switch str(item, "type") {
	case "web_search_call":
		for _, source := range asSlice(asMap(item["action"])["sources"]) {
			s := asMap(source)
			f.consult(str(s, "url"), str(s, "title"))
		}
	case "message":
		for _, part := range asSlice(item["content"]) {
			for _, annotation := range asSlice(asMap(part)["annotations"]) {
				readResponsesAnnotation(asMap(annotation), f)
			}
		}
	}
}

func readResponsesAnnotation(annotation map[string]any, f *found) {
	if str(annotation, "type") == "url_citation" {
		f.cite(str(annotation, "url"), str(annotation, "title"))
	}
}

// readAnthropicSources reads the Anthropic Messages stream: a
// web_search_result_location citation is cited, a web_search_tool_result block
// lists the pages consulted. An error result carries an object, not a list.
func readAnthropicSources(event map[string]any, f *found) {
	switch str(event, "type") {
	case "content_block_delta":
		delta := asMap(event["delta"])
		if str(delta, "type") != "citations_delta" {
			return
		}
		citation := asMap(delta["citation"])
		if str(citation, "type") == "web_search_result_location" {
			f.cite(str(citation, "url"), str(citation, "title"))
		}
	case "content_block_start":
		block := asMap(event["content_block"])
		switch str(block, "type") {
		case "text":
			if f.searched && f.texts > 0 {
				if f.breaks == nil {
					f.breaks = map[int]bool{}
				}
				f.breaks[f.texts] = true
			}
			f.searched = false
			f.texts++
		case "server_tool_use":
			f.searched = true
		case "web_search_tool_result":
			f.searched = true
			for _, result := range asSlice(block["content"]) {
				r := asMap(result)
				f.consult(str(r, "url"), str(r, "title"))
			}
		}
	}
}

// readGoogleSources reads the Gemini stream: the last grounding metadata that
// lists web chunks replaces what earlier chunks reported.
func readGoogleSources(event map[string]any, f *found) {
	candidates := asSlice(event["candidates"])
	if len(candidates) == 0 {
		return
	}
	metadata := asMap(asMap(candidates[0])["groundingMetadata"])
	chunks := asSlice(metadata["groundingChunks"])
	cited := make([]bool, len(chunks))
	for _, support := range asSlice(metadata["groundingSupports"]) {
		for _, value := range asSlice(asMap(support)["groundingChunkIndices"]) {
			index, ok := value.(float64)
			if ok && index >= 0 && index < float64(len(chunks)) && index == float64(int(index)) {
				cited[int(index)] = true
			}
		}
	}
	var next found
	for i, chunk := range chunks {
		web := asMap(asMap(chunk)["web"])
		next.consult(str(web, "uri"), str(web, "title"))
		if cited[i] {
			next.cite(str(web, "uri"), str(web, "title"))
		}
	}
	if len(next.consulted) > 0 {
		*f = next
	}
}
