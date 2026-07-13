package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantAddr string
		wantPort int
	}{
		{"host and port", "host:27018", "host", 27018},
		{"host only defaults to 27017", "host", "host", 27017},
		{"ipv6 with port", "[::1]:27017", "::1", 27017},
		{"ipv6 without port", "[::1]", "::1", 27017},
		{"empty", "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, port := SplitHostPort(tc.in)
			assert.Equal(t, tc.wantAddr, addr)
			assert.Equal(t, tc.wantPort, port)
		})
	}
}
