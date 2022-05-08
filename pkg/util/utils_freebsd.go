//go:build freebsd
// +build freebsd

package util

import (
	"github.com/pkg/errors"
)

func GetContainerPidInformationDescriptors() ([]string, error) {
	return []string{}, errors.New("this function is not supported on freebsd")
}
