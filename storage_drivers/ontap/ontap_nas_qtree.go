// Copyright 2018 NetApp, Inc. All Rights Reserved.

package ontap

import (
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	trident "github.com/netapp/trident/config"
	"github.com/netapp/trident/storage"
	sa "github.com/netapp/trident/storage_attribute"
	drivers "github.com/netapp/trident/storage_drivers"
	"github.com/netapp/trident/storage_drivers/ontap/api"
	"github.com/netapp/trident/storage_drivers/ontap/api/azgo"
	"github.com/netapp/trident/utils"
)

const deletedQtreeNamePrefix = "deleted_"
const maxQtreeNameLength = 64
const maxQtreesPerFlexvol = 200
const defaultPruneFlexvolsPeriodSecs = uint64(600) // default to 10 minutes
const defaultResizeQuotasPeriodSecs = uint64(60)   // default to 1 minute

// For legacy reasons, these strings mustn't change
const (
	artifactPrefixDocker     = "ndvp"
	artifactPrefixKubernetes = "trident"
)

// NASQtreeStorageDriver is for NFS storage provisioning of qtrees
type NASQtreeStorageDriver struct {
	initialized         bool
	Config              drivers.OntapStorageDriverConfig
	API                 *api.Client
	Telemetry           *Telemetry
	quotaResizeMap      map[string]bool
	provMutex           *sync.Mutex
	flexvolNamePrefix   string
	flexvolExportPolicy string
	housekeepingTasks   map[string]*time.Ticker
}

func (d *NASQtreeStorageDriver) GetConfig() *drivers.OntapStorageDriverConfig {
	return &d.Config
}

func (d *NASQtreeStorageDriver) GetAPI() *api.Client {
	return d.API
}

func (d *NASQtreeStorageDriver) GetTelemetry() *Telemetry {
	return d.Telemetry
}

// Name is for returning the name of this driver
func (d *NASQtreeStorageDriver) Name() string {
	return drivers.OntapNASQtreeStorageDriverName
}

func (d *NASQtreeStorageDriver) FlexvolNamePrefix() string {
	return d.flexvolNamePrefix
}

// Initialize from the provided config
func (d *NASQtreeStorageDriver) Initialize(
	context trident.DriverContext, configJSON string, commonConfig *drivers.CommonStorageDriverConfig,
) error {

	if commonConfig.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "Initialize", "Type": "NASQtreeStorageDriver"}
		log.WithFields(fields).Debug(">>>> Initialize")
		defer log.WithFields(fields).Debug("<<<< Initialize")
	}

	// Parse the config
	config, err := InitializeOntapConfig(context, configJSON, commonConfig)
	if err != nil {
		return fmt.Errorf("error initializing %s driver: %v", d.Name(), err)
	}

	d.API, err = InitializeOntapDriver(config)
	if err != nil {
		return fmt.Errorf("error initializing %s driver: %v", d.Name(), err)
	}
	d.Config = *config

	// Remap context for artifact naming so the names remain stable over time
	var artifactPrefix string
	switch context {
	case trident.ContextDocker:
		artifactPrefix = artifactPrefixDocker
	case trident.ContextKubernetes:
		artifactPrefix = artifactPrefixKubernetes
	}

	// Set up internal driver state
	d.quotaResizeMap = make(map[string]bool)
	d.provMutex = &sync.Mutex{}
	d.flexvolNamePrefix = fmt.Sprintf("%s_qtree_pool_%s_", artifactPrefix, *d.Config.StoragePrefix)
	d.flexvolNamePrefix = strings.Replace(d.flexvolNamePrefix, "__", "_", -1)
	d.flexvolExportPolicy = fmt.Sprintf("%s_qtree_pool_export_policy", artifactPrefix)

	log.WithFields(log.Fields{
		"FlexvolNamePrefix":   d.flexvolNamePrefix,
		"FlexvolExportPolicy": d.flexvolExportPolicy,
	}).Debugf("Qtree driver settings.")

	err = d.validate()
	if err != nil {
		return fmt.Errorf("error validating %s driver: %v", d.Name(), err)
	}

	// Ensure all quotas are in force after a driver restart
	d.queueAllFlexvolsForQuotaResize()

	// Do periodic housekeeping like cleaning up unused Flexvols
	d.startHousekeepingTasks()

	d.initialized = true
	return nil
}

func (d *NASQtreeStorageDriver) Initialized() bool {
	return d.initialized
}

func (d *NASQtreeStorageDriver) Terminate() {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "Terminate", "Type": "NASQtreeStorageDriver"}
		log.WithFields(fields).Debug(">>>> Terminate")
		defer log.WithFields(fields).Debug("<<<< Terminate")
	}

	// Stop housekeeping tasks
	for taskName, ticker := range d.housekeepingTasks {
		ticker.Stop()
		log.WithField("task", taskName).Debug("Stopped housekeeping task.")
	}

	// Run the housekeeping tasks one last time
	d.pruneUnusedFlexvols()
	d.reapDeletedQtrees()
	d.resizeQuotas()

	d.initialized = false
}

// Validate the driver configuration and execution environment
func (d *NASQtreeStorageDriver) validate() error {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "validate", "Type": "NASQtreeStorageDriver"}
		log.WithFields(fields).Debug(">>>> validate")
		defer log.WithFields(fields).Debug("<<<< validate")
	}

	err := ValidateNASDriver(d.API, &d.Config)
	if err != nil {
		return fmt.Errorf("driver validation failed: %v", err)
	}

	// Make sure we have an export policy for all the Flexvols we create
	err = d.ensureDefaultExportPolicy()
	if err != nil {
		return fmt.Errorf("error configuring export policy: %v", err)
	}

	return nil
}

