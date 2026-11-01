package synk

import (
	"golang.org/x/sys/unix"
	"os"
	"time"
)

func setFileTimes(f *os.File, t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Futimes(int(f.Fd()), []unix.Timeval{tv, tv})
}
