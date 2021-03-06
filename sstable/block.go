// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"encoding/binary"
	"errors"
	"sort"
	"unsafe"

	"github.com/petermattis/pebble/db"
)

func uvarintLen(v uint32) int {
	i := 0
	for v >= 0x80 {
		v >>= 7
		i++
	}
	return i + 1
}

type blockWriter struct {
	restartInterval int
	nEntries        int
	buf             []byte
	restarts        []uint32
	curKey          []byte
	prevKey         []byte
	tmp             [50]byte
}

func (w *blockWriter) store(keySize int, value []byte) {
	shared := 0
	if w.nEntries%w.restartInterval == 0 {
		w.restarts = append(w.restarts, uint32(len(w.buf)))
	} else {
		shared = db.SharedPrefixLen(w.curKey, w.prevKey)
	}

	n := binary.PutUvarint(w.tmp[0:], uint64(shared))
	n += binary.PutUvarint(w.tmp[n:], uint64(keySize-shared))
	n += binary.PutUvarint(w.tmp[n:], uint64(len(value)))
	w.buf = append(w.buf, w.tmp[:n]...)
	w.buf = append(w.buf, w.curKey[shared:]...)
	w.buf = append(w.buf, value...)

	w.nEntries++
}

func (w *blockWriter) add(key db.InternalKey, value []byte) {
	w.curKey, w.prevKey = w.prevKey, w.curKey

	size := key.Size()
	if cap(w.curKey) < size {
		w.curKey = make([]byte, 0, size*2)
	}
	w.curKey = w.curKey[:size]
	key.Encode(w.curKey)

	w.store(size, value)
}

func (w *blockWriter) finish() []byte {
	// Write the restart points to the buffer.
	if w.nEntries == 0 {
		// Every block must have at least one restart point.
		if cap(w.restarts) > 0 {
			w.restarts = w.restarts[:1]
			w.restarts[0] = 0
		} else {
			w.restarts = append(w.restarts, 0)
		}
	}
	tmp4 := w.tmp[:4]
	for _, x := range w.restarts {
		binary.LittleEndian.PutUint32(tmp4, x)
		w.buf = append(w.buf, tmp4...)
	}
	binary.LittleEndian.PutUint32(tmp4, uint32(len(w.restarts)))
	w.buf = append(w.buf, tmp4...)
	return w.buf
}

func (w *blockWriter) reset() {
	w.nEntries = 0
	w.buf = w.buf[:0]
	w.restarts = w.restarts[:0]
}

func (w *blockWriter) estimatedSize() int {
	return len(w.buf) + 4*(len(w.restarts)+1)
}

type blockEntry struct {
	offset int
	key    []byte
	val    []byte
}

// blockIter is an iterator over a single block of data.
type blockIter struct {
	cmp          db.Compare
	offset       int
	nextOffset   int
	restarts     int
	numRestarts  int
	globalSeqNum uint64
	ptr          unsafe.Pointer
	data         []byte
	key, val     []byte
	ikey         db.InternalKey
	cached       []blockEntry
	cachedBuf    []byte
	err          error
}

// blockIter implements the db.InternalIterator interface.
var _ db.InternalIterator = (*blockIter)(nil)

func newBlockIter(cmp db.Compare, block block) (*blockIter, error) {
	i := &blockIter{}
	return i, i.init(cmp, block, 0)
}

func (i *blockIter) init(cmp db.Compare, block block, globalSeqNum uint64) error {
	numRestarts := int(binary.LittleEndian.Uint32(block[len(block)-4:]))
	if numRestarts == 0 {
		return errors.New("pebble/table: invalid table (block has no restart points)")
	}
	i.cmp = cmp
	i.restarts = len(block) - 4*(1+numRestarts)
	i.numRestarts = numRestarts
	i.globalSeqNum = globalSeqNum
	i.ptr = unsafe.Pointer(&block[0])
	i.data = block
	if i.key == nil {
		i.key = make([]byte, 0, 256)
	} else {
		i.key = i.key[:0]
	}
	i.val = nil
	i.clearCache()
	return nil
}

