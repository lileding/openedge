package native

import (
	"fmt"
	"github.com/baetyl/baetyl-go/v2/utils"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	v1 "github.com/baetyl/baetyl-go/v2/spec/v1"
	"github.com/baetyl/baetyl/ami"
	"github.com/baetyl/baetyl/config"
	"github.com/baetyl/baetyl/program"
	"github.com/kardianos/service"
	"gopkg.in/yaml.v2"
)

func init() {
	ami.Register("native", newNativeImpl)
}

type nativeImpl struct {
	log *log.Logger
}

func newNativeImpl(cfg config.AmiConfig) (ami.AMI, error) {
	return &nativeImpl{
		log: log.With(log.Any("ami", "native")),
	}, nil
}

func (impl *nativeImpl) ApplyApp(ns string, app v1.Application, configs map[string]v1.Configuration, secrets map[string]v1.Secret) error {
	appDir := filepath.Join(runRootPath, ns, app.Name, app.Version)
	err := os.MkdirAll(appDir, 0755)
	if err != nil {
		return errors.Trace(err)
	}
	avs := map[string]v1.Volume{}
	for _, v := range app.Volumes {
		avs[v.Name] = v
	}
	for _, s := range app.Services {
		for i := 1; i <= s.Replica; i++ {
			// generate instance path
			insDir := filepath.Join(appDir, s.Name, strconv.Itoa(i))
			if err = os.MkdirAll(insDir, 0755); err != nil {
				return errors.Trace(err)
			}

			// apply configuration
			for _, vm := range s.VolumeMounts {
				av, ok := avs[vm.Name]
				if !ok {
					return errors.Errorf("volume (%s) not found in app volumes", vm.Name)
				}
				if av.HostPath != nil {
					os.Symlink(av.HostPath.Path, filepath.Join(insDir, vm.MountPath))
				} else if av.Config != nil {
					vc := configs[av.Config.Name]
					for name, data := range vc.Data {
						file := filepath.Join(insDir, vm.MountPath, name)
						if err = ioutil.WriteFile(file, []byte(data), 0755); err != nil {
							return errors.Trace(err)
						}
					}
				} else if av.Secret != nil {
					vs := secrets[av.Config.Name]
					for name, data := range vs.Data {
						file := filepath.Join(insDir, vm.MountPath, name)
						if err = ioutil.WriteFile(file, data, 0755); err != nil {
							return errors.Trace(err)
						}
					}
				}
			}

			// apply service
			var env []string
			for _, item := range s.Env {
				env = append(env, fmt.Sprintf("%s=%s", item.Name, item.Value))
			}
			prgCfg := program.Config{
				Name:        fmt.Sprintf("%s.%s.%s.%d", app.Name, app.Version, s.Name, i),
				DisplayName: fmt.Sprintf("%s %s", app.Name, s.Name),
				Description: app.Description,
				Dir:         insDir,
				Exec:        s.Image,
				Args:        s.Args,
				Env:         env,
				Logger: log.Config{
					Level:    "debug",
					Filename: filepath.Join(logRootPath, ns, app.Name, app.Version, fmt.Sprintf("%s-%d.log", s.Name, i)),
				},
			}
			prgYml, err := yaml.Marshal(prgCfg)
			if err != nil {
				return errors.Trace(err)
			}
			err = ioutil.WriteFile(filepath.Join(insDir, program.DefaultProgramYaml), prgYml, 0755)
			if err != nil {
				return errors.Trace(err)
			}
			svc, err := service.New(nil, &service.Config{
				Name:             prgCfg.Name,
				Description:      prgCfg.Description,
				WorkingDirectory: insDir,
				Arguments:        []string{"program"},
			})
			if err = svc.Install(); err != nil {
				return errors.Trace(err)
			}
		}
	}
	return nil
}

