package bptree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

func cursorKey(prefix byte, n int) []byte {
	key := make([]byte, 8)
	key[0] = prefix
	binary.BigEndian.PutUint32(key[4:], uint32(n))
	return key
}

func cursorValue(n int) []byte {
	value := make([]byte, 8)
	binary.BigEndian.PutUint64(value, uint64(n))
	return value
}

func TestCursorPrefixAcrossLeaves(x *testing.T) {
	t := (*T)(x)
	bpt, clean := t.bptFixed()
	defer clean()

	const count = 700
	for i := 0; i < count; i++ {
		t.assert_nil(bpt.Add(cursorKey(0x10, i), cursorValue(i)))
	}
	for i := 0; i < 250; i++ {
		t.assert_nil(bpt.Add(cursorKey(0x0f, i), cursorValue(i)))
		t.assert_nil(bpt.Add(cursorKey(0x11, i), cursorValue(i)))
	}

	from := cursorKey(0x10, 0)
	to := cursorKey(0x10, count-1)
	next := 0
	t.assert_nil(bpt.DoRange(from, to, func(key, value []byte) error {
		if next >= count {
			return fmt.Errorf("got too many keys")
		}
		expected := cursorKey(0x10, next)
		if !bytes.Equal(key, expected) {
			return fmt.Errorf("key %d: got %x, expected %x", next, key, expected)
		}
		if !bytes.Equal(value, cursorValue(next)) {
			return fmt.Errorf("value %d: got %x, expected %x", next, value, cursorValue(next))
		}
		next++
		return nil
	}))
	if next != count {
		t.Fatalf("got %d keys, expected %d", next, count)
	}
}

func TestCursorPrefixVarcharKeys(x *testing.T) {
	t := (*T)(x)
	bpt, clean := t.bpt()
	defer clean()

	key := func(prefix byte, n int) []byte {
		k := []byte{prefix, 0x20, 0x30, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(k[3:], uint32(n))
		return k
	}
	const count = 700
	for i := 0; i < count; i++ {
		t.assert_nil(bpt.Add(key(0x10, i), cursorValue(i)))
	}
	for i := 0; i < 250; i++ {
		t.assert_nil(bpt.Add(key(0x0f, i), cursorValue(i)))
		t.assert_nil(bpt.Add(key(0x11, i), cursorValue(i)))
	}

	next := 0
	t.assert_nil(bpt.DoRange(key(0x10, 0), key(0x10, count-1), func(k, _ []byte) error {
		if next >= count || !bytes.Equal(k, key(0x10, next)) {
			return fmt.Errorf("varchar key %d: got %x", next, k)
		}
		next++
		return nil
	}))
	if next != count {
		t.Fatalf("got %d varchar keys, expected %d", next, count)
	}
}

func TestCursorWriteDuringIteration(x *testing.T) {
	t := (*T)(x)
	bpt, clean := t.bptFixed()
	defer clean()

	const count = 500
	for i := 0; i < count; i++ {
		t.assert_nil(bpt.Add(cursorKey(0x20, i), cursorValue(i)))
	}

	cur, err := bpt.NewCursor(cursorKey(0x20, 0))
	t.assert_nil(err)
	defer cur.Close()

	last := []byte(nil)
	seen := 0
	for {
		key, _, err := cur.Next()
		t.assert_nil(err)
		if key == nil {
			break
		}
		if last != nil && bytes.Compare(last, key) > 0 {
			t.Fatalf("keys went backwards: %x then %x", last, key)
		}
		if key[0] != 0x20 {
			t.Fatalf("left prefix: %x", key)
		}
		last = append(last[:0], key...)
		seen++

		if seen%7 == 0 {
			t.assert_nil(bpt.Add(cursorKey(0x20, count+seen), cursorValue(count+seen)))
		}
		if seen%11 == 0 {
			t.assert_nil(bpt.Remove(key, func([]byte) bool { return true }))
		}
	}
	if seen < count {
		t.Fatalf("cursor stopped early after %d keys", seen)
	}
}

func TestRangeWriteDuringIteration(x *testing.T) {
	t := (*T)(x)
	bpt, clean := t.bptFixed()
	defer clean()

	const count = 500
	for i := 0; i < count; i++ {
		t.assert_nil(bpt.Add(cursorKey(0x40, i), cursorValue(i)))
	}

	seen := 0
	var last []byte
	t.assert_nil(bpt.DoRange(cursorKey(0x40, 0), cursorKey(0x40, count-1), func(key, _ []byte) error {
		if last != nil && bytes.Compare(last, key) > 0 {
			t.Fatalf("keys went backwards: %x then %x", last, key)
		}
		last = append(last[:0], key...)
		seen++
		if seen%9 == 0 {
			t.assert_nil(bpt.Add(cursorKey(0x40, count+seen), cursorValue(count+seen)))
		}
		if seen%13 == 0 {
			t.assert_nil(bpt.Remove(key, func([]byte) bool { return true }))
		}
		return nil
	}))
	if seen < count {
		t.Fatalf("range stopped early after %d keys", seen)
	}
}

func TestCursorRemoveCurrent(x *testing.T) {
	t := (*T)(x)
	bpt, clean := t.bptFixed()
	defer clean()

	const count = 500
	for i := 0; i < count; i++ {
		t.assert_nil(bpt.Add(cursorKey(0x50, i), cursorValue(i)))
	}

	next := 0
	cur, err := bpt.NewCursor(cursorKey(0x50, 0))
	t.assert_nil(err)
	defer cur.Close()
	for {
		key, _, err := cur.Next()
		t.assert_nil(err)
		if key == nil {
			break
		}
		if !bytes.Equal(key, cursorKey(0x50, next)) {
			t.Fatalf("key %d: got %x", next, key)
		}
		next++
		t.assert_nil(bpt.Remove(key, func([]byte) bool { return true }))
	}
	if next != count {
		t.Fatalf("got %d keys, expected %d", next, count)
	}
	if bpt.Size() != 0 {
		t.Fatalf("tree has %d entries left", bpt.Size())
	}
}

func TestCursorDuplicatesAndEarlyStop(x *testing.T) {
	t := (*T)(x)
	bpt, clean := t.bptFixed()
	defer clean()

	const keys = 20
	const dups = 30
	for i := 0; i < keys; i++ {
		for j := 0; j < dups; j++ {
			t.assert_nil(bpt.Add(cursorKey(0x30, i), cursorValue(i*dups+j)))
		}
	}

	from := cursorKey(0x30, 0)
	to := cursorKey(0x30, keys-1)
	count := 0
	var last []byte
	t.assert_nil(bpt.DoRange(from, to, func(key, _ []byte) error {
		if last != nil && bytes.Compare(last, key) > 0 {
			t.Fatalf("duplicate scan went backwards")
		}
		last = append(last[:0], key...)
		count++
		return nil
	}))
	if count != keys*dups {
		t.Fatalf("got %d duplicate entries, expected %d", count, keys*dups)
	}

	stop := errors.New("stop")
	count = 0
	err := bpt.DoRange(from, to, func(key, _ []byte) error {
		count++
		if count == 25 {
			return stop
		}
		return nil
	})
	if err != stop {
		t.Fatalf("got %v, expected %v", err, stop)
	}
	if count != 25 {
		t.Fatalf("callback ran %d times, expected 25", count)
	}
}