func (i *blockIter) readEntry() {
	ptr := unsafe.Pointer(uintptr(i.ptr) + uintptr(i.offset))
	shared, ptr := decodeVarint(ptr)
	unshared, ptr := decodeVarint(ptr)
	value, ptr := decodeVarint(ptr)
	i.key = append(i.key[:shared], getBytes(ptr, int(unshared))...)
	i.key = i.key[:len(i.key):len(i.key)]
	ptr = unsafe.Pointer(uintptr(ptr) + uintptr(unshared))
	i.val = getBytes(ptr, int(value))
	i.nextOffset = int(uintptr(ptr)-uintptr(i.ptr)) + int(value)
}

func (i *blockIter) decodeInternalKey() {
	i.ikey = db.DecodeInternalKey(i.key)
	if i.globalSeqNum != 0 {
		i.ikey.SetSeqNum(i.globalSeqNum)
	}
}

func (i *blockIter) loadEntry() {
	i.readEntry()
	i.decodeInternalKey()
}

func (i *blockIter) clearCache() {
	i.cached = i.cached[:0]
	i.cachedBuf = i.cachedBuf[:0]
}

func (i *blockIter) cacheEntry() {
	i.cachedBuf = append(i.cachedBuf, i.key...)
	i.cached = append(i.cached, blockEntry{
		offset: i.offset,
		key:    i.cachedBuf[len(i.cachedBuf)-len(i.key) : len(i.cachedBuf) : len(i.cachedBuf)],
		val:    i.val,
	})
}

// SeekGE implements InternalIterator.SeekGE, as documented in the pebble/db
// package.
func (i *blockIter) SeekGE(key []byte) {
	ikey := db.MakeSearchKey(key)

	// Find the index of the smallest restart point whose key is > the key
	// sought; index will be numRestarts if there is no such restart point.
	i.offset = 0
	index := sort.Search(i.numRestarts, func(j int) bool {
		offset := int(binary.LittleEndian.Uint32(i.data[i.restarts+4*j:]))
		// For a restart point, there are 0 bytes shared with the previous key.
		// The varint encoding of 0 occupies 1 byte.
		ptr := unsafe.Pointer(uintptr(i.ptr) + uintptr(offset+1))
		// Decode the key at that restart point, and compare it to the key sought.
		v1, ptr := decodeVarint(ptr)
		_, ptr = decodeVarint(ptr)
		s := getBytes(ptr, int(v1))
		return db.InternalCompare(i.cmp, ikey, db.DecodeInternalKey(s)) < 0
	})

	// Since keys are strictly increasing, if index > 0 then the restart point at
	// index-1 will be the largest whose key is <= the key sought.  If index ==
	// 0, then all keys in this block are larger than the key sought, and offset
	// remains at zero.
	if index > 0 {
		i.offset = int(binary.LittleEndian.Uint32(i.data[i.restarts+4*(index-1):]))
	}
	i.loadEntry()

	// Iterate from that restart point to somewhere >= the key sought.
	for ; i.Valid(); i.Next() {
		if db.InternalCompare(i.cmp, i.ikey, ikey) >= 0 {
			break
		}
	}
}

// SeekLT implements InternalIterator.SeekLT, as documented in the pebble/db
// package.
func (i *blockIter) SeekLT(key []byte) {
	ikey := db.MakeSearchKey(key)

	// Find the index of the smallest restart point whose key is >= the key
	// sought; index will be numRestarts if there is no such restart point.
	i.offset = 0
	index := sort.Search(i.numRestarts, func(j int) bool {
		offset := int(binary.LittleEndian.Uint32(i.data[i.restarts+4*j:]))
		// For a restart point, there are 0 bytes shared with the previous key.
		// The varint encoding of 0 occupies 1 byte.
		ptr := unsafe.Pointer(uintptr(i.ptr) + uintptr(offset+1))
		// Decode the key at that restart point, and compare it to the key sought.
		v1, ptr := decodeVarint(ptr)
		_, ptr = decodeVarint(ptr)
		s := getBytes(ptr, int(v1))
		return db.InternalCompare(i.cmp, ikey, db.DecodeInternalKey(s)) <= 0
	})

	// Since keys are strictly increasing, if index > 0 then the restart point at
	// index-1 will be the largest whose key is < the key sought.
	if index > 0 {
		i.offset = int(binary.LittleEndian.Uint32(i.data[i.restarts+4*(index-1):]))
	} else if index == 0 {
		// If index == 0 then all keys in this block are larger than the key
		// sought.
		i.offset = -1
		i.nextOffset = 0
		return
	}

	// Iterate from that restart point to somewhere >= the key sought, then back
	// up to the previous entry. The expectation is that we'll be performing
	// reverse iteration, so we cache the entries as we advance forward.
	i.clearCache()
	i.nextOffset = i.offset

	for {
		i.offset = i.nextOffset
		i.readEntry()
		i.decodeInternalKey()

		if db.InternalCompare(i.cmp, i.ikey, ikey) >= 0 {
			// The current key is greater than or equal to our search key. Back up to
			// the previous key which was less than our search key.
			i.Prev()
			return
		}

		i.cacheEntry()
		if i.nextOffset >= i.restarts {
			// We've reached the end of the block. Return the current key.
			break
		}
	}
}

