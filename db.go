// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

// Package pebble provides an ordered key/value store.
package pebble // import "github.com/petermattis/pebble"

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/petermattis/pebble/arenaskl"
	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/record"
	"github.com/petermattis/pebble/sstable"
	"github.com/petermattis/pebble/storage"
)

const (
	// minTableCacheSize is the minimum size of the table cache.
	minTableCacheSize = 64

	// numNonTableCacheFiles is an approximation for the number of MaxOpenFiles
	// that we don't use for table caches.
	numNonTableCacheFiles = 10
)

// Reader is a readable key/value store.
//
// It is safe to call Get and NewIter from concurrent goroutines.
type Reader interface {
	// Get gets the value for the given key. It returns ErrNotFound if the DB
	// does not contain the key.
	//
	// The caller should not modify the contents of the returned slice, but
	// it is safe to modify the contents of the argument after Get returns.
	Get(key []byte) (value []byte, err error)

	// NewIter returns an iterator that is unpositioned (Iterator.Valid() will
	// return false). The iterator can be positioned via a call to SeekGE,
	// SeekLT, First or Last.
	NewIter(o *db.IterOptions) db.Iterator

	// Close closes the Reader. It may or may not close any underlying io.Reader
	// or io.Writer, depending on how the DB was created.
	//
	// It is not safe to close a DB until all outstanding iterators are closed.
	// It is valid to call Close multiple times. Other methods should not be
	// called after the DB has been closed.
	Close() error
}

// Writer is a writable key/value store.
//
// Goroutine safety is dependent on the specific implementation.
type Writer interface {
	// Apply the operations contained in the batch to the DB.
	//
	// It is safe to modify the contents of the arguments after Apply returns.
	Apply(batch *Batch, o *db.WriteOptions) error

	// Delete deletes the value for the given key. Deletes are blind all will
	// succeed even if the given key does not exist.
	//
	// It is safe to modify the contents of the arguments after Delete returns.
	Delete(key []byte, o *db.WriteOptions) error

	// DeleteRange deletes all of the keys (and values) in the range [start,end)
	// (inclusive on start, exclusive on end).
	//
	// It is safe to modify the contents of the arguments after Delete returns.
	DeleteRange(start, end []byte, o *db.WriteOptions) error

	// Merge merges the value for the given key. The details of the merge are
	// dependent upon the configured merge operation.
	//
	// It is safe to modify the contents of the arguments after Merge returns.
	Merge(key, value []byte, o *db.WriteOptions) error

	// Set sets the value for the given key. It overwrites any previous value
	// for that key; a DB is not a multi-map.
	//
	// It is safe to modify the contents of the arguments after Set returns.
	Set(key, value []byte, o *db.WriteOptions) error
}

// DB provides a concurrent, persistent ordered key/value store.
type DB struct {
	dirname   string
	opts      *db.Options
	cmp       db.Compare
	merge     db.Merge
	inlineKey db.InlineKey

	tableCache tableCache
	newIter    tableNewIter

	commit   *commitPipeline
	fileLock io.Closer

	// Rate limiter for how much bandwidth to allow for commits, compactions, and
	// flushes.
	//
	// TODO(peter): Add a controller module that balances the limits so that
	// commits cannot happen faster than flushes and the backlog of compaction
	// work does not grow too large.
	commitController  *controller
	compactController *controller
	flushController   *controller

	// TODO(peter): describe exactly what this mutex protects. So far: every
	// field in the struct.
	mu struct {
		sync.Mutex

		closed bool

		versions versionSet

		log struct {
			number uint64
			*record.LogWriter
		}

		mem struct {
			cond sync.Cond
			// The current mutable memTable.
			mutable *memTable
			// Queue of memtables (mutable is at end). Elements are added to the end
			// of the slice and removed from the beginning. Once an index is set it
			// is never modified making a fixed slice immutable and safe for
			// concurrent reads.
			queue []*memTable
			// True when the memtable is actively been switched. Both mem.mutable and
			// log.LogWriter are invalid while switching is true.
			switching bool
		}

		compact struct {
			cond           sync.Cond
			flushing       bool
			compacting     bool
			pendingOutputs map[uint64]struct{}
		}
	}
}

var _ Reader = (*DB)(nil)
var _ Writer = (*DB)(nil)

