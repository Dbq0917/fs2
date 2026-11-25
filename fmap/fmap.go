package fmap

/*
#include "fmap.h"
*/
import "C"

import (
	// "hash/crc32"
	"os"
	"reflect"
	"syscall"
	"unsafe"
)

import (
	"github.com/timtadh/fs2/errors"
	"github.com/timtadh/fs2/slice"
)

const BLOCKSIZE = 4096

// freeMagic marks the on-disk structures of a free run. Its little
// endian byte representation is the ASCII string "FREEBLK1".
const freeMagic uint64 = 0x314b4c4245455246

// A freeblkHdr is stored in the first block of every free run. A free
// run is a maximal sequence of adjacent free blocks. The runs are kept
// in a doubly linked list so a run can be unlinked in O(1) while
// coalescing neighbors during a Free.
type freeblkHdr struct {
	magic  uint64 // always freeMagic for the start of a free run
	prev   uint64 // offset of the previous free run (0 == list head)
	next   uint64 // offset of the next free run (0 == list tail)
	length uint64 // length of the run in blocks
}

const freeHdrSize = 32

// A freeblkFtr is stored in the last block of every free run. It lets
// Free find the run immediately preceding a newly freed block in O(1)
// without scanning the whole free list (a boundary tag).
type freeblkFtr struct {
	magic uint64 // always freeMagic on the last block of a run
	head  uint64 // offset of the first block of the run
}

const freeFtrSize = 16

func loadFreeHdr(bytes []byte) *freeblkHdr {
	back := slice.AsSlice(&bytes)
	return (*freeblkHdr)(back.Array)
}

func loadFreeFtr(block []byte) *freeblkFtr {
	back := slice.AsSlice(&block)
	ptr := unsafe.Pointer(uintptr(back.Array) + uintptr(len(block)-freeFtrSize))
	return (*freeblkFtr)(ptr)
}

type ctrldata struct {
	// checksum  uint32
	blksize   uint32
	free_len  uint32
	free_head uint64
}

const ctrldataSize = 16

var ctrldataSizeActual int

func init() {
	c := &ctrldata{}
	ctrldataSizeActual = int(c.Size())
	if ctrldataSizeActual != ctrldataSize {
		panic("the ctrldataSize was an unexpected size")
	}
	var h freeblkHdr
	if int(reflect.TypeOf(h).Size()) != freeHdrSize {
		panic("the freeblkHdr was an unexpected size")
	}
	var f freeblkFtr
	if int(reflect.TypeOf(f).Size()) != freeFtrSize {
		panic("the freeblkFtr was an unexpected size")
	}
}

func (cd *ctrldata) Size() uintptr {
	return reflect.TypeOf(*cd).Size()
}

type ctrlblk struct {
	meta ctrldata
	user [BLOCKSIZE - ctrldataSize]byte
}

func load_ctrlblk(bytes []byte) (cb *ctrlblk, err error) {
	back := slice.AsSlice(&bytes)
	cb = (*ctrlblk)(back.Array)
	return cb, nil
}

func new_ctrlblk(bytes []byte, blksize uint32) (cb *ctrlblk) {
	back := slice.AsSlice(&bytes)
	cb = (*ctrlblk)(back.Array)
	cb.meta.blksize = blksize
	cb.meta.free_head = 0
	cb.meta.free_len = 0
	MemClr(cb.user[:])
	return cb
}

func (cb *ctrlblk) updateChkSum() {
	// cb.meta.checksum = crc32.ChecksumIEEE(cb.back[4:])
}

// A BlockFile represents the memory mapped file. It has a blocksize all
// operations are done as block aligned operations.
type BlockFile struct {
	path        string
	opened      bool
	size        uint64
	blksize     int
	file        *os.File
	mmap        unsafe.Pointer
	outstanding int "total outstanding pointers"
}

