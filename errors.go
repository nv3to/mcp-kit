package mcpkit

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies a failed tool call. The set is closed: a client can branch
// on the prefix of the error text without knowing the server.
type Kind string

const (
	// Invalid: the arguments are well-formed JSON but not acceptable.
	Invalid Kind = "invalid"
	// NotFound: the thing the call names does not exist.
	NotFound Kind = "not_found"
	// Conflict: the call is fine but the current state does not allow it.
	Conflict Kind = "conflict"
	// Refused: the call is not allowed here (a closed gate, a policy).
	Refused Kind = "refused"
	// Internal: anything the server did not classify.
	Internal Kind = "internal"
)

func (k Kind) valid() bool {
	switch k {
	case Invalid, NotFound, Conflict, Refused, Internal:
		return true
	}
	return false
}

// Error is a classified tool failure. Its text, "<kind>: <message>", is the
// one error shape every mcpkit server puts in a tool result with isError set.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Message }

// Errorf builds an *Error of the given kind. Handlers return it (or wrap it
// with %w) to pick the kind of the failure.
func Errorf(kind Kind, format string, args ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// classify turns any error into an *Error. An *Error found in err's chain
// keeps its kind (an unknown kind becomes Internal) and the wrapping context
// is kept in the message; any other error gets def.
func classify(err error, def Kind) *Error {
	var e *Error
	if !errors.As(err, &e) {
		return &Error{Kind: def, Message: err.Error()}
	}
	kind := e.Kind
	if !kind.valid() {
		kind = Internal
	}
	return &Error{Kind: kind, Message: strings.Replace(err.Error(), e.Error(), e.Message, 1)}
}
