// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package devmapper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/hashicorp/go-multierror"
	"github.com/moby/moby/pkg/dmesg"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	metadataFileName = "metadata.db"
	mountsDirName    = "mounts"
	fsTypeExt4       = "ext4"
)

// devmapper implements containerd's snapshotter based on Linux device-mapper targets.
type devmapper struct {
	store  *storage.MetaStore
	pool   *PoolDevice
	config *Config
}

func NewSnapshotter(ctx context.Context, configPath string) (*devmapper, error) {
	log.G(ctx).WithField("cfg_path", configPath).Info("creating devmapper snapshotter")

	config, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	// Create directories needed for snapshotter
	for _, path := range []string{config.RootPath, filepath.Join(config.RootPath, mountsDirName)} {
		if err := os.MkdirAll(path, 0755); err != nil && !os.IsExist(err) {
			return nil, errors.Wrapf(err, "failed to create root directory: %s", path)
		}
	}

	store, err := storage.NewMetaStore(filepath.Join(config.RootPath, metadataFileName))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create metastore")
	}

	poolDevice, err := NewPoolDevice(ctx, config.PoolName, config.DataDevice, config.MetadataDevice, config.DataBlockSizeSectors)
	if err != nil {
		return nil, err
	}

	return &devmapper{
		store:  store,
		config: config,
		pool:   poolDevice,
	}, nil
}

func (dm *devmapper) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	log.G(ctx).WithField("key", key).Debug("stat")

	ctx, trans, err := dm.store.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Info{}, err
	}

	defer trans.Rollback()

	_, info, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (dm *devmapper) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	log.G(ctx).Debugf("update: %s", strings.Join(fieldpaths, ", "))

	ctx, trans, err := dm.store.TransactionContext(ctx, true)
	if err != nil {
		return snapshots.Info{}, err
	}

	info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
	if err != nil {
		return snapshots.Info{}, complete(ctx, trans, err)
	}

	return info, complete(ctx, trans, nil)
}

func (dm *devmapper) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	log.G(ctx).WithField("key", key).Debug("usage")

	return snapshots.Usage{}, errors.New("usage not implemented")
}

func (dm *devmapper) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	log.G(ctx).WithField("key", key).Debug("mounts")

	ctx, trans, err := dm.store.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}

	defer trans.Rollback()

	snap, err := storage.GetSnapshot(ctx, key)
	if err != nil {
		return nil, err
	}

	return dm.buildMounts(snap), nil
}

func (dm *devmapper) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(logrus.Fields{"key": key, "parent": parent}).Debug("prepare")
	return dm.createSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
}

func (dm *devmapper) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithFields(logrus.Fields{"key": key, "parent": parent}).Debug("prepare")
	return dm.createSnapshot(ctx, snapshots.KindView, key, parent, opts...)
}

func (dm *devmapper) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	log.G(ctx).WithFields(logrus.Fields{"name": name, "key": key}).Debug("commit")

	ctx, trans, err := dm.store.TransactionContext(ctx, true)
	if err != nil {
		return err
	}

	usage := snapshots.Usage{}
	if _, err := storage.CommitActive(ctx, key, name, usage, opts...); err != nil {
		return complete(ctx, trans, err)
	}

	return complete(ctx, trans, nil)
}

func (dm *devmapper) Remove(ctx context.Context, key string) error {
	log.G(ctx).WithField("key", key).Debug("remove")

	ctx, trans, err := dm.store.TransactionContext(ctx, true)
	if err != nil {
		return err
	}

	snapID, _, err := storage.Remove(ctx, key)
	if err != nil {
		return complete(ctx, trans, err)
	}

	deviceName := dm.getDeviceName(snapID)
	if err := dm.pool.RemoveDevice(deviceName); err != nil {
		log.G(ctx).WithError(err).Errorf("failed to remove device")
		return complete(ctx, trans, err)
	}

	return complete(ctx, trans, nil)
}

func (dm *devmapper) Walk(ctx context.Context, fn func(context.Context, snapshots.Info) error) error {
	log.G(ctx).Debug("walk")

	ctx, trans, err := dm.store.TransactionContext(ctx, false)
	if err != nil {
		return err
	}

	defer trans.Rollback()
	return storage.WalkInfo(ctx, fn)
}