func (d *NASQtreeStorageDriver) startHousekeepingTasks() {

	d.housekeepingTasks = make(map[string]*time.Ticker)

	// Send EMS message on a configurable schedule
	d.Telemetry = InitializeOntapTelemetry(d)
	StartEmsHeartbeat(d)

	// Read background task timings from config file, use defaults if missing or invalid
	pruneFlexvolsPeriodSecs := defaultPruneFlexvolsPeriodSecs
	if d.Config.QtreePruneFlexvolsPeriod != "" {
		i, err := strconv.ParseUint(d.Config.QtreePruneFlexvolsPeriod, 10, 64)
		if err != nil {
			log.WithField("interval", d.Config.QtreePruneFlexvolsPeriod).Warnf(
				"Invalid Flexvol pruning interval. %v", err)
		} else {
			pruneFlexvolsPeriodSecs = i
		}
	}
	log.WithFields(log.Fields{
		"IntervalSeconds": pruneFlexvolsPeriodSecs,
	}).Debug("Configured Flexvol pruning period.")

	resizeQuotasPeriodSecs := defaultResizeQuotasPeriodSecs
	if d.Config.QtreeQuotaResizePeriod != "" {
		i, err := strconv.ParseUint(d.Config.QtreeQuotaResizePeriod, 10, 64)
		if err != nil {
			log.WithField("interval", d.Config.QtreeQuotaResizePeriod).Warnf(
				"Invalid quota resize interval. %v", err)
		} else {
			resizeQuotasPeriodSecs = i
		}
	}
	log.WithFields(log.Fields{
		"IntervalSeconds": resizeQuotasPeriodSecs,
	}).Debug("Configured quota resize period.")

	// Keep the system devoid of Flexvols with no qtrees
	d.pruneUnusedFlexvols()
	d.reapDeletedQtrees()
	pruneTicker := time.NewTicker(time.Duration(pruneFlexvolsPeriodSecs) * time.Second)
	d.housekeepingTasks["pruneTask"] = pruneTicker
	go func() {
		for range pruneTicker.C {
			d.pruneUnusedFlexvols()
			d.reapDeletedQtrees()
		}
	}()

	// Keep the quotas current
	d.resizeQuotas()
	resizeTicker := time.NewTicker(time.Duration(resizeQuotasPeriodSecs) * time.Second)
	d.housekeepingTasks["resizeTask"] = resizeTicker
	go func() {
		for range resizeTicker.C {
			d.resizeQuotas()
		}
	}()
}

// Create a qtree-backed volume with the specified options
func (d *NASQtreeStorageDriver) Create(name string, sizeBytes uint64, opts map[string]string) error {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method":    "Create",
			"Type":      "NASQtreeStorageDriver",
			"name":      name,
			"sizeBytes": sizeBytes,
			"opts":      opts,
		}
		log.WithFields(fields).Debug(">>>> Create")
		defer log.WithFields(fields).Debug("<<<< Create")
	}

	// Ensure any Flexvol we create won't be pruned before we place a qtree on it
	d.provMutex.Lock()
	defer d.provMutex.Unlock()

	// Generic user-facing message
	createError := errors.New("volume creation failed")

	// Ensure volume doesn't already exist
	exists, existsInFlexvol, err := d.API.QtreeExists(name, d.FlexvolNamePrefix())
	if err != nil {
		log.Errorf("Error checking for existing volume: %v.", err)
		return createError
	}
	if exists {
		log.WithFields(log.Fields{"qtree": name, "flexvol": existsInFlexvol}).Debug("Qtree already exists.")
		return fmt.Errorf("volume %s already exists", name)
	}

	if sizeBytes < MinimumVolumeSizeBytes {
		return fmt.Errorf("requested volume size (%d bytes) is too small; the minimum volume size is %d bytes",
			sizeBytes, MinimumVolumeSizeBytes)
	}

	// Ensure qtree name isn't too long
	if len(name) > maxQtreeNameLength {
		return fmt.Errorf("volume %s name exceeds the limit of %d characters", name, maxQtreeNameLength)
	}

	// Get Flexvol options with default fallback values
	// see also: ontap_common.go#PopulateConfigurationDefaults
	size := strconv.FormatUint(sizeBytes, 10)
	aggregate := utils.GetV(opts, "aggregate", d.Config.Aggregate)
	spaceReserve := utils.GetV(opts, "spaceReserve", d.Config.SpaceReserve)
	snapshotPolicy := utils.GetV(opts, "snapshotPolicy", d.Config.SnapshotPolicy)
	snapshotDir := utils.GetV(opts, "snapshotDir", d.Config.SnapshotDir)
	encryption := utils.GetV(opts, "encryption", d.Config.Encryption)

	enableSnapshotDir, err := strconv.ParseBool(snapshotDir)
	if err != nil {
		return fmt.Errorf("invalid boolean value for snapshotDir: %v", err)
	}

	encrypt, err := ValidateEncryptionAttribute(encryption, d.API)
	if err != nil {
		return err
	}

	// Make sure we have a Flexvol for the new qtree
	flexvol, err := d.ensureFlexvolForQtree(
		aggregate, spaceReserve, snapshotPolicy, enableSnapshotDir, encrypt)
	if err != nil {
		log.Errorf("Flexvol location/creation failed. %v", err)
		return createError
	}

	// Grow or shrink the Flexvol as needed
	flexvolSizeBytes, err := d.getOptimalSizeForFlexvol(flexvol, sizeBytes)
	if err != nil {
		log.Warnf("Could not calculate optimal Flexvol size. %v", err)

		// Lacking the optimal size, just grow the Flexvol to contain the new qtree
		resizeResponse, err := d.API.SetVolumeSize(flexvol, "+"+size)
		if err = api.GetError(resizeResponse.Result, err); err != nil {
			log.Errorf("Flexvol resize failed. %v", err)
			return createError
		}
	} else {

		// Got optimal size, so just set the Flexvol to that value
		flexvolSizeStr := strconv.FormatUint(flexvolSizeBytes, 10)
		resizeResponse, err := d.API.SetVolumeSize(flexvol, flexvolSizeStr)
		if err = api.GetError(resizeResponse.Result, err); err != nil {
			log.Errorf("Flexvol resize failed. %v", err)
			return createError
		}
	}

	// Get qtree options with default fallback values
	unixPermissions := utils.GetV(opts, "unixPermissions", d.Config.UnixPermissions)
	exportPolicy := utils.GetV(opts, "exportPolicy", d.Config.ExportPolicy)
	securityStyle := utils.GetV(opts, "securityStyle", d.Config.SecurityStyle)

	// Create the qtree
	qtreeResponse, err := d.API.QtreeCreate(name, flexvol, unixPermissions, exportPolicy, securityStyle)
	if err = api.GetError(qtreeResponse, err); err != nil {
		log.Errorf("Qtree creation failed. %v", err)
		return createError
	}

	// Add the quota
	d.addQuotaForQtree(name, flexvol, sizeBytes)
	if err != nil {
		log.Errorf("Qtree quota definition failed. %v", err)
		return createError
	}

	return nil
}

