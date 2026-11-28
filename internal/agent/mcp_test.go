package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jesdga95/quecto-harness/internal/config"
	"github.com/jesdga95/quecto-harness/internal/llm"
	"github.com/jesdga95/quecto-harness/internal/mcp"
	"github.com/jesdga95/quecto-harness/internal/perms"
	"github.com/jesdga95/quecto-harness/internal/tools"
)

// TestMCPHelperProcess is a one-tool MCP server run as a child process. It
// does nothing unless the test binary was started as one.
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_MCP_FAKE") == "" {
		return
	}
	defer os.Exit(0)
	out := json.NewEncoder(os.Stdout)
	var mu sync.Mutex
	send := func(v any) { mu.Lock(); defer mu.Unlock(); out.Encode(v) }
	state := os.Getenv("TOOL_STATE")
	if state != "" {
		go func() {
			for {
				if _, err := os.Stat(state); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
		}()
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		var result any
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			typ := "string"
			if _, err := os.Stat(state); err == nil {
				typ = "integer"
			}
			result = map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "Echoes its arguments.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": typ}}},
			}}}
		case "tools/call":
			if log := os.Getenv("CALL_LOG"); log != "" {
				if err := os.WriteFile(log, m.Params.Arguments, 0600); err != nil {
					os.Exit(2)
				}
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + string(m.Params.Arguments)}}}
		default:
			result = map[string]any{}
		}
		send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}
}

func mcpFake(name string) config.MCPServer {
	return config.MCPServer{
		Name: name, Enabled: true, Transport: mcp.TransportStdio,
		Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"},
		Env: map[string]string{"AGENT_MCP_FAKE": "1"},
	}
}

func withMCP(t *testing.T, h *harness, servers ...config.MCPServer) *mcp.Manager {
	t.Helper()
	if _, err := h.cfg.SetMCP(servers); err != nil {
		t.Fatal(err)
	}
	m := mcp.NewManager(filepath.Join(h.paths.Home, "mcp"), h.paths.Log, "test", "test-token")
	m.Configure(h.cfg.MCP())
	h.engine.SetMCP(m)
	t.Cleanup(m.Close)
	return m
}

func TestMCPToolCall(t *testing.T) {
	h := newHarness(t, "stream_search.txt", "stream_mcp.txt", "stream_plain.txt")
	withMCP(t, h, mcpFake("fake"))
	s := h.session(t)
	h.engine.Send(s.ID, "echo hi", nil)

	id := waitForCard(t, h, s.ID)
	req, ok := h.broker.Lookup(id)
	if !ok || req.Tool != "mcp__fake__echo" || req.Server != "fake" || req.MCPTool != "echo" {
		t.Fatalf("card = %+v", req)
	}
	if err := h.broker.Resolve(id, perms.Decision{Kind: perms.DecideAlways}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)

	if srv, _ := h.cfg.MCPServer("fake"); !srv.Allows("echo") {
		t.Fatalf("allow always did not reach mcp.json: %+v", srv.Allow)
	}
	doc, _, _, _ := s.Snapshot()
	var found bool
	for _, e := range doc.Entries {
		if e.Kind == "tool" && e.ToolName == "tool_call" {
			found = true
			if e.Content != `echo: {"text":"hi"}` || e.Meta["server"] != "fake" || e.Meta["tool"] != "echo" {
				t.Fatalf("tool entry = %q %v", e.Content, e.Meta)
			}
		}
	}
	if !found {
		t.Fatal("no tool entry for the MCP call")
	}

	h.replay.mu.Lock()
	first := h.replay.Requests[0]
	h.replay.mu.Unlock()
	toolList, _ := first["tools"].([]any)
	last, _ := toolList[len(toolList)-1].(map[string]any)
	fn, _ := last["function"].(map[string]any)
	if fn["name"] != "tool_call" {
		t.Fatalf("last tool sent = %v", fn)
	}
	msgs, _ := first["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	if content, _ := sys["content"].(string); strings.Contains(content, "fake") || !strings.Contains(content, "tool_search") {
		t.Fatalf("unstable system prompt:\n%s", content)
	}
}

