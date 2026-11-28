package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jesdga95/quecto-harness/internal/mcp"
)

// MCPCaller runs one tool on one MCP server for a project.
type MCPCaller interface {
	CallOffered(ctx context.Context, project string, tool mcp.Offered, args json.RawMessage, fallback time.Duration, allowed func() bool) (mcp.Rendered, error)
}

// CallMCP runs a tool an MCP server offered this turn. The output is cut to
// the output limit like every other tool's, and a result the tool marked as
// failed carries the error flag.
func (r *Runner) CallMCP(ctx context.Context, o mcp.Offered, args json.RawMessage, allowed func() bool) (Result, error) {
	if r.MCP == nil {
		return Result{}, mcp.ErrToolChanged
	}
	res, err := r.MCP.CallOffered(ctx, r.Root, o, args, r.Timeout, allowed)
	if err != nil {
		return Result{}, err
	}
	out, cut := Truncate(res.Text, r.OutputLimit)
	meta := map[string]any{"server": o.Server, "tool": o.Tool}
	if res.IsError {
		meta["error"] = true
	}
	result := Result{Output: out, Truncated: cut, Bytes: len(res.Text), Meta: meta}
	for _, img := range res.Images {
		result.Images = append(result.Images, Image{Path: o.Server + " · " + o.Tool, Mime: img.Mime, Bytes: len(img.Data), Data: img.Data})
	}
	return result, nil
}