func (impl *nativeImpl) DeleteApp(ns string, appName string) error {
	// scan app version
	curAppDir := filepath.Join(runRootPath, ns, appName)
	appVerFiles, err := ioutil.ReadDir(curAppDir)
	if err != nil {
		return errors.Trace(err)
	}
	for _, appVerFile := range appVerFiles {
		if !appVerFile.IsDir() {
			continue
		}
		// scan service
		curAppVer := appVerFile.Name()
		curAppVerDir := filepath.Join(curAppDir, curAppVer)
		svcFiles, err := ioutil.ReadDir(curAppVerDir)
		if err != nil {
			return errors.Trace(err)
		}
		for _, svcFile := range svcFiles {
			if !svcFile.IsDir() {
				continue
			}
			// scan service instance
			curSvcName := svcFile.Name()
			curSvcDir := filepath.Join(curAppVerDir, curSvcName)
			svcInsFiles, err := ioutil.ReadDir(curSvcDir)
			if err != nil {
				return errors.Trace(err)
			}
			for _, svcInsFile := range svcInsFiles {
				if !svcInsFile.IsDir() {
					continue
				}
				curSvcIns := svcInsFile.Name()
				curSvcInsDir := filepath.Join(curSvcDir, curSvcIns)
				svc, err := service.New(nil, &service.Config{
					Name:             fmt.Sprintf("%s.%s.%s.%s", appName, curAppVer, curSvcName, curSvcIns),
					WorkingDirectory: svcInsFile.Name(),
				})
				if err = svc.Uninstall(); err != nil {
					return errors.Trace(err)
				}
				err = os.RemoveAll(curSvcInsDir)
				if err != nil {
					return errors.Trace(err)
				}
			}
			err = os.RemoveAll(curSvcDir)
			if err != nil {
				return errors.Trace(err)
			}
		}
		err = os.RemoveAll(curAppVerDir)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return errors.Trace(os.RemoveAll(curAppDir))
}

func (impl *nativeImpl) StatsApps(ns string) ([]v1.AppStats, error) {
	var stats []v1.AppStats
	if utils.DirExists(runRootPath) {
		return stats, nil
	}

	curNsPath := filepath.Join(runRootPath, ns)
	if utils.DirExists(curNsPath) {
		return stats, nil
	}

	// scan app
	appFiles, err := ioutil.ReadDir(curNsPath)
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, appFile := range appFiles {
		if !appFile.IsDir() {
			continue
		}
		curAppName := appFile.Name()
		curAppPath := filepath.Join(curNsPath, curAppName)
		if utils.DirExists(curAppPath) {
			continue
		}
		// scan app version
		appVerFiles, err := ioutil.ReadDir(curAppPath)
		if err != nil {
			return nil, errors.Trace(err)
		}
		for _, appVerFile := range appVerFiles {
			if !appVerFile.IsDir() {
				continue
			}

			curAppStats := v1.AppStats{}
			curAppStats.Name = appFile.Name()
			curAppStats.Version = appVerFile.Name()
			curAppStats.InstanceStats = map[string]v1.InstanceStats{}
			stats = append(stats, curAppStats)

			curAppVer := appVerFile.Name()
			curAppVerPath := filepath.Join(curAppPath, curAppVer)
			if utils.DirExists(curAppVerPath) {
				continue
			}
			// scan service
			svcFiles, err := ioutil.ReadDir(curAppVerPath)
			if err != nil {
				return nil, errors.Trace(err)
			}
			for _, svcFile := range svcFiles {
				if !svcFile.IsDir() {
					continue
				}

				curSvcName := svcFile.Name()
				curSvcPath := filepath.Join(curAppVerPath, curSvcName)
				if utils.DirExists(curSvcPath) {
					continue
				}
				// scan service instance
				svcInsFiles, err := ioutil.ReadDir(curSvcPath)
				if err != nil {
					return nil, errors.Trace(err)
				}
				for _, svcInsFile := range svcInsFiles {
					if !svcInsFile.IsDir() {
						continue
					}

					curSvcIns := svcInsFile.Name()
					curPrgName := fmt.Sprintf("%s.%s.%s.%s", curAppName, curAppVer, curSvcName, curSvcIns)
					svc, err := service.New(nil, &service.Config{
						Name:             curPrgName,
						WorkingDirectory: svcInsFile.Name(),
					})
					curInsStats := v1.InstanceStats{
						ServiceName: curSvcName,
						Name:        curPrgName,
					}
					status, err := svc.Status()
					if err != nil {
						curInsStats.Status = v1.Unknown
						curInsStats.Cause = err.Error()
					} else {
						curInsStats.Status = prgStatusToSpecStatus(status)
					}
					curAppStats.InstanceStats[curPrgName] = curInsStats
				}
			}
		}
	}
	return stats, nil
}

func prgStatusToSpecStatus(status service.Status) v1.Status {
	switch status {
	case service.StatusRunning:
		return v1.Running
	case service.StatusStopped:
		return v1.Pending
	default:
		return v1.Unknown
	}
}

// TODO: remove
func (impl *nativeImpl) CollectNodeInfo() (*v1.NodeInfo, error) {
	return &v1.NodeInfo{
		Arch: runtime.GOARCH,
		OS:   runtime.GOOS,
	}, nil
}

func (impl *nativeImpl) CollectNodeStats() (*v1.NodeStats, error) {
	return &v1.NodeStats{
		Usage:    map[string]string{},
		Capacity: map[string]string{},
	}, nil
}

func (impl *nativeImpl) FetchLog(namespace, service string, tailLines, sinceSeconds int64) (io.ReadCloser, error) {
	panic("implement me")
}