// Get gets the value for the given key. It returns ErrNotFound if the DB
// does not contain the key.
//
// The caller should not modify the contents of the returned slice, but
// it is safe to modify the contents of the argument after Get returns.
func (d *DB) Get(key []byte) ([]byte, error) {
	d.mu.Lock()
	snapshot := atomic.LoadUint64(&d.mu.versions.visibleSeqNum)
	// Grab and reference the current version to prevent its underlying files
	// from being deleted if we have a concurrent compaction. Note that
	// version.unref() can be called without holding DB.mu.
	current := d.mu.versions.currentVersion()
	current.ref()
	defer current.unref()
	memtables := d.mu.mem.queue
	d.mu.Unlock()

	ikey := db.MakeInternalKey(key, snapshot, db.InternalKeyKindMax)

	// Look in the memtables before going to the on-disk current version.
	for i := len(memtables) - 1; i >= 0; i-- {
		mem := memtables[i]
		iter := mem.NewIter(nil)
		iter.SeekGE(key)
		value, conclusive, err := internalGet(iter, d.cmp, ikey)
		if conclusive {
			return value, err
		}
	}

	// TODO(peter): update stats, maybe schedule compaction.

	return current.get(ikey, d.newIter, d.cmp, nil)
}

// Set sets the value for the given key. It overwrites any previous value
// for that key; a DB is not a multi-map.
//
// It is safe to modify the contents of the arguments after Set returns.
func (d *DB) Set(key, value []byte, opts *db.WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.Set(key, value, opts)
	return d.Apply(b, opts)
}

// Delete deletes the value for the given key. Deletes are blind all will
// succeed even if the given key does not exist.
//
// It is safe to modify the contents of the arguments after Delete returns.
func (d *DB) Delete(key []byte, opts *db.WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.Delete(key, opts)
	return d.Apply(b, opts)
}

// DeleteRange deletes all of the keys (and values) in the range [start,end)
// (inclusive on start, exclusive on end).
//
// It is safe to modify the contents of the arguments after DeleteRange
// returns.
func (d *DB) DeleteRange(start, end []byte, opts *db.WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.DeleteRange(start, end, opts)
	return d.Apply(b, opts)
}

// Merge adds an action to the DB that merges the value at key with the new
// value. The details of the merge are dependent upon the configured merge
// operator.
//
// It is safe to modify the contents of the arguments after Merge returns.
func (d *DB) Merge(key, value []byte, opts *db.WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.Merge(key, value, opts)
	return d.Apply(b, opts)
}

// Apply the operations contained in the batch to the DB.
//
// It is safe to modify the contents of the arguments after Apply returns.
func (d *DB) Apply(batch *Batch, opts *db.WriteOptions) error {
	return d.commit.Commit(batch, opts.GetSync())
}

func (d *DB) commitApply(b *Batch, mem *memTable) error {
	err := mem.apply(b, b.seqNum())
	if err != nil {
		return err
	}
	if mem.unref() {
		d.mu.Lock()
		d.maybeScheduleFlush()
		d.mu.Unlock()
	}
	return nil
}

func (d *DB) commitSync() error {
	d.mu.Lock()
	log := d.mu.log.LogWriter
	d.mu.Unlock()
	// NB: The log might have been closed after we unlock d.mu. That's ok because
	// it will have been synced and all we're guaranteeing is that the log that
	// was open at the start of this call was synced by the end of it.
	return log.Sync()
}

func (d *DB) commitWrite(b *Batch) (*memTable, error) {
	// NB: commitWrite is called with d.mu locked.

	// Throttle writes if there are too many L0 tables.
	d.throttleWrite()

	// Switch out the memtable if there was not enough room to store the
	// batch.
	if err := d.makeRoomForWrite(b); err != nil {
		return nil, err
	}

	_, err := d.mu.log.WriteRecord(b.data)
	if err != nil {
		panic(err)
	}
	return d.mu.mem.mutable, err
}

// newIterInternal constructs a new iterator, merging in batchIter as an extra
// level.
func (d *DB) newIterInternal(batchIter db.InternalIterator, o *db.IterOptions) db.Iterator {
	d.mu.Lock()
	seqNum := atomic.LoadUint64(&d.mu.versions.visibleSeqNum)
	// TODO(peter): The sstables in current are guaranteed to have sequence
	// numbers less than d.mu.versions.logSeqNum, so why does dbIter need to check
	// sequence numbers for every iter? Perhaps the sequence number filtering
	// should be folded into mergingIter (or InternalIterator).
	//
	// Grab and reference the current version to prevent its underlying files
	// from being deleted if we have a concurrent compaction. Note that
	// version.unref() can be called without holding DB.mu.
	current := d.mu.versions.currentVersion()
	current.ref()
	memtables := d.mu.mem.queue
	d.mu.Unlock()

	var buf struct {
		dbi    dbIter
		iters  [3 + numLevels]db.InternalIterator
		levels [numLevels]levelIter
	}

	dbi := &buf.dbi
	dbi.cmp = d.cmp
	dbi.merge = d.merge
	dbi.version = current

	iters := buf.iters[:0]
	if batchIter != nil {
		iters = append(iters, batchIter)
	}

	for i := len(memtables) - 1; i >= 0; i-- {
		mem := memtables[i]
		iters = append(iters, mem.NewIter(o))
	}

	// The level 0 files need to be added from newest to oldest.
	for i := len(current.files[0]) - 1; i >= 0; i-- {
		f := &current.files[0][i]
		iter, err := d.newIter(f)
		if err != nil {
			dbi.err = err
			return dbi
		}
		iters = append(iters, iter)
	}

	// Add level iterators for the remaining files.
	levels := buf.levels[:]
	for level := 1; level < len(current.files); level++ {
		n := len(current.files[level])
		if n == 0 {
			continue
		}

		var li *levelIter
		if len(levels) > 0 {
			li = &levels[0]
			levels = levels[1:]
		} else {
			li = &levelIter{}
		}

		li.init(d.cmp, d.newIter, current.files[level])
		iters = append(iters, li)
	}

	dbi.iter = newMergingIter(d.cmp, iters...)
	dbi.seqNum = seqNum
	return dbi
}

