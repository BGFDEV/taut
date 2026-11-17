package slack

import (
	"cmp"
	"strings"
	"time"
)

// ParseTS splits a Slack timestamp such as "1709546400.000200" into whole
// seconds and microseconds using integer math only. Slack timestamps are
// message identifiers as much as they are times, so they are never routed
// through a float: float64 cannot represent every microsecond value exactly.
//
// The fractional part is optional and may have one to six digits (shorter
// fractions are right-padded, so "12.5" is 500000 microseconds). ok is false
// for anything else, including the empty string.
func ParseTS(ts string) (sec, micro int64, ok bool) {
	whole, frac, hasDot := strings.Cut(ts, ".")
	if len(whole) == 0 || len(whole) > 12 {
		return 0, 0, false
	}
	for i := 0; i < len(whole); i++ {
		c := whole[i]
		if c < '0' || c > '9' {
			return 0, 0, false
		}
		sec = sec*10 + int64(c-'0')
	}
	if hasDot {
		if len(frac) == 0 || len(frac) > 6 {
			return 0, 0, false
		}
		for i := 0; i < len(frac); i++ {
			c := frac[i]
			if c < '0' || c > '9' {
				return 0, 0, false
			}
			micro = micro*10 + int64(c-'0')
		}
		for i := len(frac); i < 6; i++ {
			micro *= 10
		}
	}
	return sec, micro, true
}

// CompareTS orders two Slack timestamps numerically as (seconds, microseconds)
// pairs. It returns -1, 0 or +1. Timestamps that do not parse sort before
// valid ones, and among themselves by plain string comparison, so the order is
// total and deterministic even on damaged data.
func CompareTS(a, b string) int {
	as, am, aok := ParseTS(a)
	bs, bm, bok := ParseTS(b)
	switch {
	case aok && bok:
		if c := cmp.Compare(as, bs); c != 0 {
			return c
		}
		return cmp.Compare(am, bm)
	case aok:
		return 1
	case bok:
		return -1
	default:
		return strings.Compare(a, b)
	}
}

// TSTime converts a Slack timestamp to a UTC time with microsecond precision.
// It returns the zero time if ts is not a valid timestamp.
func TSTime(ts string) time.Time {
	sec, micro, ok := ParseTS(ts)
	if !ok {
		return time.Time{}
	}
	return time.Unix(sec, micro*1000).UTC()
}
