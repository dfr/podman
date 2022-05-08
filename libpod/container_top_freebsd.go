//go:build freebsd
// +build freebsd

package libpod

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/containers/podman/v4/libpod/define"
	"github.com/google/shlex"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Top gathers statistics about the running processes in a container. It returns a
// []string for output
func (c *Container) Top(descriptors []string) ([]string, error) {
	conStat, err := c.State()
	if err != nil {
		return nil, errors.Wrapf(err, "unable to look up state for %s", c.ID())
	}
	if conStat != define.ContainerStateRunning {
		return nil, errors.Errorf("top can only be used on running containers")
	}

	// Also support comma-separated input.
	psgoDescriptors := []string{}
	for _, d := range descriptors {
		for _, s := range strings.Split(d, ",") {
			if s != "" {
				psgoDescriptors = append(psgoDescriptors, s)
			}
		}
	}

	// Note that the descriptors to ps(1) must be shlexed (see #12452).
	psDescriptors := []string{}
	for _, d := range descriptors {
		shSplit, err := shlex.Split(d)
		if err != nil {
			return nil, fmt.Errorf("parsing ps args: %v", err)
		}
		for _, s := range shSplit {
			if s != "" {
				psDescriptors = append(psDescriptors, s)
			}
		}
	}

	output, err := c.execPS(psDescriptors)
	if err != nil {
		return nil, errors.Wrapf(err, "error executing ps(1) in the container")
	}

	// Trick: filter the ps command from the output instead of
	// checking/requiring PIDs in the output.
	filtered := []string{}
	cmd := strings.Join(descriptors, " ")
	for _, line := range output {
		if !strings.Contains(line, cmd) {
			filtered = append(filtered, line)
		}
	}

	return filtered, nil
}

// GetContainerPidInformation returns process-related data of all processes in
// the container.  The output data can be controlled via the `descriptors`
// argument which expects format descriptors and supports all AIXformat
// descriptors of ps (1) plus some additional ones to for instance inspect the
// set of effective capabilities.  Each element in the returned string slice
// is a tab-separated string.
//
// For more details, please refer to github.com/containers/psgo.
func (c *Container) GetContainerPidInformation(descriptors []string) ([]string, error) {
	return nil, errors.New("psgo not supported on freebsd")
}

// execPS executes ps(1) with the specified args in the container.
func (c *Container) execPS(args []string) ([]string, error) {
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer wPipe.Close()
	defer rPipe.Close()

	rErrPipe, wErrPipe, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer wErrPipe.Close()
	defer rErrPipe.Close()

	streams := new(define.AttachStreams)
	streams.OutputStream = wPipe
	streams.ErrorStream = wErrPipe
	streams.AttachOutput = true
	streams.AttachError = true

	stdout := []string{}
	go func() {
		scanner := bufio.NewScanner(rPipe)
		for scanner.Scan() {
			stdout = append(stdout, scanner.Text())
		}
	}()
	stderr := []string{}
	go func() {
		scanner := bufio.NewScanner(rErrPipe)
		for scanner.Scan() {
			stderr = append(stderr, scanner.Text())
		}
	}()

	cmd := append([]string{"ps"}, args...)
	config := new(ExecConfig)
	config.Command = cmd
	ec, err := c.Exec(config, streams, nil)
	if err != nil {
		return nil, err
	} else if ec != 0 {
		return nil, errors.Errorf("Runtime failed with exit status: %d and output: %s", ec, strings.Join(stderr, " "))
	}

	if logrus.GetLevel() >= logrus.DebugLevel {
		// If we're running in debug mode or higher, we might want to have a
		// look at stderr which includes debug logs from conmon.
		for _, log := range stderr {
			logrus.Debugf("%s", log)
		}
	}

	return stdout, nil
}
