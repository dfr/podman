//go:build !linux
// +build !linux

package libpod

import (
	"errors"
)

func (r *Runtime) stopPauseProcess() error {
	return errors.New("not supported on non-linux")
}

func (r *Runtime) migrate() error {
	return errors.New("not supported on non-linux")
}
