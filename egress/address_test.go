package egress

import (
	"context"
	"errors"
	"net"
	"testing"
)

// The default dialer cannot be reached through the public API without
// leaving the machine, so its check at connect time is tested here: handed a
// forbidden address directly, it refuses before the socket connects.
func TestDefaultDialerChecksTheAddressItConnectsTo(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := p.cfg.Dial(context.Background(), "tcp", l.Addr().String())
	if err == nil {
		conn.Close()
		t.Fatal("the default dialer connected to loopback")
	}
	var refused *refusalError
	if !errors.As(err, &refused) || refused.reason != AddressNotAllowed {
		t.Errorf("got %v, want a refusal for the address", err)
	}
}
