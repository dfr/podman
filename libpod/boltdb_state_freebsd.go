//go:build freebsd
// +build freebsd

package libpod

// replaceNetNS handle network namespace transitions after updating a
// container's state.
func replaceNetNS(netNSPath string, ctr *Container, newState *ContainerState) error {
	if netNSPath != "" {
		// Check if the container's old state has a good netns
		if netNSPath == ctr.state.NetworkJail {
			newState.NetworkJail = ctr.state.NetworkJail
		} else {
			newState.NetworkJail = netNSPath
		}
	}
	return nil
}

// getNetNSPath retrieves the netns path to be stored in the database
func getNetNSPath(ctr *Container) string {
	return ctr.state.NetworkJail
}
