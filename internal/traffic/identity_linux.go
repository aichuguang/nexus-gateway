//go:build linux

package traffic

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openSource(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func fileIdentity(f *os.File) (string, error) {
	i, err := f.Stat()
	if err != nil {
		return "", err
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("file identity unavailable")
	}
	// Birth time disambiguates inode reuse without using ctime, which changes
	// during an ordinary rename. Some filesystems do not expose birth time;
	// those still retain the dev/inode and content/checkpoint safeguards.
	var extended unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &extended); err != nil {
		return "", fmt.Errorf("stable traffic file identity: %w", err)
	}
	return fmt.Sprintf("linux:%d:%d:%d:%d", uint64(s.Dev), uint64(s.Ino), extended.Btime.Sec, extended.Btime.Nsec), nil
}
