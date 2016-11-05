package contractmanager

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/modules"
)

// findUnfinishedStorageFolderAdditions will scroll through a set of state
// changes and figure out which of the unfinished storage folder additions are
// still unfinished.
func findUnfinishedStorageFolderAdditions(scs []stateChange) []*storageFolder {
	// Use a map to figure out what unfinished storage folders exist and use it
	// to remove the ones that have terminated.
	usfMap := make(map[uint16]*storageFolder)
	for _, sc := range scs {
		for _, sf := range sc.UnfinishedStorageFolderAdditions {
			usfMap[sf.Index] = sf
		}
		for _, sf := range sc.StorageFolderAdditions {
			delete(usfMap, sf.Index)
		}
		for _, index := range sc.ErroredStorageFolderAdditions {
			delete(usfMap, index)
		}
	}

	// Return the active unifinished storage folders as a slice.
	var sfs []*storageFolder
	for _, sf := range usfMap {
		sfs = append(sfs, sf)
	}
	return sfs
}

// managedAddStorageFolder will add a storage folder to the contract manager.
// The parent fucntion, contractmanager.AddStorageFolder, has already performed
// any error checking that can be performed without accessing the contract
// manager state.
//
// managedAddStorageFolder can take a long time, as it writes a giant, zeroed
// out file to disk covering the entire range of the storage folder, and
// failure can occur late in the operation. The WAL is notified that a long
// running operation is in progress, so that any changes to disk can be
// reverted in the event of unclean shutdown.
func (wal *writeAheadLog) managedAddStorageFolder(sf *storageFolder) error {
	numSectors := uint64(len(sf.Usage)) * 64
	sectorLookupSize := numSectors * sectorMetadataDiskSize
	sectorHousingSize := numSectors * modules.SectorSize
	totalSize := sectorLookupSize + sectorHousingSize
	sectorLookupName := filepath.Join(sf.Path, metadataFile)
	sectorHousingName := filepath.Join(sf.Path, sectorFile)

	// Lock the storage folder for the duration of the function.
	sf.mu.Lock()
	defer sf.mu.Unlock()

	// Update the uncommitted state to include the storage folder, returning an
	// error if any checks fail.
	var syncChan chan struct{}
	err := func() error {
		wal.mu.Lock()
		defer wal.mu.Unlock()

		// Check that the storage folder is not a duplicate. That requires
		// first checking the contract manager and then checking the WAL. The
		// number of storage folders are also counted, to make sure that the
		// maximum number of storage folders allowed is not exceeded.
		for _, csf := range wal.cm.storageFolders {
			// The conflicting storage folder may e in the process of being
			// removed, however we refuse to add a replacement storage folder
			// until the existing one has been removed entirely.
			if sf.Path == csf.Path {
				return errRepeatFolder
			}
		}

		// Check that there is room for another storage folder.
		if uint64(len(wal.cm.storageFolders)) > maximumStorageFolders {
			return errMaxStorageFolders
		}

		// Determine the index of the storage folder by scanning for an empty
		// spot in the folderLocations map. A random starting place is chosen
		// to keep good average and worst-case runtime.
		var iterator int
		var index uint16
		rand, err := crypto.RandIntn(65536)
		if err != nil {
			wal.cm.log.Critical("no entropy for random iteration when adding a storage folder")
		}
		index = uint16(rand)
		for iterator = 0; iterator < 65536; iterator++ {
			// check the list of unique folders we created earlier.
			_, exists := uniqueFolders[index]
			if !exists {
				break
			}
			index++
		}
		if iterator == 65536 {
			wal.cm.log.Critical("Previous check indicated that there was room to add another storage folder, but folderLocations set is full.")
			return errMaxStorageFolders
		}
		// Assign the empty index to the storage folder.
		sf.Index = index

		// Create the file that is used with the storage folder.
		sf.metadataFile, err = wal.cm.dependencies.createFile(sectorLookupName)
		if err != nil {
			return build.ExtendErr("could not create storage folder file", err)
		}
		sf.sectorFile, err = wal.cm.dependencies.createFile(sectorHousingName)
		if err != nil {
			return build.ExtendErr("could not create storage folder file", err)
		}
		// Establish the progress fields for the add operation in the storage
		// folder.
		atomic.StoreUint64(&sf.atomicProgressDenominator, totalSize)

		// Add the storage folder to the list of storage folders.
		wal.cm.storageFolders[index] = sf

		// Add the storage folder to the list of unfinished storage folder
		// additions. There should be no chance of error between this append
		// operation and the completed commitment to the unfinished storage
		// folder addition (signaled by `<-syncChan` a few lines down).
		wal.appendChange(stateChange{
			UnfinishedStorageFolderAdditions: []*storageFolder{sf},
		})
		// Grab the sync channel so we know when the unfinished storage folder
		// addition has been committed to on disk.
		syncChan = wal.syncChan
		return nil
	}()
	if err != nil {
		return err
	}
	// Block until the commitment to the unfinished storage folder addition is
	// complete.
	<-syncChan

	// If there's an error in the rest of the function, the storage folder
	// needs to be removed from the list of unfinished storage folder
	// additions. Because the WAL is append-only, a stateChange needs to be
	// appended which indicates that the storage folder was unable to be added
	// successfully.
	defer func() {
		if err != nil {
			wal.mu.Lock()
			defer wal.mu.Unlock()

			// Delete the storage folder from the storage folders map.
			delete(wal.cm.storageFolders, sf.index)

			// Remove the leftover files from the failed operation.
			err = build.ComposeErrors(err, os.Remove(sectorLookupName))
			err = build.ComposeErrors(err, os.Remove(sectorHousingName))

			// Signal in the WAL that the unfinished storage folder addition
			// has failed.
			err = build.ComposeErrors(err, wal.appendChange(stateChange{
				ErroredStorageFolderAdditions: []uint16{sf.Index},
			}))
		}
	}()

	// Allocate the files on disk for the storage folder.
	writeCount := sectorHousingSize / 4e6
	finalWriteSize := sectorHousingSize % 4e6
	writeData := make([]byte, 4e6)
	for i := uint64(0); i < writeCount; i++ {
		_, err = sf.sectorFile.Write(writeData)
		if err != nil {
			return build.ExtendErr("could not allocate storage folder", err)
		}
		// After each iteration, update the progress numerator.
		atomic.AddUint64(&sf.atomicProgressNumerator, 4e6)
	}
	writeData = writeData[:finalWriteSize]
	_, err = sf.sectorFile.Write(writeData)
	if err != nil {
		return build.ExtendErr("could not allocate sector data file", err)
	}

	// Write the metadata file.
	writeData = make([]byte, sectorLookupSize)
	_, err = sf.metadataFile.Write(writeData)
	if err != nil {
		return build.ExtendErr("could not allocate sector metadata file", err)
	}

	// The file creation process is essentially complete at this point, report
	// complete progress.
	atomic.StoreUint64(&sf.atomicProgressDenominator, totalSize)

	// Sync the files.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err = sf.metadataFile.Sync()
		if err != nil {
			return build.ExtendErr("could not syncronize allocated sector metadata file", err)
		}
	}()
	go func() {
		defer wg.Done()
		err = sf.sectorFile.Sync()
		if err != nil {
			return build.ExtendErr("could not syncronize allocated sector data file", err)
		}
	}()
	wg.Wait()

	// Simulate power failure at this point for some testing scenarios.
	if wal.cm.dependencies.disrupt("incompleteAddStorageFolder") {
		return nil
	}

	// Storage folder addition has completed successfully, commit the addition
	// through the WAL.
	wal.mu.Lock()
	wal.cm.storageFolders[sf.Index] = sf
	err = wal.appendChange(stateChange{
		StorageFolderAdditions: []*storageFolder{sf},
	})
	wal.mu.Unlock()
	if err != nil {
		return build.ExtendErr("storage folder commitment assignment failed", err)
	}

	// Wait to confirm the storage folder addition has completed until the WAL
	// entry has synced.
	syncChan = wal.syncChan
	<-syncChan

	// Set the progress back to '0'.
	atomic.StoreUint64(&sf.atomicProgressNumerator, 0)
	atomic.StoreUint64(&sf.atomicProgressDenominator, 0)
	return nil
}

