package renter

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/modules"
)

var (
	errInsufficientContracts = errors.New("not enough contracts to upload file")

	// Erasure-coded piece size
	pieceSize = modules.SectorSize - crypto.TwofishOverhead

	// defaultDataPieces is the number of data pieces per erasure-coded chunk
	defaultDataPieces = func() int {
		switch build.Release {
		case "dev":
			return 1
		case "standard":
			return 4
		case "testing":
			return 1
		}
		panic("undefined defaultDataPieces")
	}()

	// defaultParityPieces is the number of parity pieces per erasure-coded
	// chunk
	defaultParityPieces = func() int {
		switch build.Release {
		case "dev":
			return 1
		case "standard":
			return 20
		case "testing":
			return 8
		}
		panic("undefined defaultParityPieces")
	}()
)

// Upload instructs the renter to start tracking a file. The renter will
// automatically upload and repair tracked files using a background loop.
func (r *Renter) Upload(up modules.FileUploadParams) error {
	// Enforce nickname rules.
	if strings.HasPrefix(up.SiaPath, "/") {
		return errors.New("nicknames cannot begin with /")
	}
	if up.SiaPath == "" {
		return ErrEmptyFilename
	}

	// Check for a nickname conflict.
	lockID := r.mu.RLock()
	_, exists := r.files[up.SiaPath]
	r.mu.RUnlock(lockID)
	if exists {
		return ErrPathOverload
	}

	// Fill in any missing upload params with sensible defaults.
	fileInfo, err := os.Stat(up.Source)
	if err != nil {
		return err
	}
	if up.ErasureCode == nil {
		up.ErasureCode, _ = NewRSCode(defaultDataPieces, defaultParityPieces)
	}

	// Check that we have contracts to upload to. We need at least (data +
	// parity/2) contracts; since NumPieces = data + parity, we arrive at the
	// expression below.
	if nContracts := len(r.hostContractor.Contracts()); nContracts < (up.ErasureCode.NumPieces()+up.ErasureCode.MinPieces())/2 && build.Release != "testing" {
		return fmt.Errorf("not enough contracts to upload file: got %v, needed %v", nContracts, (up.ErasureCode.NumPieces()+up.ErasureCode.MinPieces())/2)
	}

	// Create file object.
	f := newFile(up.SiaPath, up.ErasureCode, pieceSize, uint64(fileInfo.Size()))
	f.mode = uint32(fileInfo.Mode())

	// Add file to renter.
	lockID = r.mu.Lock()
	r.files[up.SiaPath] = f
	r.tracking[up.SiaPath] = trackedFile{
		RepairPath: up.Source,
	}
	r.saveSync()
	r.mu.Unlock(lockID)

	// Save the .sia file to the renter directory.
	err = r.saveFile(f)
	if err != nil {
		return err
	}

	return nil
}
