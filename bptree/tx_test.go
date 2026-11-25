package bptree

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/timtadh/fs2/fmap"
)

func testSequentialKey(i uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, i)
	return key
}

func fillFullRootLeaf(t *testing.T, bpt *BpTree) [][]byte {
	t.Helper()
	keys := make([][]byte, 0)
	for i := uint64(1); ; i++ {
		var full bool
		err := bpt.doLeaf(bpt.meta.root, func(n *leaf) error {
			full = !n.fitsAnother()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if full {
			return keys
		}
		key := testSequentialKey(i)
		if err := bpt.Add(key, key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
}

func TestAddQuotaFailureDuringSplitPreservesTree(t *testing.T) {
	bf, err := fmap.Anonymous(fmap.BLOCKSIZE)
	if err != nil {
		t.Fatal(err)
	}
	defer bf.Close()

	bpt, err := New(bf, 8, 8)
	if err != nil {
		t.Fatal(err)
	}

	keys := fillFullRootLeaf(t, bpt)

	beforeSize := bpt.Size()
	limit := bf.AllocationCount() + 1
	bf.SetAllocationQuota(limit)

	missingKey := testSequentialKey(999999)
	err = bpt.Add(missingKey, missingKey)
	if err == nil {
		t.Fatal("expected quota failure during split")
	}

	if err := bpt.Verify(); err != nil {
		t.Fatalf("tree invalid after quota failure: %v", err)
	}
	if got := bpt.Size(); got != beforeSize {
		t.Fatalf("size changed after failed split: got %d want %d", got, beforeSize)
	}
	for _, key := range keys {
		has, err := bpt.Has(key)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("existing key %x disappeared after failed split", key)
		}
	}
	has, err := bpt.Has(missingKey)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("failed insert unexpectedly visible")
	}

	bf.SetAllocationQuota(-1)
	if err := bpt.Add(missingKey, missingKey); err != nil {
		t.Fatalf("retry after rollback failed: %v", err)
	}
	if err := bpt.Verify(); err != nil {
		t.Fatalf("tree invalid after retry: %v", err)
	}
	has, err = bpt.Has(missingKey)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("retried key is missing")
	}
}

func TestAddAllocationFailureSweepPreservesTree(t *testing.T) {
	for _, varchar := range []bool{false, true} {
		name := "fixed"
		keySize, valueSize := 8, 8
		if varchar {
			name = "varchar"
			keySize, valueSize = -1, -1
		}
		t.Run(name, func(t *testing.T) {
			for failAt := 1; failAt <= 12; failAt++ {
				bf, err := fmap.Anonymous(fmap.BLOCKSIZE)
				if err != nil {
					t.Fatal(err)
				}
				bpt, err := New(bf, keySize, valueSize)
				if err != nil {
					t.Fatal(err)
				}
				for i := uint64(0); i < 40; i++ {
					key := testSequentialKey(i)
					if err := bpt.Add(key, key); err != nil {
						t.Fatal(err)
					}
				}

				beforeSize := bpt.Size()
				calls := 0
				bf.SetBeforeAllocate(func(blocks int) error {
					calls++
					if calls == failAt {
						return fmap.ErrAllocationQuota
					}
					return nil
				})
				failedKey := testSequentialKey(1000 + uint64(failAt))
				failErr := bpt.Add(failedKey, failedKey)
				bf.SetBeforeAllocate(nil)

				if err := bpt.Verify(); err != nil {
					t.Fatalf("failAt=%d err=%v tree invalid: %v", failAt, failErr, err)
				}
				if failErr != nil && bpt.Size() != beforeSize {
					t.Fatalf("failAt=%d size changed after rollback: got %d want %d", failAt, bpt.Size(), beforeSize)
				}

				retryKey := testSequentialKey(2000 + uint64(failAt))
				if err := bpt.Add(retryKey, retryKey); err != nil {
					t.Fatalf("failAt=%d retry failed: %v", failAt, err)
				}
				if err := bpt.Verify(); err != nil {
					t.Fatalf("failAt=%d tree invalid after retry: %v", failAt, err)
				}
				if err := bf.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestOpenRecoversInterruptedLeafSplit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fs2-recover.db")
	bf, err := fmap.CreateBlockFile(path)
	if err != nil {
		t.Fatal(err)
	}

	bpt, err := New(bf, 8, 8)
	if err != nil {
		t.Fatal(err)
	}
	keys := fillFullRootLeaf(t, bpt)
	beforeSize := bpt.Size()
	root := bpt.meta.root

	tx, err := bf.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	newKey := testSequentialKey(999999)
	a, b, err := bpt.leafSplit(root, nil, newKey, newKey)
	if err != nil {
		t.Fatal(err)
	}
	if a != root || b == 0 {
		t.Fatalf("unexpected split addresses a=%d b=%d root=%d", a, b, root)
	}
	_ = tx

	if err := bf.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := bf.Close(); err != nil {
		t.Fatal(err)
	}

	recoveredBF, err := fmap.OpenBlockFile(path)
	if err != nil {
		t.Fatalf("open recovery failed: %v", err)
	}
	defer recoveredBF.Close()

	recovered, err := Open(recoveredBF)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Verify(); err != nil {
		t.Fatalf("tree invalid after crash recovery: %v", err)
	}
	if got := recovered.Size(); got != beforeSize {
		t.Fatalf("size changed after recovery: got %d want %d", got, beforeSize)
	}
	for _, key := range keys {
		has, err := recovered.Has(key)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("key %x lost during interrupted split recovery", key)
		}
	}
	has, err := recovered.Has(newKey)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("interrupted split key survived recovery")
	}

	if err := recovered.Add(newKey, newKey); err != nil {
		t.Fatalf("insert after recovery failed: %v", err)
	}
	if err := recovered.Verify(); err != nil {
		t.Fatalf("tree invalid after post-recovery insert: %v", err)
	}
}
