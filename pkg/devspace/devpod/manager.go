package devpod

import (
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/devspace/services/podreplace"
	"github.com/loft-sh/devspace/pkg/util/lockfactory"
	logpkg "github.com/loft-sh/devspace/pkg/util/log"
	"github.com/loft-sh/devspace/pkg/util/stringutil"
	"sync"
	"time"
)

type Manager interface {
	// StartMultiple will start multiple or all dev pods
	StartMultiple(ctx *devspacecontext.Context, devPods []string) error

	// Start will start a DevPod if it is not yet started
	Start(ctx *devspacecontext.Context, devPod *latest.DevPod) error

	// Reset will stop the DevPod if it exists and reset the replaced pods
	Reset(ctx *devspacecontext.Context, name string) error

	// Stop will stop the DevPod
	Stop(name string)

	// Wait will wait until all DevPods are stopped
	Wait()
}

type devPodManager struct {
	lockFactory lockfactory.LockFactory

	mapLock sync.Mutex

	devPods     map[string]*devPod
	restartPods map[string]bool
}

func NewManager() Manager {
	return &devPodManager{
		lockFactory: lockfactory.NewDefaultLockFactory(),
		devPods:     map[string]*devPod{},
	}
}

func (d *devPodManager) StartMultiple(ctx *devspacecontext.Context, devPods []string) error {
	ctx, tomb := ctx.WithNewTomb()
	tomb.Go(func() error {
		for devPodName, devPod := range ctx.Config.Config().Dev {
			if len(devPods) > 0 && !stringutil.Contains(devPods, devPodName) {
				continue
			}

			func(devPod *latest.DevPod) {
				tomb.Go(func() error {
					return d.Start(ctx, devPod)
				})
			}(devPod)
		}
		return nil
	})

	return tomb.Wait()
}

type DevPodAlreadyExists struct{}

func (DevPodAlreadyExists) Error() string {
	return "dev pod already exists, please make sure to stop the dev pod before rerunning it"
}

func (d *devPodManager) Wait() {
	devPods := map[string]*devPod{}
	d.mapLock.Lock()
	for k, v := range d.devPods {
		devPods[k] = v
	}
	d.mapLock.Unlock()

	for _, dp := range devPods {
		<-dp.Done()
	}
}

func (d *devPodManager) Start(originalContext *devspacecontext.Context, devPodConfig *latest.DevPod) error {
	lock := d.lockFactory.GetLock(devPodConfig.Name)
	lock.Lock()
	defer lock.Unlock()

	var dp *devPod
	d.mapLock.Lock()
	dp = d.devPods[devPodConfig.Name]
	d.mapLock.Unlock()

	// create a DevPod logger
	prefix := devPodConfig.Name
	unionLogger := logpkg.NewUnionLogger(logpkg.NewDefaultPrefixLogger(prefix, originalContext.Log.WithoutPrefix()), logpkg.GetDevPodFileLogger(prefix))

	// check if already running
	if dp != nil && dp.Alive() {
		return DevPodAlreadyExists{}
	}

	// create a new dev pod
	dp = newDevPod()
	d.mapLock.Lock()
	d.devPods[devPodConfig.Name] = dp
	d.mapLock.Unlock()

	// start the dev pod
	err := dp.Start(originalContext.WithLogger(unionLogger), devPodConfig)
	if err != nil {
		return err
	}

	// restart dev pod if necessary
	go func() {
		<-dp.Done()
		if originalContext.IsDone() {
			return
		}

		// try restarting the dev pod if it has stopped because of
		// a lost connection
		if _, ok := dp.Err().(DevPodLostConnection); ok {
			for {
				err = d.Start(originalContext, devPodConfig)
				if err != nil {
					if originalContext.IsDone() {
						return
					} else if _, ok := err.(DevPodAlreadyExists); ok {
						return
					}

					originalContext.Log.Infof("Restart dev %s because of: %v", devPodConfig.Name, err)
					time.Sleep(time.Second * 10)
					continue
				}

				return
			}
		}
	}()

	return nil
}

func (d *devPodManager) Reset(ctx *devspacecontext.Context, name string) error {
	lock := d.lockFactory.GetLock(name)
	lock.Lock()
	defer lock.Unlock()

	d.mapLock.Lock()
	defer d.mapLock.Unlock()

	d.stop(name)
	devPod, ok := ctx.Config.RemoteCache().GetDevPod(name)
	if ok {
		_, err := podreplace.NewPodReplacer().RevertReplacePod(ctx, &devPod)
		return err
	}

	return nil
}

func (d *devPodManager) Stop(name string) {
	lock := d.lockFactory.GetLock(name)
	lock.Lock()
	defer lock.Unlock()

	d.mapLock.Lock()
	defer d.mapLock.Unlock()

	d.stop(name)
}

func (d *devPodManager) stop(name string) {
	dp := d.devPods[name]
	if dp == nil {
		return
	}

	// stop the dev pod
	dp.Stop()
	delete(d.devPods, name)
}
