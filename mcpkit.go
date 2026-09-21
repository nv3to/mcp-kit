// Package mcpkit is an opinionated way to build a stdio MCP server in Go on
// top of the official SDK (github.com/modelcontextprotocol/go-sdk).
//
// A server is New(name, version); tools are added with the typed AddTool,
// whose input and output schemas come from the In and Out structs. The
// opinions: a tool must have a description, every handler error has the one
// shape "<kind>: <message>", the only transport is stdio and nothing but
// protocol frames reaches stdout, and a gate hook can veto any call.
//
// The package is written to be extracted into its own repository: it
// imports nothing from the module it lives in.
package mcpkit

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool describes a tool. Both fields are required.
type Tool struct {
	Name        string
	Description string
}

// Server is an MCP server under construction or serving. Register tools and
// the gate before Serve.
type Server struct {
	srv *mcp.Server
	log *slog.Logger

	mu    sync.Mutex
	names map[string]bool
	gate  func(tool string) error
}

// New returns a server with no tools. Its logger writes to stderr and is the
// one logger of the process: the SDK logs through it too.
func New(name, version string) *Server {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := &Server{
		srv:   mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, &mcp.ServerOptions{Logger: log}),
		log:   log,
		names: map[string]bool{},
	}
	s.srv.AddReceivingMiddleware(s.middleware)
	return s
}

// Logger is the server's logger (stderr). Handlers log through it, never to
// stdout.
func (s *Server) Logger() *slog.Logger { return s.log }

// SetGate installs a hook consulted before every tool call with the tool's
// name. A non-nil error refuses the call: the handler does not run and the
// caller gets the error as a tool result (kind "refused" unless the error is
// an *Error). Servers use it for feature gating. Nil removes the hook.
func (s *Server) SetGate(gate func(tool string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = gate
}

// Unwrap returns the SDK server. It exists for mcptest, which connects an
// in-memory transport; serving it on any other transport bypasses the
// stdio-only rule.
func (s *Server) Unwrap() *mcp.Server { return s.srv }

// AddTool registers a typed tool. The input schema is derived from In (a
// struct or map; describe fields with the `jsonschema:"..."` tag) and the
// output schema from Out; the result is returned as structured JSON plus its
// text form. It returns an error, never panics, when the tool is refused: no
// description, an invalid or duplicate name, or types with no usable schema.
//
// A handler error becomes a tool result with isError and the text
// "<kind>: <message>" (see Errorf); an error that is not an *Error is
// "internal".
func AddTool[In, Out any](s *Server, t Tool, h func(ctx context.Context, in In) (Out, error)) (err error) {
	if !validName(t.Name) {
		return Errorf(Invalid, "tool %q: the name must be 1-128 characters of [A-Za-z0-9_.-]", t.Name)
	}
	if strings.TrimSpace(t.Description) == "" {
		return Errorf(Invalid, "tool %q: a description is required", t.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names[t.Name] {
		return Errorf(Conflict, "tool %q is already registered", t.Name)
	}
	// The SDK panics on a type it cannot derive a schema from.
	defer func() {
		if r := recover(); r != nil {
			err = Errorf(Invalid, "tool %q: %v", t.Name, r)
		}
	}()
	mcp.AddTool(s.srv, &mcp.Tool{Name: t.Name, Description: t.Description},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			out, err := h(ctx, in)
			if err != nil {
				var zero Out
				return nil, zero, classify(err, Internal)
			}
			return nil, out, nil
		})
	s.names[t.Name] = true
	return nil
}

func validName(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}
	for _, r := range n {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}

// middleware consults the gate before every tools/call and gives the SDK's
// own tool errors (argument validation) the one error shape. Protocol-level
// errors, such as an unknown tool, pass through untouched.
func (s *Server) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
			s.mu.Lock()
			gate := s.gate
			s.mu.Unlock()
			if gate != nil {
				if err := gate(call.Params.Name); err != nil {
					return errorResult(classify(err, Refused)), nil
				}
			}
		}
		res, err := next(ctx, method, req)
		if err != nil {
			return res, err
		}
		if ctr, ok := res.(*mcp.CallToolResult); ok && ctr.IsError {
			// A handler error is already an *Error; what is left came from
			// the SDK, which rejected the arguments.
			if cause := ctr.GetError(); cause != nil {
				var e *Error
				if !errors.As(cause, &e) {
					return errorResult(&Error{Kind: Invalid, Message: cause.Error()}), nil
				}
			}
		}
		return res, nil
	}
}

func errorResult(e *Error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: e.Error()}},
		IsError: true,
	}
}