// cleanupUnfinishedStorageFolderAdditions will purge any unfinished storage
// folder additions from the previous run.
func (wal *writeAheadLog) cleanupUnfinishedStorageFolderAdditions(scs []stateChange) error {
	sfs := findUnfinishedStorageFolderAdditions(scs)
	for _, sf := range sfs {
		// Remove any leftover files.
		sectorLookupName := filepath.Join(sf.Path, metadataFile)
		sectorHousingName := filepath.Join(sf.Path, sectorFile)
		err := os.Remove(sectorLookupName)
		if err != nil {
			wal.cm.log.Println("Unable to remove documented sector metadata lookup:", sectorLookupName, err)
		}
		err = os.Remove(sectorHousingName)
		if err != nil {
			wal.cm.log.Println("Unable to remove documented sector housing:", sectorHousingName, err)
		}

		// Delete the storage folder from the storage folders map.
		delete(wal.cm.storageFolders, sf.index)

		// Append an error call to the changeset, indicating that the storage
		// folder add was not completed successfully.
		err = wal.appendChange(stateChange{
			ErroredStorageFolderAdditions: []uint16{sf.Index},
		})
		if err != nil {
			return build.ExtendErr("unable to close out unfinished storage folder addition", err)
		}
	}
	return nil
}

// commitAddStorageFolder integrates a pending AddStorageFolder call into the
// state. commitAddStorageFolder should only be called during WAL recovery.
func (wal *writeAheadLog) commitAddStorageFolder(sf *storageFolder) {
	var err error
	sf.sectorFile, err = wal.cm.dependencies.openFile(filepath.Join(sf.Path, sectorFile), os.O_RDWR, 0700)
	if err != nil {
		wal.cm.log.Println("Difficulties opening sector file for ", sf.Path, ":", err)
	}
	sf.sectorFile, err = wal.cm.dependencies.openFile(filepath.Join(sf.Path, sectorFile), os.O_RDWR, 0700)
	if err != nil {
		wal.cm.log.Println("Difficulties opening sector metadata file for", sf.Path, ":", err)
	}
	wal.cm.storageFolders[sf.Index] = sf
}

