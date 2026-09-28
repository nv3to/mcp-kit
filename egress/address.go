package egress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"
)

// refusalError carries a refusal out of the dial path, through the
// transport's wrapping, back to the handler that answers the client.
type refusalError struct {
	reason Reason
	addr   netip.Addr
}

func (e *refusalError) Error() string {
	return fmt.Sprintf("egress: %s: %s", e.reason, e.addr)
}

// reserved are the forbidden ranges that netip.Addr has no predicate for.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // shared address space; holds the metadata address 100.100.100.200
	netip.MustParsePrefix("192.0.0.0/24"),  // protocol assignments; holds the metadata address 192.0.0.192
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, and the broadcast address
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64: an IPv4 address in disguise
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

// forbidden reports whether the proxy must not connect to addr: loopback,
// private and unique-local ranges, link-local (which holds the metadata
// address 169.254.169.254), unspecified, multicast and the reserved ranges.
// An IPv4 address mapped into IPv6 is judged as the IPv4 address it is.
func forbidden(addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() ||
		addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsUnspecified() ||
		addr.IsMulticast() {
		return true
	}
	for _, prefix := range reserved {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// dial resolves the host of address, refuses if any of its addresses is
// forbidden, and connects to the first one that answers. It hands the dialer
// the checked address and never the name, so a name that resolves
// differently a moment later has nothing to change.
func (p *Proxy) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("egress: port %q: %w", portText, err)
	}
	var addrs []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{literal}
	} else if addrs, err = p.cfg.Resolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("egress: %s has no address", host)
	}
	for _, addr := range addrs {
		if forbidden(addr) {
			return nil, &refusalError{reason: AddressNotAllowed, addr: addr}
		}
	}
	for _, addr := range addrs {
		target := netip.AddrPortFrom(addr.Unmap(), uint16(port))
		var conn net.Conn
		if conn, err = p.cfg.Dial(ctx, network, target.String()); err == nil {
			return conn, nil
		}
	}
	return nil, err
}

// control is the default dialer's last check, made on the address the socket
// is about to connect to.
func control(network, address string, _ syscall.RawConn) error {
	target, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if forbidden(target.Addr()) {
		return &refusalError{reason: AddressNotAllowed, addr: target.Addr()}
	}
	return nil
}
