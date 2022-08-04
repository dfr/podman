//go:build freebsd
// +build freebsd

package libpod

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/checkpoint-restore/go-criu/v5/stats"
	cdi "github.com/container-orchestrated-devices/container-device-interface/pkg/cdi"
	"github.com/containers/buildah/pkg/chrootuser"
	"github.com/containers/buildah/pkg/overlay"
	butil "github.com/containers/buildah/util"
	"github.com/containers/common/libnetwork/etchosts"
	"github.com/containers/common/libnetwork/resolvconf"
	"github.com/containers/common/libnetwork/types"
	"github.com/containers/common/pkg/chown"
	"github.com/containers/common/pkg/config"
	"github.com/containers/common/pkg/subscriptions"
	"github.com/containers/common/pkg/umask"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/libpod/events"
	"github.com/containers/podman/v4/pkg/annotations"
	"github.com/containers/podman/v4/pkg/checkpoint/crutils"
	"github.com/containers/podman/v4/pkg/criu"
	"github.com/containers/podman/v4/pkg/lookup"
	"github.com/containers/podman/v4/pkg/rootless"
	"github.com/containers/podman/v4/pkg/util"
	"github.com/containers/podman/v4/version"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/lockfile"
	securejoin "github.com/cyphar/filepath-securejoin"
	runcuser "github.com/opencontainers/runc/libcontainer/user"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var O_PATH = 0

func (c *Container) mountSHM(shmOptions string) error {
	return nil
}

func (c *Container) unmountSHM(path string) error {
	return nil
}

// prepare mounts the container and sets up other required resources like net
// namespaces
func (c *Container) prepare() error {
	var (
		wg                              sync.WaitGroup
		jailName                        string
		networkStatus                   map[string]types.StatusBlock
		createNetNSErr, mountStorageErr error
		mountPoint                      string
		tmpStateLock                    sync.Mutex
	)

	wg.Add(2)

	go func() {
		defer wg.Done()
		// Set up network namespace if not already set up
		noNetNS := c.state.NetworkJail == ""
		if c.config.CreateNetNS && noNetNS && !c.config.PostConfigureNetNS {
			jailName, networkStatus, createNetNSErr = c.runtime.createNetNS(c)
			if createNetNSErr != nil {
				return
			}

			tmpStateLock.Lock()
			defer tmpStateLock.Unlock()

			// Assign NetNS attributes to container
			c.state.NetworkJail = jailName
			c.state.NetworkStatus = networkStatus
		}
	}()
	// Mount storage if not mounted
	go func() {
		defer wg.Done()
		mountPoint, mountStorageErr = c.mountStorage()

		if mountStorageErr != nil {
			return
		}

		tmpStateLock.Lock()
		defer tmpStateLock.Unlock()

		// Finish up mountStorage
		c.state.Mounted = true
		c.state.Mountpoint = mountPoint

		logrus.Debugf("Created root filesystem for container %s at %s", c.ID(), c.state.Mountpoint)
	}()

	wg.Wait()

	var createErr error
	if mountStorageErr != nil {
		if createErr != nil {
			logrus.Errorf("Preparing container %s: %v", c.ID(), createErr)
		}
		createErr = mountStorageErr
	}

	if createErr != nil {
		return createErr
	}

	// Save changes to container state
	logrus.Debugf("saving state for %v: NetworkJail=%s", c.ID(), c.state.containerPlatformState.NetworkJail)
	if err := c.save(); err != nil {
		return err
	}
	logrus.Debugf("saved state for %v: NetworkJail=%s", c.ID(), c.state.containerPlatformState.NetworkJail)

	return nil
}

// isWorkDirSymlink returns true if resolved workdir is symlink or a chain of symlinks,
// and final resolved target is present either on  volume, mount or inside of container
// otherwise it returns false. Following function is meant for internal use only and
// can change at any point of time.
func (c *Container) isWorkDirSymlink(resolvedPath string) bool {
	// We cannot create workdir since explicit --workdir is
	// set in config but workdir could also be a symlink.
	// If its a symlink lets check if resolved link is present
	// on the container or not.

	// If we can resolve symlink and resolved link is present on the container
	// then return nil cause its a valid use-case.

	maxSymLinks := 0
	for {
		// Linux only supports a chain of 40 links.
		// Reference: https://github.com/torvalds/linux/blob/master/include/linux/namei.h#L13
		if maxSymLinks > 40 {
			break
		}
		resolvedSymlink, err := os.Readlink(resolvedPath)
		if err != nil {
			// End sym-link resolution loop.
			break
		}
		if resolvedSymlink != "" {
			_, resolvedSymlinkWorkdir, err := c.resolvePath(c.state.Mountpoint, resolvedSymlink)
			if isPathOnVolume(c, resolvedSymlinkWorkdir) || isPathOnBindMount(c, resolvedSymlinkWorkdir) {
				// Resolved symlink exists on external volume or mount
				return true
			}
			if err != nil {
				// Could not resolve path so end sym-link resolution loop.
				break
			}
			if resolvedSymlinkWorkdir != "" {
				resolvedPath = resolvedSymlinkWorkdir
				_, err := os.Stat(resolvedSymlinkWorkdir)
				if err == nil {
					// Symlink resolved successfully and resolved path exists on container,
					// this is a valid use-case so return nil.
					logrus.Debugf("Workdir is a symlink with target to %q and resolved symlink exists on container", resolvedSymlink)
					return true
				}
			}
		}
		maxSymLinks++
	}
	return false
}

// resolveWorkDir resolves the container's workdir and, depending on the
// configuration, will create it, or error out if it does not exist.
// Note that the container must be mounted before.
func (c *Container) resolveWorkDir() error {
	workdir := c.WorkingDir()

	// If the specified workdir is a subdir of a volume or mount,
	// we don't need to do anything.  The runtime is taking care of
	// that.
	if isPathOnVolume(c, workdir) || isPathOnBindMount(c, workdir) {
		logrus.Debugf("Workdir %q resolved to a volume or mount", workdir)
		return nil
	}

	_, resolvedWorkdir, err := c.resolvePath(c.state.Mountpoint, workdir)
	if err != nil {
		return err
	}
	logrus.Debugf("Workdir %q resolved to host path %q", workdir, resolvedWorkdir)

	st, err := os.Stat(resolvedWorkdir)
	if err == nil {
		if !st.IsDir() {
			return errors.Errorf("workdir %q exists on container %s, but is not a directory", workdir, c.ID())
		}
		return nil
	}
	if !c.config.CreateWorkingDir {
		// No need to create it (e.g., `--workdir=/foo`), so let's make sure
		// the path exists on the container.
		if err != nil {
			if os.IsNotExist(err) {
				// If resolved Workdir path gets marked as a valid symlink,
				// return nil cause this is valid use-case.
				if c.isWorkDirSymlink(resolvedWorkdir) {
					return nil
				}
				return errors.Errorf("workdir %q does not exist on container %s", workdir, c.ID())
			}
			// This might be a serious error (e.g., permission), so
			// we need to return the full error.
			return errors.Wrapf(err, "error detecting workdir %q on container %s", workdir, c.ID())
		}
		return nil
	}
	if err := os.MkdirAll(resolvedWorkdir, 0755); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return errors.Wrapf(err, "error creating container %s workdir", c.ID())
	}

	// Ensure container entrypoint is created (if required).
	uid, gid, _, err := chrootuser.GetUser(c.state.Mountpoint, c.User())
	if err != nil {
		return errors.Wrapf(err, "error looking up %s inside of the container %s", c.User(), c.ID())
	}
	if err := os.Chown(resolvedWorkdir, int(uid), int(gid)); err != nil {
		return errors.Wrapf(err, "error chowning container %s workdir to container root", c.ID())
	}

	return nil
}

// cleanupNetwork unmounts and cleans up the container's network
func (c *Container) cleanupNetwork() error {
	if c.config.NetNsCtr != "" {
		return nil
	}
	netDisabled, err := c.NetworkDisabled()
	if err != nil {
		return err
	}
	if netDisabled {
		return nil
	}

	// Stop the container's network namespace (if it has one)
	if err := c.runtime.teardownNetNS(c); err != nil {
		logrus.Errorf("Unable to cleanup network for container %s: %q", c.ID(), err)
	}

	if c.valid {
		return c.save()
	}

	return nil
}

// reloadNetwork reloads the network for the given container, recreating
// firewall rules.
func (c *Container) reloadNetwork() error {
	result, err := c.runtime.reloadContainerNetwork(c)
	if err != nil {
		return err
	}

	c.state.NetworkStatus = result

	return c.save()
}

func (c *Container) getUserOverrides() *lookup.Overrides {
	var hasPasswdFile, hasGroupFile bool
	overrides := lookup.Overrides{}
	for _, m := range c.config.Spec.Mounts {
		if m.Destination == "/etc/passwd" {
			overrides.ContainerEtcPasswdPath = m.Source
			hasPasswdFile = true
		}
		if m.Destination == "/etc/group" {
			overrides.ContainerEtcGroupPath = m.Source
			hasGroupFile = true
		}
		if m.Destination == "/etc" {
			if !hasPasswdFile {
				overrides.ContainerEtcPasswdPath = filepath.Join(m.Source, "passwd")
			}
			if !hasGroupFile {
				overrides.ContainerEtcGroupPath = filepath.Join(m.Source, "group")
			}
		}
	}
	if path, ok := c.state.BindMounts["/etc/passwd"]; ok {
		overrides.ContainerEtcPasswdPath = path
	}
	return &overrides
}

func lookupHostUser(name string) (*runcuser.ExecUser, error) {
	var execUser runcuser.ExecUser
	// Lookup User on host
	u, err := util.LookupUser(name)
	if err != nil {
		return &execUser, err
	}
	uid, err := strconv.ParseUint(u.Uid, 8, 32)
	if err != nil {
		return &execUser, err
	}

	gid, err := strconv.ParseUint(u.Gid, 8, 32)
	if err != nil {
		return &execUser, err
	}
	execUser.Uid = int(uid)
	execUser.Gid = int(gid)
	execUser.Home = u.HomeDir
	return &execUser, nil
}

