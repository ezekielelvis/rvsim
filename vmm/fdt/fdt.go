// Package fdt builds flattened device tree blobs (DTB), as specified in the
// Devicetree Specification v0.4, chapter 5.
package fdt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// Node is a device tree node. Build a tree with Child and the property
// setters, then serialize the root with Blob.
type Node struct {
	name     string
	props    []prop
	children []*Node
}

type prop struct {
	name  string
	value []byte
}

// NewRoot returns the root node ("/").
func NewRoot() *Node { return &Node{} }

// Child appends a child node and returns it.
func (n *Node) Child(name string) *Node {
	c := &Node{name: name}
	n.children = append(n.children, c)
	return c
}

func (n *Node) set(name string, v []byte) *Node {
	for i := range n.props {
		if n.props[i].name == name {
			n.props[i].value = v
			return n
		}
	}
	n.props = append(n.props, prop{name, v})
	return n
}

// Empty sets a property with no value (e.g. "interrupt-controller").
func (n *Node) Empty(name string) *Node { return n.set(name, nil) }

// String sets a NUL-terminated string property.
func (n *Node) String(name, s string) *Node { return n.set(name, append([]byte(s), 0)) }

// Strings sets a string-list property.
func (n *Node) Strings(name string, ss ...string) *Node {
	var b []byte
	for _, s := range ss {
		b = append(append(b, s...), 0)
	}
	return n.set(name, b)
}

// U32 sets a property of big-endian 32-bit cells.
func (n *Node) U32(name string, cells ...uint32) *Node {
	b := make([]byte, 4*len(cells))
	for i, c := range cells {
		binary.BigEndian.PutUint32(b[4*i:], c)
	}
	return n.set(name, b)
}

// U64 sets a property of 64-bit values, each encoded as two cells.
func (n *Node) U64(name string, vals ...uint64) *Node {
	b := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.BigEndian.PutUint64(b[8*i:], v)
	}
	return n.set(name, b)
}

const (
	magic        = 0xd00dfeed
	version      = 17
	lastCompat   = 16
	tokBeginNode = 1
	tokEndNode   = 2
	tokProp      = 3
	tokEnd       = 9
	headerSize   = 40
)

// Blob serializes the tree rooted at n. Property names are deduplicated in
// the strings block; the output is deterministic for a given tree.
func (n *Node) Blob() ([]byte, error) {
	var strs bytes.Buffer
	offsets := map[string]uint32{}
	var names []string
	collect(n, func(p prop) {
		if _, ok := offsets[p.name]; !ok {
			offsets[p.name] = 0
			names = append(names, p.name)
		}
	})
	sort.Strings(names) // stable layout independent of insertion order
	for _, s := range names {
		offsets[s] = uint32(strs.Len())
		strs.WriteString(s)
		strs.WriteByte(0)
	}

	var st bytes.Buffer
	if err := n.write(&st, offsets, true); err != nil {
		return nil, err
	}
	putU32(&st, tokEnd)

	// Layout: header, memory reservation map (empty), structure, strings.
	rsvOff := uint32(headerSize)
	structOff := rsvOff + 16 // one terminating {0, 0} entry
	stringsOff := structOff + uint32(st.Len())
	total := stringsOff + uint32(strs.Len())

	var out bytes.Buffer
	for _, v := range []uint32{magic, total, structOff, stringsOff, rsvOff, version, lastCompat, 0,
		uint32(strs.Len()), uint32(st.Len())} {
		putU32(&out, v)
	}
	out.Write(make([]byte, 16))
	out.Write(st.Bytes())
	out.Write(strs.Bytes())
	return out.Bytes(), nil
}

func collect(n *Node, f func(prop)) {
	for _, p := range n.props {
		f(p)
	}
	for _, c := range n.children {
		collect(c, f)
	}
}

func (n *Node) write(b *bytes.Buffer, offsets map[string]uint32, root bool) error {
	if !root && n.name == "" {
		return fmt.Errorf("fdt: unnamed node")
	}
	putU32(b, tokBeginNode)
	b.WriteString(n.name)
	b.WriteByte(0)
	pad(b)
	for _, p := range n.props {
		putU32(b, tokProp)
		putU32(b, uint32(len(p.value)))
		putU32(b, offsets[p.name])
		b.Write(p.value)
		pad(b)
	}
	for _, c := range n.children {
		if err := c.write(b, offsets, false); err != nil {
			return err
		}
	}
	putU32(b, tokEndNode)
	return nil
}

func putU32(b *bytes.Buffer, v uint32) {
	var w [4]byte
	binary.BigEndian.PutUint32(w[:], v)
	b.Write(w[:])
}

func pad(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}
