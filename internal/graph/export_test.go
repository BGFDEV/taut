package graph

import "time"

// Internal helpers exposed to the external test package.

func ParseRetryAfterForTest(v string, now time.Time) (time.Duration, bool) {
	return parseRetryAfter(v, now)
}

var SleepCtxForTest = sleepCtx
