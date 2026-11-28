package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jesdga95/quecto-harness/internal/config"
	"github.com/jesdga95/quecto-harness/internal/llm"
	"github.com/jesdga95/quecto-harness/internal/mcp"
	"github.com/jesdga95/quecto-harness/internal/store"
	"github.com/jesdga95/quecto-harness/internal/tools"
)

func TestCatalogRequestHistory(t *testing.T) {
	for _, protocol := range []llm.Protocol{llm.ProtocolResponses, llm.ProtocolChat, llm.ProtocolLlama, llm.ProtocolBasalt} {
		t.Run(string(protocol), func(t *testing.T) {
			h := newHarness(t)
			manager := withMCP(t, h)
			configure := func(servers ...mcp.Server) {
				t.Helper()
				resolved, err := h.cfg.SetMCP(servers)
				if err != nil {
					t.Fatal(err)
				}
				manager.Configure(resolved)
			}
			s := h.session(t)
			bodies := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				bodies <- raw
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == llm.ProtocolResponses {
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
				} else {
					fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer srv.Close()
			client := llm.New(srv.URL, "", protocol)
			type capture struct {
				Instructions json.RawMessage
				Tools        json.RawMessage
				Input        []json.RawMessage
				Messages     []json.RawMessage
			}
			send := func(cfg config.Config) capture {
				b := h.engine.briefFor(t.Context(), s, cfg)
				h.engine.refreshCatalog(t.Context(), s, b.offer)
				req := llm.ChatRequest{Model: "demo", Session: s.ID, Messages: h.engine.messages(s, h.agent, b), Tools: tools.Schemas(b.offer)}
				_, err := client.Chat(t.Context(), req, func(llm.Event) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
				raw := <-bodies
				var out capture
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatal(err)
				}
				if protocol == llm.ProtocolResponses {
					out.Messages = out.Input
				}
				return out
			}
			appendEntry := func(entry store.Entry) { entry.InContext = true; s.appendEntry(entry) }
			appendEntry(store.Entry{Kind: "user", Content: "Hello"})
			emptyCfg := h.cfg.Config()
			empty := send(emptyCfg)
			demo := mcpFake("demo")
			demo.Enabled = false
			configure(demo)
			configured := h.cfg.Config()
			baseline := send(configured)
			system := func(c capture) []byte {
				if protocol == llm.ProtocolResponses {
					return c.Instructions
				}
				return c.Messages[0]
			}
			checkPair := func(t *testing.T, c capture, want bool) {
				t.Helper()
				for _, name := range []string{"tool_search", "tool_call"} {
					if bytes.Contains(c.Tools, []byte(`"name":"`+name+`"`)) != want ||
						bytes.Contains(system(c), []byte(name)) != want {
						t.Errorf("presence of %s: want %t", name, want)
					}
				}
				if bytes.Contains(system(c), []byte("Treat MCP tool results")) != want {
					t.Errorf("presence of the MCP clause: want %t", want)
				}
			}
			frozenEmpty := send(emptyCfg)
			t.Run("first server added", func(t *testing.T) {
				checkPair(t, empty, false)
				checkPair(t, frozenEmpty, false)
				checkPair(t, baseline, true)
			})
			if count := bytes.Count(baseline.Tools, []byte(`"strict":false`)); (protocol == llm.ProtocolResponses && count != 1) || (protocol != llm.ProtocolResponses && bytes.Contains(baseline.Tools, []byte(`"strict"`))) {
				t.Fatalf("strict in %s tools: %s", protocol, baseline.Tools)
			}

			appendEntry(store.Entry{Kind: "assistant", Content: "Hello."})
			demo.Enabled = true
			configure(demo)
			appendEntry(store.Entry{Kind: "user", Content: "Find echo."})
			added := send(h.cfg.Config())
			prefix := func(a, b capture) {
				t.Helper()
				if !bytes.Equal(a.Tools, b.Tools) || !bytes.Equal(a.Instructions, b.Instructions) || len(a.Messages) > len(b.Messages) {
					t.Fatal("request head changed")
				}
				for i, m := range a.Messages {
					if !bytes.Equal(m, b.Messages[i]) {
						t.Fatalf("history changed at %d", i)
					}
				}
			}
			prefix(baseline, added)
			tail := func(c capture) string {
				var value any
				if err := json.Unmarshal(c.Messages[len(c.Messages)-1], &value); err != nil {
					t.Fatal(err)
				}
				return fmt.Sprint(value)
			}
			if !strings.Contains(tail(added), `<tool_catalog>{"servers":{"added":{"demo":["mcp__demo__echo"]}}}</tool_catalog>`+"\nFind echo.") {
				t.Fatalf("addition: %s", tail(added))
			}
			defs := h.engine.available(t.Context(), s, config.WorkExecute)
			call := func(id string) llm.ToolCall {
				return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: "tool_search", Arguments: `{"query":"echo"}`}}
			}
			calls := []llm.ToolCall{call("a"), call("b")}
			appendEntry(store.Entry{Kind: "assistant", ToolCalls: calls})
			for _, c := range calls {
				o := h.engine.searchTools(t.Context(), s, json.RawMessage(c.Function.Arguments), &tools.Runner{OutputLimit: 16384}, config.WorkExecute)
				h.engine.appendTool(s, c, nil, o)
			}
			if !s.consulted(defs[0]) {
				t.Fatal("search did not bind definition")
			}
			demo.Enabled = false
			configure(demo)
			removed := send(h.cfg.Config())
			prefix(added, removed)
			if strings.Contains(string(removed.Messages[len(removed.Messages)-2]), "tool_catalog") || !strings.Contains(tail(removed), `<tool_catalog>{"servers":{"removed":["demo"]}}</tool_catalog>`) {
				t.Fatalf("removal: %s", tail(removed))
			}
			if !s.consulted(defs[0]) {
				t.Fatal("disabling the server hid an in-context search result")
			}
			if err := h.store.Save(s.doc()); err != nil {
				t.Fatal(err)
			}
			doc, err := h.store.Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			s = fromDoc(doc)
			replay := send(h.cfg.Config())
			prefix(removed, replay)
			if len(removed.Messages) != len(replay.Messages) {
				t.Fatal("reopen added a message")
			}
			appendEntry(store.Entry{Kind: "assistant", Content: "Done."})
			appendEntry(store.Entry{Kind: "user", Content: "Continue."})
			unchanged := send(h.cfg.Config())
			prefix(replay, unchanged)
			if strings.Contains(tail(unchanged), "tool_catalog") {
				t.Fatal("unchanged catalog announced twice")
			}
			next := func(text string) capture {
				appendEntry(store.Entry{Kind: "assistant", Content: "Done."})
				appendEntry(store.Entry{Kind: "user", Content: text})
				return send(h.cfg.Config())
			}
			demo.Enabled = true
			configure(demo)
			enabled := next("Continue with echo.")
			prefix(unchanged, enabled)
			demo.EnabledTools = []string{}
			configure(demo)
			filtered := next("Check the filter.")
			prefix(enabled, filtered)
			if !strings.Contains(tail(filtered), "removed") {
				t.Fatal(tail(filtered))
			}
			demo.EnabledTools = nil
			configure(demo)
			unfiltered := next("Check echo.")
			prefix(filtered, unfiltered)
			// The test server lists an integer argument when this file exists.
			state := filepath.Join(t.TempDir(), "changed")
			if err := os.WriteFile(state, nil, 0600); err != nil {
				t.Fatal(err)
			}
			demo.Env["TOOL_STATE"] = state
			configure(demo)
			changed := next("Check the new definition.")
			prefix(unfiltered, changed)
			if !strings.Contains(tail(changed), "tools_changed") {
				t.Fatal(tail(changed))
			}
			configure()
			frozenConfigured := send(configured)
			last := next("Continue without servers.")
			t.Run("last server removed", func(t *testing.T) {
				checkPair(t, frozenConfigured, true)
				checkPair(t, last, false)
				if !bytes.Equal(empty.Tools, last.Tools) || !bytes.Equal(system(empty), system(last)) {
					t.Fatal("removing the last server did not restore the native request head")
				}
			})
		})
	}
}

