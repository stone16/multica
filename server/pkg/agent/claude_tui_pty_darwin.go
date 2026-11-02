//go:build darwin

package agent

import (
	"golang.org/x/sys/unix"
	"syscall"
	"unsafe"
)

func unlockClaudeTUIpty(fd int) error {
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return err
	}
	return unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0)
}
func claudeTUIptyName(fd int) (string, error) {
	var name [128]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0])))
	if errno != 0 {
		return "", errno
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	return string(name[:n]), nil
}
