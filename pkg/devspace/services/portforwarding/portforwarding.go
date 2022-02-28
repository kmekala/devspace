package portforwarding

import (
	"fmt"
	"github.com/loft-sh/devspace/pkg/devspace/config/loader"
	"github.com/loft-sh/devspace/pkg/util/tomb"
	"strconv"
	"strings"
	"time"

	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/devspace/hook"
	"github.com/loft-sh/devspace/pkg/devspace/services/sync"
	"github.com/loft-sh/devspace/pkg/devspace/services/targetselector"
	logpkg "github.com/loft-sh/devspace/pkg/util/log"
	"github.com/loft-sh/devspace/pkg/util/port"
	"github.com/pkg/errors"
)

// StartPortForwarding starts the port forwarding functionality
func StartPortForwarding(ctx *devspacecontext.Context, devPod *latest.DevPod, selector targetselector.TargetSelector, parent *tomb.Tomb) (retErr error) {
	if ctx == nil || ctx.Config == nil || ctx.Config.Config() == nil {
		return fmt.Errorf("DevSpace config is not set")
	}

	// forward
	initDoneArray := []chan struct{}{}
	if len(devPod.Forward) > 0 {
		initDone := make(chan struct{})
		initDoneArray = append(initDoneArray, initDone)
		parent.Go(func() error {
			defer close(initDone)

			return startPortForwardingWithHooks(ctx, devPod.Name, devPod.Forward, selector, parent)
		})
	}

	// reverse
	loader.EachDevContainer(devPod, func(devContainer *latest.DevContainer) bool {
		if len(devPod.PortMappingsReverse) > 0 {
			initDone := make(chan struct{})
			initDoneArray = append(initDoneArray, initDone)
			parent.Go(func() error {
				defer close(initDone)

				return startReversePortForwardingWithHooks(ctx, devPod.Name, string(devContainer.Arch), devContainer.PortMappingsReverse, selector.WithContainer(devContainer.Container), parent)
			})
		}
		return true
	})

	// wait until everything is initialized
	for _, initDone := range initDoneArray {
		<-initDone
	}
	return nil
}

func startReversePortForwardingWithHooks(ctx *devspacecontext.Context, name, arch string, portMappings []*latest.PortMapping, selector targetselector.TargetSelector, parent *tomb.Tomb) error {
	pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
		"reverse_port_forwarding_config": portMappings,
	}, hook.EventsForSingle("start:reversePortForwarding", name).With("reversePortForwarding.start")...)
	if pluginErr != nil {
		return pluginErr
	}

	// start reverse port forwarding
	err := startReversePortForwarding(ctx, name, arch, portMappings, selector, parent)
	if err != nil {
		pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
			"reverse_port_forwarding_config": portMappings,
			"error":                          err,
		}, hook.EventsForSingle("error:reversePortForwarding", name).With("reversePortForwarding.error")...)
		if pluginErr != nil {
			return pluginErr
		}

		return err
	}

	return nil
}

func startPortForwardingWithHooks(ctx *devspacecontext.Context, name string, portMappings []*latest.PortMapping, selector targetselector.TargetSelector, parent *tomb.Tomb) error {
	pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
		"port_forwarding_config": portMappings,
	}, hook.EventsForSingle("start:portForwarding", name).With("portForwarding.start")...)
	if pluginErr != nil {
		return pluginErr
	}

	// start port forwarding
	err := startForwarding(ctx, name, portMappings, selector, parent)
	if err != nil {
		pluginErr := hook.ExecuteHooks(ctx, map[string]interface{}{
			"port_forwarding_config": portMappings,
			"error":                  err,
		}, hook.EventsForSingle("error:portForwarding", name).With("portForwarding.error")...)
		if pluginErr != nil {
			return pluginErr
		}

		return err
	}

	return nil
}

