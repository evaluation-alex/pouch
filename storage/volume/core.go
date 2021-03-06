package volume

import (
	"fmt"
	"path"
	"reflect"
	"strings"

	metastore "github.com/alibaba/pouch/pkg/meta"
	"github.com/alibaba/pouch/storage/volume/driver"
	volerr "github.com/alibaba/pouch/storage/volume/error"
	"github.com/alibaba/pouch/storage/volume/types"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Core represents volume core struct.
type Core struct {
	Config
	store *metastore.Store
}

// NewCore returns Core struct instance with volume config.
func NewCore(cfg Config) (*Core, error) {
	c := &Core{Config: cfg}

	// initialize volume driver alias.
	if cfg.DriverAlias != "" {
		parts := strings.Split(cfg.DriverAlias, ";")
		for _, p := range parts {
			alias := strings.Split(p, "=")
			if len(alias) != 2 {
				return nil, errors.Errorf("invalid driver alias: %s", p)
			}

			if err := driver.Alias(alias[0], alias[1]); err != nil {
				return nil, errors.Wrapf(err, "failed to set driver alias: %s", p)
			}
		}
	}

	// initialize volume metadata store.
	volumeStore, err := metastore.NewStore(metastore.Config{
		Driver:  "boltdb",
		BaseDir: cfg.VolumeMetaPath,
		Buckets: []metastore.Bucket{
			{
				Name: "volume",
				Type: reflect.TypeOf(types.Volume{}),
			},
		},
	})
	if err != nil {
		logrus.Errorf("failed to create volume meta store: %v", err)
		return nil, err
	}
	c.store = volumeStore

	// set configure into each driver
	driverConfig := map[string]interface{}{
		"volume-meta-dir": path.Dir(cfg.VolumeMetaPath),
		"volume-timeout":  cfg.Timeout,
	}
	drivers, err := driver.GetAll()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get all volume driver")
	}
	for _, dv := range drivers {
		if d, ok := dv.(driver.Conf); ok {
			d.Config(driver.Contexts(), driverConfig)
		}
	}

	return c, nil
}

// GetVolume return a volume's info with specified name, If not errors.
func (c *Core) GetVolume(id types.VolumeID) (*types.Volume, error) {
	ctx := driver.Contexts()

	// first, try to get volume from local store.
	obj, err := c.store.Get(id.Name)
	if err == nil {
		v, ok := obj.(*types.Volume)
		if !ok {
			return nil, volerr.ErrVolumeNotFound
		}

		// get the volume driver.
		dv, err := driver.Get(v.Spec.Backend)
		if err != nil {
			return nil, err
		}

		// if the driver implements Getter interface.
		if d, ok := dv.(driver.Getter); ok {
			curV, err := d.Get(ctx, id.Name)
			if err != nil {
				return nil, volerr.ErrVolumeNotFound
			}

			v.Status.MountPoint = curV.Status.MountPoint
		}

		return v, nil
	}

	if err != metastore.ErrObjectNotFound {
		return nil, err
	}

	// scan all drivers
	logrus.Debugf("probing all drivers for volume with name(%s)", id.Name)
	drivers, err := driver.GetAll()
	if err != nil {
		return nil, err
	}

	for _, dv := range drivers {
		d, ok := dv.(driver.Getter)
		if !ok {
			continue
		}

		v, err := d.Get(ctx, id.Name)
		if err != nil {
			// not found, ignore it
			continue
		}

		// store volume meta
		if err := c.store.Put(v); err != nil {
			return nil, err
		}

		return v, nil
	}

	return nil, volerr.ErrVolumeNotFound
}

