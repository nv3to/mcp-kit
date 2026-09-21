package mcpkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nv3to/mcp-kit"
	"github.com/nv3to/mcp-kit/mcptest"
)

type echoIn struct {
	Text  string `json:"text" jsonschema:"the text to repeat"`
	Times int    `json:"times,omitempty" jsonschema:"how often to repeat it, default once"`
}

type echoOut struct {
	Text string `json:"text"`
}

type failIn struct {
	Kind string `json:"kind"`
}

// toy is the server every test drives: echo succeeds, fail returns an error
// of the requested kind.
func toy(t *testing.T) (*mcpkit.Server, *int) {
	t.Helper()
	s := mcpkit.New("toy", "0.1.0")
	calls := new(int)
	err := mcpkit.AddTool(s, mcpkit.Tool{Name: "echo", Description: "Repeat the text."},
		func(ctx context.Context, in echoIn) (echoOut, error) {
			*calls++
			fmt.Println("stray output that must never reach the protocol stream")
			n := max(in.Times, 1)
			return echoOut{Text: strings.Repeat(in.Text, n)}, nil
		})
	if err != nil {
		t.Fatalf("add echo: %v", err)
	}
	err = mcpkit.AddTool(s, mcpkit.Tool{Name: "fail", Description: "Fail with the given kind."},
		func(ctx context.Context, in failIn) (echoOut, error) {
			switch in.Kind {
			case "plain":
				return echoOut{}, errors.New("boom")
			case "wrapped":
				return echoOut{}, fmt.Errorf("while working: %w", mcpkit.Errorf(mcpkit.Conflict, "boom"))
			}
			return echoOut{}, mcpkit.Errorf(mcpkit.Kind(in.Kind), "boom")
		})
	if err != nil {
		t.Fatalf("add fail: %v", err)
	}
	return s, calls
}

func TestListToolsDerivesSchemas(t *testing.T) {
	s, _ := toy(t)
	tools := mcptest.Connect(t, s).ListTools()

	var echo *mcp.Tool
	for _, tool := range tools {
		if tool.Name == "echo" {
			echo = tool
		}
	}
	if len(tools) != 2 || echo == nil {
		t.Fatalf("tools = %d, echo found = %v; want 2 and true", len(tools), echo != nil)
	}
	if echo.Description != "Repeat the text." {
		t.Errorf("description = %q", echo.Description)
	}

	in, ok := echo.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema is %T, want an object", echo.InputSchema)
	}
	if in["type"] != "object" {
		t.Errorf("input type = %v, want object", in["type"])
	}
	props, _ := in["properties"].(map[string]any)
	text, _ := props["text"].(map[string]any)
	if text["type"] != "string" || text["description"] != "the text to repeat" {
		t.Errorf("text property = %v", text)
	}
	if _, ok := props["times"].(map[string]any); !ok {
		t.Errorf("times property missing: %v", props)
	}
	required, _ := in["required"].([]any)
	if len(required) != 1 || required[0] != "text" {
		t.Errorf("required = %v, want [text]: times is omitempty", required)
	}

	out, ok := echo.OutputSchema.(map[string]any)
	if !ok {
		t.Fatalf("output schema is %T, want an object", echo.OutputSchema)
	}
	outProps, _ := out["properties"].(map[string]any)
	if _, ok := outProps["text"]; !ok {
		t.Errorf("output schema lacks text: %v", out)
	}
}

func TestTypedCallRoundTrips(t *testing.T) {
	s, calls := toy(t)
	res := mcptest.Connect(t, s).Call("echo", map[string]any{"text": "ab", "times": 2})

	if res.IsError {
		t.Fatalf("unexpected tool error: %s", mcptest.Text(res))
	}
	var got echoOut
	mcptest.Decode(t, res, &got)
	if got.Text != "abab" {
		t.Errorf("structured text = %q, want abab", got.Text)
	}
	if text := mcptest.Text(res); text != `{"text":"abab"}` {
		t.Errorf("text form = %q, want the JSON of the output", text)
	}
	if *calls != 1 {
		t.Errorf("handler ran %d times, want 1", *calls)
	}
}