// Create a volume clone
func (d *NASQtreeStorageDriver) CreateClone(name, source, snapshot string, opts map[string]string) error {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method":   "CreateClone",
			"Type":     "NASQtreeStorageDriver",
			"name":     name,
			"source":   source,
			"snapshot": snapshot,
			"opts":     opts,
		}
		log.WithFields(fields).Debug(">>>> CreateClone")
		defer log.WithFields(fields).Debug("<<<< CreateClone")
	}

	return errors.New("cloning with the ONTAP NAS Economy driver is not supported")
}

// Destroy the volume
func (d *NASQtreeStorageDriver) Destroy(name string) error {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method": "Destroy",
			"Type":   "NASQtreeStorageDriver",
			"name":   name,
		}
		log.WithFields(fields).Debug(">>>> Destroy")
		defer log.WithFields(fields).Debug("<<<< Destroy")
	}

	// Ensure the deleted qtree reaping job doesn't interfere with this workflow
	d.provMutex.Lock()
	defer d.provMutex.Unlock()

	// Generic user-facing message
	deleteError := errors.New("volume deletion failed")

	exists, flexvol, err := d.API.QtreeExists(name, d.FlexvolNamePrefix())
	if err != nil {
		log.Errorf("Error checking for existing qtree. %v", err)
		return deleteError
	}
	if !exists {
		log.WithField("qtree", name).Warn("Qtree not found.")
		return nil
	}

	// Rename qtree so it doesn't show up in lists while ONTAP is deleting it in the background.
	// Ensure the deleted name doesn't exceed the qtree name length limit of 64 characters.
	path := fmt.Sprintf("/vol/%s/%s", flexvol, name)
	deletedName := deletedQtreeNamePrefix + name + "_" + utils.RandomString(5)
	if len(deletedName) > maxQtreeNameLength {
		trimLength := len(deletedQtreeNamePrefix) + 10
		deletedName = deletedQtreeNamePrefix + name[trimLength:] + "_" + utils.RandomString(5)
	}
	deletedPath := fmt.Sprintf("/vol/%s/%s", flexvol, deletedName)

	renameResponse, err := d.API.QtreeRename(path, deletedPath)
	if err = api.GetError(renameResponse, err); err != nil {
		log.Errorf("Qtree rename failed. %v", err)
		return deleteError
	}

	// Destroy the qtree in the background.  If this fails, try to restore the original qtree name.
	destroyResponse, err := d.API.QtreeDestroyAsync(deletedPath, true)
	if err = api.GetError(destroyResponse, err); err != nil {
		log.Errorf("Qtree async delete failed. %v", err)
		defer d.API.QtreeRename(deletedPath, path)
		return deleteError
	}

	return nil
}

// Attach the volume
func (d *NASQtreeStorageDriver) Attach(name, mountpoint string, opts map[string]string) error {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method":     "Attach",
			"Type":       "NASQtreeStorageDriver",
			"name":       name,
			"mountpoint": mountpoint,
			"opts":       opts,
		}
		log.WithFields(fields).Debug(">>>> Attach")
		defer log.WithFields(fields).Debug("<<<< Attach")
	}

	// Check if qtree exists, and find its Flexvol so we can build the export location
	exists, flexvol, err := d.API.QtreeExists(name, d.FlexvolNamePrefix())
	if err != nil {
		log.Errorf("Error checking for existing qtree. %v", err)
		return errors.New("volume mount failed")
	}
	if !exists {
		log.WithField("qtree", name).Debug("Qtree not found.")
		return fmt.Errorf("volume %s not found", name)
	}

	exportPath := fmt.Sprintf("%s:/%s/%s", d.Config.DataLIF, flexvol, name)

	return MountVolume(exportPath, mountpoint, &d.Config)
}

// Detach the volume
func (d *NASQtreeStorageDriver) Detach(name, mountpoint string) error {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method":     "Detach",
			"Type":       "NASQtreeStorageDriver",
			"name":       name,
			"mountpoint": mountpoint,
		}
		log.WithFields(fields).Debug(">>>> Detach")
		defer log.WithFields(fields).Debug("<<<< Detach")
	}

	exists, _, err := d.API.QtreeExists(name, d.FlexvolNamePrefix())
	if err != nil {
		log.Warnf("Error checking for existing qtree. %v", err)
	}
	if !exists {
		log.WithField("qtree", name).Warn("Qtree not found, attempting unmount anyway.")
	}

	return UnmountVolume(mountpoint, &d.Config)
}

// Return the list of snapshots associated with the named volume
func (d *NASQtreeStorageDriver) SnapshotList(name string) ([]storage.Snapshot, error) {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method": "SnapshotList",
			"Type":   "NASQtreeStorageDriver",
			"name":   name,
		}
		log.WithFields(fields).Debug(">>>> SnapshotList")
		defer log.WithFields(fields).Debug("<<<< SnapshotList")
	}

	// Qtrees can't have snapshots, so return an empty list
	return []storage.Snapshot{}, nil
}