func (dm *devmapper) Close() error {
	log.L.Debug("close")

	var result *multierror.Error

	if err := dm.store.Close(); err != nil {
		result = multierror.Append(result, err)
	}

	if err := dm.pool.Close(context.Background(), false, false); err != nil {
		result = multierror.Append(result, err)
	}

	return result.ErrorOrNil()
}

func (dm *devmapper) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	ctx, trans, err := dm.store.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	snap, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
	if err != nil {
		return nil, complete(ctx, trans, err)
	}

	if len(snap.ParentIDs) == 0 {
		deviceName := dm.getDeviceName(snap.ID)
		log.G(ctx).Debugf("creating new thin device '%s'", deviceName)

		deviceID, err := dm.pool.CreateThinDevice(deviceName, dm.config.BaseImageSizeBytes)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to create thin device for snapshot %s", snap.ID)
			return nil, complete(ctx, trans, err)
		}

		log.G(ctx).Debugf("created thin device with id %d", deviceID)
		if err := dm.mkfs(ctx, deviceName); err != nil {
			return nil, complete(ctx, trans, err)
		}
	} else {
		parentDeviceName := dm.getDeviceName(snap.ParentIDs[0])
		snapDeviceName := dm.getDeviceName(snap.ID)
		log.G(ctx).Debugf("creating snapshot device '%s' from '%s'", snapDeviceName, parentDeviceName)

		snapDeviceID, err := dm.pool.CreateSnapshotDevice(parentDeviceName, snapDeviceName, dm.config.BaseImageSizeBytes)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to create snapshot device from parent %s", parentDeviceName)
			return nil, complete(ctx, trans, err)
		}

		log.G(ctx).Debugf("created snapshot device with id %d", snapDeviceID)
	}

	mounts := dm.buildMounts(snap)

	// This is needed only to pass containerd's snapshotter suite tests
	_ = mount.WithTempMount(ctx, mounts, func(root string) error {
		return os.Remove(filepath.Join(root, "lost+found"))
	})

	return mounts, complete(ctx, trans, nil)
}

func (dm *devmapper) mkfs(ctx context.Context, deviceName string) error {
	args := []string{
		"-E",
		"nodiscard,lazy_itable_init=0,lazy_journal_init=0",
		dm.pool.GetDevicePath(deviceName),
	}

	log.G(ctx).Debugf("mkfs.ext4 %s", strings.Join(args, " "))
	output, err := exec.Command("mkfs.ext4", args...).CombinedOutput()
	if err != nil {
		log.G(ctx).WithError(err).Errorf(
			"failed to write fs: %s\ndmesg: %s\n",
			string(output),
			string(dmesg.Dmesg(256)))
		return err
	}

	log.G(ctx).Debugf("mkfs:\n%s", string(output))
	return nil
}

func (dm *devmapper) getMountDir(id string) string {
	return filepath.Join(dm.config.RootPath, mountsDirName, id)
}

func (dm *devmapper) getDeviceName(snapID string) string {
	// Add pool name as prefix to avoid collisions with devices from other pools
	return fmt.Sprintf("%s-snap-%s", dm.config.PoolName, snapID)
}

func (dm *devmapper) getDevicePath(snap storage.Snapshot) string {
	name := dm.getDeviceName(snap.ID)
	return dm.pool.GetDevicePath(name)
}

func (dm *devmapper) buildMounts(snap storage.Snapshot) []mount.Mount {
	var options []string

	if snap.Kind != snapshots.KindActive {
		options = append(options, "ro")
	}

	mounts := []mount.Mount{
		{
			Source:  dm.getDevicePath(snap),
			Type:    fsTypeExt4,
			Options: options,
		},
	}

	return mounts
}

func complete(ctx context.Context, trans storage.Transactor, err error) error {
	if err != nil {
		if terr := trans.Rollback(); terr != nil {
			log.G(ctx).WithError(terr).Error("failed to rollback transaction")
		}
	} else {
		if terr := trans.Commit(); terr != nil {
			log.G(ctx).WithError(terr).Error("failed to commit transaction")
		}
	}

	return err
}
