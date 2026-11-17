//go:build unix

package tokenbroker

import (
	"io/fs"
	"syscall"
)

// ownerUID returns the user ID that owns the file described by info.
func ownerUID(info fs.FileInfo) (uid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