// Generate spec for a container
// Accepts a map of the container's dependencies
func (c *Container) generateSpec(ctx context.Context) (*spec.Spec, error) {
	overrides := c.getUserOverrides()
	execUser, err := lookup.GetUserGroupInfo(c.state.Mountpoint, c.config.User, overrides)
	if err != nil {
		if util.StringInSlice(c.config.User, c.config.HostUsers) {
			execUser, err = lookupHostUser(c.config.User)
		}
		if err != nil {
			return nil, err
		}
	}

	g := generate.NewFromSpec(c.config.Spec)

	// If network namespace was requested, add it now
	if c.config.CreateNetNS {
		g.AddAnnotation("org.freebsd.parentJail", c.state.NetworkJail)
	}

	if err := c.makeBindMounts(); err != nil {
		return nil, err
	}

	if err := c.mountNotifySocket(g); err != nil {
		return nil, err
	}

	// Get host UID and GID based on the container process UID and GID.
	hostUID, hostGID, err := butil.GetHostIDs(util.IDtoolsToRuntimeSpec(c.config.IDMappings.UIDMap), util.IDtoolsToRuntimeSpec(c.config.IDMappings.GIDMap), uint32(execUser.Uid), uint32(execUser.Gid))
	if err != nil {
		return nil, err
	}

	// Add named volumes
	for _, namedVol := range c.config.NamedVolumes {
		volume, err := c.runtime.GetVolume(namedVol.Name)
		if err != nil {
			return nil, errors.Wrapf(err, "error retrieving volume %s to add to container %s", namedVol.Name, c.ID())
		}
		mountPoint, err := volume.MountPoint()
		if err != nil {
			return nil, err
		}

		overlayFlag := false
		upperDir := ""
		workDir := ""
		for _, o := range namedVol.Options {
			if o == "O" {
				overlayFlag = true
			}
			if overlayFlag && strings.Contains(o, "upperdir") {
				splitOpt := strings.SplitN(o, "=", 2)
				if len(splitOpt) > 1 {
					upperDir = splitOpt[1]
					if upperDir == "" {
						return nil, errors.New("cannot accept empty value for upperdir")
					}
				}
			}
			if overlayFlag && strings.Contains(o, "workdir") {
				splitOpt := strings.SplitN(o, "=", 2)
				if len(splitOpt) > 1 {
					workDir = splitOpt[1]
					if workDir == "" {
						return nil, errors.New("cannot accept empty value for workdir")
					}
				}
			}
		}

		if overlayFlag {
			var overlayMount spec.Mount
			var overlayOpts *overlay.Options
			contentDir, err := overlay.TempDir(c.config.StaticDir, c.RootUID(), c.RootGID())
			if err != nil {
				return nil, err
			}

			if (upperDir != "" && workDir == "") || (upperDir == "" && workDir != "") {
				return nil, errors.Wrapf(err, "must specify both upperdir and workdir")
			}

			overlayOpts = &overlay.Options{RootUID: c.RootUID(),
				RootGID:                c.RootGID(),
				UpperDirOptionFragment: upperDir,
				WorkDirOptionFragment:  workDir,
				GraphOpts:              c.runtime.store.GraphOptions(),
			}

			overlayMount, err = overlay.MountWithOptions(contentDir, mountPoint, namedVol.Dest, overlayOpts)
			if err != nil {
				return nil, errors.Wrapf(err, "mounting overlay failed %q", mountPoint)
			}

			for _, o := range namedVol.Options {
				switch o {
				case "U":
					if err := c.ChangeHostPathOwnership(mountPoint, true, int(hostUID), int(hostGID)); err != nil {
						return nil, err
					}

					if err := c.ChangeHostPathOwnership(contentDir, true, int(hostUID), int(hostGID)); err != nil {
						return nil, err
					}
				}
			}
			g.AddMount(overlayMount)
		} else {
			volMount := spec.Mount{
				Type:        "nullfs",
				Source:      mountPoint,
				Destination: namedVol.Dest,
				Options:     namedVol.Options,
			}
			g.AddMount(volMount)
		}
	}

	// Check if the spec file mounts contain the options z, Z or U.
	// If they have z or Z, relabel the source directory and then remove the option.
	// If they have U, chown the source directory and them remove the option.
	for i := range g.Config.Mounts {
		m := &g.Config.Mounts[i]
		var options []string
		for _, o := range m.Options {
			switch o {
			case "U":
				if m.Type == "tmpfs" {
					options = append(options, []string{fmt.Sprintf("uid=%d", execUser.Uid), fmt.Sprintf("gid=%d", execUser.Gid)}...)
				} else {
					// only chown on initial creation of container
					if err := c.ChangeHostPathOwnership(m.Source, true, int(hostUID), int(hostGID)); err != nil {
						return nil, err
					}
				}
			case "z":
				fallthrough
			case "Z":
				if err := c.relabel(m.Source, c.MountLabel(), label.IsShared(o)); err != nil {
					return nil, err
				}

			default:
				options = append(options, o)
			}
		}
		m.Options = options
	}

	// Add bind mounts to container
	if false {
		for dstPath, srcPath := range c.state.BindMounts {
			newMount := spec.Mount{
				Type:        "nullfs",
				Source:      srcPath,
				Destination: dstPath,
				Options:     []string{},
			}
			if !MountExists(g.Mounts(), dstPath) {
				g.AddMount(newMount)
			} else {
				logrus.Infof("User mount overriding libpod mount at %q", dstPath)
			}
		}
	}

	// Add bind mounts to container
	for dstPath, srcPath := range c.state.BindMounts {
		newMount := spec.Mount{
			Type:        "nullfs",
			Source:      srcPath,
			Destination: dstPath,
			Options:     []string{},
		}
		if !MountExists(g.Mounts(), dstPath) {
			g.AddMount(newMount)
		} else {
			logrus.Infof("User mount overriding libpod mount at %q", dstPath)
		}
	}

	// Add overlay volumes
	for _, overlayVol := range c.config.OverlayVolumes {
		contentDir, err := overlay.TempDir(c.config.StaticDir, c.RootUID(), c.RootGID())
		if err != nil {
			return nil, err
		}
		overlayMount, err := overlay.Mount(contentDir, overlayVol.Source, overlayVol.Dest, c.RootUID(), c.RootGID(), c.runtime.store.GraphOptions())
		if err != nil {
			return nil, errors.Wrapf(err, "mounting overlay failed %q", overlayVol.Source)
		}

		// Check overlay volume options
		for _, o := range overlayVol.Options {
			switch o {
			case "U":
				if err := c.ChangeHostPathOwnership(overlayVol.Source, true, int(hostUID), int(hostGID)); err != nil {
					return nil, err
				}

				if err := c.ChangeHostPathOwnership(contentDir, true, int(hostUID), int(hostGID)); err != nil {
					return nil, err
				}
			}
		}

		g.AddMount(overlayMount)
	}

	// Add image volumes as overlay mounts
	for _, volume := range c.config.ImageVolumes {
		// Mount the specified image.
		img, _, err := c.runtime.LibimageRuntime().LookupImage(volume.Source, nil)
		if err != nil {
			return nil, errors.Wrapf(err, "error creating image volume %q:%q", volume.Source, volume.Dest)
		}
		mountPoint, err := img.Mount(ctx, nil, "")
		if err != nil {
			return nil, errors.Wrapf(err, "error mounting image volume %q:%q", volume.Source, volume.Dest)
		}

		contentDir, err := overlay.TempDir(c.config.StaticDir, c.RootUID(), c.RootGID())
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create TempDir in the %s directory", c.config.StaticDir)
		}

		var overlayMount spec.Mount
		if volume.ReadWrite {
			overlayMount, err = overlay.Mount(contentDir, mountPoint, volume.Dest, c.RootUID(), c.RootGID(), c.runtime.store.GraphOptions())
		} else {
			overlayMount, err = overlay.MountReadOnly(contentDir, mountPoint, volume.Dest, c.RootUID(), c.RootGID(), c.runtime.store.GraphOptions())
		}
		if err != nil {
			return nil, errors.Wrapf(err, "creating overlay mount for image %q failed", volume.Source)
		}
		g.AddMount(overlayMount)
	}

	hasHomeSet := false
	for _, s := range c.config.Spec.Process.Env {
		if strings.HasPrefix(s, "HOME=") {
			hasHomeSet = true
			break
		}
	}
	if !hasHomeSet && execUser.Home != "" {
		c.config.Spec.Process.Env = append(c.config.Spec.Process.Env, fmt.Sprintf("HOME=%s", execUser.Home))
	}

	if c.config.User != "" {
		// User and Group must go together
		g.SetProcessUID(uint32(execUser.Uid))
		g.SetProcessGID(uint32(execUser.Gid))
	}

	if c.config.Umask != "" {
		decVal, err := strconv.ParseUint(c.config.Umask, 8, 32)
		if err != nil {
			return nil, errors.Wrapf(err, "Invalid Umask Value")
		}
		umask := uint32(decVal)
		g.Config.Process.User.Umask = &umask
	}

	// Add addition groups if c.config.GroupAdd is not empty
	if len(c.config.Groups) > 0 {
		gids, err := lookup.GetContainerGroups(c.config.Groups, c.state.Mountpoint, overrides)
		if err != nil {
			return nil, errors.Wrapf(err, "error looking up supplemental groups for container %s", c.ID())
		}
		for _, gid := range gids {
			g.AddProcessAdditionalGid(gid)
		}
	}

	// Look up and add groups the user belongs to, if a group wasn't directly specified
	if !strings.Contains(c.config.User, ":") {
		// the gidMappings that are present inside the container user namespace
		var gidMappings []idtools.IDMap

		switch {
		case len(c.config.IDMappings.GIDMap) > 0:
			gidMappings = c.config.IDMappings.GIDMap
		case rootless.IsRootless():
			// Check whether the current user namespace has enough gids available.
			availableGids, err := rootless.GetAvailableGids()
			if err != nil {
				return nil, errors.Wrapf(err, "cannot read number of available GIDs")
			}
			gidMappings = []idtools.IDMap{{
				ContainerID: 0,
				HostID:      0,
				Size:        int(availableGids),
			}}
		default:
			gidMappings = []idtools.IDMap{{
				ContainerID: 0,
				HostID:      0,
				Size:        math.MaxInt32,
			}}
		}
		for _, gid := range execUser.Sgids {
			isGIDAvailable := false
			for _, m := range gidMappings {
				if gid >= m.ContainerID && gid < m.ContainerID+m.Size {
					isGIDAvailable = true
					break
				}
			}
			if isGIDAvailable {
				g.AddProcessAdditionalGid(uint32(gid))
			} else {
				logrus.Warnf("Additional gid=%d is not present in the user namespace, skip setting it", gid)
			}
		}
	}

	// Add shared namespaces from other containers
	if c.config.NetNsCtr != "" {
		if err := c.addNetworkContainer(&g, c.config.NetNsCtr); err != nil {
			return nil, err
		}
	}

	availableUIDs, availableGIDs, err := rootless.GetAvailableIDMaps()
	if err != nil {
		if os.IsNotExist(err) {
			// The kernel-provided files only exist if user namespaces are supported
			logrus.Debugf("User or group ID mappings not available: %s", err)
		} else {
			return nil, err
		}
	} else {
		g.Config.Linux.UIDMappings = rootless.MaybeSplitMappings(g.Config.Linux.UIDMappings, availableUIDs)
		g.Config.Linux.GIDMappings = rootless.MaybeSplitMappings(g.Config.Linux.GIDMappings, availableGIDs)
	}

	// Hostname handling:
	// If we have a UTS namespace, set Hostname in the OCI spec.
	// Set the HOSTNAME environment variable unless explicitly overridden by
	// the user (already present in OCI spec). If we don't have a UTS ns,
	// set it to the host's hostname instead.
	hostname := c.Hostname()
	foundUTS := false

	// TODO: make this optional, needs progress on adding FreeBSD section to the spec
	foundUTS = true
	g.SetHostname(hostname)

	if !foundUTS {
		tmpHostname, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		hostname = tmpHostname
	}
	needEnv := true
	for _, checkEnv := range g.Config.Process.Env {
		if strings.SplitN(checkEnv, "=", 2)[0] == "HOSTNAME" {
			needEnv = false
			break
		}
	}
	if needEnv {
		g.AddProcessEnv("HOSTNAME", hostname)
	}

	g.SetRootPath(c.state.Mountpoint)
	g.AddAnnotation(annotations.Created, c.config.CreatedTime.Format(time.RFC3339Nano))
	g.AddAnnotation("org.opencontainers.image.stopSignal", fmt.Sprintf("%d", c.config.StopSignal))

	if _, exists := g.Config.Annotations[annotations.ContainerManager]; !exists {
		g.AddAnnotation(annotations.ContainerManager, annotations.ContainerManagerLibpod)
	}

	// Warning: CDI may alter g.Config in place.
	if len(c.config.CDIDevices) > 0 {
		registry := cdi.GetRegistry()
		if errs := registry.GetErrors(); len(errs) > 0 {
			logrus.Debugf("The following errors were triggered when creating the CDI registry: %v", errs)
		}
		_, err := registry.InjectDevices(g.Config, c.config.CDIDevices...)
		if err != nil {
			return nil, errors.Wrapf(err, "error setting up CDI devices")
		}
	}

	// Mounts need to be sorted so paths will not cover other paths
	mounts := sortMounts(g.Mounts())
	g.ClearMounts()

	// Determine property of RootPropagation based on volume properties. If
	// a volume is shared, then keep root propagation shared. This should
	// work for slave and private volumes too.
	//
	// For slave volumes, it can be either [r]shared/[r]slave.
	//
	// For private volumes any root propagation value should work.
	rootPropagation := ""
	for _, m := range mounts {
		// We need to remove all symlinks from tmpfs mounts.
		// Runc and other runtimes may choke on them.
		// Easy solution: use securejoin to do a scoped evaluation of
		// the links, then trim off the mount prefix.
		if m.Type == "tmpfs" {
			finalPath, err := securejoin.SecureJoin(c.state.Mountpoint, m.Destination)
			if err != nil {
				return nil, errors.Wrapf(err, "error resolving symlinks for mount destination %s", m.Destination)
			}
			trimmedPath := strings.TrimPrefix(finalPath, strings.TrimSuffix(c.state.Mountpoint, "/"))
			m.Destination = trimmedPath
		}
		g.AddMount(m)
		/*
			for _, opt := range m.Options {
					switch opt {
					case MountShared, MountRShared:
						if rootPropagation != MountShared && rootPropagation != MountRShared {
							rootPropagation = MountShared
						}
					case MountSlave, MountRSlave:
						if rootPropagation != MountShared && rootPropagation != MountRShared && rootPropagation != MountSlave && rootPropagation != MountRSlave {
							rootPropagation = MountRSlave
						}
					}
			}*/
	}

	if rootPropagation != "" {
		logrus.Debugf("Set root propagation to %q", rootPropagation)
		if err := g.SetLinuxRootPropagation(rootPropagation); err != nil {
			return nil, err
		}
	}

	// Warning: precreate hooks may alter g.Config in place.
	if c.state.ExtensionStageHooks, err = c.setupOCIHooks(ctx, g.Config); err != nil {
		return nil, errors.Wrapf(err, "error setting up OCI Hooks")
	}
	if len(c.config.EnvSecrets) > 0 {
		manager, err := c.runtime.SecretsManager()
		if err != nil {
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		for name, secr := range c.config.EnvSecrets {
			_, data, err := manager.LookupSecretData(secr.Name)
			if err != nil {
				return nil, err
			}
			g.AddProcessEnv(name, string(data))
		}
	}

	// Pass down the LISTEN_* environment (see #10443).
	for _, key := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if val, ok := os.LookupEnv(key); ok {
			// Force the PID to `1` since we cannot rely on (all
			// versions of) all runtimes to do it for us.
			if key == "LISTEN_PID" {
				val = "1"
			}
			g.AddProcessEnv(key, val)
		}
	}

	return g.Config, nil
}