// NewIter returns an iterator that is unpositioned (Iterator.Valid() will
// return false). The iterator can be positioned via a call to SeekGE,
// SeekLT, First or Last.
func (d *DB) NewIter(o *db.IterOptions) db.Iterator {
	return d.newIterInternal(nil, o)
}

// NewBatch returns a new empty write-only batch. Any reads on the batch will
// return an error. If the batch is committed it will be applied to the DB.
func (d *DB) NewBatch() *Batch {
	return newBatch(d)
}

// NewIndexedBatch returns a new empty read-write batch. Any reads on the batch
// will read from both the batch and the DB. If the batch is committed it will
// be applied to the DB. An indexed batch is slower that a non-indexed batch
// for insert operations. If you do not need to perform reads on the batch, use
// NewBatch instead.
func (d *DB) NewIndexedBatch() *Batch {
	return newIndexedBatch(d, d.opts.Comparer)
}

// Close closes the DB.
//
// It is not safe to close a DB until all outstanding iterators are closed.
// It is valid to call Close multiple times. Other methods should not be
// called after the DB has been closed.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mu.closed {
		return nil
	}
	for d.mu.compact.compacting || d.mu.compact.flushing {
		d.mu.compact.cond.Wait()
	}
	err := d.tableCache.Close()
	err = firstError(err, d.mu.log.Close())
	err = firstError(err, d.fileLock.Close())
	d.commit.Close()
	d.mu.closed = true
	return err
}

// Compact the specified range of keys in the database.
//
// TODO(peter): unimplemented
func (d *DB) Compact(start, end []byte /* CompactionOptions */) error {
	panic("pebble.DB: Compact unimplemented")
}

// Flush the memtable to stable storage.
//
// TODO(peter): untested
func (d *DB) Flush() error {
	d.mu.Lock()
	mem := d.mu.mem.mutable
	err := d.makeRoomForWrite(nil)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	<-mem.flushed
	return nil
}

// firstError returns the first non-nil error of err0 and err1, or nil if both
// are nil.
func firstError(err0, err1 error) error {
	if err0 != nil {
		return err0
	}
	return err1
}