func TestMCPCallAfterReactivation(t *testing.T) {
	for _, scenario := range []string{"unchanged", "schema changed", "search compacted"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t)
			if _, err := h.cfg.Update(func(c *config.Config) { c.Mode = config.ModeFull }); err != nil {
				t.Fatal(err)
			}
			server := mcpFake("fake")
			dir := t.TempDir()
			state, log := filepath.Join(dir, "schema"), filepath.Join(dir, "call")
			server.Env["TOOL_STATE"], server.Env["CALL_LOG"] = state, log
			manager := withMCP(t, h, server)
			s := h.session(t)
			setEnabled := func(enabled bool) {
				t.Helper()
				server.Enabled = enabled
				resolved, err := h.cfg.SetMCP([]config.MCPServer{server})
				if err != nil {
					t.Fatal(err)
				}
				manager.Configure(resolved)
			}
			send := func(message string, fixtures ...string) {
				t.Helper()
				h.replay.forModel(h.agent.Model, fixtures...)
				if _, err := h.engine.Send(s.ID, message, nil); err != nil {
					t.Fatal(err)
				}
				waitIdle(t, s)
			}
			send("Find echo.", "stream_search.txt", "stream_plain.txt")
			setEnabled(false)
			message := "Call echo."
			if scenario == "search compacted" {
				message += strings.Repeat(" More context.", compactFloor/10)
			}
			send(message, "stream_mcp.txt", "stream_plain.txt")
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("disabled server received a call: %v", err)
			}

			switch scenario {
			case "schema changed":
				if err := os.WriteFile(state, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "search compacted":
				h.replay.forModel(h.agent.Model, "stream_summary.txt")
				if err := h.engine.Compact(s.ID); err != nil {
					t.Fatal(err)
				}
				waitIdle(t, s)
				doc, _, _, _ := s.Snapshot()
				for _, entry := range doc.Entries {
					if entry.ToolName == "tool_search" && entry.InContext {
						t.Fatal("compaction kept the search result")
					}
				}
			}
			setEnabled(true)
			send("Call echo again.", "stream_mcp.txt", "stream_plain.txt")
			raw, err := os.ReadFile(log)
			if scenario == "unchanged" {
				if err != nil || string(raw) != `{"text":"hi"}` {
					t.Fatalf("reactivated call = %s, %v", raw, err)
				}
				return
			}
			if !os.IsNotExist(err) {
				t.Fatalf("stale search reached the server: %s, %v", raw, err)
			}
			doc, _, _, _ := s.Snapshot()
			var results []string
			for _, entry := range doc.Entries {
				if entry.ToolName == "tool_call" {
					results = append(results, entry.Content)
				}
			}
			if len(results) != 2 || results[0] != mcp.ErrToolChanged.Error() || results[1] != mcp.ErrToolChanged.Error() {
				t.Fatalf("disabled and reactivated calls: %q", results)
			}
		})
	}
}

func TestMCPNothingConfiguredChangesNothing(t *testing.T) {
	h := newHarness(t)
	s := h.session(t)
	rendered := newSession("reference", "/project", "local", "demo", config.ToolsetFull)
	// These digests cover the native tool definitions and prompt at fixed project, OS and date.
	for _, state := range []string{"missing", "empty"} {
		if state == "empty" {
			withMCP(t, h)
		}
		b := h.engine.briefFor(t.Context(), s, h.cfg.Config())
		raw, err := json.Marshal(tools.Schemas(b.offer))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != "3a49aa38696d3a5a5fd1308210c72ad551ab92545e6f2a5c305e4303e21b1686" {
			t.Errorf("%s mcp.json: native tool definitions changed: %s", state, raw)
		}
		system := rendered.systemMessage(h.agent, "linux", "2026-01-01", b)
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(system.Content))); got != "6f366197d53f42b9fb20a764abf1e42e2dbfa01208b81af381e40591aa77a794" {
			t.Errorf("%s mcp.json: system prompt changed: %s", state, system.Content)
		}
	}
	if _, err := os.Stat(filepath.Join(h.paths.Home, "mcp")); !os.IsNotExist(err) {
		t.Fatal("an empty server list wrote a cache")
	}
}

func TestCatalogPlan(t *testing.T) {
	h := newHarness(t)
	ro := mcpFake("docs")
	ro.ReadOnly = true
	withMCP(t, h, ro, mcpFake("writes"))
	s := h.session(t)
	s.Mode = config.WorkPlan
	got := h.engine.available(t.Context(), s, config.WorkExecute)
	if len(got) != 1 || got[0].Server != "docs" {
		t.Fatalf("plan offers %v", got)
	}
	s.Toolset = config.ToolsetMinimal
	if len(h.engine.available(t.Context(), s, config.WorkExecute)) != 0 {
		t.Fatal("minimal set offers MCP tools")
	}
}