// mountNotifySocket mounts the NOTIFY_SOCKET into the container if it's set
// and if the sdnotify mode is set to container.  It also sets c.notifySocket
// to avoid redundantly looking up the env variable.
func (c *Container) mountNotifySocket(g generate.Generator) error {
	notify, ok := os.LookupEnv("NOTIFY_SOCKET")
	if !ok {
		return nil
	}
	c.notifySocket = notify

	if c.config.SdNotifyMode != define.SdNotifyModeContainer {
		return nil
	}

	notifyDir := filepath.Join(c.bundlePath(), "notify")
	logrus.Debugf("Checking notify %q dir", notifyDir)
	if err := os.MkdirAll(notifyDir, 0755); err != nil {
		if !os.IsExist(err) {
			return errors.Wrapf(err, "unable to create notify %q dir", notifyDir)
		}
	}
	if err := label.Relabel(notifyDir, c.MountLabel(), true); err != nil {
		return errors.Wrapf(err, "relabel failed %q", notifyDir)
	}
	logrus.Debugf("Add bindmount notify %q dir", notifyDir)
	if _, ok := c.state.BindMounts["/run/notify"]; !ok {
		c.state.BindMounts["/run/notify"] = notifyDir
	}

	// Set the container's notify socket to the proxy socket created by conmon
	g.AddProcessEnv("NOTIFY_SOCKET", "/run/notify/notify.sock")

	return nil
}

// Add an existing container's network jail
func (c *Container) addNetworkContainer(g *generate.Generator, ctr string) error {
	nsCtr, err := c.runtime.state.Container(ctr)
	c.runtime.state.UpdateContainer(nsCtr)
	if err != nil {
		return errors.Wrapf(err, "error retrieving dependency %s of container %s from state", ctr, c.ID())
	}
	g.AddAnnotation("org.freebsd.parentJail", nsCtr.state.NetworkJail)
	return nil
}

func (c *Container) exportCheckpoint(options ContainerCheckpointOptions) error {
	if len(c.Dependencies()) == 1 {
		// Check if the dependency is an infra container. If it is we can checkpoint
		// the container out of the Pod.
		if c.config.Pod == "" {
			return errors.Errorf("cannot export checkpoints of containers with dependencies")
		}

		pod, err := c.runtime.state.Pod(c.config.Pod)
		if err != nil {
			return errors.Wrapf(err, "container %s is in pod %s, but pod cannot be retrieved", c.ID(), c.config.Pod)
		}
		infraID, err := pod.InfraContainerID()
		if err != nil {
			return errors.Wrapf(err, "cannot retrieve infra container ID for pod %s", c.config.Pod)
		}
		if c.Dependencies()[0] != infraID {
			return errors.Errorf("cannot export checkpoints of containers with dependencies")
		}
	}
	if len(c.Dependencies()) > 1 {
		return errors.Errorf("cannot export checkpoints of containers with dependencies")
	}
	logrus.Debugf("Exporting checkpoint image of container %q to %q", c.ID(), options.TargetFile)

	includeFiles := []string{
		"artifacts",
		metadata.DevShmCheckpointTar,
		metadata.ConfigDumpFile,
		metadata.SpecDumpFile,
		metadata.NetworkStatusFile,
		stats.StatsDump,
	}

	if c.LogDriver() == define.KubernetesLogging ||
		c.LogDriver() == define.JSONLogging {
		includeFiles = append(includeFiles, "ctr.log")
	}
	if options.PreCheckPoint {
		includeFiles = append(includeFiles, preCheckpointDir)
	} else {
		includeFiles = append(includeFiles, metadata.CheckpointDirectory)
	}
	// Get root file-system changes included in the checkpoint archive
	var addToTarFiles []string
	if !options.IgnoreRootfs {
		// To correctly track deleted files, let's go through the output of 'podman diff'
		rootFsChanges, err := c.runtime.GetDiff("", c.ID(), define.DiffContainer)
		if err != nil {
			return errors.Wrapf(err, "error exporting root file-system diff for %q", c.ID())
		}

		addToTarFiles, err := crutils.CRCreateRootFsDiffTar(&rootFsChanges, c.state.Mountpoint, c.bundlePath())
		if err != nil {
			return err
		}

		includeFiles = append(includeFiles, addToTarFiles...)
	}

	// Folder containing archived volumes that will be included in the export
	expVolDir := filepath.Join(c.bundlePath(), "volumes")

	// Create an archive for each volume associated with the container
	if !options.IgnoreVolumes {
		if err := os.MkdirAll(expVolDir, 0700); err != nil {
			return errors.Wrapf(err, "error creating volumes export directory %q", expVolDir)
		}

		for _, v := range c.config.NamedVolumes {
			volumeTarFilePath := filepath.Join("volumes", v.Name+".tar")
			volumeTarFileFullPath := filepath.Join(c.bundlePath(), volumeTarFilePath)

			volumeTarFile, err := os.Create(volumeTarFileFullPath)
			if err != nil {
				return errors.Wrapf(err, "error creating %q", volumeTarFileFullPath)
			}

			volume, err := c.runtime.GetVolume(v.Name)
			if err != nil {
				return err
			}

			mp, err := volume.MountPoint()
			if err != nil {
				return err
			}
			if mp == "" {
				return errors.Wrapf(define.ErrInternal, "volume %s is not mounted, cannot export", volume.Name())
			}

			input, err := archive.TarWithOptions(mp, &archive.TarOptions{
				Compression:      archive.Uncompressed,
				IncludeSourceDir: true,
			})
			if err != nil {
				return errors.Wrapf(err, "error reading volume directory %q", v.Dest)
			}

			_, err = io.Copy(volumeTarFile, input)
			if err != nil {
				return err
			}
			volumeTarFile.Close()

			includeFiles = append(includeFiles, volumeTarFilePath)
		}
	}

	input, err := archive.TarWithOptions(c.bundlePath(), &archive.TarOptions{
		Compression:      options.Compression,
		IncludeSourceDir: true,
		IncludeFiles:     includeFiles,
	})

	if err != nil {
		return errors.Wrapf(err, "error reading checkpoint directory %q", c.ID())
	}

	outFile, err := os.Create(options.TargetFile)
	if err != nil {
		return errors.Wrapf(err, "error creating checkpoint export file %q", options.TargetFile)
	}
	defer outFile.Close()

	if err := os.Chmod(options.TargetFile, 0600); err != nil {
		return err
	}

	_, err = io.Copy(outFile, input)
	if err != nil {
		return err
	}

	for _, file := range addToTarFiles {
		os.Remove(filepath.Join(c.bundlePath(), file))
	}

	if !options.IgnoreVolumes {
		os.RemoveAll(expVolDir)
	}

	return nil
}

