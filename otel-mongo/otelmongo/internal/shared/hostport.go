package shared

import (
	"net/url"
	"strconv"
)

// SplitHostPort extracts host and port from a "host:port" (or bare "host")
// string, IPv6-aware (e.g. "[::1]:27017"). Returns port 27017 when no port
// is present. Returns ("", 0) when host cannot be determined.
func SplitHostPort(s string) (addr string, port int) {
	u, err := url.Parse("//" + s)
	if err != nil {
		return "", 0
	}
	addr = u.Hostname()
	if addr == "" {
		return "", 0
	}
	portStr := u.Port()
	if portStr == "" {
		return addr, 27017
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 {
		return addr, 27017
	}
	return addr, p
}
