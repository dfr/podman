package libpod

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/pkg/rootless"
	"github.com/containers/storage/pkg/stringid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func (p *Pod) updateCgroup() error {
	// We need to recreate the pod's cgroup
	if p.config.UsePodCgroup {
		switch p.runtime.config.Engine.CgroupManager {
		case config.SystemdCgroupsManager:
			cgroupPath, err := systemdSliceFromPath(p.config.CgroupParent, fmt.Sprintf("libpod_pod_%s", p.ID()))
			if err != nil {
				logrus.Errorf("Creating Cgroup for pod %s: %v", p.ID(), err)
			}
			p.state.CgroupPath = cgroupPath
		case config.CgroupfsCgroupsManager:
			if rootless.IsRootless() && isRootlessCgroupSet(p.config.CgroupParent) {
				p.state.CgroupPath = filepath.Join(p.config.CgroupParent, p.ID())

				logrus.Debugf("setting pod cgroup to %s", p.state.CgroupPath)
			}
		default:
			return errors.Wrapf(define.ErrInvalidArg, "unknown cgroups manager %s specified", p.runtime.config.Engine.CgroupManager)
		}
	}
	return nil
}