func (c *Container) checkpointRestoreSupported(version int) error {
	if !criu.CheckForCriu(version) {
		return errors.Errorf("checkpoint/restore requires at least CRIU %d", version)
	}
	if !c.ociRuntime.SupportsCheckpoint() {
		return errors.Errorf("configured runtime does not support checkpoint/restore")
	}
	return nil
}

func (c *Container) checkpoint(ctx context.Context, options ContainerCheckpointOptions) (*define.CRIUCheckpointRestoreStatistics, int64, error) {
	if err := c.checkpointRestoreSupported(criu.MinCriuVersion); err != nil {
		return nil, 0, err
	}

	if c.state.State != define.ContainerStateRunning {
		return nil, 0, errors.Wrapf(define.ErrCtrStateInvalid, "%q is not running, cannot checkpoint", c.state.State)
	}

	if c.AutoRemove() && options.TargetFile == "" {
		return nil, 0, errors.Errorf("cannot checkpoint containers that have been started with '--rm' unless '--export' is used")
	}

	if err := crutils.CRCreateFileWithLabel(c.bundlePath(), "dump.log", c.MountLabel()); err != nil {
		return nil, 0, err
	}

	// Setting CheckpointLog early in case there is a failure.
	c.state.CheckpointLog = path.Join(c.bundlePath(), "dump.log")
	c.state.CheckpointPath = c.CheckpointPath()

	runtimeCheckpointDuration, err := c.ociRuntime.CheckpointContainer(c, options)
	if err != nil {
		return nil, 0, err
	}

	// Save network.status. This is needed to restore the container with
	// the same IP. Currently limited to one IP address in a container
	// with one interface.
	// FIXME: will this break something?
	if _, err := metadata.WriteJSONFile(c.getNetworkStatus(), c.bundlePath(), metadata.NetworkStatusFile); err != nil {
		return nil, 0, err
	}

	defer c.newContainerEvent(events.Checkpoint)

	// There is a bug from criu: https://github.com/checkpoint-restore/criu/issues/116
	// We have to change the symbolic link from absolute path to relative path
	if options.WithPrevious {
		os.Remove(path.Join(c.CheckpointPath(), "parent"))
		if err := os.Symlink("../pre-checkpoint", path.Join(c.CheckpointPath(), "parent")); err != nil {
			return nil, 0, err
		}
	}

	if options.TargetFile != "" {
		if err := c.exportCheckpoint(options); err != nil {
			return nil, 0, err
		}
	}

	logrus.Debugf("Checkpointed container %s", c.ID())

	if !options.KeepRunning && !options.PreCheckPoint {
		c.state.State = define.ContainerStateStopped
		c.state.Checkpointed = true
		c.state.CheckpointedTime = time.Now()
		c.state.Restored = false
		c.state.RestoredTime = time.Time{}

		// Cleanup Storage and Network
		if err := c.cleanup(ctx); err != nil {
			return nil, 0, err
		}
	}

	criuStatistics, err := func() (*define.CRIUCheckpointRestoreStatistics, error) {
		if !options.PrintStats {
			return nil, nil
		}
		statsDirectory, err := os.Open(c.bundlePath())
		if err != nil {
			return nil, errors.Wrapf(err, "Not able to open %q", c.bundlePath())
		}

		dumpStatistics, err := stats.CriuGetDumpStats(statsDirectory)
		if err != nil {
			return nil, errors.Wrap(err, "Displaying checkpointing statistics not possible")
		}

		return &define.CRIUCheckpointRestoreStatistics{
			FreezingTime: dumpStatistics.GetFreezingTime(),
			FrozenTime:   dumpStatistics.GetFrozenTime(),
			MemdumpTime:  dumpStatistics.GetMemdumpTime(),
			MemwriteTime: dumpStatistics.GetMemwriteTime(),
			PagesScanned: dumpStatistics.GetPagesScanned(),
			PagesWritten: dumpStatistics.GetPagesWritten(),
		}, nil
	}()
	if err != nil {
		return nil, 0, err
	}

	if !options.Keep && !options.PreCheckPoint {
		cleanup := []string{
			"dump.log",
			stats.StatsDump,
			metadata.ConfigDumpFile,
			metadata.SpecDumpFile,
		}
		for _, del := range cleanup {
			file := filepath.Join(c.bundlePath(), del)
			if err := os.Remove(file); err != nil {
				logrus.Debugf("Unable to remove file %s", file)
			}
		}
		// The file has been deleted. Do not mention it.
		c.state.CheckpointLog = ""
	}

	c.state.FinishedTime = time.Now()
	return criuStatistics, runtimeCheckpointDuration, c.save()
}

func (c *Container) importCheckpoint(input string) error {
	if err := crutils.CRImportCheckpointWithoutConfig(c.bundlePath(), input); err != nil {
		return err
	}

	// Make sure the newly created config.json exists on disk
	g := generate.NewFromSpec(c.config.Spec)

	if err := c.saveSpec(g.Config); err != nil {
		return errors.Wrap(err, "saving imported container specification for restore failed")
	}

	return nil
}

func (c *Container) importPreCheckpoint(input string) error {
	archiveFile, err := os.Open(input)
	if err != nil {
		return errors.Wrap(err, "failed to open pre-checkpoint archive for import")
	}

	defer archiveFile.Close()

	err = archive.Untar(archiveFile, c.bundlePath(), nil)
	if err != nil {
		return errors.Wrapf(err, "Unpacking of pre-checkpoint archive %s failed", input)
	}
	return nil
}

func (c *Container) restore(ctx context.Context, options ContainerCheckpointOptions) (criuStatistics *define.CRIUCheckpointRestoreStatistics, runtimeRestoreDuration int64, retErr error) {
	minCriuVersion := func() int {
		if options.Pod == "" {
			return criu.MinCriuVersion
		}
		return criu.PodCriuVersion
	}()
	if err := c.checkpointRestoreSupported(minCriuVersion); err != nil {
		return nil, 0, err
	}

	if options.Pod != "" && !crutils.CRRuntimeSupportsPodCheckpointRestore(c.ociRuntime.Path()) {
		return nil, 0, errors.Errorf("runtime %s does not support pod restore", c.ociRuntime.Path())
	}

	if !c.ensureState(define.ContainerStateConfigured, define.ContainerStateExited) {
		return nil, 0, errors.Wrapf(define.ErrCtrStateInvalid, "container %s is running or paused, cannot restore", c.ID())
	}

	if options.ImportPrevious != "" {
		if err := c.importPreCheckpoint(options.ImportPrevious); err != nil {
			return nil, 0, err
		}
	}

	if options.TargetFile != "" {
		if err := c.importCheckpoint(options.TargetFile); err != nil {
			return nil, 0, err
		}
	}

	// Let's try to stat() CRIU's inventory file. If it does not exist, it makes
	// no sense to try a restore. This is a minimal check if a checkpoint exist.
	if _, err := os.Stat(filepath.Join(c.CheckpointPath(), "inventory.img")); os.IsNotExist(err) {
		return nil, 0, errors.Wrapf(err, "a complete checkpoint for this container cannot be found, cannot restore")
	}

	if err := crutils.CRCreateFileWithLabel(c.bundlePath(), "restore.log", c.MountLabel()); err != nil {
		return nil, 0, err
	}

	// Setting RestoreLog early in case there is a failure.
	c.state.RestoreLog = path.Join(c.bundlePath(), "restore.log")
	c.state.CheckpointPath = c.CheckpointPath()

	// Read network configuration from checkpoint
	var netStatus map[string]types.StatusBlock
	_, err := metadata.ReadJSONFile(&netStatus, c.bundlePath(), metadata.NetworkStatusFile)
	if err != nil {
		logrus.Infof("Failed to unmarshal network status, cannot restore the same ip/mac: %v", err)
	}
	// If the restored container should get a new name, the IP address of
	// the container will not be restored. This assumes that if a new name is
	// specified, the container is restored multiple times.
	// TODO: This implicit restoring with or without IP depending on an
	//       unrelated restore parameter (--name) does not seem like the
	//       best solution.
	if err == nil && options.Name == "" && (!options.IgnoreStaticIP || !options.IgnoreStaticMAC) {
		// The file with the network.status does exist. Let's restore the
		// container with the same networks settings as during checkpointing.
		networkOpts, err := c.networks()
		if err != nil {
			return nil, 0, err
		}

		netOpts := make(map[string]types.PerNetworkOptions, len(netStatus))
		for network, perNetOpts := range networkOpts {
			// unset mac and ips before we start adding the ones from the status
			perNetOpts.StaticMAC = nil
			perNetOpts.StaticIPs = nil
			for name, netInt := range netStatus[network].Interfaces {
				perNetOpts.InterfaceName = name
				if !options.IgnoreStaticIP {
					perNetOpts.StaticMAC = netInt.MacAddress
				}
				if !options.IgnoreStaticIP {
					for _, netAddress := range netInt.Subnets {
						perNetOpts.StaticIPs = append(perNetOpts.StaticIPs, netAddress.IPNet.IP)
					}
				}
				// Normally interfaces have a length of 1, only for some special cni configs we could get more.
				// For now just use the first interface to get the ips this should be good enough for most cases.
				break
			}
			netOpts[network] = perNetOpts
		}
		c.perNetworkOpts = netOpts
	}

	defer func() {
		if retErr != nil {
			if err := c.cleanup(ctx); err != nil {
				logrus.Errorf("Cleaning up container %s: %v", c.ID(), err)
			}
		}
	}()

	if err := c.prepare(); err != nil {
		return nil, 0, err
	}

	// Read config
	jsonPath := filepath.Join(c.bundlePath(), "config.json")
	logrus.Debugf("generate.NewFromFile at %v", jsonPath)
	g, err := generate.NewFromFile(jsonPath)
	if err != nil {
		logrus.Debugf("generate.NewFromFile failed with %v", err)
		return nil, 0, err
	}

	// Restoring from an import means that we are doing migration
	if options.TargetFile != "" {
		g.SetRootPath(c.state.Mountpoint)
	}

	if options.Pod != "" {
		// Running in a Pod means that we have to change all namespace settings to
		// the ones from the infrastructure container.
		pod, err := c.runtime.LookupPod(options.Pod)
		if err != nil {
			return nil, 0, errors.Wrapf(err, "pod %q cannot be retrieved", options.Pod)
		}

		infraContainer, err := pod.InfraContainer()
		if err != nil {
			return nil, 0, errors.Wrapf(err, "cannot retrieved infra container from pod %q", options.Pod)
		}

		infraContainer.lock.Lock()
		if err := infraContainer.syncContainer(); err != nil {
			infraContainer.lock.Unlock()
			return nil, 0, errors.Wrapf(err, "Error syncing infrastructure container %s status", infraContainer.ID())
		}
		if infraContainer.state.State != define.ContainerStateRunning {
			if err := infraContainer.initAndStart(ctx); err != nil {
				infraContainer.lock.Unlock()
				return nil, 0, errors.Wrapf(err, "Error starting infrastructure container %s status", infraContainer.ID())
			}
		}
		infraContainer.lock.Unlock()
		g.AddAnnotation("org.freebsd.parentJail", infraContainer.state.NetworkJail)
	}

	if err := c.makeBindMounts(); err != nil {
		return nil, 0, err
	}

	if options.TargetFile != "" {
		for dstPath, srcPath := range c.state.BindMounts {
			newMount := spec.Mount{
				Type:        "nullfs",
				Source:      srcPath,
				Destination: dstPath,
				Options:     []string{"bind", "private"},
			}
			if !MountExists(g.Mounts(), dstPath) {
				g.AddMount(newMount)
			}
		}
	}

	// Cleanup for a working restore.
	if err := c.removeConmonFiles(); err != nil {
		return nil, 0, err
	}

	// Save the OCI spec to disk
	if err := c.saveSpec(g.Config); err != nil {
		return nil, 0, err
	}

	// When restoring from an imported archive, allow restoring the content of volumes.
	// Volumes are created in setupContainer()
	if options.TargetFile != "" && !options.IgnoreVolumes {
		for _, v := range c.config.NamedVolumes {
			volumeFilePath := filepath.Join(c.bundlePath(), "volumes", v.Name+".tar")

			volumeFile, err := os.Open(volumeFilePath)
			if err != nil {
				return nil, 0, errors.Wrapf(err, "failed to open volume file %s", volumeFilePath)
			}
			defer volumeFile.Close()

			volume, err := c.runtime.GetVolume(v.Name)
			if err != nil {
				return nil, 0, errors.Wrapf(err, "failed to retrieve volume %s", v.Name)
			}

			mountPoint, err := volume.MountPoint()
			if err != nil {
				return nil, 0, err
			}
			if mountPoint == "" {
				return nil, 0, errors.Wrapf(err, "unable to import volume %s as it is not mounted", volume.Name())
			}
			if err := archive.UntarUncompressed(volumeFile, mountPoint, nil); err != nil {
				return nil, 0, errors.Wrapf(err, "Failed to extract volume %s to %s", volumeFilePath, mountPoint)
			}
		}
	}

	// Before actually restarting the container, apply the root file-system changes
	if !options.IgnoreRootfs {
		if err := crutils.CRApplyRootFsDiffTar(c.bundlePath(), c.state.Mountpoint); err != nil {
			return nil, 0, err
		}

		if err := crutils.CRRemoveDeletedFiles(c.ID(), c.bundlePath(), c.state.Mountpoint); err != nil {
			return nil, 0, err
		}
	}

	runtimeRestoreDuration, err = c.ociRuntime.CreateContainer(c, &options)
	if err != nil {
		return nil, 0, err
	}

	criuStatistics, err = func() (*define.CRIUCheckpointRestoreStatistics, error) {
		if !options.PrintStats {
			return nil, nil
		}
		statsDirectory, err := os.Open(c.bundlePath())
		if err != nil {
			return nil, errors.Wrapf(err, "Not able to open %q", c.bundlePath())
		}

		restoreStatistics, err := stats.CriuGetRestoreStats(statsDirectory)
		if err != nil {
			return nil, errors.Wrap(err, "Displaying restore statistics not possible")
		}

		return &define.CRIUCheckpointRestoreStatistics{
			PagesCompared:   restoreStatistics.GetPagesCompared(),
			PagesSkippedCow: restoreStatistics.GetPagesSkippedCow(),
			ForkingTime:     restoreStatistics.GetForkingTime(),
			RestoreTime:     restoreStatistics.GetRestoreTime(),
			PagesRestored:   restoreStatistics.GetPagesRestored(),
		}, nil
	}()
	if err != nil {
		return nil, 0, err
	}

	logrus.Debugf("Restored container %s", c.ID())

	c.state.State = define.ContainerStateRunning
	c.state.Checkpointed = false
	c.state.Restored = true
	c.state.CheckpointedTime = time.Time{}
	c.state.RestoredTime = time.Now()

	if !options.Keep {
		// Delete all checkpoint related files. At this point, in theory, all files
		// should exist. Still ignoring errors for now as the container should be
		// restored and running. Not erroring out just because some cleanup operation
		// failed. Starting with the checkpoint directory
		err = os.RemoveAll(c.CheckpointPath())
		if err != nil {
			logrus.Debugf("Non-fatal: removal of checkpoint directory (%s) failed: %v", c.CheckpointPath(), err)
		}
		c.state.CheckpointPath = ""
		err = os.RemoveAll(c.PreCheckPointPath())
		if err != nil {
			logrus.Debugf("Non-fatal: removal of pre-checkpoint directory (%s) failed: %v", c.PreCheckPointPath(), err)
		}
		cleanup := [...]string{
			"restore.log",
			"dump.log",
			stats.StatsDump,
			stats.StatsRestore,
			metadata.NetworkStatusFile,
			metadata.RootFsDiffTar,
			metadata.DeletedFilesFile,
		}
		for _, del := range cleanup {
			file := filepath.Join(c.bundlePath(), del)
			err = os.Remove(file)
			if err != nil {
				logrus.Debugf("Non-fatal: removal of checkpoint file (%s) failed: %v", file, err)
			}
		}
		c.state.CheckpointLog = ""
		c.state.RestoreLog = ""
	}

	return criuStatistics, runtimeRestoreDuration, c.save()
}

