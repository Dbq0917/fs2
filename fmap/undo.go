package fmap

import (
	"github.com/timtadh/fs2/errors"
)

var ErrAllocationQuota = errors.Errorf("allocation quota exceeded")

type UndoTx struct {
	bf         *BlockFile
	snapshots  map[uint64][]byte
	blocks     map[uint64]int
	owned      map[uint64]struct{}
	live       map[uint64]struct{}
	freedOld   map[uint64]struct{}
	reused     map[uint64]struct{}
	dirty      map[uint64]struct{}
	recorded   map[uint64]struct{}
	depth      int
	txDepth    int
	persistent bool
	onRollback []func()
}

func (bf *BlockFile) BeginUndo() *UndoTx {
	if bf.undo != nil {
		bf.undo.txDepth++
		return bf.undo
	}
	tx := &UndoTx{
		bf:        bf,
		snapshots: make(map[uint64][]byte),
		blocks:    make(map[uint64]int),
		owned:     make(map[uint64]struct{}),
		live:      make(map[uint64]struct{}),
		freedOld:  make(map[uint64]struct{}),
		reused:    make(map[uint64]struct{}),
		dirty:     make(map[uint64]struct{}),
		recorded:  make(map[uint64]struct{}),
		txDepth:   1,
	}
	bf.undo = tx
	return tx
}

func (bf *BlockFile) UndoTransaction() *UndoTx {
	return bf.undo
}

func (bf *BlockFile) EndUndo() {
	bf.undo = nil
}

func (tx *UndoTx) OnRollback(fn func()) {
	if tx == nil || fn == nil {
		return
	}
	tx.onRollback = append(tx.onRollback, fn)
}

func (bf *BlockFile) UndoDo(offset, blocks uint64, do func([]byte) error) error {
	if bf.undo == nil {
		return bf.Do(offset, blocks, do)
	}
	tx := bf.undo
	if err := tx.snapshot(offset, blocks); err != nil {
		return err
	}
	tx.depth++
	var doErr error
	err := bf.Do(offset, blocks, func(bytes []byte) error {
		doErr = do(bytes)
		tx.noteDirty(offset, bytes)
		return doErr
	})
	tx.depth--
	if tx.depth == 0 && tx.persistent {
		if persistErr := tx.persistDirty(); persistErr != nil && err == nil {
			err = persistErr
		}
	}
	return err
}

func (tx *UndoTx) noteDirty(offset uint64, bytes []byte) {
	if _, owned := tx.owned[offset]; owned {
		return
	}
	image, ok := tx.snapshots[offset]
	if !ok || len(image) != len(bytes) {
		tx.dirty[offset] = struct{}{}
		return
	}
	for i := range image {
		if image[i] != bytes[i] {
			tx.dirty[offset] = struct{}{}
			return
		}
	}
}

func (tx *UndoTx) snapshot(offset, blocks uint64) error {
	if _, ok := tx.snapshots[offset]; ok {
		return nil
	}
	var image []byte
	err := tx.bf.Do(offset, blocks, func(bytes []byte) error {
		image = make([]byte, len(bytes))
		copy(image, bytes)
		return nil
	})
	if err != nil {
		return err
	}
	tx.snapshots[offset] = image
	tx.blocks[offset] = int(blocks)
	return nil
}

func (tx *UndoTx) trackAllocation(offset uint64, blocks int) error {
	if tx == nil {
		return nil
	}
	blockSize := uint64(tx.bf.BlockSize())
	for i := 0; i < blocks; i++ {
		addr := offset + uint64(i)*blockSize
		tx.owned[addr] = struct{}{}
		tx.live[addr] = struct{}{}
		delete(tx.freedOld, addr)
		if _, ok := tx.snapshots[addr]; ok {
			tx.reused[addr] = struct{}{}
			delete(tx.live, addr)
		}
	}
	if err := tx.recordAllocation(offset, uint32(blocks)); err != nil {
		return err
	}
	if blocks == 1 {
		if _, ok := tx.reused[offset]; ok {
			return tx.markAllocationReused(offset)
		}
	}
	return nil
}

func (tx *UndoTx) trackFree(offset uint64) {
	if tx == nil {
		return
	}
	delete(tx.live, offset)
	if _, owned := tx.owned[offset]; !owned {
		tx.freedOld[offset] = struct{}{}
		tx.recordOldFree(offset)
	}
	tx.recordFree(offset)
}

