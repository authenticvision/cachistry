package cache

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func atime(info os.FileInfo) time.Time {
	switch stat := info.Sys().(type) {
	case *syscall.Stat_t:
		return time.Unix(stat.Atimespec.Sec, stat.Atimespec.Nsec)
	case *unix.Stat_t:
		return time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
	default:
		panic(fmt.Sprintf("fetch atime: unknown os.FileInfo.Sys() type: %T", stat))
	}
}
