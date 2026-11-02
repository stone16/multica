//go:build darwin || linux

package agent

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func openClaudeTUIpty() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	fd := int(master.Fd())
	if err = unlockClaudeTUIpty(fd); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	name, err := claudeTUIptyName(fd)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0o600)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 60, Col: 220}); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, nil, fmt.Errorf("set Claude TUI window size: %w", err)
	}
	return master, slave, nil
}