// Zero the bytes of the passed in slice. It uses the length not the
// capacity of the slice. It uses the libc function memset under the
// hood to do this. Go has an implementation of this function in
// assembly in the runtime package but I could not find a nice way to
// expose it. So here is the libc version exposed (via several method
// calls and the cgo interface).
func MemClr(bytes []byte) {
	memClr(slice.AsSlice(&bytes).Array, uintptr(len(bytes)))
}

func memClr(ptr unsafe.Pointer, size uintptr) {
	C.memclr(ptr, C.size_t(size))
}

// Create a blockfile with the standard block size (4096 which is
// normally the OS page size).
func CreateBlockFile(path string) (*BlockFile, error) {
	return CreateBlockFileCustomBlockSize(path, BLOCKSIZE)
}

// Create a blockfile with a custom blocksize. Note, the size must be a
// multiple of 4096.
func CreateBlockFileCustomBlockSize(path string, blksize uint32) (*BlockFile, error) {
	if path == "" {
		return nil, errors.Errorf("path cannot be nil")
	}
	if blksize%4096 != 0 {
		return nil, errors.Errorf("blocksize must be divisible by 4096")
	}
	bf := &BlockFile{
		path:    path,
		blksize: int(blksize),
	}
	var err error
	bf.file, bf.mmap, bf.size, err = create(path, blksize)
	if err != nil {
		return nil, err
	}
	bf.opened = true
	err = bf.init_ctrl(blksize)
	if err != nil {
		return nil, err
	}
	return bf, nil
}

// Create an anonymous blockfile. There is no backing file. The
// blocksize must be divisible by 4096 as usual
func Anonymous(blksize uint32) (*BlockFile, error) {
	if blksize%4096 != 0 {
		return nil, errors.Errorf("blocksize must be divisible by 4096")
	}
	bf := &BlockFile{
		blksize: int(blksize),
	}
	var err error
	bf.mmap, bf.size, err = anon_create(blksize)
	if err != nil {
		return nil, err
	}
	bf.opened = true
	err = bf.init_ctrl(blksize)
	if err != nil {
		return nil, err
	}
	return bf, nil
}

// Open a previously created BlockFile. This will fail if you didn't use
// the creation functions to create the file (or at least have undefined
// effects).
func OpenBlockFile(path string) (*BlockFile, error) {
	f, mmap, err := open(path)
	if err != nil {
		return nil, err
	}
	bf := &BlockFile{
		path:    path,
		file:    f,
		mmap:    mmap,
		opened:  true,
		blksize: BLOCKSIZE, // set the initial block size to a safe size
	}
	bf.size, err = bf.fileSize()
	if err != nil {
		return nil, err
	}
	var blksize uint64
	err = bf.ctrl(func(ctrl *ctrlblk) error {
		blksize = uint64(ctrl.meta.blksize)
		return nil
	})
	if err != nil {
		return nil, err
	}
	bf.blksize = int(blksize)
	return bf, nil
}

// The flag used when creating the file
var CREATEFLAG = os.O_RDWR | os.O_CREATE | syscall.O_NOATIME | os.O_TRUNC

func anon_create(blksize uint32) (unsafe.Pointer, uint64, error) {
	ptr, err := do_anon_map(blksize)
	if err != nil {
		return nil, 0, err
	}
	return ptr, uint64(blksize), nil
}

func create(path string, blksize uint32) (*os.File, unsafe.Pointer, uint64, error) {
	f, err := do_open(path, CREATEFLAG)
	if err != nil {
		return nil, nil, 0, err
	}
	err = f.Truncate(int64(blksize))
	if err != nil {
		return nil, nil, 0, err
	}
	ptr, err := do_map(f)
	if err != nil {
		return nil, nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, 0, err
	}
	return f, ptr, uint64(fi.Size()), nil
}

// The flag used when opening the file
var OPENFLAG = os.O_RDWR | os.O_CREATE | syscall.O_NOATIME

