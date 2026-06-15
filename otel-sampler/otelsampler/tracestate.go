package otelsampler

import (
	"strconv"
	"strings"
)

func insertOrUpdateTraceStateThKeyValue(existingOT, thkv string) string {
	if existingOT == "" {
		return thkv
	}

	start := -1
	var end int
	if strings.HasPrefix(existingOT, "th:") {
		start = 0
	} else if idx := strings.Index(existingOT, ";th:"); idx != -1 {
		start = idx + 1
	}
	if start == -1 {
		return thkv + ";" + existingOT
	}

	for end = start; end < len(existingOT); end++ {
		if existingOT[end] == ';' {
			end++
			break
		}
	}

	if end == len(existingOT) {
		return strings.TrimSuffix(thkv+";"+existingOT[:start], ";")
	}
	return thkv + ";" + existingOT[:start] + existingOT[end:]
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
