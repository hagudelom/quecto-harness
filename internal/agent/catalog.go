package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/jesdga95/quecto-harness/internal/config"
	"github.com/jesdga95/quecto-harness/internal/llm"
	"github.com/jesdga95/quecto-harness/internal/mcp"
	"github.com/jesdga95/quecto-harness/internal/perms"
	"github.com/jesdga95/quecto-harness/internal/store"
	"github.com/jesdga95/quecto-harness/internal/tools"
)

func (e *Engine) available(ctx context.Context, s *Session, turnMode config.WorkMode) []mcp.Offered {
	s.mu.Lock()
	project, set, mode := s.Project, s.Toolset, s.Mode
	s.mu.Unlock()
	all := e.mcpTools(ctx, s, project, set)
	if mode == config.WorkPlan || turnMode == config.WorkPlan {
		return slices.DeleteFunc(all, func(t mcp.Offered) bool { return !t.ReadOnly })
	}
	return all
}

type catalogChange struct{ kind, server, name string }

func catalogChanges(before, after store.CatalogView) []catalogChange {
	var changes []catalogChange
	for _, server := range slices.Sorted(maps.Keys(before)) {
		if _, ok := after[server]; !ok {
			changes = append(changes, catalogChange{"removed", server, ""})
		}
	}
	for _, server := range slices.Sorted(maps.Keys(after)) {
		old, known := before[server]
		next := after[server]
		for _, name := range slices.Sorted(maps.Keys(old)) {
			if _, ok := next[name]; !ok {
				changes = append(changes, catalogChange{"tools_removed", server, name})
			}
		}
		for _, name := range slices.Sorted(maps.Keys(next)) {
			version, ok := old[name]
			switch {
			case !known:
				changes = append(changes, catalogChange{"added", server, name})
			case !ok:
				changes = append(changes, catalogChange{"tools_added", server, name})
			case version != next[name]:
				changes = append(changes, catalogChange{"tools_changed", server, name})
			}
		}
	}
	return changes
}

const catalogOpen = "<tool_catalog>"
const catalogClose = "</tool_catalog>"
const catalogLimit = 2048

func catalogSignal(changes []catalogChange) json.RawMessage {
	if len(changes) == 0 {
		return nil
	}
	encode := func(n int) []byte {
		servers := map[string]any{}
		groups := map[string]map[string][]string{}
		var removed []string
		for _, c := range changes[:n] {
			if c.kind == "removed" {
				removed = append(removed, c.server)
				continue
			}
			if groups[c.kind] == nil {
				groups[c.kind] = map[string][]string{}
				servers[c.kind] = groups[c.kind]
			}
			wire, _ := mcp.WireName(c.server, c.name)
			groups[c.kind][c.server] = append(groups[c.kind][c.server], wire)
		}
		if len(removed) > 0 {
			servers["removed"] = removed
		}
		data := map[string]any{"servers": servers}
		if n < len(changes) {
			data["omitted"] = len(changes) - n
		}
		out, _ := json.Marshal(data)
		return out
	}
	fits := func(data []byte) bool { return len(data)+len(catalogOpen)+len(catalogClose) <= catalogLimit }
	// Each item costs at least two quote bytes. Bound the trial encodings
	// even when the full catalog contains thousands of names.
	end := min(len(changes), catalogLimit/2)
	if data := encode(end); fits(data) {
		return data
	}
	firstTooLarge := sort.Search(end, func(i int) bool { return !fits(encode(i + 1)) })
	return encode(firstTooLarge)
}

func (e *Engine) refreshCatalog(ctx context.Context, s *Session, offer tools.Offer) bool {
	var inventory []mcp.Offered
	if offer.Sends("tool_search") {
		inventory = e.available(ctx, s, offer.Mode)
	}
	return s.recordCatalog(inventory)
}

func (s *Session) recordCatalog(offers []mcp.Offered) bool {
	view := store.CatalogView{}
	for _, t := range offers {
		if view[t.Server] == nil {
			view[t.Server] = map[string]string{}
		}
		view[t.Server][t.Tool] = t.Version
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var carrier *store.Entry
	var previous store.CatalogView
	for i := len(s.Entries) - 1; i >= 0; i-- {
		entry := &s.Entries[i]
		if !entry.InContext {
			continue
		}
		if carrier != nil && carrier.Kind == "user" && carrier.Meta["steer"] == true {
			// A result and updates queued while it ran share the next request.
			if entry.Kind == "tool" || (entry.Kind == "user" && entry.Meta["steer"] == true) {
				carrier = entry
				continue
			}
		}
		if carrier == nil {
			switch entry.Kind {
			case "user", "tool", "compaction":
				carrier = entry
				continue
			case "assistant":
				return false
			}
		}
		if entry.Catalog != nil {
			previous = entry.Catalog.View
			break
		}
		if entry.Kind == "compaction" {
			break
		}
	}
	if carrier == nil {
		return false
	}
	if carrier.Kind == "compaction" {
		previous = nil
	}
	signal := catalogSignal(catalogChanges(previous, view))
	if len(signal) == 0 {
		changed := carrier.Catalog != nil
		carrier.Catalog = nil
		return changed
	}
	carrier.Catalog = &store.CatalogUpdate{Signal: signal, View: view}
	return true
}

func catalogText(e store.Entry) string {
	if e.Catalog == nil {
		return ""
	}
	return catalogOpen + string(e.Catalog.Signal) + catalogClose
}

// consulted compares the current definition with the last search result for
// this tool that the conversation still holds.
func (s *Session) consulted(t mcp.Offered) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.Entries) - 1; i >= 0; i-- {
		e := s.Entries[i]
		if !e.InContext {
			continue
		}
		if v, ok := e.Discovered[t.Wire]; ok {
			return v == t.Version
		}
	}
	return false
}