// AddStorageFolder adds a storage folder to the contract manager.
func (cm *ContractManager) AddStorageFolder(path string, size uint64) error {
	err := cm.tg.Add()
	if err != nil {
		return err
	}
	defer cm.tg.Done()

	// Check that the storage folder being added meets the size requirements.
	sectors := size / modules.SectorSize
	if sectors > maximumSectorsPerStorageFolder {
		return errLargeStorageFolder
	}
	if sectors < minimumSectorsPerStorageFolder {
		return errSmallStorageFolder
	}
	if (size/modules.SectorSize)%storageFolderGranularity != 0 {
		return errStorageFolderGranularity
	}
	// Check that the path is an absolute path.
	if !filepath.IsAbs(path) {
		return errRelativePath
	}

	// Check that the folder being linked to both exists and is a folder.
	pathInfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !pathInfo.Mode().IsDir() {
		return errStorageFolderNotFolder
	}

	// Create a storage folder object and add it to the WAL.
	newSF := &storageFolder{
		Path:  path,
		Usage: make([]uint64, size/modules.SectorSize/64),

		queuedSectors: make(map[sectorID]uint32),
	}
	err = cm.wal.managedAddStorageFolder(newSF)
	if err != nil {
		cm.log.Println("Call to AddStorageFolder has failed:", err)
		return err
	}
	return nil
}