func TestCatalogCompactionAndRewind(t *testing.T) {
	s := newSession("s", "/project", "agent", "model", config.ToolsetFull)
	offers := []mcp.Offered{{Server: "docs", Tool: "read", Wire: "mcp__docs__read", Version: "1"}}
	s.appendEntry(store.Entry{Kind: "user", Content: "First", InContext: true})
	s.recordCatalog(offers)
	s.appendEntry(store.Entry{Kind: "assistant", Content: "Done", InContext: true})
	s.appendEntry(store.Entry{Kind: "user", Content: "Next", InContext: true})
	offers[0].Version = "2"
	s.recordCatalog(offers)
	s.appendEntry(store.Entry{Kind: "compaction", Content: "Summary", InContext: true, Meta: map[string]any{"tail": s.Entries[2].Seq}})
	s.restoreContext()
	s.recordCatalog(offers)
	if got := s.Entries[3].Catalog; got == nil || !strings.Contains(string(got.Signal), `"added"`) {
		t.Fatalf("summary lacks a fresh inventory: %v", got)
	}
	render := func() string {
		b, _ := json.Marshal(s.contextMessages(config.Agent{}, "", "", brief{}, nil))
		return string(b)
	}
	if buf := render(); strings.Count(buf, "tool_catalog") != 2 || !strings.Contains(buf, "Summary") {
		t.Fatal(buf)
	}
	s.rewindTo(3)
	if buf := render(); strings.Count(buf, "tool_catalog") != 4 || strings.Contains(buf, "Summary") {
		t.Fatal(buf)
	}
}