func (tx *UndoTx) Rollback() error {
	if tx == nil || tx.bf.undo != tx {
		return nil
	}
	bf := tx.bf
	bf.undo = nil
	if tx.persistent {
		_ = bf.clearTxHeader()
	}

	var firstErr error
	for offset := range tx.freedOld {
		if err := bf.removeFreeBlock(offset); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for offset, image := range tx.snapshots {
		if _, owned := tx.owned[offset]; owned {
			if _, reused := tx.reused[offset]; !reused {
				continue
			}
		}
		blocks := tx.blocks[offset]
		if blocks <= 0 {
			blocks = 1
		}
		err := bf.Do(offset, uint64(blocks), func(dst []byte) error {
			copy(dst, image)
			return nil
		})
		if err != nil {
			firstErr = err
		}
	}

	for offset := range tx.live {
		if err := bf.Free(offset); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	for _, fn := range tx.onRollback {
		fn()
	}
	return firstErr
}

func (tx *UndoTx) Commit() error {
	if tx == nil {
		return nil
	}
	bf := tx.bf
	if bf.undo != tx {
		return nil
	}
	if tx.txDepth > 1 {
		tx.txDepth--
		return nil
	}
	if tx.persistent {
		if err := bf.markTxCommitted(); err != nil {
			return err
		}
		if err := bf.Sync(); err != nil {
			return err
		}
	}
	bf.undo = nil
	var firstErr error
	if tx.persistent {
		if header, ok, readErr := bf.readTxHeader(); readErr == nil && ok {
			for i := uint32(0); i < header.UndoCount; i++ {
				entry := header.Undos[i]
				if entry.State == txUndoArena {
					continue
				}
				blocks := int(entry.Blocks)
				if blocks <= 0 {
					blocks = 1
				}
				for b := 0; b < blocks; b++ {
					addr := entry.Image + uint64(b)*uint64(bf.BlockSize())
					if freeErr := bf.Free(addr); freeErr != nil && firstErr == nil {
						firstErr = freeErr
					}
				}
			}
		}
	}
	if tx.persistent {
		if err := bf.clearTxHeader(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (bf *BlockFile) SetAllocationQuota(limit int64) {
	if limit < 0 {
		bf.quotaSet = false
		bf.allocLimit = 0
		return
	}
	bf.allocLimit = limit
	bf.quotaSet = true
}

func (bf *BlockFile) AllocationQuota() int64 {
	return bf.allocLimit
}

func (bf *BlockFile) AllocationCount() int64 {
	return bf.allocCount
}

func (bf *BlockFile) SetBeforeAllocate(fn func(blocks int) error) {
	bf.beforeAlloc = fn
}

func (bf *BlockFile) removeFreeBlock(target uint64) error {
	var head uint64
	err := bf.ctrl(func(ctrl *ctrlblk) error {
		head = ctrl.meta.free_head
		return nil
	})
	if err != nil {
		return err
	}

	var prev uint64
	for cur := head; cur != 0; {
		var next uint64
		err = bf.Do(cur, 1, func(bytes []byte) error {
			next = loadFreeBlk(bytes).next
			return nil
		})
		if err != nil {
			return err
		}
		if cur == target {
			if prev == 0 {
				return bf.ctrl(func(ctrl *ctrlblk) error {
					ctrl.meta.free_head = next
					if ctrl.meta.free_len > 0 {
						ctrl.meta.free_len--
					}
					return nil
				})
			}
			err = bf.Do(prev, 1, func(bytes []byte) error {
				loadFreeBlk(bytes).next = next
				return nil
			})
			if err != nil {
				return err
			}
			return bf.ctrl(func(ctrl *ctrlblk) error {
				if ctrl.meta.free_len > 0 {
					ctrl.meta.free_len--
				}
				return nil
			})
		}
		prev = cur
		cur = next
	}
	return errors.Errorf("free block %v is not on the free list", target)
}

func (bf *BlockFile) checkAllocation(blocks int) error {
	if bf.beforeAlloc != nil {
		if err := bf.beforeAlloc(blocks); err != nil {
			return err
		}
	}
	if bf.quotaSet && bf.allocCount+int64(blocks) > bf.allocLimit {
		return ErrAllocationQuota
	}
	bf.allocCount += int64(blocks)
	return nil
}
