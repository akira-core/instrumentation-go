package otelsampler

import (
	"strconv"
	"strings"
)

// insertOrUpdateOTSubKey returns the "ot" member value with the sub-key that
// starts with prefix (e.g. "th:" or "rv:") replaced by kv, moved to the front.
// When the sub-key is absent, kv is prepended. existingOT is the raw value of
// the tracestate "ot" member (sub-keys separated by ";").
func insertOrUpdateOTSubKey(existingOT, prefix, kv string) string {
	if existingOT == "" {
		return kv
	}

	start := -1
	var end int
	if strings.HasPrefix(existingOT, prefix) {
		start = 0
	} else if idx := strings.Index(existingOT, ";"+prefix); idx != -1 {
		start = idx + 1
	}
	if start == -1 {
		return kv + ";" + existingOT
	}

	for end = start; end < len(existingOT); end++ {
		if existingOT[end] == ';' {
			end++
			break
		}
	}

	if end == len(existingOT) {
		return strings.TrimSuffix(kv+";"+existingOT[:start], ";")
	}
	return kv + ";" + existingOT[:start] + existingOT[end:]
}

func tracestateRandomness(otts string) (randomness uint64, hasRandomness bool) {
	var start int
	if strings.HasPrefix(otts, "rv:") {
		start = 3
	} else if idx := strings.Index(otts, ";rv:"); idx != -1 {
		start = idx + 4
	} else {
		return 0, false
	}

	if len(otts) < start+14 || (len(otts) > start+14 && otts[start+14] != ';') {
		return 0, false
	}

	rv, err := strconv.ParseUint(otts[start:start+14], 16, 56)
	if err != nil {
		return 0, false
	}
	return rv, true
}