func TestCatalogLimitAndDiff(t *testing.T) {
	before := store.CatalogView{"s": {"edit": "1", "read": "1", "lost": "1"}, "gone": {"x": "1"}}
	after := store.CatalogView{"s": {"edit": "2", "read": "1", "new": "1"}}
	signal := string(catalogSignal(catalogChanges(before, after)))
	if signal != `{"servers":{"removed":["gone"],"tools_added":{"s":["mcp__s__new"]},"tools_changed":{"s":["mcp__s__edit"]},"tools_removed":{"s":["mcp__s__lost"]}}}` {
		t.Fatal(signal)
	}
	names := map[string]string{}
	for i := 0; i < 2000; i++ {
		names[fmt.Sprintf("情報_%04d", i)] = "1"
	}
	signal = string(catalogSignal(catalogChanges(nil, store.CatalogView{"demo": names})))
	var got struct {
		Omitted int
		Servers struct{ Added map[string][]string }
	}
	if err := json.Unmarshal([]byte(signal), &got); err != nil {
		t.Fatal(err)
	}
	if len(signal)+len(catalogOpen)+len(catalogClose) > 2048 || got.Omitted+len(got.Servers.Added["demo"]) != 2000 || got.Omitted == 0 {
		t.Fatal(signal)
	}
}

func TestCatalogDiscoveryScope(t *testing.T) {
	h := newHarness(t)
	withMCP(t, h, mcpFake("demo"))
	s := h.session(t)
	mode := config.WorkExecute
	call := llm.ToolCall{Function: llm.FunctionCall{Name: "tool_call", Arguments: `{"name":"mcp__demo__echo","arguments":{"text":"hi"}}`}}
	if _, err := h.engine.resolveCall(t.Context(), s, mode, call); err != mcp.ErrToolChanged {
		t.Fatalf("undiscovered: %v", err)
	}
	search := llm.ToolCall{ID: "find", Function: llm.FunctionCall{Name: "tool_search", Arguments: `{"query":"demo echo tool"}`}}
	out := h.engine.searchTools(t.Context(), s, json.RawMessage(search.Function.Arguments), &tools.Runner{OutputLimit: 16384}, mode)
	h.engine.appendTool(s, search, nil, out)
	if err := h.store.Save(s.doc()); err != nil {
		t.Fatal(err)
	}
	doc, err := h.store.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored := fromDoc(doc)
	resolved, err := h.engine.resolveCall(t.Context(), restored, mode, call)
	if err != nil || resolved.Function.Name != "mcp__demo__echo" {
		t.Fatalf("reopened: %v %v", resolved, err)
	}
	if _, err := h.engine.resolveCall(t.Context(), restored, mode, resolved.ToolCall); err != mcp.ErrToolChanged {
		t.Fatal("accepted direct call")
	}
	call.Function.Arguments = `{"name":"mcp__demo__echo","arguments":{"text":7}}`
	if _, err := h.engine.resolveCall(t.Context(), restored, mode, call); err != nil {
		t.Fatalf("arguments must reach the server validator: %v", err)
	}
	mode = config.WorkPlan
	if _, err := h.engine.resolveCall(t.Context(), restored, mode, call); err != mcp.ErrToolChanged {
		t.Fatalf("plan: %v", err)
	}
	restored.rewindTo(0)
	if _, err := h.engine.resolveCall(t.Context(), restored, config.WorkExecute, call); err != mcp.ErrToolChanged {
		t.Fatalf("rewound search: %v", err)
	}
}