// Return the list of volumes associated with this tenant
func (d *NASQtreeStorageDriver) List() ([]string, error) {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "List", "Type": "NASQtreeStorageDriver"}
		log.WithFields(fields).Debug(">>>> List")
		defer log.WithFields(fields).Debug("<<<< List")
	}

	// Generic user-facing message
	listError := errors.New("volume list failed")

	prefix := *d.Config.StoragePrefix
	volumes := make([]string, 0)

	// Get all qtrees in all Flexvols managed by this driver
	listResponse, err := d.API.QtreeList(prefix, d.FlexvolNamePrefix())
	if err = api.GetError(listResponse, err); err != nil {
		log.Errorf("Qtree list failed. %v", err)
		return volumes, listError
	}

	// AttributesList() returns []QtreeInfoType
	for _, qtree := range listResponse.Result.AttributesList() {
		vol := qtree.Qtree()[len(prefix):]
		volumes = append(volumes, vol)
	}

	return volumes, nil
}

// Test for the existence of a volume
func (d *NASQtreeStorageDriver) Get(name string) error {

	if d.Config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "Get", "Type": "NASQtreeStorageDriver"}
		log.WithFields(fields).Debug(">>>> Get")
		defer log.WithFields(fields).Debug("<<<< Get")
	}

	// Generic user-facing message
	getError := fmt.Errorf("volume %s not found", name)

	exists, flexvol, err := d.API.QtreeExists(name, d.FlexvolNamePrefix())
	if err != nil {
		log.Errorf("Error checking for existing qtree. %v", err)
		return getError
	}
	if !exists {
		log.WithField("qtree", name).Debug("Qtree not found.")
		return getError
	}

	log.WithFields(log.Fields{"qtree": name, "flexvol": flexvol}).Debug("Qtree found.")

	return nil
}

// ensureFlexvolForQtree accepts a set of Flexvol characteristics and either finds one to contain a new
// qtree or it creates a new Flexvol with the needed attributes.
func (d *NASQtreeStorageDriver) ensureFlexvolForQtree(
	aggregate, spaceReserve, snapshotPolicy string, enableSnapshotDir bool, encrypt *bool,
) (string, error) {

	// Check if a suitable Flexvol already exists
	flexvol, err := d.getFlexvolForQtree(aggregate, spaceReserve, snapshotPolicy, enableSnapshotDir, encrypt)
	if err != nil {
		return "", fmt.Errorf("error finding Flexvol for qtree: %v", err)
	}

	// Found one!
	if flexvol != "" {
		return flexvol, nil
	}

	// Nothing found, so create a suitable Flexvol
	flexvol, err = d.createFlexvolForQtree(aggregate, spaceReserve, snapshotPolicy, enableSnapshotDir, encrypt)
	if err != nil {
		return "", fmt.Errorf("error creating Flexvol for qtree: %v", err)
	}

	return flexvol, nil
}

// createFlexvolForQtree creates a new Flexvol matching the specified attributes for
// the purpose of containing qtrees supplied as container volumes by this driver.
// Once this method returns, the Flexvol exists, is mounted, and has a default tree
// quota.
func (d *NASQtreeStorageDriver) createFlexvolForQtree(
	aggregate, spaceReserve, snapshotPolicy string, enableSnapshotDir bool, encrypt *bool,
) (string, error) {

	flexvol := d.FlexvolNamePrefix() + utils.RandomString(10)
	size := "1g"
	unixPermissions := "0700"
	exportPolicy := d.flexvolExportPolicy
	securityStyle := "unix"

	encryption := false
	if encrypt != nil {
		encryption = *encrypt
	}

	log.WithFields(log.Fields{
		"name":            flexvol,
		"aggregate":       aggregate,
		"size":            size,
		"spaceReserve":    spaceReserve,
		"snapshotPolicy":  snapshotPolicy,
		"unixPermissions": unixPermissions,
		"snapshotDir":     enableSnapshotDir,
		"exportPolicy":    exportPolicy,
		"securityStyle":   securityStyle,
		"encryption":      encryption,
	}).Debug("Creating Flexvol for qtrees.")

	// Create the Flexvol
	createResponse, err := d.API.VolumeCreate(
		flexvol, aggregate, size, spaceReserve, snapshotPolicy,
		unixPermissions, exportPolicy, securityStyle, encrypt)
	if err = api.GetError(createResponse, err); err != nil {
		return "", fmt.Errorf("error creating Flexvol: %v", err)
	}

	// Disable '.snapshot' as needed
	if !enableSnapshotDir {
		snapDirResponse, err := d.API.VolumeDisableSnapshotDirectoryAccess(flexvol)
		if err = api.GetError(snapDirResponse, err); err != nil {
			defer d.API.VolumeDestroy(flexvol, true)
			return "", fmt.Errorf("error disabling snapshot directory access: %v", err)
		}
	}

	// Mount the volume at the specified junction
	mountResponse, err := d.API.VolumeMount(flexvol, "/"+flexvol)
	if err = api.GetError(mountResponse, err); err != nil {
		defer d.API.VolumeDestroy(flexvol, true)
		return "", fmt.Errorf("error mounting Flexvol: %v", err)
	}

	// If LS mirrors are present on the SVM root volume, update them
	UpdateLoadSharingMirrors(d.API)

	// Create the default quota rule so we can use quota-resize for new qtrees
	err = d.addDefaultQuotaForFlexvol(flexvol)
	if err != nil {
		defer d.API.VolumeDestroy(flexvol, true)
		return "", fmt.Errorf("error adding default quota to Flexvol: %v", err)
	}

	return flexvol, nil
}

