package target

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"unicode/utf8"
)

var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"::ffff:0:0/96",
	"64:ff9b::/96",
	"100::/64",
	"2001:db8::/32",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
)

var ErrBlockedAddress = errors.New("target resolves to a private or reserved address")

func Normalize(input string) (string, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", fmt.Errorf("target is required")
	}
	if len(value) > 253 {
		return "", fmt.Errorf("target is too long")
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("target is not valid UTF-8")
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, "/?#@") {
		return "", fmt.Errorf("target must be a hostname or IP address, not a URL")
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("target contains whitespace")
	}
	if addr, err := netip.ParseAddr(strings.Trim(value, "[]")); err == nil {
		if !AddressAllowed(addr) {
			return "", ErrBlockedAddress
		}
		return addr.String(), nil
	}
	value = strings.TrimSuffix(strings.ToLower(value), ".")
	if err := validateHostname(value); err != nil {
		return "", err
	}
	return value, nil
}

func ResolveAllowed(ctx context.Context, resolver *net.Resolver, name string) ([]netip.Addr, error) {
	normalized, err := Normalize(name)
	if err != nil {
		return nil, err
	}
	if addr, err := netip.ParseAddr(normalized); err == nil {
		if !AddressAllowed(addr) {
			return nil, ErrBlockedAddress
		}
		return []netip.Addr{addr}, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	records, err := resolver.LookupIPAddr(ctx, normalized)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(records))
	for _, record := range records {
		addr, ok := netip.AddrFromSlice(record.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if AddressAllowed(addr) {
			out = append(out, addr)
		}
	}
	if len(out) == 0 {
		return nil, ErrBlockedAddress
	}
	return out, nil
}

func AddressAllowed(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func validateHostname(name string) error {
	if name == "" {
		return fmt.Errorf("hostname is empty")
	}
	if len(name) > 253 {
		return fmt.Errorf("hostname is too long")
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return fmt.Errorf("hostname must contain at least one dot")
	}
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("hostname contains an empty label")
		}
		if len(label) > 63 {
			return fmt.Errorf("hostname label is too long")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("hostname label must not start or end with a hyphen")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return fmt.Errorf("hostname contains unsupported character")
		}
	}
	return nil
}

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			panic(err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}