// Retrieves a container's "root" net namespace container dependency.
func (c *Container) getRootNetNsDepCtr() (depCtr *Container, err error) {
	containersVisited := map[string]int{c.config.ID: 1}
	nextCtr := c.config.NetNsCtr
	for nextCtr != "" {
		// Make sure we aren't in a loop
		if _, visited := containersVisited[nextCtr]; visited {
			return nil, errors.New("loop encountered while determining net namespace container")
		}
		containersVisited[nextCtr] = 1

		depCtr, err = c.runtime.state.Container(nextCtr)
		if err != nil {
			return nil, errors.Wrapf(err, "error fetching dependency %s of container %s", c.config.NetNsCtr, c.ID())
		}
		// This should never happen without an error
		if depCtr == nil {
			break
		}
		nextCtr = depCtr.config.NetNsCtr
	}

	if depCtr == nil {
		return nil, errors.New("unexpected error depCtr is nil without reported error from runtime state")
	}
	return depCtr, nil
}

// Ensure standard bind mounts are mounted into all root directories (including chroot directories)
func (c *Container) mountIntoRootDirs(mountName string, mountPath string) error {
	c.state.BindMounts[mountName] = mountPath

	for _, chrootDir := range c.config.ChrootDirs {
		c.state.BindMounts[filepath.Join(chrootDir, mountName)] = mountPath
		logrus.Debugf("bind chrootDir=%s, mountName=%s", chrootDir, mountName)
	}
	// TODO: copy the file

	return nil
}