// getFlexvolForQtree returns a Flexvol (from the set of existing Flexvols) that
// matches the specified Flexvol attributes and does not already contain more
// than the maximum configured number of qtrees.  No matching Flexvols is not
// considered an error.  If more than one matching Flexvol is found, one of those
// is returned at random.
func (d *NASQtreeStorageDriver) getFlexvolForQtree(
	aggregate, spaceReserve, snapshotPolicy string, enableSnapshotDir bool, encrypt *bool,
) (string, error) {

	// Get all volumes matching the specified attributes
	volListResponse, err := d.API.VolumeListByAttrs(
		d.FlexvolNamePrefix(), aggregate, spaceReserve, snapshotPolicy, enableSnapshotDir, encrypt)

	if err = api.GetError(volListResponse, err); err != nil {
		return "", fmt.Errorf("error enumerating Flexvols: %v", err)
	}

	// Weed out the Flexvols already having too many qtrees
	var volumes []string
	for _, volAttrs := range volListResponse.Result.AttributesList() {
		volIDAttrs := volAttrs.VolumeIdAttributes()
		volName := string(volIDAttrs.Name())

		count, err := d.API.QtreeCount(volName)
		if err != nil {
			return "", fmt.Errorf("error enumerating qtrees: %v", err)
		}

		if count < maxQtreesPerFlexvol {
			volumes = append(volumes, volName)
		}
	}

	// Pick a Flexvol.  If there are multiple matches, pick one at random.
	switch len(volumes) {
	case 0:
		return "", nil
	case 1:
		return volumes[0], nil
	default:
		rand.Seed(time.Now().UnixNano())
		return volumes[rand.Intn(len(volumes))], nil
	}
}

// getOptimalSizeForFlexvol sums up all the disk limit quota rules on a Flexvol and adds the size of
// the new qtree being added as well as the current Flexvol snapshot reserve.  This value may be used
// to grow (or shrink) the Flexvol as new qtrees are being added.
func (d *NASQtreeStorageDriver) getOptimalSizeForFlexvol(
	flexvol string, newQtreeSizeBytes uint64,
) (uint64, error) {

	// Get more info about the Flexvol
	volAttrs, err := d.API.VolumeGet(flexvol)
	if err != nil {
		return 0, err
	}
	volSpaceAttrs := volAttrs.VolumeSpaceAttributes()
	snapReserveMultiplier := 1.0 + (float64(volSpaceAttrs.PercentageSnapshotReserve()) / 100.0)

	totalDiskLimitBytes, err := d.getTotalHardDiskLimitQuota(flexvol)
	if err != nil {
		return 0, err
	}

	usableSpaceBytes := float64(newQtreeSizeBytes + totalDiskLimitBytes)
	flexvolSizeBytes := uint64(usableSpaceBytes * snapReserveMultiplier)

	log.WithFields(log.Fields{
		"flexvol":               flexvol,
		"snapReserveMultiplier": snapReserveMultiplier,
		"totalDiskLimitBytes":   totalDiskLimitBytes,
		"newQtreeSizeBytes":     newQtreeSizeBytes,
		"flexvolSizeBytes":      flexvolSizeBytes,
	}).Debug("Calculated optimal size for Flexvol with new qtree.")

	return flexvolSizeBytes, nil
}

// addDefaultQuotaForFlexvol adds a default quota rule to a Flexvol so that quotas for
// new qtrees may be added on demand with simple quota resize instead of a heavyweight
// quota reinitialization.
func (d *NASQtreeStorageDriver) addDefaultQuotaForFlexvol(flexvol string) error {

	response, err := d.API.QuotaSetEntry("", flexvol, "", "tree", "-")
	if err = api.GetError(response, err); err != nil {
		return fmt.Errorf("error adding default quota: %v", err)
	}

	d.disableQuotas(flexvol, true)
	if err != nil {
		return fmt.Errorf("error adding default quota: %v", err)
	}

	d.enableQuotas(flexvol, true)
	if err != nil {
		return fmt.Errorf("error adding default quota: %v", err)
	}

	return nil
}

// addQuotaForQtree adds a tree quota to a Flexvol/qtree with a hard disk size limit.
func (d *NASQtreeStorageDriver) addQuotaForQtree(qtree, flexvol string, sizeBytes uint64) error {

	target := fmt.Sprintf("/vol/%s/%s", flexvol, qtree)
	sizeKB := strconv.FormatUint(sizeBytes/1024, 10)

	response, err := d.API.QuotaSetEntry("", flexvol, target, "tree", sizeKB)
	if err = api.GetError(response, err); err != nil {
		return fmt.Errorf("error adding qtree quota: %v", err)
	}

	// Mark this Flexvol as needing a quota resize
	d.quotaResizeMap[flexvol] = true

	return nil
}

// enableQuotas disables quotas on a Flexvol, optionally waiting for the operation to finish.
func (d *NASQtreeStorageDriver) disableQuotas(flexvol string, wait bool) error {

	status, err := d.getQuotaStatus(flexvol)
	if err != nil {
		return fmt.Errorf("error disabling quotas: %v", err)
	}
	if status == "corrupt" {
		return fmt.Errorf("error disabling quotas: quotas are corrupt on Flexvol %s", flexvol)
	}

	if status != "off" {
		offResponse, err := d.API.QuotaOff(flexvol)
		if err = api.GetError(offResponse, err); err != nil {
			return fmt.Errorf("error disabling quotas: %v", err)
		}
	}

	if wait {
		for status != "off" {
			time.Sleep(1 * time.Second)

			status, err = d.getQuotaStatus(flexvol)
			if err != nil {
				return fmt.Errorf("error disabling quotas: %v", err)
			}
			if status == "corrupt" {
				return fmt.Errorf("error disabling quotas: quotas are corrupt on flexvol %s", flexvol)
			}
		}
	}

	return nil
}