func open(path string) (*os.File, unsafe.Pointer, error) {
	f, err := do_open(path, OPENFLAG)
	if err != nil {
		return nil, nil, err
	}
	ptr, err := do_map(f)
	if err != nil {
		return nil, nil, err
	}
	return f, ptr, nil
}

func do_open(path string, FLAG int) (*os.File, error) {
	f, err := os.OpenFile(path, FLAG, 0666)
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func do_map(f *os.File) (unsafe.Pointer, error) {
	var mmap unsafe.Pointer = unsafe.Pointer(uintptr(0))
	errno := C.create_mmap(&mmap, C.int(f.Fd()))
	if errno != 0 {
		return nil, errors.Errorf("Could not create map fd = %d, %d", f.Fd(), errno)
	}
	return mmap, nil
}

func do_anon_map(length uint32) (unsafe.Pointer, error) {
	var mmap unsafe.Pointer = unsafe.Pointer(uintptr(0))
	errno := C.create_anon_mmap(&mmap, C.size_t(length))
	if errno != 0 {
		return nil, errors.Errorf("Could not create anon map. length = %d, %d", length, errno)
	}
	return mmap, nil
}

// Close the file. Unmaps the region. There must be no outstanding
// blocks.
func (self *BlockFile) Close() error {
	if !self.opened {
		return errors.Errorf("File is not open")
	}
	if self.outstanding > 0 {
		return errors.Errorf("Tried to close file when there were outstanding pointers (%d)", self.outstanding)
	}
	if self.file != nil {
		if errno := C.destroy_mmap(self.mmap, C.int(self.file.Fd())); errno != 0 {
			return errors.Errorf("destroy_mmap failed, %d", errno)
		}
		if err := self.file.Close(); err != nil {
			return err
		} else {
			self.file = nil
		}
	} else {
		if errno := C.destroy_anon_mmap(self.mmap, C.size_t(self.size)); errno != 0 {
			return errors.Errorf("destroy_mmap failed, %d", errno)
		}
	}
	self.opened = false
	return nil
}

// Remove the underlying file. (must be already closed).
func (self *BlockFile) Remove() error {
	if self.opened {
		return errors.Errorf("Expected file to be closed")
	}
	if self.Path() == "" {
		return errors.Errorf("This was an anonymous map")
	}
	return os.Remove(self.Path())
}

func (self *BlockFile) init_ctrl(blksize uint32) error {
	return self.Do(0, 1, func(bytes []byte) error {
		_ = new_ctrlblk(bytes, blksize)
		return self.Sync()
	})
}

func (self *BlockFile) ctrl(do func(*ctrlblk) error) error {
	return self.Do(0, 1, func(bytes []byte) error {
		cb, err := load_ctrlblk(bytes)
		if err != nil {
			return err
		}
		err = do(cb)
		cb.updateChkSum()
		return err
	})
}

// Get the "control data" this free form data which is stored in the
// control block file. You can put whatever you want in here.
func (self *BlockFile) ControlData() (data []byte, err error) {
	err = self.ctrl(func(ctrl *ctrlblk) error {
		data = make([]byte, len(ctrl.user))
		copy(data, ctrl.user[:])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Put user data into the control block of the file.
func (self *BlockFile) SetControlData(data []byte) (err error) {
	err = self.SetControlDataNoSync(data)
	if err != nil {
		return err
	}
	return self.Sync()
}

// Same as SetControlData but does not call Sync() at the end.
func (self *BlockFile) SetControlDataNoSync(data []byte) (err error) {
	return self.ctrl(func(ctrl *ctrlblk) error {
		if len(data) > len(ctrl.user) {
			return errors.Errorf("control data was too large")
		}
		copy(ctrl.user[:], data)
		return nil
	})
}

// The file system path to this file.
func (self *BlockFile) Path() string {
	return self.path
}

// The blocksize for this file.
func (self *BlockFile) BlockSize() int {
	return self.blksize
}

func (self *BlockFile) Size() (uint64, error) {
	return self.size, nil
}

// The size of this file in bytes.
func (self *BlockFile) fileSize() (uint64, error) {
	if !self.opened {
		return 0, errors.Errorf("File is not open")
	}
	fi, err := self.file.Stat()
	if err != nil {
		return 0, err
	}
	return uint64(fi.Size()), nil
}

func (self *BlockFile) resize(size uint64) error {
	if self.outstanding > 0 {
		return errors.Errorf("cannot resize the file while there are outstanding pointers")
	}
	if !self.opened {
		return errors.Errorf("File is not open")
	}
	if self.file == nil {
		return self.anonResize(size)
	}
	var new_mmap unsafe.Pointer
	errno := C.resize(self.mmap, &new_mmap, C.int(self.file.Fd()), C.size_t(size))
	if errno != 0 {
		return errors.Errorf("resize failed, %d", errno)
	}
	self.size = size
	self.mmap = new_mmap
	return nil
}

func (self *BlockFile) anonResize(size uint64) error {
	var new_mmap unsafe.Pointer
	errno := C.anon_resize(self.mmap, &new_mmap, C.size_t(self.size), C.size_t(size))
	if errno != 0 {
		return errors.Errorf("resize failed, %d", errno)
	}
	self.size = size
	self.mmap = new_mmap
	return nil
}

// blockOffset converts a block index into a byte offset.
func (self *BlockFile) blockOffset(index uint64) uint64 {
	return index * uint64(self.blksize)
}

// offsetIndex converts a byte offset into a block index.
func (self *BlockFile) offsetIndex(offset uint64) uint64 {
	return offset / uint64(self.blksize)
}

// validBlock reports whether the block index is inside the mapped
// region (block 0 is the control block and is never free).
func (self *BlockFile) validBlock(index uint64) bool {
	if index == 0 {
		return false
	}
	return self.blockOffset(index)+uint64(self.blksize) <= self.size
}

// readFreeHdr loads the free run header at the given offset. It returns
// an error when the block is not the start of a free run.
func (self *BlockFile) readFreeHdr(offset uint64) (head, length uint64, err error) {
	err = self.Do(offset, 1, func(bytes []byte) error {
		hdr := loadFreeHdr(bytes)
		if hdr.magic != freeMagic {
			return errors.Errorf("block at %d is not the start of a free run", offset)
		}
		head = offset
		length = hdr.length
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return head, length, nil
}

// nextFreeRun returns the free run starting at the block following
// blockIndex when one exists there. Allocated blocks do not carry the
// free magic, so this is an O(1) neighbor lookup rather than a scan.
func (self *BlockFile) nextFreeRun(blockIndex uint64) (head, length uint64, found bool, err error) {
	nextIndex := blockIndex + 1
	if !self.validBlock(nextIndex) {
		return 0, 0, false, nil
	}
	offset := self.blockOffset(nextIndex)
	err = self.Do(offset, 1, func(bytes []byte) error {
		hdr := loadFreeHdr(bytes)
		if hdr.magic != freeMagic {
			return nil
		}
		found = true
		head = offset
		length = hdr.length
		return nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	return head, length, found, nil
}

// prevFreeRun returns the free run ending on the block preceding
// blockIndex. The boundary tag (footer) in that last block points back
// at the run's header, making the backward lookup O(1).
func (self *BlockFile) prevFreeRun(blockIndex uint64) (head, length uint64, found bool, err error) {
	prevIndex := blockIndex - 1
	if blockIndex == 0 || !self.validBlock(prevIndex) {
		return 0, 0, false, nil
	}
	prevOffset := self.blockOffset(prevIndex)
	err = self.Do(prevOffset, 1, func(block []byte) error {
		ftr := loadFreeFtr(block)
		if ftr.magic != freeMagic {
			return nil
		}
		var lerr error
		head, length, lerr = self.readFreeHdr(ftr.head)
		if lerr != nil {
			return lerr
		}
		// Guard against an allocated block whose bytes happen to match
		// the footer magic: the referenced run must actually end here.
		if self.offsetIndex(head)+length != prevIndex+1 {
			found = false
			head, length = 0, 0
			return nil
		}
		found = true
		return nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	return head, length, found, nil
}

// writeFreeRun writes the header of the run and the footer on its last
// block. Interior blocks are left untouched.
func (self *BlockFile) writeFreeRun(head, length uint64) error {
	if length == 0 {
		return errors.Errorf("cannot write an empty free run at %d", head)
	}
	headIndex := self.offsetIndex(head)
	tailOffset := self.blockOffset(headIndex + length - 1)
	return self.Do(head, 1, func(bytes []byte) error {
		hdr := loadFreeHdr(bytes)
		hdr.magic = freeMagic
		hdr.length = length
		return self.Do(tailOffset, 1, func(tailBlock []byte) error {
			ftr := loadFreeFtr(tailBlock)
			ftr.magic = freeMagic
			ftr.head = head
			return nil
		})
	})
}

// linkFreeRun inserts the run at the head of the free run list.
func (self *BlockFile) linkFreeRun(ctrl *ctrlblk, head, length uint64) error {
	oldHead := ctrl.meta.free_head
	err := self.Do(head, 1, func(bytes []byte) error {
		hdr := loadFreeHdr(bytes)
		hdr.magic = freeMagic
		hdr.prev = 0
		hdr.next = oldHead
		hdr.length = length
		if oldHead != 0 {
			return self.Do(oldHead, 1, func(oldBytes []byte) error {
				loadFreeHdr(oldBytes).prev = head
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	ctrl.meta.free_head = head
	ctrl.meta.free_len += uint32(length)
	return nil
}

// unlinkFreeRun removes the run from the doubly linked list, clears its
// boundary tags and accounts for its blocks.
func (self *BlockFile) unlinkFreeRun(ctrl *ctrlblk, head, length uint64) error {
	var prevOff, nextOff uint64
	err := self.Do(head, 1, func(bytes []byte) error {
		hdr := loadFreeHdr(bytes)
		if hdr.magic != freeMagic || hdr.length != length {
			return errors.Errorf("invalid free run header at %d", head)
		}
		prevOff, nextOff = hdr.prev, hdr.next
		hdr.magic = 0
		return nil
	})
	if err != nil {
		return err
	}
	if prevOff == 0 {
		ctrl.meta.free_head = nextOff
	} else {
		err = self.Do(prevOff, 1, func(bytes []byte) error {
			loadFreeHdr(bytes).next = nextOff
			return nil
		})
		if err != nil {
			return err
		}
	}
	if nextOff != 0 {
		err = self.Do(nextOff, 1, func(bytes []byte) error {
			loadFreeHdr(bytes).prev = prevOff
			return nil
		})
		if err != nil {
			return err
		}
	}
	tailOffset := self.blockOffset(self.offsetIndex(head) + length - 1)
	err = self.Do(tailOffset, 1, func(block []byte) error {
		ftr := loadFreeFtr(block)
		ftr.magic = 0
		ftr.head = 0
		return nil
	})
	if err != nil {
		return err
	}
	ctrl.meta.free_len -= uint32(length)
	return nil
}

// findFreeRun walks the free run list and returns the first run with at
// least n blocks (first fit). The walk happens on the allocation path,
// never on the single block Free path.
func (self *BlockFile) findFreeRun(n uint64) (head, length uint64, err error) {
	err = self.ctrl(func(ctrl *ctrlblk) error {
		var lerr error
		head, length, lerr = self.findFreeRunFrom(ctrl, n)
		return lerr
	})
	if err != nil {
		return 0, 0, err
	}
	return head, length, nil
}

// countFreeRuns reports how many distinct free runs (contiguous free
// segments) are currently tracked. It is primarily a test aid.
func (self *BlockFile) countFreeRuns() (runs int, err error) {
	err = self.ctrl(func(ctrl *ctrlblk) error {
		cursor := ctrl.meta.free_head
		for cursor != 0 {
			_, _, lerr := self.readFreeHdr(cursor)
			if lerr != nil {
				return lerr
			}
			runs++
			cursor = self.freeRunNext(cursor)
		}
		return nil
	})
	return runs, err
}

// checkFreeList verifies the free run list invariants: header/footer
// boundary tags agree, prev/next links are consistent, runs stay inside
// the file, runs never overlap or touch (they would have coalesced), and
// the block accounting matches free_len. It is a test/diagnostic aid.
func (self *BlockFile) checkFreeList() error {
	return self.ctrl(func(ctrl *ctrlblk) error {
		var prev uint64
		cursor := ctrl.meta.free_head
		var accounted uint64
		type span struct {
			start, end uint64
		}
		spans := make([]span, 0)
		for cursor != 0 {
			var hdr *freeblkHdr
			err := self.Do(cursor, 1, func(bytes []byte) error {
				hdr = loadFreeHdr(bytes)
				if hdr.magic != freeMagic {
					return errors.Errorf("run at %d has a bad magic", cursor)
				}
				if hdr.prev != prev {
					return errors.Errorf("run at %d has prev %d, expected %d", cursor, hdr.prev, prev)
				}
				if hdr.length == 0 {
					return errors.Errorf("run at %d has zero length", cursor)
				}
				return nil
			})
			if err != nil {
				return err
			}
			startIndex := self.offsetIndex(cursor)
			endIndex := startIndex + hdr.length
			if !self.validBlock(endIndex - 1) {
				return errors.Errorf("run at %d extends past end of file", cursor)
			}
			tailOff := self.blockOffset(endIndex - 1)
			err = self.Do(tailOff, 1, func(block []byte) error {
				ftr := loadFreeFtr(block)
				if ftr.magic != freeMagic || ftr.head != cursor {
					return errors.Errorf("bad footer on run ending at %d", tailOff)
				}
				return nil
			})
			if err != nil {
				return err
			}
			spans = append(spans, span{startIndex, endIndex})
			accounted += hdr.length
			prev, cursor = cursor, hdr.next
		}
		if accounted != uint64(ctrl.meta.free_len) {
			return errors.Errorf("free_len is %d but runs account for %d", ctrl.meta.free_len, accounted)
		}
		for i := range spans {
			for j := i + 1; j < len(spans); j++ {
				a, b := spans[i], spans[j]
				if a.start < b.end && b.start < a.end {
					return errors.Errorf("free runs %d and %d overlap", a.start, b.start)
				}
				if a.end == b.start || b.end == a.start {
					return errors.Errorf("adjacent free runs at %d were not coalesced", a.start)
				}
			}
		}
		return nil
	})
}

// freeRunNext reads the next pointer of the run at offset.
func (self *BlockFile) freeRunNext(offset uint64) uint64 {
	var next uint64
	err := self.Do(offset, 1, func(bytes []byte) error {
		next = loadFreeHdr(bytes).next
		return nil
	})
	if err != nil {
		panic(err)
	}
	return next
}

// Free the block at the given offset. The offset is in bytes from the
// start of the file. The block is merged in O(1) with the free runs
// immediately before and after it when they are physically adjacent; no
// full free list traversal is performed.
func (self *BlockFile) Free(offset uint64) error {
	if offset == 0 || offset%uint64(self.blksize) != 0 {
		return errors.Errorf("invalid free offset %d", offset)
	}
	blockIndex := self.offsetIndex(offset)
	if !self.validBlock(blockIndex) {
		return errors.Errorf("free offset %d is outside the file", offset)
	}
	return self.ctrl(func(ctrl *ctrlblk) error {
		// Reject the obvious double free cases via the boundary tags.
		// This must run before zeroing the block.
		var doubleFree bool
		err := self.Do(offset, 1, func(bytes []byte) error {
			if loadFreeHdr(bytes).magic == freeMagic {
				doubleFree = true
			}
			ftr := loadFreeFtr(bytes)
			if ftr.magic == freeMagic && ftr.head == offset {
				doubleFree = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		if doubleFree {
			return errors.Errorf("block at %d is already free", offset)
		}
		// Zero the block so every stale boundary tag from a prior
		// allocation is gone before this block joins the free set.
		if _, err := self.zero(offset, 1); err != nil {
			return err
		}

		runHead := offset
		runLen := uint64(1)

		nextHead, nextLen, hasNext, err := self.nextFreeRun(blockIndex)
		if err != nil {
			return err
		}
		if hasNext {
			if err := self.unlinkFreeRun(ctrl, nextHead, nextLen); err != nil {
				return err
			}
			runLen += nextLen
		}

		prevHead, prevLen, hasPrev, err := self.prevFreeRun(blockIndex)
		if err != nil {
			return err
		}
		if hasPrev {
			if err := self.unlinkFreeRun(ctrl, prevHead, prevLen); err != nil {
				return err
			}
			runHead = prevHead
			runLen += prevLen
		}

		if err := self.writeFreeRun(runHead, runLen); err != nil {
			return err
		}
		if err := self.linkFreeRun(ctrl, runHead, runLen); err != nil {
			return err
		}
		return nil
	})
}

// takeFromRun removes n blocks from the front of the free run at head.
// The remaining suffix stays in the list as a trimmed run.
func (self *BlockFile) takeFromRun(ctrl *ctrlblk, head, runLen, n uint64) (offset uint64, err error) {
	if err := self.unlinkFreeRun(ctrl, head, runLen); err != nil {
		return 0, err
	}
	if runLen > n {
		newHead := head + n*uint64(self.blksize)
		newLen := runLen - n
		if err := self.writeFreeRun(newHead, newLen); err != nil {
			return 0, err
		}
		if err := self.linkFreeRun(ctrl, newHead, newLen); err != nil {
			return 0, err
		}
	}
	return head, nil
}

func (self *BlockFile) zero(offset uint64, n int) (uint64, error) {
	for i := 0; i < n; i++ {
		err := self.Do(offset+uint64(i*self.blksize), 1, func(block []byte) error {
			ptr := slice.AsSlice(&block).Array
			memClr(ptr, uintptr(len(block)))
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return offset, nil
}

func (self *BlockFile) alloc(n int) (offset uint64, err error) {
	start_size := self.size
	amt := uint64(self.blksize) * uint64(n)
	if err := self.resize(self.size + amt); err != nil {
		return 0, err
	}
	return start_size, nil
}

func (self *BlockFile) allocOne() (offset uint64, err error) {
	n := uint64(256)
	start_size := self.size
	amt := uint64(self.blksize) * n
	if err := self.resize(self.size + amt); err != nil {
		return 0, err
	}
	for i := uint64(1); i < n; i++ {
		o := i * uint64(self.blksize)
		err := self.Free(start_size + o)
		if err != nil {
			return 0, err
		}
	}
	return start_size, nil
}

// What is the address of the file in the address space of the program.
// Use this at your own risk!
func (self *BlockFile) Address() uintptr {
	return uintptr(self.mmap)
}

// Is the address given still the address of the memory map?
func (self *BlockFile) Valid(address uintptr) bool {
	return address == uintptr(self.mmap)
}

// Allocate 1 block and return its offset.
func (self *BlockFile) Allocate() (offset uint64, err error) {
	if !self.opened {
		return 0, errors.Errorf("File is not open")
	}
	var resize bool
	err = self.ctrl(func(ctrl *ctrlblk) error {
		if ctrl.meta.free_len == 0 {
			resize = true
			return nil
		}
		head, length, lerr := self.findFreeRunFrom(ctrl, 1)
		if lerr != nil {
			return lerr
		}
		offset, lerr = self.takeFromRun(ctrl, head, length, 1)
		return lerr
	})
	if err != nil {
		return 0, err
	}
	if resize {
		offset, err = self.allocOne()
		if err != nil {
			return 0, err
		}
	}
	return self.zero(offset, 1)
}

// Allocate n blocks. Return the offset of the first block. These are
// guaranteed to be sequential. A contiguous free run is reused whenever
// one exists; the file is only extended (at the tail) when no run is
// large enough.
func (self *BlockFile) AllocateBlocks(n int) (offset uint64, err error) {
	if !self.opened {
		return 0, errors.Errorf("File is not open")
	}
	if n <= 0 {
		return 0, errors.Errorf("AllocateBlocks requires a positive count, got %d", n)
	}
	var resize bool
	err = self.ctrl(func(ctrl *ctrlblk) error {
		head, length, lerr := self.findFreeRunFrom(ctrl, uint64(n))
		if lerr != nil {
			resize = true
			return nil
		}
		offset, lerr = self.takeFromRun(ctrl, head, length, uint64(n))
		return lerr
	})
	if err != nil {
		return 0, err
	}
	if resize {
		offset, err = self.alloc(n)
		if err != nil {
			return 0, err
		}
	}
	return self.zero(offset, n)
}

// findFreeRunFrom scans the free run list starting at the control
// block's list head and returns the first run of at least n blocks.
func (self *BlockFile) findFreeRunFrom(ctrl *ctrlblk, n uint64) (head, length uint64, err error) {
	cursor := ctrl.meta.free_head
	for cursor != 0 {
		h, runLen, lerr := self.readFreeHdr(cursor)
		if lerr != nil {
			return 0, 0, lerr
		}
		if runLen >= n {
			return h, runLen, nil
		}
		cursor = self.freeRunNext(cursor)
	}
	return 0, 0, errors.Errorf("no free run of %d blocks available", n)
}

// Load the blocks at the give offset then call the callback, `do`,
// passing in the loaded bytes. This function releases those bytes after
// your callback is done. This is the recommended interface to the
// contents of the memory mapped region.
func (self *BlockFile) Do(offset, blocks uint64, do func([]byte) error) error {
	bytes, err := self.Get(offset, blocks)
	if err != nil {
		return err
	}
	err = do(bytes)
	self.Release(bytes)
	return err
}

// Get the bytes at the offset and block count. You probably want to use
// Do instead. You must call Release() on the bytes when done.
func (self *BlockFile) Get(offset, blocks uint64) ([]byte, error) {
	if !self.opened {
		return nil, errors.Errorf("File is not open")
	}
	length := blocks * uint64(self.blksize)
	if (offset + length) > uint64(self.size) {
		return nil, errors.Errorf("Get outside of the file, (%d) %d + %d > %d", offset+length, offset, length, self.size)
	}
	self.outstanding += 1
	slice := &slice.Slice{
		Array: unsafe.Pointer(uintptr(self.mmap) + uintptr(offset)),
		Len:   int(length),
		Cap:   int(length),
	}
	return *slice.AsBytes(), nil
}

// Release() bytes aquired with Get(). Should error if the bytes where
// not allocated from the mapping. But why take chances, you probably
// want to use the Do interface instead.
func (self *BlockFile) Release(bytes []byte) error {
	self.outstanding -= 1
	return nil
}

// Sync the mmap'ed changes to disk. This uses the async interface (via
// the MS_ASYNC flag) so the changes may not be written by the time this
// method returns. However, they will be written soon.
func (self *BlockFile) Sync() error {
	if self.file != nil {
		errno := C.sync_mmap(self.mmap, C.int(self.file.Fd()))
		if errno != 0 {
			return errors.Errorf("sync_mmap failed, %d", errno)
		}
	}
	return nil
}
