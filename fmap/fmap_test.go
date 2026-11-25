package fmap

import "testing"

import (
	"math/rand"
	"runtime/debug"
)

var path string = "/tmp/__mmap_bf"

type T testing.T

func (t *T) assert(errors ...error) {
	for _, err := range errors {
		if err != nil {
			t.Log(string(debug.Stack()))
			t.Fatal(err)
		}
	}
}

func (t *T) blkfile() *BlockFile {
	bf, err := CreateBlockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bf
}

func (t *T) cleanup(bf *BlockFile) {
	err := bf.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = bf.Remove()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateBlockFile(t *testing.T) {
	bf, err := CreateBlockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = bf.ctrl(func(cb *ctrlblk) error {
		// if cb.meta.checksum == 0 {
		// t.Errorf("No checksum")
		// }
		if cb.meta.blksize != 4096 {
			t.Errorf("Blocksize was not 4096")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = bf.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = bf.Remove()
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenBlockFile(t *testing.T) {
	{
		bf, err := CreateBlockFile(path)
		if err != nil {
			t.Fatal(err)
		}
		err = bf.ctrl(func(cb *ctrlblk) error {
			// if cb.meta.checksum == 0 {
			// t.Errorf("No checksum")
			// }
			if cb.meta.blksize != 4096 {
				t.Errorf("Blocksize was not 4096")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = bf.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	bf, err := OpenBlockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = bf.ctrl(func(cb *ctrlblk) error {
		// if cb.meta.checksum == 0 {
		// t.Errorf("No checksum")
		// }
		if cb.meta.blksize != 4096 {
			t.Errorf("Blocksize was not 4096")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = bf.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = bf.Remove()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAllocate(x *testing.T) {
	t := (*T)(x)
	bf := t.blkfile()
	defer t.cleanup(bf)
	var off uint64
	var err error
	off, err = bf.Allocate()
	t.assert(err)
	// var size uint64
	// size, err = bf.Size()
	// t.assert(err)
	// if size != uint64(bf.BlockSize())*2 {
	// t.Errorf("Size of the file did not increase")
	// }
	t.assert(bf.Do(off, 1, func(bytes []byte) error {
		bytes[15] = 12
		return nil
	}))
	t.assert(bf.Do(off, 1, func(bytes []byte) error {
		for i, b := range bytes {
			if i == 15 && b != 12 {
				t.Errorf("bytes[15] != 12")
			} else if i != 15 && b != 0 {
				t.Errorf("bytes[%d] != 0", i)
			}
		}
		return nil
	}))
}

func TestFree(x *testing.T) {
	t := (*T)(x)
	bf, err := Anonymous(4096)
	t.assert(err)
	var off uint64
	off, err = bf.Allocate()
	t.assert(err)
	// var size uint64
	// size, err = bf.Size()
	// t.assert(err)
	// if size != uint64(bf.BlockSize())*2 {
	// t.Errorf("Size of the file did not increase")
	// }
	t.assert(bf.Do(off, 1, func(bytes []byte) error {
		bytes[15] = 12
		return nil
	}))
	t.assert(bf.Do(off, 1, func(bytes []byte) error {
		for i, b := range bytes {
			if i == 15 && b != 12 {
				t.Errorf("bytes[15] != 12")
			} else if i != 15 && b != 0 {
				t.Errorf("bytes[%d] != 0", i)
			}
		}
		return nil
	}))
	t.assert(bf.Free(off))

	var off2 uint64
	off2, err = bf.Allocate()
	t.assert(err)
	if off2 != off {
		t.Errorf("off2 != off")
	}
	t.assert(bf.Do(off2, 1, func(bytes []byte) error {
		for i, b := range bytes {
			if b != 0 {
				t.Errorf("bytes[%d] != 0", i)
			}
		}
		return nil
	}))
}

func TestFreeCoalescesAdjacentBlocks(x *testing.T) {
	t := (*T)(x)
	bf, err := Anonymous(4096)
	t.assert(err)
	defer func() { t.assert(bf.Close()) }()

	// Allocate a contiguous run of 4 blocks then release them one at a
	// time (the way the rest of the code base actually frees blocks).
	start, err := bf.AllocateBlocks(4)
	t.assert(err)
	offs := make([]uint64, 4)
	for i := range offs {
		offs[i] = start + uint64(i)*uint64(bf.BlockSize())
	}
	poolRuns, err := bf.countFreeRuns()
	t.assert(err)
	for _, off := range offs {
		t.assert(bf.Free(off))
	}

	// The four individually freed, adjacent blocks must be recognized
	// as a single contiguous segment of length 4.
	seg, segLen, err := bf.findFreeRun(4)
	t.assert(err)
	if seg != start {
		x.Errorf("coalesced segment should start at %d, got %d", start, seg)
	}
	if segLen != 4 {
		x.Errorf("coalesced segment should span 4 blocks, got %d", segLen)
	}
	runs, err := bf.countFreeRuns()
	t.assert(err)
	// The four adjacent blocks must form exactly one extra run rather
	// than four separate single block entries.
	if runs != poolRuns+1 {
		x.Errorf("expected %d free runs after coalescing, got %d", poolRuns+1, runs)
	}
}

func TestAllocateBlocksReusesFreedSegment(x *testing.T) {
	t := (*T)(x)
	bf, err := Anonymous(4096)
	t.assert(err)
	defer func() { t.assert(bf.Close()) }()

	start, err := bf.AllocateBlocks(4)
	t.assert(err)
	before, err := bf.Size()
	t.assert(err)
	for i := 0; i < 4; i++ {
		t.assert(bf.Free(start + uint64(i)*uint64(bf.BlockSize())))
	}

	// Re-allocating the same amount must land inside the freed segment
	// instead of extending the file.
	again, err := bf.AllocateBlocks(4)
	t.assert(err)
	if again != start {
		x.Errorf("expected reuse at %d, got %d", start, again)
	}
	after, err := bf.Size()
	t.assert(err)
	if after != before {
		x.Errorf("file grew from %d to %d although a free segment existed", before, after)
	}
}

func TestAllocateBlocksSplitsOversizedRun(x *testing.T) {
	t := (*T)(x)
	bf, err := Anonymous(4096)
	t.assert(err)
	defer func() { t.assert(bf.Close()) }()

	start, err := bf.AllocateBlocks(4)
	t.assert(err)
	for i := 0; i < 4; i++ {
		t.assert(bf.Free(start + uint64(i)*uint64(bf.BlockSize())))
	}

	// Take 2 blocks out of the 4 block run; the run should be trimmed,
	// not consumed whole.
	two, err := bf.AllocateBlocks(2)
	t.assert(err)
	if two != start {
		x.Errorf("expected allocation at %d, got %d", start, two)
	}
	// The unconsumed suffix must remain a 2 block free run directly
	// after the allocated pair.
	restOff := start + 2*uint64(bf.BlockSize())
	_, restLen, err := bf.readFreeHdr(restOff)
	t.assert(err)
	if restLen != 2 {
		x.Errorf("expected a remaining run of 2 blocks at %d, got %d", restOff, restLen)
	}

	// The pool could satisfy the request first, so free it from
	// contention by asking through the known run header instead.
	var reused uint64
	t.assert(bf.ctrl(func(cb *ctrlblk) error {
		var lerr error
		reused, lerr = bf.takeFromRun(cb, restOff, restLen, 2)
		return lerr
	}))
	if reused != restOff {
		x.Errorf("expected remainder reuse at %d, got %d", restOff, reused)
	}
}

func TestFreeAllocCycleDoesNotGrowFile(x *testing.T) {
	t := (*T)(x)
	bf, err := Anonymous(4096)
	t.assert(err)
	defer func() { t.assert(bf.Close()) }()

	const n = 16
	base, err := bf.AllocateBlocks(n)
	t.assert(err)
	maxSize, err := bf.Size()
	t.assert(err)

	// Repeatedly delete the whole segment one block at a time and then
	// ask for it back as a contiguous allocation. The file must not
	// grow beyond its first high-water mark.
	for round := 0; round < 5; round++ {
		for i := 0; i < n; i++ {
			t.assert(bf.Free(base + uint64(i)*uint64(bf.BlockSize())))
		}
		got, err := bf.AllocateBlocks(n)
		t.assert(err)
		if got != base {
			x.Fatalf("round %d: expected reuse at %d, got %d", round, base, got)
		}
		size, err := bf.Size()
		t.assert(err)
		if size > maxSize {
			x.Fatalf("round %d: file grew from %d to %d", round, maxSize, size)
		}
	}
}

func TestFreeRunSurvivesReopen(x *testing.T) {
	t := (*T)(x)
	bf := t.blkfile()

	start, err := bf.AllocateBlocks(3)
	t.assert(err)
	for i := 0; i < 3; i++ {
		t.assert(bf.Free(start + uint64(i)*uint64(bf.BlockSize())))
	}
	t.assert(bf.Close())

	reopened, err := OpenBlockFile(path)
	t.assert(err)
	again, err := reopened.AllocateBlocks(3)
	t.assert(err)
	if again != start {
		x.Errorf("after reopen expected reuse at %d, got %d", start, again)
	}
	t.assert(reopened.Close())
	t.assert(reopened.Remove())
}

func TestDoubleFreeRejected(x *testing.T) {
	t := (*T)(x)
	bf, err := Anonymous(4096)
	t.assert(err)
	defer func() { t.assert(bf.Close()) }()

	off, err := bf.Allocate()
	t.assert(err)
	t.assert(bf.Free(off))
	if err := bf.Free(off); err == nil {
		x.Fatal("a double free should have returned an error")
	}
	t.assert(bf.checkFreeList())
}

func TestRandomAllocFreeKeepsInvariants(x *testing.T) {
	t := (*T)(x)
	bf, err := Anonymous(4096)
	t.assert(err)
	defer func() { t.assert(bf.Close()) }()

	// Drain (but keep allocated) the allocOne() preallocation pool so it
	// cannot satisfy any of the requests made below. The only free space
	// left is then the region under test itself.
	drained := make([]uint64, 0)
	for {
		var freeLen uint32
		t.assert(bf.ctrl(func(cb *ctrlblk) error {
			freeLen = cb.meta.free_len
			return nil
		}))
		if freeLen == 0 {
			break
		}
		off, err := bf.Allocate()
		t.assert(err)
		drained = append(drained, off)
	}
	t.assert(bf.checkFreeList())

	// A contiguous region we hand out and take back in a random order.
	// The high water mark is captured afterwards so any later growth
	// proves the allocator failed to reuse free space.
	const pool = 48
	base, err := bf.AllocateBlocks(pool)
	t.assert(err)
	highWater, err := bf.Size()
	t.assert(err)
	t.assert(bf.checkFreeList())

	rng := rand.New(rand.NewSource(0xC0FFEE))
	inRegion := func(off uint64) bool {
		return off >= base && off < base+pool*uint64(bf.BlockSize())
	}
	// Seed the free set with a third of the region so allocation always
	// has somewhere inside the region to land.
	free := make(map[uint64]bool)
	for i := 0; i < pool; i += 3 {
		off := base + uint64(i)*uint64(bf.BlockSize())
		t.assert(bf.Free(off))
		free[off] = true
	}
	for round := 0; round < 200; round++ {
		if rng.Intn(2) == 0 && len(free) > 0 {
			got, err := bf.Allocate()
			t.assert(err)
			if !inRegion(got) {
				x.Fatalf("round %d: allocation %d escaped the test region", round, got)
			}
			if !free[got] {
				x.Fatalf("round %d: allocated block %d was not free", round, got)
			}
			delete(free, got)
		} else {
			// Free a currently allocated block chosen at random.
			pick := rng.Intn(pool)
			off := base + uint64(pick)*uint64(bf.BlockSize())
			if free[off] {
				continue
			}
			t.assert(bf.Free(off))
			free[off] = true
		}
		if e := bf.checkFreeList(); e != nil {
			x.Fatalf("round %d: %v", round, e)
		}
	}

	// Return every outstanding block, then a full sized contiguous
	// request has to be served from coalesced free space.
	for i := 0; i < pool; i++ {
		off := base + uint64(i)*uint64(bf.BlockSize())
		if !free[off] {
			t.assert(bf.Free(off))
		}
	}
	t.assert(bf.checkFreeList())
	again, err := bf.AllocateBlocks(pool)
	t.assert(err)
	if again != base {
		x.Errorf("expected whole pool reused at %d, got %d", base, again)
	}
	size, err := bf.Size()
	t.assert(err)
	if size > highWater {
		x.Errorf("file grew from %d to %d despite reusable free runs", highWater, size)
	}
	t.assert(bf.checkFreeList())
}
