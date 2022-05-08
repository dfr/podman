//go:build freebsd

package libpod

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/containers/buildah"
	"github.com/containers/image/v5/pkg/sysregistriesv2"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/libpod/linkmode"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/system"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Info returns the store and host information
func (r *Runtime) info() (*define.Info, error) {
	info := define.Info{}
	versionInfo, err := define.GetVersion()
	if err != nil {
		return nil, errors.Wrapf(err, "error getting version info")
	}
	info.Version = versionInfo
	// get host information
	hostInfo, err := r.hostInfo()
	if err != nil {
		return nil, errors.Wrapf(err, "error getting host info")
	}
	info.Host = hostInfo

	// get store information
	storeInfo, err := r.storeInfo()
	if err != nil {
		return nil, errors.Wrapf(err, "error getting store info")
	}
	info.Store = storeInfo
	registries := make(map[string]interface{})

	sys := r.SystemContext()
	data, err := sysregistriesv2.GetRegistries(sys)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting registries")
	}
	for _, reg := range data {
		registries[reg.Prefix] = reg
	}
	regs, err := sysregistriesv2.UnqualifiedSearchRegistries(sys)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting registries")
	}
	if len(regs) > 0 {
		registries["search"] = regs
	}
	volumePlugins := make([]string, 0, len(r.config.Engine.VolumePlugins)+1)
	// the local driver always exists
	volumePlugins = append(volumePlugins, "local")
	for plugin := range r.config.Engine.VolumePlugins {
		volumePlugins = append(volumePlugins, plugin)
	}
	info.Plugins.Volume = volumePlugins
	info.Plugins.Network = r.network.Drivers()
	info.Plugins.Log = logDrivers

	info.Registries = registries
	return &info, nil
}

// top-level "host" info
func (r *Runtime) hostInfo() (*define.HostInfo, error) {
	// lets say OS, arch, number of cpus, amount of memory, maybe os distribution/version, hostname, kernel version, uptime
	mi, err := system.ReadMemInfo()
	if err != nil {
		return nil, errors.Wrapf(err, "error reading memory info")
	}

	hostDistributionInfo := r.GetHostDistributionInfo()

	kv, err := readKernelVersion()
	if err != nil {
		return nil, errors.Wrapf(err, "error reading kernel version")
	}

	host, err := os.Hostname()
	if err != nil {
		return nil, errors.Wrapf(err, "error getting hostname")
	}

	info := define.HostInfo{
		Arch:           runtime.GOARCH,
		BuildahVersion: buildah.Version,
		Linkmode:       linkmode.Linkmode(),
		CPUs:           runtime.NumCPU(),
		Distribution:   hostDistributionInfo,
		LogDriver:      r.config.Containers.LogDriver,
		EventLogger:    r.eventer.String(),
		Hostname:       host,
		IDMappings:     define.IDMappings{},
		Kernel:         kv,
		MemFree:        mi.MemFree,
		MemTotal:       mi.MemTotal,
		NetworkBackend: r.config.Network.NetworkBackend,
		OS:             runtime.GOOS,
		Security: define.SecurityInfo{
			DefaultCapabilities: strings.Join(r.config.Containers.DefaultCapabilities, ","),
			Rootless:            false,
			SECCOMPEnabled:      false,
			SELinuxEnabled:      false,
		},
		Slirp4NetNS: define.SlirpInfo{},
		SwapFree:    mi.SwapFree,
		SwapTotal:   mi.SwapTotal,
	}

	conmonInfo, ociruntimeInfo, err := r.defaultOCIRuntime.RuntimeInfo()
	if err != nil {
		logrus.Errorf("Getting info on OCI runtime %s: %v", r.defaultOCIRuntime.Name(), err)
	} else {
		info.Conmon = conmonInfo
		info.OCIRuntime = ociruntimeInfo
	}

	up, err := readUptime()
	if err != nil {
		return nil, errors.Wrapf(err, "error reading up time")
	}
	// Convert uptime in seconds to a human-readable format
	upSeconds := up + "s"
	upDuration, err := time.ParseDuration(upSeconds)
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing system uptime")
	}

	// TODO Isn't there a simple lib for this, something like humantime?
	hoursFound := false
	var timeBuffer bytes.Buffer
	var hoursBuffer bytes.Buffer
	for _, elem := range upDuration.String() {
		timeBuffer.WriteRune(elem)
		if elem == 'h' || elem == 'm' {
			timeBuffer.WriteRune(' ')
			if elem == 'h' {
				hoursFound = true
			}
		}
		if !hoursFound {
			hoursBuffer.WriteRune(elem)
		}
	}

	info.Uptime = timeBuffer.String()
	if hoursFound {
		hours, err := strconv.ParseFloat(hoursBuffer.String(), 64)
		if err == nil {
			days := hours / 24
			info.Uptime = fmt.Sprintf("%s (Approximately %.2f days)", info.Uptime, days)
		}
	}

	return &info, nil
}