type resolvedCall struct {
	llm.ToolCall
	offered *mcp.Offered
}

func (e *Engine) resolveCall(ctx context.Context, s *Session, mode config.WorkMode, call llm.ToolCall) (resolvedCall, error) {
	target := resolvedCall{ToolCall: call}
	if mcp.IsWire(call.Function.Name) {
		return target, mcp.ErrToolChanged
	}
	if call.Function.Name != "tool_call" {
		return target, nil
	}
	var args struct {
		Name      string
		Arguments json.RawMessage
	}
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || len(args.Arguments) == 0 || args.Arguments[0] != '{' {
		return target, errors.New("Pass a qualified name and an arguments object.")
	}
	for _, t := range e.available(ctx, s, mode) {
		if t.Wire == args.Name && s.consulted(t) {
			target.offered = &t
			target.Function.Name = t.Wire
			target.Function.Arguments = string(args.Arguments)
			return target, nil
		}
	}
	return target, mcp.ErrToolChanged
}

func (e *Engine) dispatchPermission(s *Session, owner, project string, t mcp.Offered, record *store.PermissionRecord) func() bool {
	return func() bool {
		s.mu.Lock()
		permitted := s.AgentID == owner && s.Project == project && (s.Mode != config.WorkPlan || t.ReadOnly)
		s.mu.Unlock()
		if !permitted {
			return false
		}
		if record != nil && (record.Decision == "auto" || record.Decision == "full") {
			return e.perms.EvaluateMCP(t.Server, t.Tool, e.cfg.Config().Mode) == perms.ActionAllow
		}
		return true
	}
}

type searchDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Schema       json.RawMessage `json:"inputSchema"`
	Instructions string          `json:"instructions,omitempty"`
}

func (e *Engine) searchTools(ctx context.Context, s *Session, raw json.RawMessage, runner *tools.Runner, mode config.WorkMode) outcome {
	var args struct{ Query *string }
	if json.Unmarshal(raw, &args) != nil || args.Query == nil {
		return failedTool(errors.New("Pass a search query."))
	}
	query := strings.TrimSpace(*args.Query)
	all := e.available(ctx, s, mode)
	matches := searchCatalog(all, query)
	return searchResult(runner, matches, query)
}

func searchResult(runner *tools.Runner, matches []mcp.Offered, query string) outcome {
	versions := map[string]string{}
	servers := map[string]bool{}
	var body strings.Builder
	ends := make([]int, 0, len(matches))
	body.WriteByte('[')
	for i, t := range matches {
		def := searchDefinition{Name: t.Wire, Description: t.Description, Schema: t.Schema}
		if !servers[t.Server] {
			def.Instructions = t.Instructions
			servers[t.Server] = true
		}
		if i > 0 {
			body.WriteString(",\n")
		}
		data, _ := json.Marshal(def)
		body.Write(data)
		ends = append(ends, body.Len())
	}
	body.WriteByte(']')
	result, available := runner.LimitOutput("tool_search", body.String())
	for i, t := range matches {
		if ends[i] > available {
			break
		}
		versions[t.Wire] = t.Version
	}
	meta := map[string]any{"server": "MCP", "tool": "Search", "query": query}
	if len(servers) == 1 {
		meta["server"] = matches[0].Server
	}
	return outcome{content: result.Output, truncated: result.Truncated, discovered: versions, meta: meta}
}

// A qualified name selects one definition. Free text ranks matches by the
// number of query terms found, keeping server order for ties.
func searchCatalog(all []mcp.Offered, query string) []mcp.Offered {
	terms := strings.Fields(strings.ToLower(query))
	scores := map[string]int{}
	var matches []mcp.Offered
	for _, t := range all {
		if t.Wire == query {
			return []mcp.Offered{t}
		}
		text := strings.ToLower(t.Wire + " " + t.Tool + " " + t.Description)
		for _, term := range terms {
			if strings.Contains(text, term) {
				scores[t.Wire]++
			}
		}
		if len(terms) == 0 || scores[t.Wire] > 0 {
			matches = append(matches, t)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return scores[matches[i].Wire] > scores[matches[j].Wire] })
	return matches
}

func failedTool(err error) outcome {
	return outcome{content: err.Error(), meta: map[string]any{"error": true}}
}
