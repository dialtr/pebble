// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/record"
	"github.com/petermattis/pebble/storage"
)

// TODO(peter): describe what a versionSet is.
type versionSet struct {
	// Immutable fields.
	dirname string
	opts    *db.Options
	fs      storage.Storage
	cmp     db.Compare
	cmpName string

	// Mutable fields.
	versions versionList

	logNumber          uint64
	prevLogNumber      uint64
	nextFileNumber     uint64
	logSeqNum          uint64 // next seqNum to use for WAL writes
	visibleSeqNum      uint64 // visible seqNum (< logSeqNum)
	manifestFileNumber uint64

	manifestFile storage.File
	manifest     *record.Writer
}

// load loads the version set from the manifest file.
func (vs *versionSet) load(dirname string, opts *db.Options) error {
	vs.dirname = dirname
	vs.opts = opts
	vs.fs = opts.Storage
	vs.cmp = opts.Comparer.Compare
	vs.cmpName = opts.Comparer.Name
	vs.versions.init()
	// For historical reasons, the next file number is initialized to 2.
	vs.nextFileNumber = 2

	// Read the CURRENT file to find the current manifest file.
	current, err := vs.fs.Open(dbFilename(dirname, fileTypeCurrent, 0))
	if err != nil {
		return fmt.Errorf("pebble: could not open CURRENT file for DB %q: %v", dirname, err)
	}
	defer current.Close()
	stat, err := current.Stat()
	if err != nil {
		return err
	}
	n := stat.Size()
	if n == 0 {
		return fmt.Errorf("pebble: CURRENT file for DB %q is empty", dirname)
	}
	if n > 4096 {
		return fmt.Errorf("pebble: CURRENT file for DB %q is too large", dirname)
	}
	b := make([]byte, n)
	_, err = current.ReadAt(b, 0)
	if err != nil {
		return err
	}
	if b[n-1] != '\n' {
		return fmt.Errorf("pebble: CURRENT file for DB %q is malformed", dirname)
	}
	b = b[:n-1]

	// Read the versionEdits in the manifest file.
	var bve bulkVersionEdit
	manifest, err := vs.fs.Open(dirname + string(os.PathSeparator) + string(b))
	if err != nil {
		return fmt.Errorf("pebble: could not open manifest file %q for DB %q: %v", b, dirname, err)
	}
	defer manifest.Close()
	rr := record.NewReader(manifest)
	for {
		r, err := rr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		var ve versionEdit
		err = ve.decode(r)
		if err != nil {
			return err
		}
		if ve.comparatorName != "" {
			if ve.comparatorName != vs.cmpName {
				return fmt.Errorf("pebble: manifest file %q for DB %q: "+
					"comparer name from file %q != comparer name from db.Options %q",
					b, dirname, ve.comparatorName, vs.cmpName)
			}
		}
		bve.accumulate(&ve)
		if ve.logNumber != 0 {
			vs.logNumber = ve.logNumber
		}
		if ve.prevLogNumber != 0 {
			vs.prevLogNumber = ve.prevLogNumber
		}
		if ve.nextFileNumber != 0 {
			vs.nextFileNumber = ve.nextFileNumber
		}
		if ve.lastSequence != 0 {
			vs.logSeqNum = ve.lastSequence
		}
	}
	if vs.logNumber == 0 || vs.nextFileNumber == 0 {
		if vs.nextFileNumber == 2 {
			// We have a freshly created DB.
		} else {
			return fmt.Errorf("pebble: incomplete manifest file %q for DB %q", b, dirname)
		}
	}
	vs.markFileNumUsed(vs.logNumber)
	vs.markFileNumUsed(vs.prevLogNumber)
	vs.manifestFileNumber = vs.nextFileNum()

	newVersion, err := bve.apply(opts, nil, vs.cmp)
	if err != nil {
		return err
	}
	vs.append(newVersion)
	return nil
}