// ExistVolume return 'true' if volume be found and not errors.
func (c *Core) ExistVolume(id types.VolumeID) (bool, error) {
	_, err := c.GetVolume(id)
	if err != nil {
		if ec, ok := err.(volerr.CoreError); ok && ec.IsVolumeNotFound() {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CreateVolume use to create a volume, if failed, will return error info.
func (c *Core) CreateVolume(id types.VolumeID) (*types.Volume, error) {
	exist, err := c.ExistVolume(id)
	if err != nil {
		return nil, err
	} else if exist {
		return nil, volerr.ErrVolumeExisted
	}

	dv, err := driver.Get(id.Driver)
	if err != nil {
		return nil, err
	}

	volume, err := dv.Create(driver.Contexts(), id)
	if err != nil {
		return nil, err
	}

	// create the meta
	if err := c.store.Put(volume); err != nil {
		return nil, err
	}

	return volume, nil
}

// ListVolumes return all volumes.
// Param 'labels' use to filter the volumes, only return those you want.
func (c *Core) ListVolumes(labels map[string]string) ([]*types.Volume, error) {
	var retVolumes = make([]*types.Volume, 0)

	// list local meta store.
	metaList, err := c.store.List()
	if err != nil {
		return nil, err
	}

	// scan all drivers.
	logrus.Debugf("probing all drivers for listing volume")
	drivers, err := driver.GetAll()
	if err != nil {
		return nil, err
	}

	ctx := driver.Contexts()

	var realVolumes = map[string]*types.Volume{}
	var volumeDrivers = map[string]driver.Driver{}

	for _, dv := range drivers {
		volumeDrivers[dv.Name(ctx)] = dv

		d, ok := dv.(driver.Lister)
		if !ok {
			// not Lister, ignore it.
			continue
		}
		vList, err := d.List(ctx)
		if err != nil {
			logrus.Warnf("volume driver %s list error: %v", dv.Name(ctx), err)
			continue
		}

		for _, v := range vList {
			realVolumes[v.Name] = v
		}
	}

	for name, obj := range metaList {
		v, ok := obj.(*types.Volume)
		if !ok {
			continue
		}

		d, ok := volumeDrivers[v.Spec.Backend]
		if !ok {
			// driver not exist, ignore it
			continue
		}

		// the local driver and tmpfs driver
		if d.StoreMode(ctx).IsLocal() {
			retVolumes = append(retVolumes, v)
			continue
		}

		rv, ok := realVolumes[name]
		if !ok {
			// real volume not exist, ignore it
			continue
		}
		v.Status.MountPoint = rv.Status.MountPoint

		delete(realVolumes, name)

		retVolumes = append(retVolumes, v)
	}

	for _, v := range realVolumes {
		// found new volumes, store the meta
		logrus.Warningf("found new volume %s", v.Name)
		c.store.Put(v)

		retVolumes = append(retVolumes, v)

	}

	return retVolumes, nil
}

// ListVolumeName return the name of all volumes only.
// Param 'labels' use to filter the volume's names, only return those you want.
func (c *Core) ListVolumeName(labels map[string]string) ([]string, error) {
	var names []string

	volumes, err := c.ListVolumes(labels)
	if err != nil {
		return names, err
	}

	for _, v := range volumes {
		names = append(names, v.Name)
	}

	return names, nil
}

// RemoveVolume remove volume from storage and meta information, if not success return error.
func (c *Core) RemoveVolume(id types.VolumeID) error {
	v, dv, err := c.GetVolumeDriver(id)
	if err != nil {
		return errors.Wrap(err, "Remove volume: "+id.String())
	}

	// Call driver's Remove method to remove the volume.
	if err := dv.Remove(driver.Contexts(), v); err != nil {
		return err
	}

	// delete the meta
	if err := c.store.Remove(id.Name); err != nil {
		return err
	}

	return nil
}

// VolumePath return the path of volume on node host.
func (c *Core) VolumePath(id types.VolumeID) (string, error) {
	v, dv, err := c.GetVolumeDriver(id)
	if err != nil {
		return "", errors.Wrap(err, fmt.Sprintf("Get volume: %s path", id.String()))
	}

	return c.volumePath(v, dv)
}

// GetVolumeDriver return the backend driver and volume with specified volume's id.
func (c *Core) GetVolumeDriver(id types.VolumeID) (*types.Volume, driver.Driver, error) {
	v, err := c.GetVolume(id)
	if err != nil {
		return nil, nil, err
	}
	dv, err := driver.Get(v.Spec.Backend)
	if err != nil {
		return nil, nil, errors.Errorf("failed to get backend driver %s: %v", v.Spec.Backend, err)
	}
	return v, dv, nil
}

// AttachVolume to enable a volume on local host.
func (c *Core) AttachVolume(id types.VolumeID, extra map[string]string) (*types.Volume, error) {
	v, dv, err := c.GetVolumeDriver(id)
	if err != nil {
		return nil, err
	}

	ctx := driver.Contexts()

	// merge extra to volume spec extra.
	for key, value := range extra {
		v.Spec.Extra[key] = value
	}

	if d, ok := dv.(driver.AttachDetach); ok {
		if err := d.Attach(ctx, v); err != nil {
			return nil, err
		}
	}

	// update meta info.
	if err := c.store.Put(v); err != nil {
		return nil, err
	}

	return v, nil
}

// DetachVolume to disable a volume on local host.
func (c *Core) DetachVolume(id types.VolumeID, extra map[string]string) (*types.Volume, error) {
	v, dv, err := c.GetVolumeDriver(id)
	if err != nil {
		return nil, err
	}

	ctx := driver.Contexts()

	// merge extra to volume spec extra.
	for key, value := range extra {
		v.Spec.Extra[key] = value
	}

	// if volume has referance, skip to detach volume.
	ref := v.Option(types.OptionRef)
	if d, ok := dv.(driver.AttachDetach); ok && ref == "" {
		if err := d.Detach(ctx, v); err != nil {
			return nil, err
		}
	}

	// update meta info.
	if err := c.store.Put(v); err != nil {
		return nil, err
	}

	return v, nil
}
