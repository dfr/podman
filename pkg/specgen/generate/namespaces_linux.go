package generate

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containers/common/libimage"
	"github.com/containers/common/libnetwork/types"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/libpod"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/pkg/rootless"
	"github.com/containers/podman/v4/pkg/specgen"
	"github.com/containers/podman/v4/pkg/util"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func specConfigureNamespaces(s *specgen.SpecGenerator, g *generate.Generator, rt *libpod.Runtime, pod *libpod.Pod) error {
	// PID
	switch s.PidNS.NSMode {
	case specgen.Path:
		if _, err := os.Stat(s.PidNS.Value); err != nil {
			return errors.Wrap(err, "cannot find specified PID namespace path")
		}
		if err := g.AddOrReplaceLinuxNamespace(string(spec.PIDNamespace), s.PidNS.Value); err != nil {
			return err
		}
	case specgen.Host:
		if err := g.RemoveLinuxNamespace(string(spec.PIDNamespace)); err != nil {
			return err
		}
	case specgen.Private:
		if err := g.AddOrReplaceLinuxNamespace(string(spec.PIDNamespace), ""); err != nil {
			return err
		}
	}

	// IPC
	switch s.IpcNS.NSMode {
	case specgen.Path:
		if _, err := os.Stat(s.IpcNS.Value); err != nil {
			return errors.Wrap(err, "cannot find specified IPC namespace path")
		}
		if err := g.AddOrReplaceLinuxNamespace(string(spec.IPCNamespace), s.IpcNS.Value); err != nil {
			return err
		}
	case specgen.Host:
		if err := g.RemoveLinuxNamespace(string(spec.IPCNamespace)); err != nil {
			return err
		}
	case specgen.Private:
		if err := g.AddOrReplaceLinuxNamespace(string(spec.IPCNamespace), ""); err != nil {
			return err
		}
	}

	// UTS
	switch s.UtsNS.NSMode {
	case specgen.Path:
		if _, err := os.Stat(s.UtsNS.Value); err != nil {
			return errors.Wrap(err, "cannot find specified UTS namespace path")
		}
		if err := g.AddOrReplaceLinuxNamespace(string(spec.UTSNamespace), s.UtsNS.Value); err != nil {
			return err
		}
	case specgen.Host:
		if err := g.RemoveLinuxNamespace(string(spec.UTSNamespace)); err != nil {
			return err
		}
	case specgen.Private:
		if err := g.AddOrReplaceLinuxNamespace(string(spec.UTSNamespace), ""); err != nil {
			return err
		}
	}

	hostname := s.Hostname
	if hostname == "" {
		switch {
		case s.UtsNS.NSMode == specgen.FromPod:
			hostname = pod.Hostname()
		case s.UtsNS.NSMode == specgen.FromContainer:
			utsCtr, err := rt.LookupContainer(s.UtsNS.Value)
			if err != nil {
				return errors.Wrapf(err, "error looking up container to share uts namespace with")
			}
			hostname = utsCtr.Hostname()
		case (s.NetNS.NSMode == specgen.Host && hostname == "") || s.UtsNS.NSMode == specgen.Host:
			tmpHostname, err := os.Hostname()
			if err != nil {
				return errors.Wrap(err, "unable to retrieve hostname of the host")
			}
			hostname = tmpHostname
		default:
			logrus.Debug("No hostname set; container's hostname will default to runtime default")
		}
	}

	g.RemoveHostname()
	if s.Hostname != "" || s.UtsNS.NSMode != specgen.Host {
		// Set the hostname in the OCI configuration only if specified by
		// the user or if we are creating a new UTS namespace.
		// TODO: Should we be doing this for pod or container shared
		// namespaces?
		g.SetHostname(hostname)
	}
	if _, ok := s.Env["HOSTNAME"]; !ok && s.Hostname != "" {
		g.AddProcessEnv("HOSTNAME", hostname)
	}

	// User
	if _, err := specgen.SetupUserNS(s.IDMappings, s.UserNS, g); err != nil {
		return err
	}

	// Cgroup
	switch s.CgroupNS.NSMode {
	case specgen.Path:
		if _, err := os.Stat(s.CgroupNS.Value); err != nil {
			return errors.Wrap(err, "cannot find specified cgroup namespace path")
		}
		if err := g.AddOrReplaceLinuxNamespace(string(spec.CgroupNamespace), s.CgroupNS.Value); err != nil {
			return err
		}
	case specgen.Host:
		if err := g.RemoveLinuxNamespace(string(spec.CgroupNamespace)); err != nil {
			return err
		}
	case specgen.Private:
		if err := g.AddOrReplaceLinuxNamespace(string(spec.CgroupNamespace), ""); err != nil {
			return err
		}
	}

	// Net
	switch s.NetNS.NSMode {
	case specgen.Path:
		if _, err := os.Stat(s.NetNS.Value); err != nil {
			return errors.Wrap(err, "cannot find specified network namespace path")
		}
		if err := g.AddOrReplaceLinuxNamespace(string(spec.NetworkNamespace), s.NetNS.Value); err != nil {
			return err
		}
	case specgen.Host:
		if err := g.RemoveLinuxNamespace(string(spec.NetworkNamespace)); err != nil {
			return err
		}
	case specgen.Private, specgen.NoNetwork:
		if err := g.AddOrReplaceLinuxNamespace(string(spec.NetworkNamespace), ""); err != nil {
			return err
		}
	}

	if g.Config.Annotations == nil {
		g.Config.Annotations = make(map[string]string)
	}
	if s.PublishExposedPorts {
		g.Config.Annotations[define.InspectAnnotationPublishAll] = define.InspectResponseTrue
	} else {
		g.Config.Annotations[define.InspectAnnotationPublishAll] = define.InspectResponseFalse
	}

	return nil
}