// Make standard bind mounts to include in the container
func (c *Container) makeBindMounts() error {
	if err := os.Chown(c.state.RunDir, c.RootUID(), c.RootGID()); err != nil {
		return errors.Wrap(err, "cannot chown run directory")
	}

	if c.state.BindMounts == nil {
		c.state.BindMounts = make(map[string]string)
	}
	netDisabled, err := c.NetworkDisabled()
	if err != nil {
		return err
	}

	if !netDisabled {
		// If /etc/resolv.conf and /etc/hosts exist, delete them so we
		// will recreate. Only do this if we aren't sharing them with
		// another container.
		if c.config.NetNsCtr == "" {
			if resolvePath, ok := c.state.BindMounts["/etc/resolv.conf"]; ok {
				if err := os.Remove(resolvePath); err != nil && !os.IsNotExist(err) {
					return errors.Wrapf(err, "container %s", c.ID())
				}
				delete(c.state.BindMounts, "/etc/resolv.conf")
			}
			if hostsPath, ok := c.state.BindMounts["/etc/hosts"]; ok {
				if err := os.Remove(hostsPath); err != nil && !os.IsNotExist(err) {
					return errors.Wrapf(err, "container %s", c.ID())
				}
				delete(c.state.BindMounts, "/etc/hosts")
			}
		}

		if c.config.NetNsCtr != "" && (!c.config.UseImageResolvConf || !c.config.UseImageHosts) {
			// We share a net namespace.
			// We want /etc/resolv.conf and /etc/hosts from the
			// other container. Unless we're not creating both of
			// them.
			depCtr, err := c.getRootNetNsDepCtr()
			if err != nil {
				return errors.Wrapf(err, "error fetching network namespace dependency container for container %s", c.ID())
			}

			// We need that container's bind mounts
			bindMounts, err := depCtr.BindMounts()
			if err != nil {
				return errors.Wrapf(err, "error fetching bind mounts from dependency %s of container %s", depCtr.ID(), c.ID())
			}

			// The other container may not have a resolv.conf or /etc/hosts
			// If it doesn't, don't copy them
			resolvPath, exists := bindMounts["/etc/resolv.conf"]
			if !c.config.UseImageResolvConf && exists {
				err := c.mountIntoRootDirs("/etc/resolv.conf", resolvPath)

				if err != nil {
					return errors.Wrapf(err, "error assigning mounts to container %s", c.ID())
				}
			}

			// check if dependency container has an /etc/hosts file.
			// It may not have one, so only use it if it does.
			hostsPath, exists := bindMounts[config.DefaultHostsFile]
			if !c.config.UseImageHosts && exists {
				// we cannot use the dependency container lock due ABBA deadlocks in cleanup()
				lock, err := lockfile.GetLockfile(hostsPath)
				if err != nil {
					return fmt.Errorf("failed to lock hosts file: %w", err)
				}
				lock.Lock()

				// add the newly added container to the hosts file
				// we always use 127.0.0.1 as ip since they have the same netns
				err = etchosts.Add(hostsPath, getLocalhostHostEntry(c))
				lock.Unlock()
				if err != nil {
					return errors.Wrapf(err, "error creating hosts file for container %s which depends on container %s", c.ID(), depCtr.ID())
				}

				// finally, save it in the new container
				err = c.mountIntoRootDirs(config.DefaultHostsFile, hostsPath)
				if err != nil {
					return errors.Wrapf(err, "error assigning mounts to container %s", c.ID())
				}
			}

			if !hasCurrentUserMapped(c) {
				if err := makeAccessible(resolvPath, c.RootUID(), c.RootGID()); err != nil {
					return err
				}
				if err := makeAccessible(hostsPath, c.RootUID(), c.RootGID()); err != nil {
					return err
				}
			}
		} else {
			if !c.config.UseImageResolvConf {
				if err := c.generateResolvConf(); err != nil {
					return errors.Wrapf(err, "error creating resolv.conf for container %s", c.ID())
				}
			}

			if !c.config.UseImageHosts {
				if err := c.createHosts(); err != nil {
					return errors.Wrapf(err, "error creating hosts file for container %s", c.ID())
				}
			}
		}

		if c.state.BindMounts["/etc/hosts"] != "" {
			if err := c.relabel(c.state.BindMounts["/etc/hosts"], c.config.MountLabel, true); err != nil {
				return err
			}
		}

		if c.state.BindMounts["/etc/resolv.conf"] != "" {
			if err := c.relabel(c.state.BindMounts["/etc/resolv.conf"], c.config.MountLabel, true); err != nil {
				return err
			}
		}
	} else {
		if err := c.createHosts(); err != nil {
			return errors.Wrapf(err, "error creating hosts file for container %s", c.ID())
		}
	}

	if c.config.Passwd == nil || *c.config.Passwd {
		newPasswd, newGroup, err := c.generatePasswdAndGroup()
		if err != nil {
			return errors.Wrapf(err, "error creating temporary passwd file for container %s", c.ID())
		}
		if newPasswd != "" {
			// Make /etc/passwd
			// If it already exists, delete so we can recreate
			delete(c.state.BindMounts, "/etc/passwd")
			c.state.BindMounts["/etc/passwd"] = newPasswd
		}
		if newGroup != "" {
			// Make /etc/group
			// If it already exists, delete so we can recreate
			delete(c.state.BindMounts, "/etc/group")
			c.state.BindMounts["/etc/group"] = newGroup
		}
	}

	// Make /etc/localtime
	ctrTimezone := c.Timezone()
	if ctrTimezone != "" {
		// validate the format of the timezone specified if it's not "local"
		if ctrTimezone != "local" {
			_, err = time.LoadLocation(ctrTimezone)
			if err != nil {
				return errors.Wrapf(err, "error finding timezone for container %s", c.ID())
			}
		}
		if _, ok := c.state.BindMounts["/etc/localtime"]; !ok {
			var zonePath string
			if ctrTimezone == "local" {
				zonePath, err = filepath.EvalSymlinks("/etc/localtime")
				if err != nil {
					return errors.Wrapf(err, "error finding local timezone for container %s", c.ID())
				}
			} else {
				zone := filepath.Join("/usr/share/zoneinfo", ctrTimezone)
				zonePath, err = filepath.EvalSymlinks(zone)
				if err != nil {
					return errors.Wrapf(err, "error setting timezone for container %s", c.ID())
				}
			}
			localtimePath, err := c.copyTimezoneFile(zonePath)
			if err != nil {
				return errors.Wrapf(err, "error setting timezone for container %s", c.ID())
			}
			c.state.BindMounts["/etc/localtime"] = localtimePath
		}
	}

	// Make .containerenv if it does not exist
	if _, ok := c.state.BindMounts["/run/.containerenv"]; !ok {
		containerenv := c.runtime.graphRootMountedFlag(c.config.Spec.Mounts)
		isRootless := 0
		if rootless.IsRootless() {
			isRootless = 1
		}
		imageID, imageName := c.Image()

		if c.Privileged() {
			// Populate the .containerenv with container information
			containerenv = fmt.Sprintf(`engine="podman-%s"
name=%q
id=%q
image=%q
imageid=%q
rootless=%d
%s`, version.Version.String(), c.Name(), c.ID(), imageName, imageID, isRootless, containerenv)
		}
		containerenvPath, err := c.writeStringToRundir(".containerenv", containerenv)
		if err != nil {
			return errors.Wrapf(err, "error creating containerenv file for container %s", c.ID())
		}
		c.state.BindMounts["/run/.containerenv"] = containerenvPath
	}

	// Add Subscription Mounts
	subscriptionMounts := subscriptions.MountsWithUIDGID(c.config.MountLabel, c.state.RunDir, c.runtime.config.Containers.DefaultMountsFile, c.state.Mountpoint, c.RootUID(), c.RootGID(), rootless.IsRootless(), false)
	for _, mount := range subscriptionMounts {
		if _, ok := c.state.BindMounts[mount.Destination]; !ok {
			c.state.BindMounts[mount.Destination] = mount.Source
		}
	}

	// Secrets are mounted by getting the secret data from the secrets manager,
	// copying the data into the container's static dir,
	// then mounting the copied dir into /run/secrets.
	// The secrets mounting must come after subscription mounts, since subscription mounts
	// creates the /run/secrets dir in the container where we mount as well.
	if len(c.Secrets()) > 0 {
		// create /run/secrets if subscriptions did not create
		if err := c.createSecretMountDir(); err != nil {
			return errors.Wrapf(err, "error creating secrets mount")
		}
		for _, secret := range c.Secrets() {
			secretFileName := secret.Name
			base := "/run/secrets"
			if secret.Target != "" {
				secretFileName = secret.Target
				//If absolute path for target given remove base.
				if filepath.IsAbs(secretFileName) {
					base = ""
				}
			}
			src := filepath.Join(c.config.SecretsPath, secret.Name)
			dest := filepath.Join(base, secretFileName)
			c.state.BindMounts[dest] = src
		}
	}

	return nil
}

// generateResolvConf generates a containers resolv.conf
func (c *Container) generateResolvConf() error {
	var (
		networkNameServers   []string
		networkSearchDomains []string
	)

	netStatus := c.getNetworkStatus()
	for _, status := range netStatus {
		if status.DNSServerIPs != nil {
			for _, nsIP := range status.DNSServerIPs {
				networkNameServers = append(networkNameServers, nsIP.String())
			}
			logrus.Debugf("Adding nameserver(s) from network status of '%q'", status.DNSServerIPs)
		}
		if status.DNSSearchDomains != nil {
			networkSearchDomains = append(networkSearchDomains, status.DNSSearchDomains...)
			logrus.Debugf("Adding search domain(s) from network status of '%q'", status.DNSSearchDomains)
		}
	}

	ipv6, err := c.checkForIPv6(netStatus)
	if err != nil {
		return err
	}

	nameservers := make([]string, 0, len(c.runtime.config.Containers.DNSServers)+len(c.config.DNSServer))
	nameservers = append(nameservers, c.runtime.config.Containers.DNSServers...)
	for _, ip := range c.config.DNSServer {
		nameservers = append(nameservers, ip.String())
	}
	// If the user provided dns, it trumps all; then dns masq; then resolv.conf
	var search []string
	keepHostServers := false
	if len(nameservers) == 0 {
		keepHostServers = true
		// first add the nameservers from the networks status
		nameservers = networkNameServers
		// when we add network dns server we also have to add the search domains
		search = networkSearchDomains
	}

	if len(c.config.DNSSearch) > 0 || len(c.runtime.config.Containers.DNSSearches) > 0 {
		customSearch := make([]string, 0, len(c.config.DNSSearch)+len(c.runtime.config.Containers.DNSSearches))
		customSearch = append(customSearch, c.runtime.config.Containers.DNSSearches...)
		customSearch = append(customSearch, c.config.DNSSearch...)
		search = customSearch
	}

	options := make([]string, 0, len(c.config.DNSOption)+len(c.runtime.config.Containers.DNSOptions))
	options = append(options, c.runtime.config.Containers.DNSOptions...)
	options = append(options, c.config.DNSOption...)

	destPath := filepath.Join(c.state.RunDir, "resolv.conf")

	if err := resolvconf.New(&resolvconf.Params{
		IPv6Enabled:     ipv6,
		KeepHostServers: keepHostServers,
		Nameservers:     nameservers,
		Options:         options,
		Path:            destPath,
		Searches:        search,
	}); err != nil {
		return errors.Wrapf(err, "error building resolv.conf for container %s", c.ID())
	}

	return c.bindMountRootFile(destPath, resolvconf.DefaultResolvConf)
}