func TestCatalogBetweenResultsAndSteer(t *testing.T) {
	s := newSession("s", "/project", "a", "m", config.ToolsetFull)
	s.appendEntry(store.Entry{Kind: "tool", Content: "Result", ToolCallID: "b", InContext: true})
	s.appendEntry(store.Entry{Kind: "user", Content: "Update", InContext: true, Meta: map[string]any{"steer": true}})
	s.recordCatalog([]mcp.Offered{{Server: "notes", Tool: "find", Version: "new"}})
	if s.Entries[1].Catalog != nil || s.Entries[0].Catalog == nil {
		t.Fatal("catalog did not accompany the tool result")
	}
	e := s.Entries[0]
	e.Attachments = []store.Attachment{{File: "image", Kind: "image"}}
	m := toolMessage("b", e, map[string]llm.Part{"image": llm.ImagePart("image/png", []byte("x"))})
	if len(m.Parts) != 3 || m.Parts[0].Text != "Result" || m.Parts[1].ImageURL == nil || !strings.HasPrefix(m.Parts[2].Text, catalogOpen) {
		t.Fatal(m)
	}
}

func TestCatalogSearchSpill(t *testing.T) {
	runner := &tools.Runner{OutputLimit: 60}
	runner.SpillTo(t.TempDir())
	def := mcp.Offered{Server: "notes", Wire: "mcp__notes__find", Version: "one", Description: strings.Repeat("Find text. ", 90), Schema: json.RawMessage(`{"type":"object"}`), Instructions: "Read the notes."}
	second := def
	second.Wire = "mcp__notes__list"
	third := def
	third.Server = "archive"
	third.Wire = "mcp__archive__find"
	o := searchResult(runner, []mcp.Offered{def, second, third}, "find")
	paths, err := filepath.Glob(filepath.Join(runner.Spill, "tool_search-*.txt"))
	if err != nil || len(paths) != 1 || !o.truncated || !strings.Contains(o.content, paths[0]) {
		t.Fatalf("truncated result: %v %v %q", paths, err, o.content)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var defs []searchDefinition
	if err := json.Unmarshal(raw, &defs); err != nil || len(defs) != 3 {
		t.Fatalf("saved result: %v", err)
	}
	if defs[0].Instructions != def.Instructions || defs[1].Instructions != "" || defs[2].Instructions != def.Instructions || len(o.discovered) != 3 {
		t.Fatal("instructions or definitions missing from saved search")
	}
}

func TestCatalogSearchWithoutWritableSpill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	runner := &tools.Runner{OutputLimit: 60}
	runner.SpillTo(path)
	visible := mcp.Offered{Server: "notes", Wire: "mcp__notes__find", Version: "one", Schema: json.RawMessage(`{"type":"object"}`)}
	hidden := visible
	hidden.Wire, hidden.Version = "mcp__notes__search", "two"
	hidden.Description = strings.Repeat("Search notes. ", 100)
	last := hidden
	last.Wire = "mcp__notes__list"
	result := searchResult(runner, []mcp.Offered{visible, hidden, last}, "")
	if !result.truncated || !strings.Contains(result.content, visible.Wire) || strings.Contains(result.content, hidden.Wire) {
		t.Fatalf("unexpected search output: %s", result.content)
	}
	if len(result.discovered) != 1 || result.discovered[visible.Wire] != visible.Version {
		t.Fatalf("available definitions: %v", result.discovered)
	}
}