// TODO(peter): describe what this function does and how it interacts
// concurrently with a running pebble.
//
// d.mu must be held when calling this, for the enclosing *DB d.
//
// TODO(peter): actually pass d.mu, and drop and re-acquire it around the I/O.
func (vs *versionSet) logAndApply(opts *db.Options, dirname string, ve *versionEdit) error {
	if ve.logNumber != 0 {
		if ve.logNumber < vs.logNumber || vs.nextFileNumber <= ve.logNumber {
			panic(fmt.Sprintf("pebble: inconsistent versionEdit logNumber %d", ve.logNumber))
		}
	}
	ve.nextFileNumber = vs.nextFileNumber
	ve.lastSequence = atomic.LoadUint64(&vs.logSeqNum)

	var bve bulkVersionEdit
	bve.accumulate(ve)
	newVersion, err := bve.apply(opts, vs.currentVersion(), vs.cmp)
	if err != nil {
		return err
	}

	if vs.manifest == nil {
		if err := vs.createManifest(dirname); err != nil {
			return err
		}
	}

	w, err := vs.manifest.Next()
	if err != nil {
		return err
	}
	if err := ve.encode(w); err != nil {
		return err
	}
	if err := vs.manifest.Flush(); err != nil {
		return err
	}
	if err := vs.manifestFile.Sync(); err != nil {
		return err
	}
	if err := setCurrentFile(dirname, vs.opts.Storage, vs.manifestFileNumber); err != nil {
		return err
	}

	// Install the new version.
	vs.append(newVersion)
	if ve.logNumber != 0 {
		vs.logNumber = ve.logNumber
	}
	if ve.prevLogNumber != 0 {
		vs.prevLogNumber = ve.prevLogNumber
	}
	return nil
}

// createManifest creates a manifest file that contains a snapshot of vs.
func (vs *versionSet) createManifest(dirname string) (err error) {
	var (
		filename     = dbFilename(dirname, fileTypeManifest, vs.manifestFileNumber)
		manifestFile storage.File
		manifest     *record.Writer
	)
	defer func() {
		if manifest != nil {
			manifest.Close()
		}
		if manifestFile != nil {
			manifestFile.Sync()
			manifestFile.Close()
		}
		if err != nil {
			vs.fs.Remove(filename)
		}
	}()
	manifestFile, err = vs.fs.Create(filename)
	if err != nil {
		return err
	}
	manifest = record.NewWriter(manifestFile)

	snapshot := versionEdit{
		comparatorName: vs.cmpName,
	}
	// TODO(peter): save compaction pointers.
	for level, fileMetadata := range vs.currentVersion().files {
		for _, meta := range fileMetadata {
			snapshot.newFiles = append(snapshot.newFiles, newFileEntry{
				level: level,
				meta:  meta,
			})
		}
	}

	w, err1 := manifest.Next()
	if err1 != nil {
		return err1
	}
	if err := snapshot.encode(w); err != nil {
		return err
	}

	vs.manifest, manifest = manifest, nil
	vs.manifestFile, manifestFile = manifestFile, nil
	return nil
}

func (vs *versionSet) markFileNumUsed(fileNum uint64) {
	if vs.nextFileNumber <= fileNum {
		vs.nextFileNumber = fileNum + 1
	}
}

func (vs *versionSet) nextFileNum() uint64 {
	x := vs.nextFileNumber
	vs.nextFileNumber++
	return x
}

func (vs *versionSet) append(v *version) {
	if v.refs != 0 {
		panic("pebble: version should be unreferenced")
	}
	if !vs.versions.empty() {
		vs.versions.back().unrefLocked()
	}
	v.ref()
	vs.versions.pushBack(v)
}

func (vs *versionSet) currentVersion() *version {
	return vs.versions.back()
}

func (vs *versionSet) addLiveFileNums(m map[uint64]struct{}) {
	for v := vs.versions.root.next; v != &vs.versions.root; v = v.next {
		for _, ff := range v.files {
			for _, f := range ff {
				m[f.fileNum] = struct{}{}
			}
		}
	}
}
