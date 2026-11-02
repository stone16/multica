//go:build linux

package agent

import (
	"golang.org/x/sys/unix"
	"strconv"
)

func unlockClaudeTUIpty(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0) }
func claudeTUIptyName(fd int) (string, error) {
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return "", err
	}
	return "/dev/pts/" + strconv.Itoa(n), nil
}