// enableQuotas enables quotas on a Flexvol, optionally waiting for the operation to finish.
func (d *NASQtreeStorageDriver) enableQuotas(flexvol string, wait bool) error {

	status, err := d.getQuotaStatus(flexvol)
	if err != nil {
		return fmt.Errorf("error enabling quotas: %v", err)
	}
	if status == "corrupt" {
		return fmt.Errorf("error enabling quotas: quotas are corrupt on flexvol %s", flexvol)
	}

	if status == "off" {
		onResponse, err := d.API.QuotaOn(flexvol)
		if err = api.GetError(onResponse, err); err != nil {
			return fmt.Errorf("error enabling quotas: %v", err)
		}
	}

	if wait {
		for status != "on" {
			time.Sleep(1 * time.Second)

			status, err = d.getQuotaStatus(flexvol)
			if err != nil {
				return fmt.Errorf("error enabling quotas: %v", err)
			}
			if status == "corrupt" {
				return fmt.Errorf("error enabling quotas: quotas are corrupt on flexvol %s", flexvol)
			}
		}
	}

	return nil
}

// queueAllFlexvolsForQuotaResize flags every Flexvol managed by this driver as
// needing a quota resize.  This is called once on driver startup to handle the
// case where the driver was shut down with pending quota resize operations.
func (d *NASQtreeStorageDriver) queueAllFlexvolsForQuotaResize() {

	// Get list of Flexvols managed by this driver
	volumeListResponse, err := d.API.VolumeList(d.FlexvolNamePrefix())
	if err = api.GetError(volumeListResponse, err); err != nil {
		log.Errorf("Error listing Flexvols: %v", err)
	}

	for _, volAttrs := range volumeListResponse.Result.AttributesList() {
		volIDAttrs := volAttrs.VolumeIdAttributes()
		flexvol := string(volIDAttrs.Name())
		d.quotaResizeMap[flexvol] = true
	}
}

// resizeQuotas may be called by a background task, or by a method that changed
// the qtree population on a Flexvol.  Flexvols needing an update must be flagged
// in quotaResizeMap.  Any failures that occur are simply logged, and the resize
// operation will be attempted each time this method is called until it succeeds.
func (d *NASQtreeStorageDriver) resizeQuotas() {

	// Ensure we don't forget any Flexvol that is involved in a qtree provisioning workflow
	d.provMutex.Lock()
	defer d.provMutex.Unlock()

	log.Debug("Housekeeping, resizing quotas.")

	for flexvol, resize := range d.quotaResizeMap {

		if resize {
			resizeResponse, err := d.API.QuotaResize(flexvol)
			if err != nil {
				log.WithFields(log.Fields{"flexvol": flexvol, "error": err}).Debug("Error resizing quotas.")
				continue
			}
			if zerr := api.NewZapiError(resizeResponse); !zerr.IsPassed() {

				if zerr.Code() == azgo.EVOLUMEDOESNOTEXIST {
					// Volume gone, so no need to try again
					log.WithField("flexvol", flexvol).Debug("Volume does not exist.")
					delete(d.quotaResizeMap, flexvol)
				} else {
					log.WithFields(log.Fields{"flexvol": flexvol, "error": zerr}).Debug("Error resizing quotas.")
				}

				continue
			}

			log.WithField("flexvol", flexvol).Debug("Started quota resize.")

			// Resize start succeeded, so no need to try again
			delete(d.quotaResizeMap, flexvol)
		}
	}
}

// getQuotaStatus returns the status of the quotas on a Flexvol
func (d *NASQtreeStorageDriver) getQuotaStatus(flexvol string) (string, error) {

	statusResponse, err := d.API.QuotaStatus(flexvol)
	if err = api.GetError(statusResponse, err); err != nil {
		return "", fmt.Errorf("error getting quota status for Flexvol %s: %v", flexvol, err)
	}

	return statusResponse.Result.Status(), nil

}

// getTotalHardDiskLimitQuota returns the sum of all disk limit quota rules on a Flexvol
func (d *NASQtreeStorageDriver) getTotalHardDiskLimitQuota(flexvol string) (uint64, error) {

	listResponse, err := d.API.QuotaEntryList(flexvol)
	if err != nil {
		return 0, err
	}

	var totalDiskLimitKB uint64

	for _, rule := range listResponse.Result.AttributesList() {
		diskLimitKB, err := strconv.ParseUint(rule.DiskLimit(), 10, 64)
		if err != nil {
			continue
		}
		totalDiskLimitKB += diskLimitKB
	}

	return totalDiskLimitKB * 1024, nil
}

// pruneUnusedFlexvols is called periodically by a background task.  Any Flexvols
// that are managed by this driver (discovered by virtue of having a well-known
// hardcoded prefix on their names) that have no qtrees are deleted.
func (d *NASQtreeStorageDriver) pruneUnusedFlexvols() {

	// Ensure we don't prune any Flexvol that is involved in a qtree provisioning workflow
	d.provMutex.Lock()
	defer d.provMutex.Unlock()

	log.Debug("Housekeeping, checking for managed Flexvols with no qtrees.")

	// Get list of Flexvols managed by this driver
	volumeListResponse, err := d.API.VolumeList(d.FlexvolNamePrefix())
	if err = api.GetError(volumeListResponse, err); err != nil {
		log.Errorf("Error listing Flexvols. %v", err)
	}

	var flexvols []string
	for _, volAttrs := range volumeListResponse.Result.AttributesList() {
		volIDAttrs := volAttrs.VolumeIdAttributes()
		volName := string(volIDAttrs.Name())
		flexvols = append(flexvols, volName)
	}

	// Destroy any Flexvol if it is devoid of qtrees
	for _, flexvol := range flexvols {
		qtreeCount, err := d.API.QtreeCount(flexvol)
		if err == nil && qtreeCount == 0 {
			log.WithField("flexvol", flexvol).Debug("Housekeeping, deleting managed Flexvol with no qtrees.")
			d.API.VolumeDestroy(flexvol, true)
		}
	}
}