func (r *Runtime) getContainerStoreInfo() (define.ContainerStore, error) {
	var (
		paused, running, stopped int
	)
	cs := define.ContainerStore{}
	cons, err := r.GetAllContainers()
	if err != nil {
		return cs, err
	}
	cs.Number = len(cons)
	for _, con := range cons {
		state, err := con.State()
		if err != nil {
			if errors.Cause(err) == define.ErrNoSuchCtr {
				// container was probably removed
				cs.Number--
				continue
			}
			return cs, err
		}
		switch state {
		case define.ContainerStateRunning:
			running++
		case define.ContainerStatePaused:
			paused++
		default:
			stopped++
		}
	}
	cs.Paused = paused
	cs.Stopped = stopped
	cs.Running = running
	return cs, nil
}

// top-level "store" info
func (r *Runtime) storeInfo() (*define.StoreInfo, error) {
	// lets say storage driver in use, number of images, number of containers
	configFile, err := storage.DefaultConfigFile(false)
	if err != nil {
		return nil, err
	}
	images, err := r.store.Images()
	if err != nil {
		return nil, errors.Wrapf(err, "error getting number of images")
	}
	conInfo, err := r.getContainerStoreInfo()
	if err != nil {
		return nil, err
	}
	imageInfo := define.ImageStore{Number: len(images)}

	info := define.StoreInfo{
		ImageStore:      imageInfo,
		ImageCopyTmpDir: os.Getenv("TMPDIR"),
		ContainerStore:  conInfo,
		GraphRoot:       r.store.GraphRoot(),
		RunRoot:         r.store.RunRoot(),
		GraphDriverName: r.store.GraphDriverName(),
		GraphOptions:    nil,
		VolumePath:      r.config.Engine.VolumePath,
		ConfigFile:      configFile,
	}
	graphOptions := map[string]interface{}{}
	for _, o := range r.store.GraphOptions() {
		split := strings.SplitN(o, "=", 2)
		if strings.HasSuffix(split[0], "mount_program") {
			version, err := programVersion(split[1])
			if err != nil {
				logrus.Warnf("Failed to retrieve program version for %s: %v", split[1], err)
			}
			program := map[string]interface{}{}
			program["Executable"] = split[1]
			program["Version"] = version
			program["Package"] = packageVersion(split[1])
			graphOptions[split[0]] = program
		} else {
			graphOptions[split[0]] = split[1]
		}
	}
	info.GraphOptions = graphOptions

	statusPairs, err := r.store.Status()
	if err != nil {
		return nil, err
	}
	status := map[string]string{}
	for _, pair := range statusPairs {
		status[pair[0]] = pair[1]
	}
	info.GraphStatus = status
	return &info, nil
}

func bytesToString(buf []byte) string {
	i := 0
	for i < len(buf) && buf[i] != 0 {
		i++
	}
	return string(buf[:i])
}

func readKernelVersion() (string, error) {
	var uname unix.Utsname
	if err := unix.Uname(&uname); err != nil {
		return "", err
	}
	return bytesToString(uname.Release[:]), nil
}

func readUptime() (string, error) {
	var ts unix.Timespec
	_, _, err := unix.Syscall(unix.SYS_CLOCK_GETTIME, uintptr(unix.CLOCK_UPTIME), uintptr(unsafe.Pointer(&ts)), 0)
	if err != 0 {
		return "", err
	}
	return fmt.Sprintf("%.2f", float64(ts.Sec)+float64(ts.Nsec)*1e-9), nil
}

// GetHostDistributionInfo returns a map containing the host's distribution and version
func (r *Runtime) GetHostDistributionInfo() define.DistributionInfo {
	// Populate values in case we cannot find the values
	// or the file
	dist := define.DistributionInfo{
		Distribution: "unknown",
		Version:      "unknown",
	}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return dist
	}
	defer f.Close()

	l := bufio.NewScanner(f)
	for l.Scan() {
		if strings.HasPrefix(l.Text(), "ID=") {
			dist.Distribution = strings.TrimPrefix(l.Text(), "ID=")
		}
		if strings.HasPrefix(l.Text(), "VARIANT_ID=") {
			dist.Variant = strings.Trim(strings.TrimPrefix(l.Text(), "VARIANT_ID="), "\"")
		}
		if strings.HasPrefix(l.Text(), "VERSION_ID=") {
			dist.Version = strings.Trim(strings.TrimPrefix(l.Text(), "VERSION_ID="), "\"")
		}
		if strings.HasPrefix(l.Text(), "VERSION_CODENAME=") {
			dist.Codename = strings.Trim(strings.TrimPrefix(l.Text(), "VERSION_CODENAME="), "\"")
		}
	}
	return dist
}
