//go:build !darwin && !linux

package agent

import (
	"errors"
	"os"
)

func openClaudeTUIpty() (master, slave *os.File, err error) {
	return nil, nil, errors.New("claude tui is unsupported on this platform")
}
