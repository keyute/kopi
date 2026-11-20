// The entry syntax and matching are adapted from src/sandbox/domain-pattern.ts
// of Anthropic's sandbox-runtime, https://github.com/anthropics/sandbox-runtime
// at commit 8f8623ee7c50b9dc973d2b159c08ef48ad332ca4.
// Copyright 2025 Anthropic
// Licensed under the Apache License, Version 2.0; see srt/LICENSE in this directory.
// Modified by the Kopi authors: ported to Go; refuses IP literals, localhost
// and a bare * in the deny list, which the original accepts.

package sandbox

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// domain is one allow or deny entry: a host name, or with wildcard the
// subdomains of host.
type domain struct {
	host     string
	wildcard bool
	port     int // 0 matches any port
}

func parseDomain(entry string) (domain, error) {
	s := strings.ToLower(entry)
	var d domain
	if i := strings.LastIndexByte(s, ':'); i >= 0 && isDigits(s[i+1:]) {
		port, err := strconv.Atoi(s[i+1:])
		if err != nil || port < 1 || port > 65535 || s[i+1] == '0' {
			return domain{}, fmt.Errorf("port %q is not 1 to 65535", s[i+1:])
		}
		d.port = port
		s = s[:i]
	}
	if s == "" || strings.Contains(s, "://") || strings.ContainsAny(s, "/:") || s == "localhost" {
		return domain{}, fmt.Errorf("%q is not a host name", entry)
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return domain{}, fmt.Errorf("%q is an IP address", entry)
	}
	name := s
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		d.wildcard = true
		name = rest
	}
	if strings.Contains(name, "*") {
		return domain{}, fmt.Errorf("%q uses * other than as a leading *. label", entry)
	}
	if len(name) > 253 {
		return domain{}, fmt.Errorf("%q is longer than 253 bytes", entry)
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return domain{}, fmt.Errorf("%q needs at least two labels", entry)
	}
	if isNumericLabel(labels[len(labels)-1]) {
		return domain{}, fmt.Errorf("%q ends in a numeric label", entry)
	}
	for _, label := range labels {
		if label == "" || strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return domain{}, fmt.Errorf("%q has an empty label or a character outside a-z, 0-9, - and _", entry)
		}
	}
	d.host = name
	return d, nil
}

// isNumericLabel is the WHATWG URL "ends in a number" test for a last label:
// the system resolver reads such a name as IPv4 shorthand.
func isNumericLabel(label string) bool {
	if hex, ok := strings.CutPrefix(label, "0x"); ok {
		return strings.Trim(hex, "0123456789abcdef") == ""
	}
	return isDigits(label)
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// ValidateDomain checks one entry of the network allow or deny list.
func ValidateDomain(entry string) error {
	if _, err := parseDomain(entry); err != nil {
		return fmt.Errorf("%w: %w; want a host name such as example.com or *.example.com, with an optional :port", ErrInvalidDomain, err)
	}
	return nil
}

// matches reports whether the entry covers host, already lower-cased, on port.
// A wildcard covers subdomains only, never the apex or an IP literal.
func (d domain) matches(host string, port int) bool {
	if d.port != 0 && d.port != port {
		return false
	}
	if !d.wildcard {
		return host == d.host
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	return strings.HasSuffix(host, "."+d.host)
}