// reapDeletedQtrees is called periodically by a background task.  Any qtrees
// that have been deleted (discovered by virtue of having a well-known hardcoded
// prefix on their names) are destroyed.  This is only needed for the exceptional case
// in which a qtree was renamed (prior to being destroyed) but the subsequent
// destroy call failed or was never made due to a process interruption.
func (d *NASQtreeStorageDriver) reapDeletedQtrees() {

	// Ensure we don't reap any qtree that is involved in a qtree delete workflow
	d.provMutex.Lock()
	defer d.provMutex.Unlock()

	log.Debug("Housekeeping, checking for deleted qtrees.")

	// Get all deleted qtrees in all FlexVols managed by this driver
	prefix := deletedQtreeNamePrefix + *d.Config.StoragePrefix
	listResponse, err := d.API.QtreeList(prefix, d.FlexvolNamePrefix())
	if err = api.GetError(listResponse, err); err != nil {
		log.Errorf("Error listing deleted qtrees. %v", err)
	}

	// AttributesList() returns []QtreeInfoType
	for _, qtree := range listResponse.Result.AttributesList() {
		qtreePath := fmt.Sprintf("/vol/%s/%s", qtree.Volume(), qtree.Qtree())
		log.WithField("qtree", qtreePath).Debug("Housekeeping, reaping deleted qtree.")
		d.API.QtreeDestroyAsync(qtreePath, true)
	}
}

// ensureDefaultExportPolicy checks for an export policy with a well-known name that will be suitable
// for setting on a Flexvol and will enable access to all qtrees therein.  If the policy exists, the
// method assumes it created the policy itself and that all is good.  If the policy does not exist,
// it is created and populated with a rule that allows access to NFS qtrees.  This method should be
// called once during driver initialization.
func (d *NASQtreeStorageDriver) ensureDefaultExportPolicy() error {

	policyResponse, err := d.API.ExportPolicyCreate(d.flexvolExportPolicy)
	if err != nil {
		return fmt.Errorf("error creating export policy %s: %v", d.flexvolExportPolicy, err)
	}
	if zerr := api.NewZapiError(policyResponse); !zerr.IsPassed() {
		if zerr.Code() == azgo.EDUPLICATEENTRY {
			log.WithField("exportPolicy", d.flexvolExportPolicy).Debug("Export policy already exists.")
		} else {
			return fmt.Errorf("error creating export policy %s: %v", d.flexvolExportPolicy, zerr)
		}
	}

	return d.ensureDefaultExportPolicyRule()
}

// ensureDefaultExportPolicyRule guarantees that the export policy used on Flexvols managed by this
// driver has at least one rule, which is necessary (but not always sufficient) to enable qtrees
// to be mounted by clients.
func (d *NASQtreeStorageDriver) ensureDefaultExportPolicyRule() error {

	ruleListResponse, err := d.API.ExportRuleGetIterRequest(d.flexvolExportPolicy)
	if err = api.GetError(ruleListResponse, err); err != nil {
		return fmt.Errorf("error listing export policy rules: %v", err)
	}

	if ruleListResponse.Result.NumRecords() == 0 {

		// No rules, so create one
		ruleResponse, err := d.API.ExportRuleCreate(
			d.flexvolExportPolicy, "0.0.0.0/0",
			[]string{"nfs"}, []string{"any"}, []string{"any"}, []string{"any"})
		if err = api.GetError(ruleResponse, err); err != nil {
			return fmt.Errorf("error creating export rule: %v", err)
		}
	} else {
		log.WithField("exportPolicy", d.flexvolExportPolicy).Debug("Export policy has at least one rule.")
	}

	return nil
}

// Retrieve storage backend capabilities
func (d *NASQtreeStorageDriver) GetStorageBackendSpecs(backend *storage.Backend) error {

	backend.Name = "ontapnaseco_" + d.Config.DataLIF
	poolAttrs := d.GetStoragePoolAttributes()
	return getStorageBackendSpecsCommon(d, backend, poolAttrs)
}

func (d *NASQtreeStorageDriver) GetStoragePoolAttributes() map[string]sa.Offer {

	return map[string]sa.Offer{
		sa.BackendType:      sa.NewStringOffer(d.Name()),
		sa.Snapshots:        sa.NewBoolOffer(false),
		sa.Clones:           sa.NewBoolOffer(false),
		sa.Encryption:       sa.NewBoolOffer(d.API.SupportsFeature(api.NetAppVolumeEncryption)),
		sa.ProvisioningType: sa.NewStringOffer("thick", "thin"),
	}
}

func (d *NASQtreeStorageDriver) GetVolumeOpts(
	volConfig *storage.VolumeConfig,
	pool *storage.Pool,
	requests map[string]sa.Request,
) (map[string]string, error) {
	return getVolumeOptsCommon(volConfig, pool, requests), nil
}

func (d *NASQtreeStorageDriver) GetInternalVolumeName(name string) string {
	return getInternalVolumeNameCommon(d.Config.CommonStorageDriverConfig, name)
}

func (d *NASQtreeStorageDriver) CreatePrepare(volConfig *storage.VolumeConfig) bool {
	return createPrepareCommon(d, volConfig)
}

func (d *NASQtreeStorageDriver) CreateFollowup(volConfig *storage.VolumeConfig) error {

	// Determine which Flexvol contains the qtree
	exists, flexvol, err := d.API.QtreeExists(volConfig.InternalName, d.FlexvolNamePrefix())
	if err != nil {
		return fmt.Errorf("could not determine if qtree %s exists: %v", volConfig.InternalName, err)
	}
	if !exists {
		return fmt.Errorf("could not find qtree %s", volConfig.InternalName)
	}

	// Set export path info on the volume config
	volConfig.AccessInfo.NfsServerIP = d.Config.DataLIF
	volConfig.AccessInfo.NfsPath = fmt.Sprintf("/%s/%s", flexvol, volConfig.InternalName)

	return nil
}

func (d *NASQtreeStorageDriver) GetProtocol() trident.Protocol {
	return trident.File
}

