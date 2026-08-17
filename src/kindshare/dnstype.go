package main

import "github.com/miekg/dns"

// dnsTypeByName maps a textual record type to its numeric code.
func dnsTypeByName(s string) (uint16, bool) {
	for code, name := range dns.TypeToString {
		if name == s {
			return code, true
		}
	}
	return 0, false
}
