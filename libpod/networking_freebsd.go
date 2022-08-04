//go:build freebsd
// +build freebsd

package libpod

import (
	jdec "encoding/json"
	err "errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/containers/buildah/pkg/jail"
	"github.com/containers/common/libnetwork/types"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/libpod/events"
	"github.com/containers/podman/v4/pkg/namespaces"
	"github.com/containers/podman/v4/pkg/util"
	"github.com/containers/storage/pkg/lockfile"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

const (
	// slirp4netnsMTU the default MTU override
	slirp4netnsMTU = 65520

	// default slirp4ns subnet
	defaultSlirp4netnsSubnet = "10.0.2.0/24"

	// rootlessNetNsName is the file name for the rootless network namespace bind mount
	rootlessNetNsName = "rootless-netns"

	// rootlessNetNsSilrp4netnsPidFile is the name of the rootless netns slirp4netns pid file
	rootlessNetNsSilrp4netnsPidFile = "rootless-netns-slirp4netns.pid"

	// persistentCNIDir is the directory where the CNI files are stored
	persistentCNIDir = "/var/lib/cni"
)

type Netstat struct {
	Statistics NetstatInterface `json:"statistics"`
}

type NetstatInterface struct {
	Interface []NetstatAddress `json:"interface"`
}

type NetstatAddress struct {
	Name    string `json:"name"`
	Flags   string `json:"flags"`
	Mtu     int    `json:"mtu"`
	Network string `json:"network"`
	Address string `json:"address"`

	ReceivedPackets uint64 `json:"received-packets"`
	ReceivedBytes   uint64 `json:"received-bytes"`
	ReceivedErrors  uint64 `json:"received-errors"`

	SentPackets uint64 `json:"sent-packets"`
	SentBytes   uint64 `json:"sent-bytes"`
	SentErrors  uint64 `json:"send-errors"`

	DroppedPackets uint64 `json:"dropped-packets"`

	Collisions uint64 `json:"collisions"`
}

// convertPortMappings will remove the HostIP part from the ports when running inside podman machine.
// This is need because a HostIP of 127.0.0.1 would now allow the gvproxy forwarder to reach to open ports.
// For machine the HostIP must only be used by gvproxy and never in the VM.
func (c *Container) convertPortMappings() []types.PortMapping {
	if !c.runtime.config.Engine.MachineEnabled || len(c.config.PortMappings) == 0 {
		return c.config.PortMappings
	}
	// if we run in a machine VM we have to ignore the host IP part
	newPorts := make([]types.PortMapping, 0, len(c.config.PortMappings))
	for _, port := range c.config.PortMappings {
		port.HostIP = ""
		newPorts = append(newPorts, port)
	}
	return newPorts
}

func (c *Container) getNetworkOptions(networkOpts map[string]types.PerNetworkOptions) (types.NetworkOptions, error) {
	opts := types.NetworkOptions{
		ContainerID:   c.config.ID,
		ContainerName: getCNIPodName(c),
	}
	opts.PortMappings = c.convertPortMappings()

	// If the container requested special network options use this instead of the config.
	// This is the case for container restore or network reload.
	if c.perNetworkOpts != nil {
		opts.Networks = c.perNetworkOpts
	} else {
		opts.Networks = networkOpts
	}
	return opts, nil
}

type RootlessNetNS struct {
	dir  string
	Lock lockfile.Locker
}

// getPath will join the given path to the rootless netns dir
func (r *RootlessNetNS) getPath(path string) string {
	return filepath.Join(r.dir, path)
}

// Do - run the given function in the rootless netns.
// It does not lock the rootlessCNI lock, the caller
// should only lock when needed, e.g. for cni operations.
func (r *RootlessNetNS) Do(toRun func() error) error {
	return err.New("not supported on freebsd")
}

// Cleanup the rootless network namespace if needed.
// It checks if we have running containers with the bridge network mode.
// Cleanup() expects that r.Lock is locked
func (r *RootlessNetNS) Cleanup(runtime *Runtime) error {
	return err.New("not supported on freebsd")
}

// GetRootlessNetNs returns the rootless netns object. If create is set to true
// the rootless network namespace will be created if it does not exists already.
// If called as root it returns always nil.
// On success the returned RootlessCNI lock is locked and must be unlocked by the caller.
func (r *Runtime) GetRootlessNetNs(new bool) (*RootlessNetNS, error) {
	return nil, err.New("not supported on freebsd")
}

// setUpNetwork will set up the the networks, on error it will also tear down the cni
// networks. If rootless it will join/create the rootless network namespace.
func (r *Runtime) setUpNetwork(ns string, opts types.NetworkOptions) (map[string]types.StatusBlock, error) {
	return r.network.Setup(ns, types.SetupOptions{NetworkOptions: opts})
}

// getCNIPodName return the pod name (hostname) used by CNI and the dnsname plugin.
// If we are in the pod network namespace use the pod name otherwise the container name
func getCNIPodName(c *Container) string {
	if c.config.NetMode.IsPod() || c.IsInfra() {
		pod, err := c.runtime.state.Pod(c.PodID())
		if err == nil {
			return pod.Name()
		}
	}
	return c.Name()
}

// Create and configure a new network namespace for a container
func (r *Runtime) configureNetNS(ctr *Container, jailName string) (status map[string]types.StatusBlock, rerr error) {
	if err := r.exposeMachinePorts(ctr.config.PortMappings); err != nil {
		return nil, err
	}
	defer func() {
		// make sure to unexpose the gvproxy ports when an error happens
		if rerr != nil {
			if err := r.unexposeMachinePorts(ctr.config.PortMappings); err != nil {
				logrus.Errorf("failed to free gvproxy machine ports: %v", err)
			}
		}
	}()
	networks, err := ctr.networks()
	if err != nil {
		return nil, err
	}
	// All networks have been removed from the container.
	// This is effectively forcing net=none.
	if len(networks) == 0 {
		return nil, nil
	}

	netOpts, err := ctr.getNetworkOptions(networks)
	if err != nil {
		return nil, err
	}
	netStatus, err := r.setUpNetwork(jailName, netOpts)
	if err != nil {
		return nil, err
	}

	return netStatus, err
}

// Create and configure a new network namespace for a container
func (r *Runtime) createNetNS(ctr *Container) (netJail string, q map[string]types.StatusBlock, retErr error) {
	jailName := ctr.config.ID + "-vnet"

	jconf := jail.NewConfig()
	jconf.Set("name", jailName)
	jconf.Set("vnet", jail.NEW)
	jconf.Set("children.max", 1)
	jconf.Set("persist", true)
	jconf.Set("enforce_statfs", 0)
	jconf.Set("devfs_ruleset", 4)
	jconf.Set("allow.raw_sockets", true)
	jconf.Set("allow.chflags", true)
	jconf.Set("allow.mount", true)
	jconf.Set("allow.mount.devfs", true)
	jconf.Set("allow.mount.nullfs", true)
	jconf.Set("allow.mount.fdescfs", true)
	jconf.Set("securelevel", -1)
	_, err := jail.Create(jconf)

	logrus.Debugf("Created network jail at %s for container %s", jailName, ctr.ID())

	var networkStatus map[string]types.StatusBlock
	networkStatus, err = r.configureNetNS(ctr, jailName)
	return jailName, networkStatus, err
}

// Tear down a container's network configuration and joins the
// rootless net ns as rootless user
func (r *Runtime) teardownNetwork(ns string, opts types.NetworkOptions) error {
	err := r.network.Teardown(ns, types.TeardownOptions{NetworkOptions: opts})
	return errors.Wrapf(err, "error tearing down network namespace configuration for container %s", opts.ContainerID)
}

// Tear down a container's CNI network configuration, but do not tear down the
// namespace itself.
func (r *Runtime) teardownCNI(ctr *Container) error {
	if ctr.state.NetworkJail == "" {
		// The container has no network namespace, we're set
		return nil
	}

	logrus.Debugf("Tearing down network namespace at %s for container %s", ctr.state.NetworkJail, ctr.ID())

	networks, err := ctr.networks()
	if err != nil {
		return err
	}

	if !ctr.config.NetMode.IsSlirp4netns() && len(networks) > 0 {
		netOpts, err := ctr.getNetworkOptions(networks)
		if err != nil {
			return err
		}
		return r.teardownNetwork(ctr.state.NetworkJail, netOpts)
	}
	return nil
}

// Tear down a network namespace, undoing all state associated with it.
func (r *Runtime) teardownNetNS(ctr *Container) error {
	if err := r.unexposeMachinePorts(ctr.config.PortMappings); err != nil {
		// do not return an error otherwise we would prevent network cleanup
		logrus.Errorf("failed to free gvproxy machine ports: %v", err)
	}
	if err := r.teardownCNI(ctr); err != nil {
		return err
	}

	if ctr.state.NetworkJail != "" {
		// Rather than destroying the jail immediately, reset the
		// persist flag so that it will live until the container is
		// done.
		netjail, err := jail.FindByName(ctr.state.NetworkJail)
		if err != nil {
			return errors.Wrapf(err, "error finding network jail %s", ctr.state.NetworkJail)
		}
		jconf := jail.NewConfig()
		jconf.Set("persist", false)
		params := make(map[string]interface{})
		params["persist"] = false
		if err := netjail.Set(jconf); err != nil {
			return errors.Wrapf(err, "error releasing network jail %s", ctr.state.NetworkJail)
		}

		ctr.state.NetworkJail = ""
	}

	return nil
}

// isBridgeNetMode checks if the given network mode is bridge.
// It returns nil when it is set to bridge and an error otherwise.
func isBridgeNetMode(n namespaces.NetworkMode) error {
	if !n.IsBridge() {
		return errors.Wrapf(define.ErrNetworkModeInvalid, "%q is not supported", n)
	}
	return nil
}

// Reload only works with containers with a configured network.
// It will tear down, and then reconfigure, the network of the container.
// This is mainly used when a reload of firewall rules wipes out existing
// firewall configuration.
// Efforts will be made to preserve MAC and IP addresses, but this only works if
// the container only joined a single CNI network, and was only assigned a
// single MAC or IP.
// Only works on root containers at present, though in the future we could
// extend this to stop + restart slirp4netns
func (r *Runtime) reloadContainerNetwork(ctr *Container) (map[string]types.StatusBlock, error) {
	if ctr.state.NetworkJail == "" {
		return nil, errors.Wrapf(define.ErrCtrStateInvalid, "container %s network is not configured, refusing to reload", ctr.ID())
	}
	if err := isBridgeNetMode(ctr.config.NetMode); err != nil {
		return nil, err
	}
	logrus.Infof("Going to reload container %s network", ctr.ID())

	err := r.teardownCNI(ctr)
	if err != nil {
		// teardownCNI will error if the iptables rules do not exists and this is the case after
		// a firewall reload. The purpose of network reload is to recreate the rules if they do
		// not exists so we should not log this specific error as error. This would confuse users otherwise.
		// iptables-legacy and iptables-nft will create different errors make sure to match both.
		b, rerr := regexp.MatchString("Couldn't load target `CNI-[a-f0-9]{24}':No such file or directory|Chain 'CNI-[a-f0-9]{24}' does not exist", err.Error())
		if rerr == nil && !b {
			logrus.Error(err)
		} else {
			logrus.Info(err)
		}
	}

	networkOpts, err := ctr.networks()
	if err != nil {
		return nil, err
	}

	// Set the same network settings as before..
	netStatus := ctr.getNetworkStatus()
	for network, perNetOpts := range networkOpts {
		for name, netInt := range netStatus[network].Interfaces {
			perNetOpts.InterfaceName = name
			perNetOpts.StaticMAC = netInt.MacAddress
			for _, netAddress := range netInt.Subnets {
				perNetOpts.StaticIPs = append(perNetOpts.StaticIPs, netAddress.IPNet.IP)
			}
			// Normally interfaces have a length of 1, only for some special cni configs we could get more.
			// For now just use the first interface to get the ips this should be good enough for most cases.
			break
		}
		networkOpts[network] = perNetOpts
	}
	ctr.perNetworkOpts = networkOpts

	return r.configureNetNS(ctr, ctr.state.NetworkJail)
}

func getContainerNetIO(ctr *Container) (*netlink.LinkStatistics, error) {
	jailName := ctr.state.NetworkJail
	if jailName == "" {
		// If netNSPath is empty, it was set as none, and no netNS was set up
		// this is a valid state and thus return no error, nor any statistics
		return nil, nil
	}

	// FIXME get the interface from the container netstatus
	cmd := exec.Command("jexec", jailName, "netstat", "-bI", "eth0", "--libxo", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	stats := Netstat{}
	if err := jdec.Unmarshal(out, &stats); err != nil {
		return nil, err
	}

	// Find the link stats
	for _, ifaddr := range stats.Statistics.Interface {
		if ifaddr.Mtu > 0 {
			return &netlink.LinkStatistics{
				RxPackets:  ifaddr.ReceivedPackets,
				TxPackets:  ifaddr.SentPackets,
				RxBytes:    ifaddr.ReceivedBytes,
				TxBytes:    ifaddr.SentBytes,
				RxErrors:   ifaddr.ReceivedErrors,
				TxErrors:   ifaddr.SentErrors,
				RxDropped:  ifaddr.DroppedPackets,
				Collisions: ifaddr.Collisions,
			}, nil
		}
	}

	return &netlink.LinkStatistics{}, nil
}

// Produce an InspectNetworkSettings containing information on the container
// network.
func (c *Container) getContainerNetworkInfo() (*define.InspectNetworkSettings, error) {
	if c.config.NetNsCtr != "" {
		netNsCtr, err := c.runtime.GetContainer(c.config.NetNsCtr)
		if err != nil {
			return nil, err
		}
		// see https://github.com/containers/podman/issues/10090
		// the container has to be locked for syncContainer()
		netNsCtr.lock.Lock()
		defer netNsCtr.lock.Unlock()
		// Have to sync to ensure that state is populated
		if err := netNsCtr.syncContainer(); err != nil {
			return nil, err
		}
		logrus.Debugf("Container %s shares network namespace, retrieving network info of container %s", c.ID(), c.config.NetNsCtr)

		return netNsCtr.getContainerNetworkInfo()
	}

	settings := new(define.InspectNetworkSettings)
	settings.Ports = makeInspectPortBindings(c.config.PortMappings, c.config.ExposedPorts)

	networks, err := c.networks()
	if err != nil {
		return nil, err
	}

	netStatus := c.getNetworkStatus()
	// If this is empty, we're probably slirp4netns
	if len(netStatus) == 0 {
		return settings, nil
	}

	// If we have networks - handle that here
	if len(networks) > 0 {
		if len(networks) != len(netStatus) {
			return nil, errors.Wrapf(define.ErrInternal, "network inspection mismatch: asked to join %d network(s) %v, but have information on %d network(s)", len(networks), networks, len(netStatus))
		}

		settings.Networks = make(map[string]*define.InspectAdditionalNetwork)

		for name, opts := range networks {
			result := netStatus[name]
			addedNet := new(define.InspectAdditionalNetwork)
			addedNet.NetworkID = name

			basicConfig, err := resultToBasicNetworkConfig(result)
			if err != nil {
				return nil, err
			}
			addedNet.Aliases = opts.Aliases

			addedNet.InspectBasicNetworkConfig = basicConfig

			settings.Networks[name] = addedNet
		}

		// if not only the default network is connected we can return here
		// otherwise we have to populate the InspectBasicNetworkConfig settings
		_, isDefaultNet := networks[c.runtime.config.Network.DefaultNetwork]
		if !(len(networks) == 1 && isDefaultNet) {
			return settings, nil
		}
	}

	// If not joining networks, we should have at most 1 result
	if len(netStatus) > 1 {
		return nil, errors.Wrapf(define.ErrInternal, "should have at most 1 network status result if not joining networks, instead got %d", len(netStatus))
	}

	if len(netStatus) == 1 {
		for _, status := range netStatus {
			basicConfig, err := resultToBasicNetworkConfig(status)
			if err != nil {
				return nil, err
			}
			settings.InspectBasicNetworkConfig = basicConfig
		}
	}
	return settings, nil
}

func (c *Container) joinedNetworkNSPath() string {
	for _, namespace := range c.config.Spec.Linux.Namespaces {
		if namespace.Type == spec.NetworkNamespace {
			return namespace.Path
		}
	}
	return ""
}

// resultToBasicNetworkConfig produces an InspectBasicNetworkConfig from a CNI
// result
func resultToBasicNetworkConfig(result types.StatusBlock) (define.InspectBasicNetworkConfig, error) {
	config := define.InspectBasicNetworkConfig{}
	interfaceNames := make([]string, 0, len(result.Interfaces))
	for interfaceName := range result.Interfaces {
		interfaceNames = append(interfaceNames, interfaceName)
	}
	// ensure consistent inspect results by sorting
	sort.Strings(interfaceNames)
	for _, interfaceName := range interfaceNames {
		netInt := result.Interfaces[interfaceName]
		for _, netAddress := range netInt.Subnets {
			size, _ := netAddress.IPNet.Mask.Size()
			if netAddress.IPNet.IP.To4() != nil {
				//ipv4
				if config.IPAddress == "" {
					config.IPAddress = netAddress.IPNet.IP.String()
					config.IPPrefixLen = size
					config.Gateway = netAddress.Gateway.String()
				} else {
					config.SecondaryIPAddresses = append(config.SecondaryIPAddresses, define.Address{Addr: netAddress.IPNet.IP.String(), PrefixLength: size})
				}
			} else {
				//ipv6
				if config.GlobalIPv6Address == "" {
					config.GlobalIPv6Address = netAddress.IPNet.IP.String()
					config.GlobalIPv6PrefixLen = size
					config.IPv6Gateway = netAddress.Gateway.String()
				} else {
					config.SecondaryIPv6Addresses = append(config.SecondaryIPv6Addresses, define.Address{Addr: netAddress.IPNet.IP.String(), PrefixLength: size})
				}
			}
		}
		if config.MacAddress == "" {
			config.MacAddress = netInt.MacAddress.String()
		} else {
			config.AdditionalMacAddresses = append(config.AdditionalMacAddresses, netInt.MacAddress.String())
		}
	}
	return config, nil
}

type logrusDebugWriter struct {
	prefix string
}

func (w *logrusDebugWriter) Write(p []byte) (int, error) {
	logrus.Debugf("%s%s", w.prefix, string(p))
	return len(p), nil
}

// NetworkDisconnect removes a container from the network
func (c *Container) NetworkDisconnect(nameOrID, netName string, force bool) error {
	// only the bridge mode supports cni networks
	if err := isBridgeNetMode(c.config.NetMode); err != nil {
		return err
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	networks, err := c.networks()
	if err != nil {
		return err
	}

	// check if network exists and if the input is a ID we get the name
	// CNI only uses names so it is important that we only use the name
	netName, err = c.runtime.normalizeNetworkName(netName)
	if err != nil {
		return err
	}

	_, nameExists := networks[netName]
	if !nameExists && len(networks) > 0 {
		return errors.Errorf("container %s is not connected to network %s", nameOrID, netName)
	}

	if err := c.syncContainer(); err != nil {
		return err
	}
	// get network status before we disconnect
	networkStatus := c.getNetworkStatus()

	if err := c.runtime.state.NetworkDisconnect(c, netName); err != nil {
		return err
	}

	c.newNetworkEvent(events.NetworkDisconnect, netName)
	if !c.ensureState(define.ContainerStateRunning, define.ContainerStateCreated) {
		return nil
	}

	opts := types.NetworkOptions{
		ContainerID:   c.config.ID,
		ContainerName: getCNIPodName(c),
	}
	opts.PortMappings = c.convertPortMappings()
	opts.Networks = map[string]types.PerNetworkOptions{
		netName: networks[netName],
	}

	// update network status if container is running
	oldStatus, statusExist := networkStatus[netName]
	delete(networkStatus, netName)
	c.state.NetworkStatus = networkStatus
	err = c.save()
	if err != nil {
		return err
	}

	// Update resolv.conf if required
	if statusExist {
		stringIPs := make([]string, 0, len(oldStatus.DNSServerIPs))
		for _, ip := range oldStatus.DNSServerIPs {
			stringIPs = append(stringIPs, ip.String())
		}
		if len(stringIPs) == 0 {
			return nil
		}
		logrus.Debugf("Removing DNS Servers %v from resolv.conf", stringIPs)
		if err := c.removeNameserver(stringIPs); err != nil {
			return err
		}
	}

	return nil
}

// ConnectNetwork connects a container to a given network
func (c *Container) NetworkConnect(nameOrID, netName string, netOpts types.PerNetworkOptions) error {
	// only the bridge mode supports cni networks
	if err := isBridgeNetMode(c.config.NetMode); err != nil {
		return err
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	networks, err := c.networks()
	if err != nil {
		return err
	}

	// check if network exists and if the input is a ID we get the name
	// CNI only uses names so it is important that we only use the name
	netName, err = c.runtime.normalizeNetworkName(netName)
	if err != nil {
		return err
	}

	if err := c.syncContainer(); err != nil {
		return err
	}

	// get network status before we connect
	networkStatus := c.getNetworkStatus()

	// always add the short id as alias for docker compat
	netOpts.Aliases = append(netOpts.Aliases, c.config.ID[:12])

	if netOpts.InterfaceName == "" {
		netOpts.InterfaceName = getFreeInterfaceName(networks)
		if netOpts.InterfaceName == "" {
			return errors.New("could not find free network interface name")
		}
	}

	if err := c.runtime.state.NetworkConnect(c, netName, netOpts); err != nil {
		return err
	}
	c.newNetworkEvent(events.NetworkConnect, netName)
	if !c.ensureState(define.ContainerStateRunning, define.ContainerStateCreated) {
		return nil
	}

	opts := types.NetworkOptions{
		ContainerID:   c.config.ID,
		ContainerName: getCNIPodName(c),
	}
	opts.PortMappings = c.convertPortMappings()
	opts.Networks = map[string]types.PerNetworkOptions{
		netName: netOpts,
	}

	/*
		results, err := c.runtime.setUpNetwork(c.state.NetNS.Path(), opts)
		if err != nil {
			return err
		}
		if len(results) != 1 {
			return errors.New("when adding aliases, results must be of length 1")
		}
	*/
	var results map[string]types.StatusBlock

	// update network status
	if networkStatus == nil {
		networkStatus = make(map[string]types.StatusBlock, 1)
	}
	networkStatus[netName] = results[netName]
	c.state.NetworkStatus = networkStatus

	err = c.save()
	if err != nil {
		return err
	}

	// The first network needs a port reload to set the correct child ip for the rootlessport process.
	// Adding a second network does not require a port reload because the child ip is still valid.

	ipv6, err := c.checkForIPv6(networkStatus)
	if err != nil {
		return err
	}

	// Update resolv.conf if required
	stringIPs := make([]string, 0, len(results[netName].DNSServerIPs))
	for _, ip := range results[netName].DNSServerIPs {
		if (ip.To4() == nil) && !ipv6 {
			continue
		}
		stringIPs = append(stringIPs, ip.String())
	}
	if len(stringIPs) == 0 {
		return nil
	}
	logrus.Debugf("Adding DNS Servers %v to resolv.conf", stringIPs)
	if err := c.addNameserver(stringIPs); err != nil {
		return err
	}

	return nil
}

// get a free interface name for a new network
// return an empty string if no free name was found
func getFreeInterfaceName(networks map[string]types.PerNetworkOptions) string {
	ifNames := make([]string, 0, len(networks))
	for _, opts := range networks {
		ifNames = append(ifNames, opts.InterfaceName)
	}
	for i := 0; i < 100000; i++ {
		ifName := fmt.Sprintf("eth%d", i)
		if !util.StringInSlice(ifName, ifNames) {
			return ifName
		}
	}
	return ""
}

// DisconnectContainerFromNetwork removes a container from its CNI network
func (r *Runtime) DisconnectContainerFromNetwork(nameOrID, netName string, force bool) error {
	ctr, err := r.LookupContainer(nameOrID)
	if err != nil {
		return err
	}
	return ctr.NetworkDisconnect(nameOrID, netName, force)
}

// ConnectContainerToNetwork connects a container to a CNI network
func (r *Runtime) ConnectContainerToNetwork(nameOrID, netName string, netOpts types.PerNetworkOptions) error {
	ctr, err := r.LookupContainer(nameOrID)
	if err != nil {
		return err
	}
	return ctr.NetworkConnect(nameOrID, netName, netOpts)
}

// normalizeNetworkName takes a network name, a partial or a full network ID and returns the network name.
// If the network is not found a errors is returned.
func (r *Runtime) normalizeNetworkName(nameOrID string) (string, error) {
	net, err := r.network.NetworkInspect(nameOrID)
	if err != nil {
		return "", err
	}
	return net.Name, nil
}

// ocicniPortsToNetTypesPorts convert the old port format to the new one
// while deduplicating ports into ranges
func ocicniPortsToNetTypesPorts(ports []types.OCICNIPortMapping) []types.PortMapping {
	if len(ports) == 0 {
		return nil
	}

	newPorts := make([]types.PortMapping, 0, len(ports))

	// first sort the ports
	sort.Slice(ports, func(i, j int) bool {
		return compareOCICNIPorts(ports[i], ports[j])
	})

	// we already check if the slice is empty so we can use the first element
	currentPort := types.PortMapping{
		HostIP:        ports[0].HostIP,
		HostPort:      uint16(ports[0].HostPort),
		ContainerPort: uint16(ports[0].ContainerPort),
		Protocol:      ports[0].Protocol,
		Range:         1,
	}

	for i := 1; i < len(ports); i++ {
		if ports[i].HostIP == currentPort.HostIP &&
			ports[i].Protocol == currentPort.Protocol &&
			ports[i].HostPort-int32(currentPort.Range) == int32(currentPort.HostPort) &&
			ports[i].ContainerPort-int32(currentPort.Range) == int32(currentPort.ContainerPort) {
			currentPort.Range = currentPort.Range + 1
		} else {
			newPorts = append(newPorts, currentPort)
			currentPort = types.PortMapping{
				HostIP:        ports[i].HostIP,
				HostPort:      uint16(ports[i].HostPort),
				ContainerPort: uint16(ports[i].ContainerPort),
				Protocol:      ports[i].Protocol,
				Range:         1,
			}
		}
	}
	newPorts = append(newPorts, currentPort)
	return newPorts
}

// compareOCICNIPorts will sort the ocicni ports by
// 1) host ip
// 2) protocol
// 3) hostPort
// 4) container port
func compareOCICNIPorts(i, j types.OCICNIPortMapping) bool {
	if i.HostIP != j.HostIP {
		return i.HostIP < j.HostIP
	}

	if i.Protocol != j.Protocol {
		return i.Protocol < j.Protocol
	}

	if i.HostPort != j.HostPort {
		return i.HostPort < j.HostPort
	}

	return i.ContainerPort < j.ContainerPort
}
