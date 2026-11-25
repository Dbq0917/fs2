package fmap

import (
	"unsafe"

	"github.com/timtadh/fs2/errors"
)

const txUserOffset = 8

const txMagic uint64 = 0x31304e5854325346 // "FS2TXN01" in little-endian bytes

const (
	txStateActive uint32 = iota + 1
	txStateCommitted
)

const (
	txAllocFreed  uint32 = 1
	txAllocReused uint32 = 2
)
const txUndoArena uint32 = 1

const (
	txMaxAllocs = 128
	txMaxUndos  = 64
	txMaxFrees  = 48
)

type txAllocEntry struct {
	Offset uint64
	Blocks uint32
	State  uint32
}

type txUndoEntry struct {
	Target uint64
	Image  uint64
	Blocks uint32
	State  uint32
}

type txControl struct {
	Magic      uint64
	State      uint32
	Version    uint32
	AllocCount uint32
	UndoCount  uint32
	Allocs     [txMaxAllocs]txAllocEntry
	Undos      [txMaxUndos]txUndoEntry
	FreeCount  uint32
	Frees      [txMaxFrees]uint64
}

func init() {
	available := BLOCKSIZE - ctrldataSize - txUserOffset - 8
	if unsafe.Sizeof(txControl{}) > uintptr(available) {
		panic("fmap transaction control record is too large")
	}
}

func txControlOn(cb *ctrlblk) *txControl {
	return (*txControl)(unsafe.Pointer(&cb.user[txUserOffset]))
}

func journalBaseOn(cb *ctrlblk) *uint64 {
	return (*uint64)(unsafe.Pointer(&cb.user[len(cb.user)-8]))
}

// BeginTx starts a physical undo transaction. All block accesses made through
// UndoDo are snapshotted in memory. Dirty pre-existing blocks additionally get
// persistent before-images so OpenBlockFile can finish an interrupted rollback
// by following the small transaction record in the control block.
func (bf *BlockFile) BeginTx() (*UndoTx, error) {
	tx := bf.BeginUndo()
	if tx.txDepth > 1 {
		return tx, nil
	}
	tx.persistent = true
	if err := bf.ensureJournalArena(); err != nil {
		bf.EndUndo()
		return nil, err
	}
	err := bf.ctrl(func(cb *ctrlblk) error {
		MemClr(cb.user[txUserOffset : len(cb.user)-8])
		header := txControlOn(cb)
		header.Magic = txMagic
		header.Version = 1
		header.State = txStateActive
		return nil
	})
	if err != nil {
		bf.EndUndo()
		return nil, err
	}
	_ = bf.Sync()
	return tx, nil
}

func (bf *BlockFile) ensureJournalArena() error {
	if bf.journalBase != 0 {
		return nil
	}
	var base uint64
	err := bf.ctrl(func(cb *ctrlblk) error {
		base = *journalBaseOn(cb)
		return nil
	})
	if err != nil {
		return err
	}
	if base == 0 {
		base, err = bf.alloc(txMaxUndos)
		if err != nil {
			return err
		}
		if _, err = bf.zero(base, txMaxUndos); err != nil {
			return err
		}
		err = bf.ctrl(func(cb *ctrlblk) error {
			*journalBaseOn(cb) = base
			return nil
		})
		if err != nil {
			return err
		}
	}
	bf.journalBase = base
	return bf.Sync()
}

func (bf *BlockFile) withTxHeader(edit func(*txControl) error) error {
	return bf.ctrl(func(cb *ctrlblk) error {
		return edit(txControlOn(cb))
	})
}

func (bf *BlockFile) readTxHeader() (header txControl, ok bool, err error) {
	err = bf.ctrl(func(cb *ctrlblk) error {
		h := txControlOn(cb)
		if h.Magic != txMagic {
			return nil
		}
		header = *h
		ok = true
		return nil
	})
	return header, ok, err
}

func (bf *BlockFile) clearTxHeader() error {
	err := bf.ctrl(func(cb *ctrlblk) error {
		MemClr(cb.user[txUserOffset : len(cb.user)-8])
		return nil
	})
	if err != nil {
		return err
	}
	return bf.Sync()
}

func (bf *BlockFile) markTxCommitted() error {
	return bf.withTxHeader(func(h *txControl) error {
		if h.Magic != txMagic {
			return errors.Errorf("transaction record is missing")
		}
		h.State = txStateCommitted
		return nil
	})
}

func (tx *UndoTx) recordAllocation(offset uint64, blocks uint32) error {
	if tx == nil || !tx.persistent {
		return nil
	}
	return tx.bf.withTxHeader(func(h *txControl) error {
		if h.Magic != txMagic {
			return errors.Errorf("transaction record is missing")
		}
		if h.AllocCount >= txMaxAllocs {
			return errors.Errorf("transaction allocated too many blocks")
		}
		blockSize := uint64(tx.bf.BlockSize())
		end := offset + uint64(blocks)*blockSize
		kept := uint32(0)
		for i := uint32(0); i < h.FreeCount; i++ {
			addr := h.Frees[i]
			if addr >= offset && addr < end {
				continue
			}
			h.Frees[kept] = addr
			kept++
		}
		for i := kept; i < h.FreeCount; i++ {
			h.Frees[i] = 0
		}
		h.FreeCount = kept
		h.Allocs[h.AllocCount] = txAllocEntry{Offset: offset, Blocks: blocks}
		h.AllocCount++
		return nil
	})
}