func startForwarding(ctx *devspacecontext.Context, name string, portMappings []*latest.PortMapping, selector targetselector.TargetSelector, parent *tomb.Tomb) error {
	if ctx.IsDone() {
		return nil
	}

	// start port forwarding
	pod, err := selector.SelectSinglePod(ctx.Context, ctx.KubeClient, ctx.Log)
	if err != nil {
		return errors.Wrap(err, "error selecting pod")
	} else if pod == nil {
		return nil
	}

	ports := make([]string, len(portMappings))
	addresses := make([]string, len(portMappings))
	for index, value := range portMappings {
		if value.LocalPort == nil {
			return errors.Errorf("port is not defined in portmapping %d", index)
		}

		localPort := strconv.Itoa(*value.LocalPort)
		remotePort := localPort
		if value.RemotePort != nil {
			remotePort = strconv.Itoa(*value.RemotePort)
		}

		open, _ := port.Check(*value.LocalPort)
		if !open {
			ctx.Log.Warnf("Seems like port %d is already in use. Is another application using that port?", *value.LocalPort)
		}

		ports[index] = localPort + ":" + remotePort
		if value.BindAddress == "" {
			addresses[index] = "localhost"
		} else {
			addresses[index] = value.BindAddress
		}
	}

	readyChan := make(chan struct{})
	errorChan := make(chan error)
	pf, err := ctx.KubeClient.NewPortForwarder(pod, ports, addresses, make(chan struct{}), readyChan, errorChan)
	if err != nil {
		return errors.Errorf("Error starting port forwarding: %v", err)
	}

	go func() {
		err := pf.ForwardPorts(ctx.Context)
		if err != nil {
			errorChan <- err
		}
	}()

	// Wait till forwarding is ready
	select {
	case <-ctx.Context.Done():
		return nil
	case <-readyChan:
		ctx.Log.Donef("Port forwarding started on %s (%s/%s)", strings.Join(ports, ", "), pod.Namespace, pod.Name)
	case err := <-errorChan:
		return errors.Wrap(err, "forward ports")
	case <-time.After(20 * time.Second):
		return errors.Errorf("Timeout waiting for port forwarding to start")
	}

	parent.Go(func() error {
		fileLog := logpkg.GetDevPodFileLogger(name)
		select {
		case <-ctx.Context.Done():
			pf.Close()
			stopPortForwarding(ctx, name, portMappings, fileLog, parent)
		case err := <-errorChan:
			if err != nil {
				fileLog.Errorf("Portforwarding restarting, because: %v", err)
				sync.PrintPodError(ctx.Context, ctx.KubeClient, pod, fileLog)
				pf.Close()
				hook.LogExecuteHooks(ctx.WithLogger(fileLog), map[string]interface{}{
					"port_forwarding_config": portMappings,
					"error":                  err,
				}, hook.EventsForSingle("restart:portForwarding", name).With("portForwarding.restart")...)

				for {
					err = startForwarding(ctx.WithLogger(fileLog), name, portMappings, selector, parent)
					if err != nil {
						hook.LogExecuteHooks(ctx.WithLogger(fileLog), map[string]interface{}{
							"port_forwarding_config": portMappings,
							"error":                  err,
						}, hook.EventsForSingle("restart:portForwarding", name).With("portForwarding.restart")...)
						fileLog.Errorf("Error restarting port-forwarding: %v", err)
						fileLog.Errorf("Will try again in 15 seconds")

						select {
						case <-time.After(time.Second * 15):
							continue
						case <-ctx.Context.Done():
							stopPortForwarding(ctx, name, portMappings, fileLog, parent)
							return nil
						}
					}

					break
				}
			}
		}
		return nil
	})

	return nil
}

func stopPortForwarding(ctx *devspacecontext.Context, name string, portMappings []*latest.PortMapping, fileLog logpkg.Logger, parent *tomb.Tomb) {
	hook.LogExecuteHooks(ctx.WithLogger(fileLog), map[string]interface{}{
		"port_forwarding_config": portMappings,
	}, hook.EventsForSingle("stop:portForwarding", name).With("portForwarding.stop")...)
	parent.Kill(nil)
	fileLog.Done("Stopped port forwarding")
}
