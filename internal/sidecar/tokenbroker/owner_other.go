//go:build !unix

package tokenbroker

import "io/fs"

// ownerUID is unavailable on this platform; the ownership check is skipped.
func ownerUID(fs.FileInfo) (uid int, ok bool) { return 0, false }