func (tx *UndoTx) recordFree(offset uint64) {
	if tx == nil || !tx.persistent {
		return
	}
	_ = tx.bf.withTxHeader(func(h *txControl) error {
		for i := uint32(0); i < h.AllocCount; i++ {
			entry := &h.Allocs[i]
			if entry.Offset == offset && entry.Blocks == 1 {
				entry.State = txAllocFreed
				return nil
			}
		}
		return nil
	})
}

func (tx *UndoTx) recordOldFree(offset uint64) {
	if tx == nil || !tx.persistent {
		return
	}
	_ = tx.bf.withTxHeader(func(h *txControl) error {
		for i := uint32(0); i < h.FreeCount; i++ {
			if h.Frees[i] == offset {
				return nil
			}
		}
		if h.FreeCount >= txMaxFrees {
			return errors.Errorf("transaction freed too many old blocks")
		}
		h.Frees[h.FreeCount] = offset
		h.FreeCount++
		return nil
	})
}

func (tx *UndoTx) markAllocationReused(offset uint64) error {
	if tx == nil || !tx.persistent {
		return nil
	}
	return tx.bf.withTxHeader(func(h *txControl) error {
		for i := h.AllocCount; i > 0; i-- {
			entry := &h.Allocs[i-1]
			if entry.Offset == offset && entry.Blocks == 1 {
				entry.State = txAllocReused
				return nil
			}
		}
		return nil
	})
}

func (tx *UndoTx) persistDirty() error {
	if tx == nil || !tx.persistent {
		return nil
	}
	for target := range tx.dirty {
		if _, done := tx.recorded[target]; done {
			continue
		}
		image := tx.snapshots[target]
		blocks := tx.blocks[target]
		if blocks <= 0 {
			blocks = 1
		}
		var index uint32
		err := tx.bf.withTxHeader(func(h *txControl) error {
			if h.UndoCount >= txMaxUndos {
				return errors.Errorf("transaction changed too many blocks")
			}
			index = h.UndoCount
			return nil
		})
		if err != nil {
			return err
		}

		var imageOffset uint64
		if blocks == 1 && tx.bf.journalBase != 0 {
			imageOffset = tx.bf.journalBase + uint64(index)*uint64(tx.bf.BlockSize())
		} else {
			imageOffset, err = tx.bf.AllocateBlocks(blocks)
			if err != nil {
				return err
			}
		}
		err = tx.bf.Do(imageOffset, uint64(blocks), func(dst []byte) error {
			copy(dst, image)
			return nil
		})
		if err != nil {
			return err
		}
		err = tx.bf.withTxHeader(func(h *txControl) error {
			state := uint32(0)
			if blocks == 1 && tx.bf.journalBase != 0 {
				state = txUndoArena
			}
			h.Undos[index] = txUndoEntry{
				Target: target,
				Image:  imageOffset,
				Blocks: uint32(blocks),
				State:  state,
			}
			if h.UndoCount == index {
				h.UndoCount++
			}
			return nil
		})
		if err != nil {
			return err
		}
		tx.recorded[target] = struct{}{}
		_ = tx.bf.Sync()
	}
	return nil
}

func (bf *BlockFile) recoverTx() error {
	header, ok, err := bf.readTxHeader()
	if err != nil || !ok {
		return err
	}
	if header.AllocCount > txMaxAllocs || header.UndoCount > txMaxUndos || header.FreeCount > txMaxFrees {
		return errors.Errorf("invalid transaction record")
	}

	if header.State == txStateCommitted {
		for i := uint32(0); i < header.UndoCount; i++ {
			entry := header.Undos[i]
			if entry.State == txUndoArena {
				continue
			}
			if entry.Blocks == 0 {
				entry.Blocks = 1
			}
			for b := uint32(0); b < entry.Blocks; b++ {
				addr := entry.Image + uint64(b)*uint64(bf.BlockSize())
				if err := bf.Free(addr); err != nil {
					return err
				}
			}
		}
		return bf.clearTxHeader()
	}

	for i := uint32(0); i < header.FreeCount; i++ {
		if err := bf.removeFreeBlock(header.Frees[i]); err != nil {
			return err
		}
	}
	for i := int(header.UndoCount) - 1; i >= 0; i-- {
		entry := header.Undos[i]
		blocks := uint64(entry.Blocks)
		if blocks == 0 {
			blocks = 1
		}
		err := bf.Do(entry.Image, blocks, func(src []byte) error {
			return bf.Do(entry.Target, blocks, func(dst []byte) error {
				copy(dst, src)
				return nil
			})
		})
		if err != nil {
			return err
		}
	}

	for i := uint32(0); i < header.AllocCount; i++ {
		entry := header.Allocs[i]
		if entry.State == txAllocFreed || entry.State == txAllocReused || entry.Blocks == 0 {
			continue
		}
		for b := uint32(0); b < entry.Blocks; b++ {
			addr := entry.Offset + uint64(b)*uint64(bf.BlockSize())
			if err := bf.Free(addr); err != nil {
				return err
			}
		}
	}
	return bf.clearTxHeader()
}