func TestCatalogRevokedDuringApproval(t *testing.T) {
	for _, change := range []string{"schema", "disabled", "tool", "destination", "plan"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t, "stream_search.txt", "stream_mcp.txt", "stream_plain.txt")
			srv := mcpFake("fake")
			dir := t.TempDir()
			srv.Env["TOOL_STATE"] = filepath.Join(dir, "changed")
			srv.Env["CALL_LOG"] = filepath.Join(dir, "called")
			manager := withMCP(t, h, srv)
			s := h.session(t)
			h.engine.Send(s.ID, "echo hi", nil)
			card := waitForCard(t, h, s.ID)
			switch change {
			case "schema":
				if err := os.WriteFile(srv.Env["TOOL_STATE"], nil, 0600); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					defs, _ := manager.Tools(t.Context(), s.Project)
					if len(defs) == 1 && strings.Contains(string(defs[0].Schema), "integer") {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("schema notification was not applied")
					}
					time.Sleep(5 * time.Millisecond)
				}
			case "plan":
				if err := h.engine.SetMode(s.ID, config.WorkPlan); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				srv.Enabled = false
			case "tool":
				srv.EnabledTools = []string{}
			case "destination":
				srv.Env["OTHER"] = "1"
			}
			if _, err := h.cfg.SetMCP([]mcp.Server{srv}); err != nil {
				t.Fatal(err)
			}
			manager.Configure(h.cfg.MCP())
			if err := h.broker.Resolve(card, perms.Decision{Kind: perms.DecideAlways}); err != nil {
				t.Fatal(err)
			}
			waitIdle(t, s)
			if cfg, _ := h.cfg.MCPServer("fake"); cfg.Allows("echo") {
				t.Fatal("stale approval persisted")
			}
			if _, err := os.Stat(srv.Env["CALL_LOG"]); !os.IsNotExist(err) {
				t.Fatalf("call log: %v", err)
			}
			doc, _, _, _ := s.Snapshot()
			for _, entry := range doc.Entries {
				if entry.ToolName == "tool_call" && entry.Content == mcp.ErrToolChanged.Error() && entry.Permission.Tool == "mcp__fake__echo" {
					return
				}
			}
			t.Fatal("missing stale result and permission record")
		})
	}
}

type parallelMCP struct {
	arrived chan string
	release chan struct{}
}

func (p parallelMCP) CallOffered(ctx context.Context, project string, t mcp.Offered, args json.RawMessage, timeout time.Duration, allowed func() bool) (mcp.Rendered, error) {
	if !allowed() {
		return mcp.Rendered{}, mcp.ErrToolChanged
	}
	p.arrived <- t.Wire
	select {
	case <-p.release:
		return mcp.Rendered{Text: t.Wire}, nil
	case <-ctx.Done():
		return mcp.Rendered{}, ctx.Err()
	}
}

func TestParallelDiscoveredReads(t *testing.T) {
	h := newHarness(t)
	a, b := mcpFake("a"), mcpFake("b")
	a.ReadOnly, b.ReadOnly = true, true
	withMCP(t, h, a, b, mcpFake("writer"))
	cfg, err := h.cfg.Update(func(c *config.Config) { c.Mode = config.ModeFull })
	if err != nil {
		t.Fatal(err)
	}
	s := h.session(t)
	mock := parallelMCP{make(chan string, 2), make(chan struct{})}
	runner := &tools.Runner{Root: s.Project, MCP: mock}
	search := h.engine.searchTools(t.Context(), s, json.RawMessage(`{"query":"echo"}`), runner, s.Mode)
	h.engine.appendTool(s, llm.ToolCall{ID: "lookup"}, nil, search)
	var calls []llm.ToolCall
	for _, server := range []string{"a", "b", "writer"} {
		calls = append(calls, llm.ToolCall{ID: server, Function: llm.FunctionCall{Name: "tool_call", Arguments: `{"name":"mcp__` + server + `__echo","arguments":{}}`}})
	}
	offer := h.engine.briefFor(t.Context(), s, cfg).offer
	if target, err := h.engine.resolveCall(t.Context(), s, s.Mode, calls[2]); err != nil || target.offered == nil || target.offered.ReadOnly {
		t.Fatalf("expected a discovered writable tool: %v, %v", target.offered, err)
	}
	mixed := []llm.ToolCall{calls[0], calls[2], calls[1]}
	if batch := h.engine.readRun(t.Context(), s, cfg, offer, runner, mixed); len(batch) != 1 || batch[0].target.ID != calls[0].ID {
		t.Fatalf("parallel batch crossed a writable call: %v", batch)
	}
	calls = calls[:2]
	batch := h.engine.readRun(t.Context(), s, cfg, offer, runner, calls)
	if len(batch) != 2 {
		t.Fatalf("batch length %d", len(batch))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.engine.runReads(ctx, s, h.agent, runner, calls, batch, 0, &repeats{}, offer, &turnClock{})
		close(done)
	}()
	reached := map[string]bool{}
	for range 2 {
		select {
		case name := <-mock.arrived:
			reached[name] = true
		case <-ctx.Done():
			<-done
			t.Fatal("reads did not overlap")
		}
	}
	close(mock.release)
	<-done
	doc, _, _, _ := s.Snapshot()
	results := doc.Entries[len(doc.Entries)-2:]
	for i, entry := range results {
		name := "mcp__" + calls[i].ID + "__echo"
		if !reached[name] || entry.Permission.Tool != name || entry.Content != name || entry.ToolCallID != calls[i].ID {
			t.Fatalf("wrong result or permission: %+v", entry)
		}
	}
	h.engine.save(s)
}