// Check if a container uses IPv6.
func (c *Container) checkForIPv6(netStatus map[string]types.StatusBlock) (bool, error) {
	for _, status := range netStatus {
		for _, netInt := range status.Interfaces {
			for _, netAddress := range netInt.Subnets {
				// Note: only using To16() does not work since it also returns a valid ip for ipv4
				if netAddress.IPNet.IP.To4() == nil && netAddress.IPNet.IP.To16() != nil {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// Add a new nameserver to the container's resolv.conf, ensuring that it is the
// first nameserver present.
// Usable only with running containers.
func (c *Container) addNameserver(ips []string) error {
	// Take no action if container is not running.
	if !c.ensureState(define.ContainerStateRunning, define.ContainerStateCreated) {
		return nil
	}

	// Do we have a resolv.conf at all?
	path, ok := c.state.BindMounts[resolvconf.DefaultResolvConf]
	if !ok {
		return nil
	}

	if err := resolvconf.Add(path, ips); err != nil {
		return fmt.Errorf("adding new nameserver to container %s resolv.conf: %w", c.ID(), err)
	}

	return nil
}

// Remove an entry from the existing resolv.conf of the container.
// Usable only with running containers.
func (c *Container) removeNameserver(ips []string) error {
	// Take no action if container is not running.
	if !c.ensureState(define.ContainerStateRunning, define.ContainerStateCreated) {
		return nil
	}

	// Do we have a resolv.conf at all?
	path, ok := c.state.BindMounts[resolvconf.DefaultResolvConf]
	if !ok {
		return nil
	}

	if err := resolvconf.Remove(path, ips); err != nil {
		return fmt.Errorf("removing nameservers from container %s resolv.conf: %w", c.ID(), err)
	}

	return nil
}

func getLocalhostHostEntry(c *Container) etchosts.HostEntries {
	return etchosts.HostEntries{{IP: "127.0.0.1", Names: []string{c.Hostname(), c.config.Name}}}
}

// getHostsEntries returns the container ip host entries for the correct netmode
func (c *Container) getHostsEntries() (etchosts.HostEntries, error) {
	var entries etchosts.HostEntries
	names := []string{c.Hostname(), c.config.Name}
	switch {
	case c.config.NetMode.IsBridge():
		entries = etchosts.GetNetworkHostEntries(c.state.NetworkStatus, names...)
	default:
		// check for net=none
		/*if !c.config.CreateNetNS {
			for _, ns := range c.config.Spec.Linux.Namespaces {
				if ns.Type == spec.NetworkNamespace {
					if ns.Path == "" {
						entries = etchosts.HostEntries{{IP: "127.0.0.1", Names: names}}
					}
					break
				}
			}
		}*/
	}
	return entries, nil
}

func (c *Container) createHosts() error {
	var containerIPsEntries etchosts.HostEntries
	var err error
	// if we configure the netns after the container create we should not add
	// the hosts here since we have no information about the actual ips
	// instead we will add them in c.completeNetworkSetup()
	if !c.config.PostConfigureNetNS {
		containerIPsEntries, err = c.getHostsEntries()
		if err != nil {
			return fmt.Errorf("failed to get container ip host entries: %w", err)
		}
	}
	baseHostFile, err := etchosts.GetBaseHostFile(c.runtime.config.Containers.BaseHostsFile, c.state.Mountpoint)
	if err != nil {
		return err
	}

	targetFile := filepath.Join(c.state.RunDir, "hosts")
	err = etchosts.New(&etchosts.Params{
		BaseFile:                 baseHostFile,
		ExtraHosts:               c.config.HostAdd,
		ContainerIPs:             containerIPsEntries,
		HostContainersInternalIP: etchosts.GetHostContainersInternalIP(c.runtime.config, c.state.NetworkStatus, c.runtime.network),
		TargetFile:               targetFile,
	})
	if err != nil {
		return err
	}

	return c.bindMountRootFile(targetFile, config.DefaultHostsFile)
}

// bindMountRootFile will chown and relabel the source file to make it usable in the container.
// It will also add the path to the container bind mount map.
// source is the path on the host, dest is the path in the container.
func (c *Container) bindMountRootFile(source, dest string) error {
	if err := os.Chown(source, c.RootUID(), c.RootGID()); err != nil {
		return err
	}
	if err := label.Relabel(source, c.MountLabel(), false); err != nil {
		return err
	}

	return c.mountIntoRootDirs(dest, source)
}

// generateGroupEntry generates an entry or entries into /etc/group as
// required by container configuration.
// Generally speaking, we will make an entry under two circumstances:
// 1. The container is started as a specific user:group, and that group is both
//    numeric, and does not already exist in /etc/group.
// 2. It is requested that Libpod add the group that launched Podman to
//    /etc/group via AddCurrentUserPasswdEntry (though this does not trigger if
//    the group in question already exists in /etc/passwd).
// Returns group entry (as a string that can be appended to /etc/group) and any
// error that occurred.
func (c *Container) generateGroupEntry() (string, error) {
	groupString := ""

	// Things we *can't* handle: adding the user we added in
	// generatePasswdEntry to any *existing* groups.
	addedGID := 0
	if c.config.AddCurrentUserPasswdEntry {
		entry, gid, err := c.generateCurrentUserGroupEntry()
		if err != nil {
			return "", err
		}
		groupString += entry
		addedGID = gid
	}
	if c.config.User != "" {
		entry, _, err := c.generateUserGroupEntry(addedGID)
		if err != nil {
			return "", err
		}
		groupString += entry
	}

	return groupString, nil
}

// Make an entry in /etc/group for the group of the user running podman iff we
// are rootless.
func (c *Container) generateCurrentUserGroupEntry() (string, int, error) {
	gid := rootless.GetRootlessGID()
	if gid == 0 {
		return "", 0, nil
	}

	g, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return "", 0, errors.Wrapf(err, "failed to get current group")
	}

	// Lookup group name to see if it exists in the image.
	_, err = lookup.GetGroup(c.state.Mountpoint, g.Name)
	if err != runcuser.ErrNoGroupEntries {
		return "", 0, err
	}

	// Lookup GID to see if it exists in the image.
	_, err = lookup.GetGroup(c.state.Mountpoint, g.Gid)
	if err != runcuser.ErrNoGroupEntries {
		return "", 0, err
	}

	// We need to get the username of the rootless user so we can add it to
	// the group.
	username := ""
	uid := rootless.GetRootlessUID()
	if uid != 0 {
		u, err := user.LookupId(strconv.Itoa(uid))
		if err != nil {
			return "", 0, errors.Wrapf(err, "failed to get current user to make group entry")
		}
		username = u.Username
	}

	// Make the entry.
	return fmt.Sprintf("%s:x:%s:%s\n", g.Name, g.Gid, username), gid, nil
}

// Make an entry in /etc/group for the group the container was specified to run
// as.
func (c *Container) generateUserGroupEntry(addedGID int) (string, int, error) {
	if c.config.User == "" {
		return "", 0, nil
	}

	splitUser := strings.SplitN(c.config.User, ":", 2)
	group := splitUser[0]
	if len(splitUser) > 1 {
		group = splitUser[1]
	}

	gid, err := strconv.ParseUint(group, 10, 32)
	if err != nil {
		return "", 0, nil // nolint: nilerr
	}

	if addedGID != 0 && addedGID == int(gid) {
		return "", 0, nil
	}

	// Check if the group already exists
	_, err = lookup.GetGroup(c.state.Mountpoint, group)
	if err != runcuser.ErrNoGroupEntries {
		return "", 0, err
	}

	return fmt.Sprintf("%d:x:%d:%s\n", gid, gid, splitUser[0]), int(gid), nil
}

// generatePasswdEntry generates an entry or entries into /etc/passwd as
// required by container configuration.
// Generally speaking, we will make an entry under two circumstances:
// 1. The container is started as a specific user who is not in /etc/passwd.
//    This only triggers if the user is given as a *numeric* ID.
// 2. It is requested that Libpod add the user that launched Podman to
//    /etc/passwd via AddCurrentUserPasswdEntry (though this does not trigger if
//    the user in question already exists in /etc/passwd) or the UID to be added
//    is 0).
// 3. The user specified additional host user accounts to add the the /etc/passwd file
// Returns password entry (as a string that can be appended to /etc/passwd) and
// any error that occurred.
func (c *Container) generatePasswdEntry() (string, error) {
	passwdString := ""

	addedUID := 0
	for _, userid := range c.config.HostUsers {
		// Lookup User on host
		u, err := util.LookupUser(userid)
		if err != nil {
			return "", err
		}
		entry, err := c.userPasswdEntry(u)
		if err != nil {
			return "", err
		}
		passwdString += entry
	}
	if c.config.AddCurrentUserPasswdEntry {
		entry, uid, _, err := c.generateCurrentUserPasswdEntry()
		if err != nil {
			return "", err
		}
		passwdString += entry
		addedUID = uid
	}
	if c.config.User != "" {
		entry, _, _, err := c.generateUserPasswdEntry(addedUID)
		if err != nil {
			return "", err
		}
		passwdString += entry
	}

	return passwdString, nil
}

// generateCurrentUserPasswdEntry generates an /etc/passwd entry for the user
// running the container engine.
// Returns a passwd entry for the user, and the UID and GID of the added entry.
func (c *Container) generateCurrentUserPasswdEntry() (string, int, int, error) {
	uid := rootless.GetRootlessUID()
	if uid == 0 {
		return "", 0, 0, nil
	}

	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", 0, 0, errors.Wrapf(err, "failed to get current user")
	}
	pwd, err := c.userPasswdEntry(u)
	if err != nil {
		return "", 0, 0, err
	}

	return pwd, uid, rootless.GetRootlessGID(), nil
}

func (c *Container) userPasswdEntry(u *user.User) (string, error) {
	// Lookup the user to see if it exists in the container image.
	_, err := lookup.GetUser(c.state.Mountpoint, u.Username)
	if err != runcuser.ErrNoPasswdEntries {
		return "", err
	}

	// Lookup the UID to see if it exists in the container image.
	_, err = lookup.GetUser(c.state.Mountpoint, u.Uid)
	if err != runcuser.ErrNoPasswdEntries {
		return "", err
	}

	// If the user's actual home directory exists, or was mounted in - use
	// that.
	homeDir := c.WorkingDir()
	hDir := u.HomeDir
	for hDir != "/" {
		if MountExists(c.config.Spec.Mounts, hDir) {
			homeDir = u.HomeDir
			break
		}
		hDir = filepath.Dir(hDir)
	}
	if homeDir != u.HomeDir {
		for _, hDir := range c.UserVolumes() {
			if hDir == u.HomeDir {
				homeDir = u.HomeDir
				break
			}
		}
	}
	// Set HOME environment if not already set
	hasHomeSet := false
	for _, s := range c.config.Spec.Process.Env {
		if strings.HasPrefix(s, "HOME=") {
			hasHomeSet = true
			break
		}
	}
	if !hasHomeSet {
		c.config.Spec.Process.Env = append(c.config.Spec.Process.Env, fmt.Sprintf("HOME=%s", homeDir))
	}
	if c.config.PasswdEntry != "" {
		return c.passwdEntry(u.Username, u.Uid, u.Gid, u.Name, homeDir), nil
	}

	return fmt.Sprintf("%s:*:%s:%s:%s:%s:/bin/sh\n", u.Username, u.Uid, u.Gid, u.Name, homeDir), nil
}

// generateUserPasswdEntry generates an /etc/passwd entry for the container user
// to run in the container.
// The UID and GID of the added entry will also be returned.
// Accepts one argument, that being any UID that has already been added to the
// passwd file by other functions; if it matches the UID we were given, we don't
// need to do anything.
func (c *Container) generateUserPasswdEntry(addedUID int) (string, int, int, error) {
	var (
		groupspec string
		gid       int
	)
	if c.config.User == "" {
		return "", 0, 0, nil
	}
	splitSpec := strings.SplitN(c.config.User, ":", 2)
	userspec := splitSpec[0]
	if len(splitSpec) > 1 {
		groupspec = splitSpec[1]
	}
	// If a non numeric User, then don't generate passwd
	uid, err := strconv.ParseUint(userspec, 10, 32)
	if err != nil {
		return "", 0, 0, nil // nolint: nilerr
	}

	if addedUID != 0 && int(uid) == addedUID {
		return "", 0, 0, nil
	}

	// Lookup the user to see if it exists in the container image
	_, err = lookup.GetUser(c.state.Mountpoint, userspec)
	if err != runcuser.ErrNoPasswdEntries {
		return "", 0, 0, err
	}

	if groupspec != "" {
		ugid, err := strconv.ParseUint(groupspec, 10, 32)
		if err == nil {
			gid = int(ugid)
		} else {
			group, err := lookup.GetGroup(c.state.Mountpoint, groupspec)
			if err != nil {
				return "", 0, 0, errors.Wrapf(err, "unable to get gid %s from group file", groupspec)
			}
			gid = group.Gid
		}
	}

	if c.config.PasswdEntry != "" {
		entry := c.passwdEntry(fmt.Sprintf("%d", uid), fmt.Sprintf("%d", uid), fmt.Sprintf("%d", gid), "container user", c.WorkingDir())
		return entry, int(uid), gid, nil
	}

	return fmt.Sprintf("%d:*:%d:%d:container user:%s:/bin/sh\n", uid, uid, gid, c.WorkingDir()), int(uid), gid, nil
}

func (c *Container) passwdEntry(username string, uid, gid, name, homeDir string) string {
	s := c.config.PasswdEntry
	s = strings.Replace(s, "$USERNAME", username, -1)
	s = strings.Replace(s, "$UID", uid, -1)
	s = strings.Replace(s, "$GID", gid, -1)
	s = strings.Replace(s, "$NAME", name, -1)
	s = strings.Replace(s, "$HOME", homeDir, -1)
	return s + "\n"
}

// generatePasswdAndGroup generates container-specific passwd and group files
// iff g.config.User is a number or we are configured to make a passwd entry for
// the current user or the user specified HostsUsers
// Returns path to file to mount at /etc/passwd, path to file to mount at
// /etc/group, and any error that occurred. If no passwd/group file were
// required, the empty string will be returned for those path (this may occur
// even if no error happened).
// This may modify the mounted container's /etc/passwd and /etc/group instead of
// making copies to bind-mount in, so we don't break useradd (it wants to make a
// copy of /etc/passwd and rename the copy to /etc/passwd, which is impossible
// with a bind mount). This is done in cases where the container is *not*
// read-only. In this case, the function will return nothing ("", "", nil).
func (c *Container) generatePasswdAndGroup() (string, string, error) {
	if !c.config.AddCurrentUserPasswdEntry && c.config.User == "" &&
		len(c.config.HostUsers) == 0 {
		return "", "", nil
	}

	needPasswd := true
	needGroup := true

	// First, check if there's a mount at /etc/passwd or group, we don't
	// want to interfere with user mounts.
	if MountExists(c.config.Spec.Mounts, "/etc/passwd") {
		needPasswd = false
	}
	if MountExists(c.config.Spec.Mounts, "/etc/group") {
		needGroup = false
	}

	// Next, check if we already made the files. If we didn't, don't need to
	// do anything more.
	if needPasswd {
		passwdPath := filepath.Join(c.config.StaticDir, "passwd")
		if _, err := os.Stat(passwdPath); err == nil {
			needPasswd = false
		}
	}
	if needGroup {
		groupPath := filepath.Join(c.config.StaticDir, "group")
		if _, err := os.Stat(groupPath); err == nil {
			needGroup = false
		}
	}

	// Next, check if the container even has a /etc/passwd or /etc/group.
	// If it doesn't we don't want to create them ourselves.
	if needPasswd {
		exists, err := c.checkFileExistsInRootfs("/etc/passwd")
		if err != nil {
			return "", "", err
		}
		needPasswd = exists
	}
	if needGroup {
		exists, err := c.checkFileExistsInRootfs("/etc/group")
		if err != nil {
			return "", "", err
		}
		needGroup = exists
	}

	// If we don't need a /etc/passwd or /etc/group at this point we can
	// just return.
	if !needPasswd && !needGroup {
		return "", "", nil
	}

	passwdPath := ""
	groupPath := ""

	ro := c.IsReadOnly()

	if needPasswd {
		passwdEntry, err := c.generatePasswdEntry()
		if err != nil {
			return "", "", err
		}

		needsWrite := passwdEntry != ""
		switch {
		case ro && needsWrite:
			logrus.Debugf("Making /etc/passwd for container %s", c.ID())
			originPasswdFile, err := securejoin.SecureJoin(c.state.Mountpoint, "/etc/passwd")
			if err != nil {
				return "", "", errors.Wrapf(err, "error creating path to container %s /etc/passwd", c.ID())
			}
			orig, err := ioutil.ReadFile(originPasswdFile)
			if err != nil && !os.IsNotExist(err) {
				return "", "", err
			}
			passwdFile, err := c.writeStringToStaticDir("passwd", string(orig)+passwdEntry)
			if err != nil {
				return "", "", errors.Wrapf(err, "failed to create temporary passwd file")
			}
			if err := os.Chmod(passwdFile, 0644); err != nil {
				return "", "", err
			}
			passwdPath = passwdFile
		case !ro && needsWrite:
			logrus.Debugf("Modifying container %s /etc/passwd", c.ID())
			containerPasswd, err := securejoin.SecureJoin(c.state.Mountpoint, "/etc/passwd")
			if err != nil {
				return "", "", errors.Wrapf(err, "error looking up location of container %s /etc/passwd", c.ID())
			}

			f, err := os.OpenFile(containerPasswd, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return "", "", errors.Wrapf(err, "container %s", c.ID())
			}
			defer f.Close()

			if _, err := f.WriteString(passwdEntry); err != nil {
				return "", "", errors.Wrapf(err, "unable to append to container %s /etc/passwd", c.ID())
			}
		default:
			logrus.Debugf("Not modifying container %s /etc/passwd", c.ID())
		}
	}
	if needGroup {
		groupEntry, err := c.generateGroupEntry()
		if err != nil {
			return "", "", err
		}

		needsWrite := groupEntry != ""
		switch {
		case ro && needsWrite:
			logrus.Debugf("Making /etc/group for container %s", c.ID())
			originGroupFile, err := securejoin.SecureJoin(c.state.Mountpoint, "/etc/group")
			if err != nil {
				return "", "", errors.Wrapf(err, "error creating path to container %s /etc/group", c.ID())
			}
			orig, err := ioutil.ReadFile(originGroupFile)
			if err != nil && !os.IsNotExist(err) {
				return "", "", err
			}
			groupFile, err := c.writeStringToStaticDir("group", string(orig)+groupEntry)
			if err != nil {
				return "", "", errors.Wrapf(err, "failed to create temporary group file")
			}
			if err := os.Chmod(groupFile, 0644); err != nil {
				return "", "", err
			}
			groupPath = groupFile
		case !ro && needsWrite:
			logrus.Debugf("Modifying container %s /etc/group", c.ID())
			containerGroup, err := securejoin.SecureJoin(c.state.Mountpoint, "/etc/group")
			if err != nil {
				return "", "", errors.Wrapf(err, "error looking up location of container %s /etc/group", c.ID())
			}

			f, err := os.OpenFile(containerGroup, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return "", "", errors.Wrapf(err, "container %s", c.ID())
			}
			defer f.Close()

			if _, err := f.WriteString(groupEntry); err != nil {
				return "", "", errors.Wrapf(err, "unable to append to container %s /etc/group", c.ID())
			}
		default:
			logrus.Debugf("Not modifying container %s /etc/group", c.ID())
		}
	}

	return passwdPath, groupPath, nil
}

func isRootlessCgroupSet(cgroup string) bool {
	return false
}

func (c *Container) expectPodCgroup() (bool, error) {
	return false, nil
}

func (c *Container) getOCICgroupPath() (string, error) {
	return "", nil
}

func (c *Container) copyTimezoneFile(zonePath string) (string, error) {
	var localtimeCopy string = filepath.Join(c.state.RunDir, "localtime")
	file, err := os.Stat(zonePath)
	if err != nil {
		return "", err
	}
	if file.IsDir() {
		return "", errors.New("Invalid timezone: is a directory")
	}
	src, err := os.Open(zonePath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dest, err := os.Create(localtimeCopy)
	if err != nil {
		return "", err
	}
	defer dest.Close()
	_, err = io.Copy(dest, src)
	if err != nil {
		return "", err
	}
	if err := c.relabel(localtimeCopy, c.config.MountLabel, false); err != nil {
		return "", err
	}
	if err := dest.Chown(c.RootUID(), c.RootGID()); err != nil {
		return "", err
	}
	return localtimeCopy, err
}

func (c *Container) cleanupOverlayMounts() error {
	return overlay.CleanupContent(c.config.StaticDir)
}

// Check if a file exists at the given path in the container's root filesystem.
// Container must already be mounted for this to be used.
func (c *Container) checkFileExistsInRootfs(file string) (bool, error) {
	checkPath, err := securejoin.SecureJoin(c.state.Mountpoint, file)
	if err != nil {
		return false, errors.Wrapf(err, "cannot create path to container %s file %q", c.ID(), file)
	}
	stat, err := os.Stat(checkPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errors.Wrapf(err, "container %s", c.ID())
	}
	if stat.IsDir() {
		return false, nil
	}
	return true, nil
}

// Creates and mounts an empty dir to mount secrets into, if it does not already exist
func (c *Container) createSecretMountDir() error {
	src := filepath.Join(c.state.RunDir, "/run/secrets")
	_, err := os.Stat(src)
	if os.IsNotExist(err) {
		oldUmask := umask.Set(0)
		defer umask.Set(oldUmask)

		if err := os.MkdirAll(src, 0755); err != nil {
			return err
		}
		if err := label.Relabel(src, c.config.MountLabel, false); err != nil {
			return err
		}
		if err := os.Chown(src, c.RootUID(), c.RootGID()); err != nil {
			return err
		}
		c.state.BindMounts["/run/secrets"] = src
		return nil
	}

	return err
}

// Fix ownership and permissions of the specified volume if necessary.
func (c *Container) fixVolumePermissions(v *ContainerNamedVolume) error {
	vol, err := c.runtime.state.Volume(v.Name)
	if err != nil {
		return errors.Wrapf(err, "error retrieving named volume %s for container %s", v.Name, c.ID())
	}

	vol.lock.Lock()
	defer vol.lock.Unlock()

	// The volume may need a copy-up. Check the state.
	if err := vol.update(); err != nil {
		return err
	}

	// TODO: For now, I've disabled chowning volumes owned by non-Podman
	// drivers. This may be safe, but it's really going to be a case-by-case
	// thing, I think - safest to leave disabled now and re-enable later if
	// there is a demand.
	if vol.state.NeedsChown && !vol.UsesVolumeDriver() {
		vol.state.NeedsChown = false

		uid := int(c.config.Spec.Process.User.UID)
		gid := int(c.config.Spec.Process.User.GID)

		if c.config.IDMappings.UIDMap != nil {
			p := idtools.IDPair{
				UID: uid,
				GID: gid,
			}
			mappings := idtools.NewIDMappingsFromMaps(c.config.IDMappings.UIDMap, c.config.IDMappings.GIDMap)
			newPair, err := mappings.ToHost(p)
			if err != nil {
				return errors.Wrapf(err, "error mapping user %d:%d", uid, gid)
			}
			uid = newPair.UID
			gid = newPair.GID
		}

		vol.state.UIDChowned = uid
		vol.state.GIDChowned = gid

		if err := vol.save(); err != nil {
			return err
		}

		mountPoint, err := vol.MountPoint()
		if err != nil {
			return err
		}

		if err := os.Lchown(mountPoint, uid, gid); err != nil {
			return err
		}

		// Make sure the new volume matches the permissions of the target directory.
		// https://github.com/containers/podman/issues/10188
		st, err := os.Lstat(filepath.Join(c.state.Mountpoint, v.Dest))
		if err == nil {
			if stat, ok := st.Sys().(*syscall.Stat_t); ok {
				if err := os.Lchown(mountPoint, int(stat.Uid), int(stat.Gid)); err != nil {
					return err
				}
			}
			if err := os.Chmod(mountPoint, st.Mode()); err != nil {
				return err
			}
			/*
				stat := st.Sys().(*syscall.Stat_t)
				atime := time.Unix(int64(stat.Atim.Sec), int64(stat.Atim.Nsec))
				if err := os.Chtimes(mountPoint, atime, st.ModTime()); err != nil {
					return err
				}*/
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (c *Container) relabel(src, mountLabel string, recurse bool) error {
	if !selinux.GetEnabled() || mountLabel == "" {
		return nil
	}
	// only relabel on initial creation of container
	if !c.ensureState(define.ContainerStateConfigured, define.ContainerStateUnknown) {
		label, err := label.FileLabel(src)
		if err != nil {
			return err
		}
		// If labels are different, might be on a tmpfs
		if label == mountLabel {
			return nil
		}
	}
	return label.Relabel(src, mountLabel, recurse)
}

func (c *Container) ChangeHostPathOwnership(src string, recurse bool, uid, gid int) error {
	// only chown on initial creation of container
	if !c.ensureState(define.ContainerStateConfigured, define.ContainerStateUnknown) {
		st, err := os.Stat(src)
		if err != nil {
			return err
		}

		// If labels are different, might be on a tmpfs
		if int(st.Sys().(*syscall.Stat_t).Uid) == uid && int(st.Sys().(*syscall.Stat_t).Gid) == gid {
			return nil
		}
	}
	return chown.ChangeHostPathOwnership(src, recurse, uid, gid)
}
