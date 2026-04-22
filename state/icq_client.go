package state

import (
	"strconv"
	"strings"
)

// ICQ6GenerationClient reports whether the OSCAR ClientID string (from login TLV
// wire.LoginTLVTagsClientIdentity, copied into the BOS auth cookie) indicates a
// client that expects ICQ 2002 / ICQ 2003 / ICQ 6+ style BuddyArrived TLV blocks
// (OscarCaps 0x0D, ICQ direct-connect 0x0C) more strictly than ICQ 2000b when
// painting buddy flowers.
func ICQ6GenerationClient(clientID string) bool {
	s := strings.ToLower(strings.TrimSpace(clientID))
	if s == "" || !strings.Contains(s, "icq") {
		return false
	}
	// Older OSCAR ICQ builds we keep on the generic path.
	if strings.Contains(s, "2000") || strings.Contains(s, "2001") {
		return false
	}
	if strings.Contains(s, "2002") || strings.Contains(s, "2003") {
		return true
	}
	// "icq 6", "icq6", "icq 6.5", "icq 7", … — major client version at least 6.
	return icqMajorVersionAtLeast6(s)
}

// icqMajorVersionAtLeast6 reports whether some "icq" token in s is followed (after
// optional separators) by a decimal integer major version >= 6 (e.g. ICQ 6, 6.5,
// 10). Year-style tokens like "icq2003" yield 2003 >= 6 and are treated as match;
// callers should run the 2000/2001 exclusions first.
func icqMajorVersionAtLeast6(s string) bool {
	lower := strings.ToLower(s)
	from := 0
	for {
		idx := strings.Index(lower[from:], "icq")
		if idx < 0 {
			return false
		}
		pos := from + idx + 3
		// Skip any run of non-digits (e.g. "icq build 6.5" → major 6).
		for pos < len(lower) && (lower[pos] < '0' || lower[pos] > '9') {
			pos++
		}
		if pos < len(lower) && lower[pos] >= '0' && lower[pos] <= '9' {
			end := pos
			for end < len(lower) && lower[end] >= '0' && lower[end] <= '9' {
				end++
			}
			if major, err := strconv.Atoi(lower[pos:end]); err == nil && major >= 6 {
				return true
			}
		}
		from = from + idx + 1
	}
}