// writeLevel0Table writes a memtable to a level-0 on-disk table.
//
// If no error is returned, it adds the file number of that on-disk table to
// d.pendingOutputs. It is the caller's responsibility to remove that fileNum
// from that set when it has been applied to d.mu.versions.
//
// d.mu must be held when calling this, but the mutex may be dropped and
// re-acquired during the course of this method.
func (d *DB) writeLevel0Table(
	fs storage.Storage, iter db.InternalIterator,
) (meta fileMetadata, err error) {
	meta.fileNum = d.mu.versions.nextFileNum()
	filename := dbFilename(d.dirname, fileTypeTable, meta.fileNum)
	d.mu.compact.pendingOutputs[meta.fileNum] = struct{}{}
	defer func(fileNum uint64) {
		if err != nil {
			delete(d.mu.compact.pendingOutputs, fileNum)
		}
	}(meta.fileNum)

	// Release the d.mu lock while doing I/O.
	// Note the unusual order: Unlock and then Lock.
	d.mu.Unlock()
	defer d.mu.Lock()

	var (
		file storage.File
		tw   *sstable.Writer
	)
	defer func() {
		if iter != nil {
			err = firstError(err, iter.Close())
		}
		if tw != nil {
			err = firstError(err, tw.Close())
		}
		if err != nil {
			fs.Remove(filename)
			meta = fileMetadata{}
		}
	}()

	iter.First()
	if !iter.Valid() {
		return fileMetadata{}, fmt.Errorf("pebble: memtable empty")
	}

	file, err = fs.Create(filename)
	if err != nil {
		return fileMetadata{}, err
	}
	file = newRateLimitedFile(file, d.flushController)
	tw = sstable.NewWriter(file, d.opts, d.opts.Level(0))

	meta.smallest = iter.Key().Clone()
	for {
		// TODO(peter): support c.shouldStopBefore.

		meta.largest = iter.Key()
		if err1 := tw.Add(meta.largest, iter.Value()); err1 != nil {
			return fileMetadata{}, err1
		}
		if !iter.Next() {
			break
		}
	}
	meta.largest = meta.largest.Clone()

	if err1 := iter.Close(); err1 != nil {
		iter = nil
		return fileMetadata{}, err1
	}
	iter = nil

	if err1 := tw.Close(); err1 != nil {
		tw = nil
		return fileMetadata{}, err1
	}

	stat, err := tw.Stat()
	if err != nil {
		return fileMetadata{}, err
	}
	size := stat.Size()
	if size < 0 {
		return fileMetadata{}, fmt.Errorf("pebble: table file %q has negative size %d", filename, size)
	}
	meta.size = uint64(size)
	tw = nil

	// TODO(peter): After a flush we set the commit rate to 110% of the flush
	// rate. The rationale behind the 110% is to account for slack. Investigate a
	// more principled way of setting this.
	// d.commitController.limiter.SetLimit(rate.Limit(d.flushController.sensor.Rate()))
	// if false {
	// 	fmt.Printf("flush: %.1f MB/s\n", d.flushController.sensor.Rate()/float64(1<<20))
	// }

	// TODO(peter): compaction stats.

	return meta, nil
}

func (d *DB) throttleWrite() {
	if len(d.mu.versions.currentVersion().files[0]) <= d.opts.L0SlowdownWritesThreshold {
		return
	}
	// fmt.Printf("L0 slowdown writes threshold\n")
	// We are getting close to hitting a hard limit on the number of L0
	// files. Rather than delaying a single write by several seconds when we hit
	// the hard limit, start delaying each individual write by 1ms to reduce
	// latency variance.
	//
	// TODO(peter): Use more sophisticated rate limiting.
	d.mu.Unlock()
	time.Sleep(1 * time.Millisecond)
	d.mu.Lock()
}

func (d *DB) makeRoomForWrite(b *Batch) error {
	for force := b == nil; ; {
		if d.mu.mem.switching {
			d.mu.mem.cond.Wait()
			continue
		}
		if b != nil {
			err := d.mu.mem.mutable.prepare(b)
			if err == nil {
				return nil
			}
			if err != arenaskl.ErrArenaFull {
				return err
			}
		} else if !force {
			return nil
		}
		if len(d.mu.mem.queue) >= d.opts.MemTableStopWritesThreshold {
			// We have filled up the current memtable, but the previous one is still
			// being compacted, so we wait.
			// fmt.Printf("memtable stop writes threshold\n")
			d.mu.compact.cond.Wait()
			continue
		}
		if len(d.mu.versions.currentVersion().files[0]) > d.opts.L0StopWritesThreshold {
			// There are too many level-0 files, so we wait.
			// fmt.Printf("L0 stop writes threshold\n")
			d.mu.compact.cond.Wait()
			continue
		}

		newLogNumber := d.mu.versions.nextFileNum()
		d.mu.mem.switching = true
		d.mu.Unlock()

		newLogFile, err := d.opts.Storage.Create(dbFilename(d.dirname, fileTypeLog, newLogNumber))
		if err == nil {
			err = d.mu.log.Close()
			if err != nil {
				newLogFile.Close()
			}
		}

		d.mu.Lock()
		d.mu.mem.switching = false
		d.mu.mem.cond.Broadcast()

		if err != nil {
			// TODO(peter): avoid chewing through file numbers in a tight loop if there
			// is an error here.
			//
			// What to do here? Stumbling on doesn't seem worthwhile. If we failed to
			// close the previous log it is possible we lost a write.
			panic(err)
		}

		// NB: When the immutable memtable is flushed to disk it will apply a
		// versionEdit to the manifest telling it that log files < d.mu.log.number
		// have been applied.
		d.mu.log.number = newLogNumber
		d.mu.log.LogWriter = record.NewLogWriter(newLogFile)
		imm := d.mu.mem.mutable
		d.mu.mem.mutable = newMemTable(d.opts)
		d.mu.mem.queue = append(d.mu.mem.queue, d.mu.mem.mutable)
		if imm.unref() {
			d.maybeScheduleFlush()
		}
		force = false
	}
}