func (d *NASQtreeStorageDriver) StoreConfig(b *storage.PersistentStorageBackendConfig) {
	drivers.SanitizeCommonStorageDriverConfig(d.Config.CommonStorageDriverConfig)
	b.OntapConfig = &d.Config
}

func (d *NASQtreeStorageDriver) GetExternalConfig() interface{} {
	return getExternalConfig(d.Config)
}

// GetVolumeExternal queries the storage backend for all relevant info about
// a single container volume managed by this driver and returns a VolumeExternal
// representation of the volume.
func (d *NASQtreeStorageDriver) GetVolumeExternal(name string) (*storage.VolumeExternal, error) {

	qtree, err := d.API.QtreeGet(name, d.FlexvolNamePrefix())
	if err != nil {
		return nil, err
	}

	volume, err := d.API.VolumeGet(qtree.Volume())
	if err != nil {
		return nil, err
	}

	quotaTarget := fmt.Sprintf("/vol/%s/%s", qtree.Volume(), qtree.Qtree())
	quota, err := d.API.QuotaEntryGet(quotaTarget)
	if err != nil {
		return nil, err
	}

	return d.getVolumeExternal(&qtree, &volume, &quota), nil
}

// GetVolumeExternalWrappers queries the storage backend for all relevant info about
// container volumes managed by this driver.  It then writes a VolumeExternal
// representation of each volume to the supplied channel, closing the channel
// when finished.
func (d *NASQtreeStorageDriver) GetVolumeExternalWrappers(
	channel chan *storage.VolumeExternalWrapper) {

	// Let the caller know we're done by closing the channel
	defer close(channel)

	// Get all volumes matching the storage prefix
	volumesResponse, err := d.API.VolumeGetAll(d.FlexvolNamePrefix())
	if err = api.GetError(volumesResponse, err); err != nil {
		channel <- &storage.VolumeExternalWrapper{nil, err}
		return
	}

	// Get all quotas in all Flexvols matching the storage prefix
	quotasResponse, err := d.API.QuotaEntryList(d.FlexvolNamePrefix() + "*")
	if err = api.GetError(quotasResponse, err); err != nil {
		channel <- &storage.VolumeExternalWrapper{nil, err}
		return
	}

	// Get all qtrees in all Flexvols matching the storage prefix
	qtreesResponse, err := d.API.QtreeGetAll(d.FlexvolNamePrefix())
	if err = api.GetError(qtreesResponse, err); err != nil {
		channel <- &storage.VolumeExternalWrapper{nil, err}
		return
	}

	// Make a map of volumes for faster correlation with qtrees
	volumeMap := make(map[string]azgo.VolumeAttributesType)
	for _, volumeAttrs := range volumesResponse.Result.AttributesList() {
		internalName := string(volumeAttrs.VolumeIdAttributesPtr.Name())
		volumeMap[internalName] = volumeAttrs
	}

	// Make a map of quotas for faster correlation with qtrees
	quotaMap := make(map[string]azgo.QuotaEntryType)
	for _, quotaAttrs := range quotasResponse.Result.AttributesList() {
		quotaMap[quotaAttrs.QuotaTarget()] = quotaAttrs
	}

	// Convert all qtrees to VolumeExternal and write them to the channel
	for _, qtree := range qtreesResponse.Result.AttributesList() {

		// Ignore Flexvol-level qtrees
		if qtree.Qtree() == "" {
			continue
		}

		volume, ok := volumeMap[qtree.Volume()]
		if !ok {
			log.WithField("qtree", qtree.Qtree()).Warning("Flexvol not found for qtree.")
			continue
		}

		quotaTarget := fmt.Sprintf("/vol/%s/%s", qtree.Volume(), qtree.Qtree())
		quota, ok := quotaMap[quotaTarget]
		if !ok {
			log.WithField("qtree", qtree.Qtree()).Warning("Quota rule not found for qtree.")
			continue
		}

		channel <- &storage.VolumeExternalWrapper{d.getVolumeExternal(&qtree, &volume, &quota), nil}
	}
}

// getExternalVolume is a private method that accepts info about a volume
// as returned by the storage backend and formats it as a VolumeExternal
// object.
func (d *NASQtreeStorageDriver) getVolumeExternal(
	qtreeAttrs *azgo.QtreeInfoType, volumeAttrs *azgo.VolumeAttributesType,
	quotaAttrs *azgo.QuotaEntryType) *storage.VolumeExternal {

	volumeIDAttrs := volumeAttrs.VolumeIdAttributesPtr
	volumeSnapshotAttrs := volumeAttrs.VolumeSnapshotAttributesPtr

	internalName := qtreeAttrs.Qtree()
	name := internalName[len(*d.Config.StoragePrefix):]

	size, err := strconv.ParseInt(quotaAttrs.DiskLimit(), 10, 64)
	if err != nil {
		size = 0
	} else {
		size *= 1024 // convert KB to bytes
	}

	volumeConfig := &storage.VolumeConfig{
		Version:         trident.OrchestratorAPIVersion,
		Name:            name,
		InternalName:    internalName,
		Size:            strconv.FormatInt(size, 10),
		Protocol:        trident.File,
		SnapshotPolicy:  volumeSnapshotAttrs.SnapshotPolicy(),
		ExportPolicy:    qtreeAttrs.ExportPolicy(),
		SnapshotDir:     strconv.FormatBool(volumeSnapshotAttrs.SnapdirAccessEnabled()),
		UnixPermissions: qtreeAttrs.Mode(),
		StorageClass:    "",
		AccessMode:      trident.ReadWriteMany,
		AccessInfo:      storage.VolumeAccessInfo{},
		BlockSize:       "",
		FileSystem:      "",
	}

	return &storage.VolumeExternal{
		Config: volumeConfig,
		Pool:   volumeIDAttrs.ContainingAggregateName(),
	}
}
