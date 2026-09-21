// Package mcptest connects an in-memory MCP client to an mcpkit server, so a
// server's tests exercise the real protocol path (schema derivation, the gate,
// the error shape) without a process or a pipe.
package mcptest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nv3to/mcp-kit"
)

// Session is a client session connected to a server. Failures of the test
// harness itself fail the test.
type Session struct {
	t  testing.TB
	cs *mcp.ClientSession
}

// Connect serves s to a new in-memory client and closes both when the test
// ends.
func Connect(t testing.TB, s *mcpkit.Server) *Session {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Unwrap().Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("mcptest: connect server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcptest", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("mcptest: connect client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &Session{t: t, cs: cs}
}

// ListTools returns the server's tools with their schemas as a client sees
// them (InputSchema and OutputSchema are map[string]any).
func (s *Session) ListTools() []*mcp.Tool {
	s.t.Helper()
	res, err := s.cs.ListTools(context.Background(), nil)
	if err != nil {
		s.t.Fatalf("mcptest: list tools: %v", err)
	}
	return res.Tools
}

// Call calls a tool and fails the test on a protocol-level error. A tool
// error is not a protocol error: it comes back as a result with IsError set.
func (s *Session) Call(tool string, args any) *mcp.CallToolResult {
	s.t.Helper()
	res, err := s.CallErr(tool, args)
	if err != nil {
		s.t.Fatalf("mcptest: call %s: %v", tool, err)
	}
	return res
}

// CallErr is Call for tests that expect a protocol-level error, such as an
// unknown tool.
func (s *Session) CallErr(tool string, args any) (*mcp.CallToolResult, error) {
	s.t.Helper()
	return s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
}

// Text is the text of every text content block of res, joined by newlines.
func Text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Decode unmarshals the structured content of res into out.
func Decode(t testing.TB, res *mcp.CallToolResult, out any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("mcptest: marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("mcptest: decode structured content %s: %v", b, err)
	}
}
