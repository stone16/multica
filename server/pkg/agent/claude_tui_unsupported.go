//go:build !darwin && !linux

package agent

import (
	"context"
	"errors"
	"os"
)

type claudeTUIBackend struct{ cfg Config }

func (b *claudeTUIBackend) Execute(context.Context, string, ExecOptions) (*Session, error) {
	return nil, errors.New("claude tui is unsupported on this platform")
}

func openClaudeTUIpty() (master, slave *os.File, err error) {
	return nil, nil, errors.New("claude tui is unsupported on this platform")
}