// First implements InternalIterator.First, as documented in the pebble/db
// package.
func (i *blockIter) First() {
	i.offset = 0
	i.loadEntry()
}

// Last implements InternalIterator.Last, as documented in the pebble/db package.
func (i *blockIter) Last() {
	// Seek forward from the last restart point.
	i.offset = int(binary.LittleEndian.Uint32(i.data[i.restarts+4*(i.numRestarts-1):]))

	i.readEntry()
	i.clearCache()
	i.cacheEntry()

	for i.nextOffset < i.restarts {
		i.offset = i.nextOffset
		i.readEntry()
		i.cacheEntry()
	}

	i.decodeInternalKey()
}

// Next implements InternalIterator.Next, as documented in the pebble/db
// package.
func (i *blockIter) Next() bool {
	i.offset = i.nextOffset
	if !i.Valid() {
		return false
	}
	i.loadEntry()
	return true
}

// NextUserKey implements InternalIterator.NextUserKey, as documented in the
// pebble/db package.
func (i *blockIter) NextUserKey() bool {
	// TODO(peter): An sstable might contain multiple versions of the same
	// user-key. Such keys will have 8 bytes or fewer of unshared key.
	return i.Next()
}

// Prev implements InternalIterator.Prev, as documented in the pebble/db
// package.
func (i *blockIter) Prev() bool {
	if n := len(i.cached) - 1; n > 0 && i.cached[n].offset == i.offset {
		i.nextOffset = i.offset
		e := &i.cached[n-1]
		i.offset = e.offset
		i.key = e.key
		i.val = e.val
		i.decodeInternalKey()
		i.cached = i.cached[:n]
		return true
	}

	if i.offset == 0 {
		i.offset = -1
		i.nextOffset = 0
		return false
	}

	targetOffset := i.offset
	index := sort.Search(i.numRestarts, func(j int) bool {
		offset := int(binary.LittleEndian.Uint32(i.data[i.restarts+4*j:]))
		return offset >= targetOffset
	})
	i.offset = 0
	if index > 0 {
		i.offset = int(binary.LittleEndian.Uint32(i.data[i.restarts+4*(index-1):]))
	}

	i.readEntry()
	i.clearCache()
	i.cacheEntry()

	for i.nextOffset < targetOffset {
		i.offset = i.nextOffset
		i.readEntry()
		i.cacheEntry()
	}

	i.decodeInternalKey()
	return true
}

// PrevUserKey implements InternalIterator.PrevUserKey, as documented in the
// pebble/db package.
func (i *blockIter) PrevUserKey() bool {
	// TODO(peter): An sstable might contain multiple versions of the same
	// user-key. Such keys will have 8 bytes or fewer of unshared key.
	return i.Prev()
}

// Key implements InternalIterator.Key, as documented in the pebble/db package.
func (i *blockIter) Key() db.InternalKey {
	return i.ikey
}

// Value implements InternalIterator.Value, as documented in the pebble/db
// package.
func (i *blockIter) Value() []byte {
	return i.val
}

// Valid implements InternalIterator.Valid, as documented in the pebble/db
// package.
func (i *blockIter) Valid() bool {
	return i.offset >= 0 && i.offset < i.restarts
}

// Error implements InternalIterator.Error, as documented in the pebble/db
// package.
func (i *blockIter) Error() error {
	return i.err
}

// Close implements InternalIterator.Close, as documented in the pebble/db
// package.
func (i *blockIter) Close() error {
	i.val = nil
	return i.err
}
