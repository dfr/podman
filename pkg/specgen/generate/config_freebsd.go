package generate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/common/libimage"
	"github.com/containers/podman/v4/pkg/specgen"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

var (
	errNotADevice = errors.New("not a device node")
)

func addPrivilegedDevices(g *generate.Generator) error {
	return errors.New("not supported on freebsd")
}

// DevicesFromPath computes a list of devices
func DevicesFromPath(g *generate.Generator, devicePath string) error {
	return errors.New("not supported on freebsd")
}

func BlockAccessToKernelFilesystems(privileged, pidModeIsHost bool, mask, unmask []string, g *generate.Generator) {
}

// based on getDevices from runc (libcontainer/devices/devices.go)
func getDevices(path string) ([]spec.LinuxDevice, error) {
	return nil, nil
}

func addDevice(g *generate.Generator, device string) error {
	return nil
}

// ParseDevice parses device mapping string to a src, dest & permissions string
func ParseDevice(device string) (string, string, string, error) { //nolint
	var src string
	var dst string
	permissions := "rwm"
	arr := strings.Split(device, ":")
	switch len(arr) {
	case 3:
		if !IsValidDeviceMode(arr[2]) {
			return "", "", "", fmt.Errorf("invalid device mode: %s", arr[2])
		}
		permissions = arr[2]
		fallthrough
	case 2:
		if IsValidDeviceMode(arr[1]) {
			permissions = arr[1]
		} else {
			if arr[1][0] != '/' {
				return "", "", "", fmt.Errorf("invalid device mode: %s", arr[1])
			}
			dst = arr[1]
		}
		fallthrough
	case 1:
		src = arr[0]
	default:
		return "", "", "", fmt.Errorf("invalid device specification: %s", device)
	}

	if dst == "" {
		dst = src
	}
	return src, dst, permissions, nil
}

// IsValidDeviceMode checks if the mode for device is valid or not.
// IsValid mode is a composition of r (read), w (write), and m (mknod).
func IsValidDeviceMode(mode string) bool {
	var legalDeviceMode = map[rune]bool{
		'r': true,
		'w': true,
		'm': true,
	}
	if mode == "" {
		return false
	}
	for _, c := range mode {
		if !legalDeviceMode[c] {
			return false
		}
		legalDeviceMode[c] = false
	}
	return true
}

// Copied from github.com/opencontainers/runc/libcontainer/devices
// Given the path to a device look up the information about a linux device
func deviceFromPath(path string) (*spec.LinuxDevice, error) {
	var stat unix.Stat_t
	err := unix.Lstat(path, &stat)
	if err != nil {
		return nil, err
	}
	var (
		devType   string
		mode      = stat.Mode
		devNumber = uint64(stat.Rdev)
		m         = os.FileMode(mode)
	)

	switch {
	case mode&unix.S_IFBLK == unix.S_IFBLK:
		devType = "b"
	case mode&unix.S_IFCHR == unix.S_IFCHR:
		devType = "c"
	case mode&unix.S_IFIFO == unix.S_IFIFO:
		devType = "p"
	default:
		return nil, errNotADevice
	}

	return &spec.LinuxDevice{
		Type:     devType,
		Path:     path,
		FileMode: &m,
		UID:      &stat.Uid,
		GID:      &stat.Gid,
		Major:    int64(unix.Major(devNumber)),
		Minor:    int64(unix.Minor(devNumber)),
	}, nil
}

func supportAmbientCapabilities() bool {
	return false
}

func shouldMask(mask string, unmask []string) bool {
	for _, m := range unmask {
		if strings.ToLower(m) == "all" {
			return false
		}
		for _, m1 := range strings.Split(m, ":") {
			match, err := filepath.Match(m1, mask)
			if err != nil {
				logrus.Errorf(err.Error())
			}
			if match {
				return false
			}
		}
	}
	return true
}

func getSeccompConfig(s *specgen.SpecGenerator, configSpec *spec.Spec, img *libimage.Image) (*spec.LinuxSeccomp, error) {
	return nil, errors.New("seccomp not supported on freebsd")
}
