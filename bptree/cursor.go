package bptree

import (
	"bytes"
)

import (
	"github.com/timtadh/fs2/consts"
	"github.com/timtadh/fs2/errors"
)

// forwardCursor walks the leaf chain in key order. It keeps both the
// physical location and a key based recovery point. The recovery point is
// used when a write moves, splits, empties, or reuses the page holding the
// next location.
type forwardCursor struct {
	tree *BpTree
	to   []byte

	a uint64
	i int

	lastA uint64
	lastI int

	haveCandidate bool
	candidateKey  []byte
	candidateOrd  int

	done bool
}

func (self *BpTree) newForwardCursor(from, to []byte) (*forwardCursor, error) {
	cur := &forwardCursor{
		tree: self,
		to:   cloneBytes(to),
	}
	a, i, err := self.getStart(from)
	if err != nil {
		return nil, err
	}
	key, ok, err := self.keyAtLoc(a, i)
	if err != nil {
		return nil, err
	}
	if !ok || (from != nil && bytes.Compare(key, from) < 0) {
		cur.done = true
		return cur, nil
	}
	cur.a = a
	cur.i = i
	cur.haveCandidate = true
	cur.candidateKey = key
	cur.candidateOrd = 0
	return cur, nil
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// keyAtLoc copies the key at a leaf location. A stale or reused page is
// reported as !ok rather than as a structural error.
func (self *BpTree) keyAtLoc(a uint64, i int) (key []byte, ok bool, err error) {
	if a == 0 {
		return nil, false, nil
	}
	err = self.bf.Do(a, 1, func(bytes []byte) error {
		flags := consts.AsFlag(bytes)
		if flags&consts.LEAF == 0 {
			return nil
		}
		n := asLeaf(bytes)
		if i < 0 || i >= n.keyCount() {
			return nil
		}
		err := n.doKeyAt(self.varchar, i, func(k []byte) error {
			key = cloneBytes(k)
			return nil
		})
		if err != nil {
			return err
		}
		ok = true
		return nil
	})
	return key, ok, err
}

// itemAt copies the key and value at a leaf location.
func (self *BpTree) itemAt(a uint64, i int) (key, value []byte, ok bool, err error) {
	if a == 0 {
		return nil, nil, false, nil
	}
	err = self.bf.Do(a, 1, func(bytes []byte) error {
		flags := consts.AsFlag(bytes)
		if flags&consts.LEAF == 0 {
			return nil
		}
		n := asLeaf(bytes)
		if i < 0 || i >= n.keyCount() {
			return nil
		}
		err := n.doKeyAt(self.varchar, i, func(k []byte) error {
			key = cloneBytes(k)
			return nil
		})
		if err != nil {
			return err
		}
		err = n.doValueAt(self.varchar, i, func(v []byte) error {
			value = cloneBytes(v)
			return nil
		})
		if err != nil {
			return err
		}
		ok = true
		return nil
	})
	return key, value, ok, err
}

func (self *BpTree) seekCursor(key []byte, ord int) (uint64, int, []byte, bool, error) {
	a, i, err := self.getStart(key)
	if err != nil {
		return 0, 0, nil, false, err
	}
	seen := 0
	for {
		k, ok, err := self.keyAtLoc(a, i)
		if err != nil {
			return 0, 0, nil, false, err
		}
		if !ok {
			return 0, 0, nil, false, nil
		}
		cmp := bytes.Compare(k, key)
		if cmp < 0 {
			nextA, nextI, end, err := self.nextLoc(a, i)
			if err != nil {
				return 0, 0, nil, false, err
			}
			if end {
				return 0, 0, nil, false, nil
			}
			a, i = nextA, nextI
			continue
		}
		if cmp > 0 || seen >= ord {
			return a, i, k, true, nil
		}
		nextA, nextI, end, err := self.nextLoc(a, i)
		if err != nil {
			return 0, 0, nil, false, err
		}
		if end {
			return 0, 0, nil, false, nil
		}
		a, i = nextA, nextI
		seen++
	}
}

func (self *BpTree) detectCursorCandidate(cur *forwardCursor) (bool, error) {
	if !cur.haveCandidate {
		return false, nil
	}
	key, ok, err := self.keyAtLoc(cur.a, cur.i)
	if err != nil {
		return false, err
	}
	if !ok || !bytes.Equal(key, cur.candidateKey) {
		return false, nil
	}
	return true, nil
}

func (self *BpTree) recoverCursor(cur *forwardCursor) (bool, error) {
	a, i, key, ok, err := self.seekCursor(cur.candidateKey, cur.candidateOrd)
	if err != nil || !ok {
		return false, err
	}
	cur.a, cur.i = a, i
	cur.candidateKey = key
	return true, nil
}

// next returns the next key/value pair. A nil key with a nil error means
// that the cursor is exhausted. The returned slices are copies and are
// safe to retain or modify.
func (self *BpTree) next(cur *forwardCursor, withValue bool) (key, value []byte, ok bool, err error) {
	if cur.done {
		return nil, nil, false, nil
	}
	valid, err := self.detectCursorCandidate(cur)
	if err != nil {
		return nil, nil, false, err
	}
	if !valid {
		ok, err = self.recoverCursor(cur)
		if err != nil || !ok {
			cur.done = true
			return nil, nil, false, err
		}
	}

	if withValue {
		key, value, ok, err = self.itemAt(cur.a, cur.i)
	} else {
		key, ok, err = self.keyAtLoc(cur.a, cur.i)
	}
	if err != nil {
		return nil, nil, false, err
	}
	if !ok {
		cur.done = true
		return nil, nil, false, nil
	}
	if cur.to != nil && bytes.Compare(key, cur.to) > 0 {
		cur.done = true
		return nil, nil, false, nil
	}

	currentA, currentI := cur.a, cur.i
	cur.lastA, cur.lastI = currentA, currentI
	currentKey := key
	currentOrd := cur.candidateOrd
	nextA, nextI, end, err := self.nextLoc(currentA, currentI)
	if err != nil {
		return nil, nil, false, err
	}
	if end {
		cur.done = true
		cur.haveCandidate = false
	} else {
		nextKey, nextOK, err := self.keyAtLoc(nextA, nextI)
		if err != nil {
			return nil, nil, false, err
		}
		if !nextOK {
			cur.done = true
			cur.haveCandidate = false
		} else {
			cur.a, cur.i = nextA, nextI
			cur.haveCandidate = true
			cur.candidateKey = nextKey
			if bytes.Equal(nextKey, currentKey) {
				cur.candidateOrd = currentOrd + 1
			} else {
				cur.candidateOrd = 0
			}
		}
	}
	return key, value, true, nil
}

func (self *BpTree) forwardCursor(from, to []byte) (bpt_iterator, error) {
	cur, err := self.newForwardCursor(from, to)
	if err != nil {
		return nil, err
	}
	var bi bpt_iterator
	bi = func() (a uint64, i int, err error, next bpt_iterator) {
		_, _, ok, err := self.next(cur, false)
		if err != nil {
			return 0, 0, err, nil
		}
		if !ok {
			return 0, 0, nil, nil
		}
		return cur.lastA, cur.lastI, nil, bi
	}
	return bi, nil
}

// Cursor is a forward-only, ordered cursor over a B+ Tree. It holds one
// leaf position at a time and does not materialize the tree. Keys and
// values returned by Next are copies.
type Cursor struct {
	tree   *BpTree
	cur    *forwardCursor
	key    []byte
	value  []byte
	err    error
	closed bool
}

// NewCursor creates a cursor positioned at the first key greater than or
// equal to the first argument. With no arguments it starts at the first key
// in the tree. An optional second argument is an inclusive upper bound.
func (self *BpTree) NewCursor(bounds ...[]byte) (*Cursor, error) {
	var from, to []byte
	if len(bounds) > 0 {
		from = bounds[0]
	}
	if len(bounds) > 1 {
		to = bounds[1]
	}
	cur, err := self.newForwardCursor(from, to)
	if err != nil {
		return nil, err
	}
	return &Cursor{tree: self, cur: cur}, nil
}

// Cursor is an alias for NewCursor.
func (self *BpTree) Cursor(bounds ...[]byte) (*Cursor, error) {
	return self.NewCursor(bounds...)
}

// NewCursor creates a cursor for tree.
func NewCursor(tree *BpTree, bounds ...[]byte) (*Cursor, error) {
	return tree.NewCursor(bounds...)
}

// Seek repositions the cursor at the first key greater than or equal to key.
func (c *Cursor) Seek(key []byte) error {
	if c.closed {
		return errors.Errorf("cursor is closed")
	}
	cur, err := c.tree.newForwardCursor(key, nil)
	if err != nil {
		c.err = err
		return err
	}
	c.cur = cur
	c.key = nil
	c.value = nil
	c.err = nil
	return nil
}

// Next advances to the next key/value pair. A nil key with a nil error
// means that the cursor is exhausted.
func (c *Cursor) Next() (key, value []byte, err error) {
	if c.closed {
		return nil, nil, errors.Errorf("cursor is closed")
	}
	key, value, ok, err := c.tree.next(c.cur, true)
	if err != nil {
		c.err = err
		return nil, nil, err
	}
	if !ok {
		c.key, c.value = nil, nil
		return nil, nil, nil
	}
	c.key, c.value = key, value
	return key, value, nil
}

// NextOK is the boolean form of Next for callers that prefer Key and Value.
func (c *Cursor) NextOK() (bool, error) {
	key, value, err := c.Next()
	if err != nil || key == nil {
		return false, err
	}
	c.key, c.value = key, value
	return true, nil
}

// Key returns the key at the current cursor position.
func (c *Cursor) Key() []byte {
	return c.key
}

// Value returns the value at the current cursor position.
func (c *Cursor) Value() []byte {
	return c.value
}

// Err returns the last error encountered by the cursor.
func (c *Cursor) Err() error {
	return c.err
}

// Close releases the cursor.
func (c *Cursor) Close() error {
	c.closed = true
	c.cur = nil
	c.key, c.value = nil, nil
	return nil
}