func TestHandlerErrorsHaveOneShape(t *testing.T) {
	s, _ := toy(t)
	sess := mcptest.Connect(t, s)

	for _, tc := range []struct{ kind, want string }{
		{"invalid", "invalid: boom"},
		{"not_found", "not_found: boom"},
		{"conflict", "conflict: boom"},
		{"refused", "refused: boom"},
		{"internal", "internal: boom"},
		{"plain", "internal: boom"},
		{"wrapped", "conflict: while working: boom"},
		{"made-up", "internal: boom"},
	} {
		res := sess.Call("fail", map[string]any{"kind": tc.kind})
		if !res.IsError {
			t.Errorf("kind %q: IsError = false", tc.kind)
		}
		if len(res.Content) != 1 {
			t.Errorf("kind %q: %d content blocks, want 1", tc.kind, len(res.Content))
		}
		if got := mcptest.Text(res); got != tc.want {
			t.Errorf("kind %q: text = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestBadArgumentsUseTheErrorShape(t *testing.T) {
	s, calls := toy(t)
	res := mcptest.Connect(t, s).Call("echo", map[string]any{})

	if !res.IsError || !strings.HasPrefix(mcptest.Text(res), "invalid: ") {
		t.Errorf("missing required argument: IsError = %v, text = %q; want an \"invalid: \" error", res.IsError, mcptest.Text(res))
	}
	if *calls != 0 {
		t.Errorf("handler ran %d times for invalid input", *calls)
	}
}

func TestProtocolErrorsAreLeftToTheSDK(t *testing.T) {
	s, _ := toy(t)
	if res, err := mcptest.Connect(t, s).CallErr("nope", map[string]any{}); err == nil {
		t.Errorf("unknown tool: got result %v, want a protocol error", res)
	}
}

func TestToolWithoutDescriptionIsRefused(t *testing.T) {
	s, _ := toy(t)
	h := func(ctx context.Context, in echoIn) (echoOut, error) { return echoOut{}, nil }

	for _, desc := range []string{"", "  \n\t"} {
		err := mcpkit.AddTool(s, mcpkit.Tool{Name: "nodesc", Description: desc}, h)
		var e *mcpkit.Error
		if !errors.As(err, &e) || e.Kind != mcpkit.Invalid || !strings.Contains(e.Message, "description") {
			t.Errorf("description %q: err = %v, want an invalid error naming the description", desc, err)
		}
	}
	for _, tool := range mcptest.Connect(t, s).ListTools() {
		if tool.Name == "nodesc" {
			t.Errorf("the refused tool was registered")
		}
	}
}

func TestOtherRegistrationErrors(t *testing.T) {
	s, _ := toy(t)
	h := func(ctx context.Context, in echoIn) (echoOut, error) { return echoOut{}, nil }

	if err := mcpkit.AddTool(s, mcpkit.Tool{Name: "echo", Description: "again"}, h); err == nil {
		t.Errorf("duplicate name: no error")
	}
	if err := mcpkit.AddTool(s, mcpkit.Tool{Name: "bad name", Description: "x"}, h); err == nil {
		t.Errorf("invalid name: no error")
	}
	// A type with no object schema makes the SDK panic; that must be an error.
	err := mcpkit.AddTool(s, mcpkit.Tool{Name: "scalar", Description: "x"},
		func(ctx context.Context, in int) (echoOut, error) { return echoOut{}, nil })
	if err == nil {
		t.Errorf("scalar input type: no error")
	}
}

func TestGateBlocksACall(t *testing.T) {
	s, calls := toy(t)
	var asked []string
	s.SetGate(func(tool string) error {
		asked = append(asked, tool)
		if tool == "echo" {
			return errors.New("the feature is off")
		}
		return nil
	})
	sess := mcptest.Connect(t, s)

	res := sess.Call("echo", map[string]any{"text": "x"})
	if !res.IsError || mcptest.Text(res) != "refused: the feature is off" {
		t.Errorf("gated call: IsError = %v, text = %q", res.IsError, mcptest.Text(res))
	}
	if *calls != 0 {
		t.Errorf("handler ran %d times behind a closed gate", *calls)
	}

	// Another tool still passes, and the gate saw both calls.
	if res := sess.Call("fail", map[string]any{"kind": "invalid"}); mcptest.Text(res) != "invalid: boom" {
		t.Errorf("ungated call: text = %q", mcptest.Text(res))
	}
	if strings.Join(asked, ",") != "echo,fail" {
		t.Errorf("gate consulted for %v, want [echo fail]", asked)
	}
}

// syncBuffer collects what the server writes to stdout.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type readCloser struct {
	io.Reader
	io.Closer
}

func TestServeWritesOnlyProtocolFramesToStdout(t *testing.T) {
	s, calls := toy(t)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()

	// The client reads the server's stdout through a tee, so the raw bytes
	// are on record.
	var stdout syncBuffer
	tr := &mcp.IOTransport{Reader: readCloser{io.TeeReader(outR, &stdout), outR}, Writer: inW}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("connect over stdio: %v", err)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err != nil || res.IsError {
		t.Fatalf("call over stdio: res = %+v, err = %v", res, err)
	}
	if *calls != 1 {
		t.Fatalf("handler ran %d times, want 1", *calls)
	}

	// Closing stdin ends Serve.
	_ = inW.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v, want nil once stdin closes", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after stdin closed")
	}
	if os.Stdout != outW {
		t.Errorf("Serve did not restore os.Stdout")
	}
	_ = outW.Close()
	_ = cs.Close()

	frames := 0
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		frames++
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil || frame.JSONRPC != "2.0" {
			t.Errorf("stdout carries something that is not a JSON-RPC frame: %q", line)
		}
	}
	if frames < 2 {
		t.Errorf("stdout carried %d frames, want at least the initialize and call responses:\n%s", frames, stdout.String())
	}
}

// A request that is followed by EOF at once (`printf '<request>' | server`)
// is still answered before Serve returns.
func TestServeAnswersARequestFollowedByEOF(t *testing.T) {
	s, _ := toy(t)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	if _, err := inW.WriteString(initialize + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = inW.Close()

	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v, want nil once stdin closes", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after stdin closed")
	}
	_ = outW.Close()

	out, err := io.ReadAll(outR)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"serverInfo"`) {
		t.Errorf("no initialize response on stdout: %q", out)
	}
}
