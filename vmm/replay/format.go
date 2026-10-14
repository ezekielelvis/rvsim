// Package replay records a VM session's external inputs and periodic state
// hashes, and replays them, checking that execution is identical (ADR-028).
//
// File format (.rvrec), little-endian:
//
//	"RVSIMREC" | version u16 | header length u32 | header (JSON)
//	records:  tag u8 | Δstep uvarint | length uvarint | payload
//
// Δstep is relative to the previous record's step. Records are flushed as
// they are written, so a recording cut short is readable up to its last
// complete record.
package replay

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const (
	magic         = "RVSIMREC"
	formatVersion = 1
	maxHeader     = 1 << 20
	maxPayload    = 1 << 24
)

// Record tags.
const (
	TagInput      = 0x01
	TagInputEOF   = 0x02
	TagCheckpoint = 0x03
	TagEnd        = 0x04
	// 0x10-0x1f are reserved for fault injection (Phase 11).
)

// Image identifies one input image by content.
type Image struct {
	Role   string `json:"role"` // firmware, kernel, initrd, disk, dtb
	Path   string `json:"path,omitempty"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// DiskConfig is the virtio disk's configuration (its contents are an Image).
type DiskConfig struct {
	ReadOnly bool   `json:"read_only"`
	Latency  uint64 `json:"latency"`
	Serial   string `json:"serial"`
}

// Header describes everything a replay needs besides the images' bytes.
type Header struct {
	ABIVersion      uint32         `json:"abi_version"`
	CoreBuild       string         `json:"core_build"`
	Machine         machine.Config `json:"machine"`
	Bootargs        string         `json:"bootargs"`
	Disk            *DiskConfig    `json:"disk,omitempty"`
	Input           bool           `json:"input"` // host input was attached
	CheckpointEvery uint64         `json:"checkpoint_every"`
	Hash            string         `json:"hash"`
	Images          []Image        `json:"images"`
}

// Image returns the image with the given role, if any.
func (h *Header) Image(role string) (Image, bool) {
	for _, im := range h.Images {
		if im.Role == role {
			return im, true
		}
	}
	return Image{}, false
}

// End reasons.
const (
	EndExit        = "exit"        // the guest exited (Code is its exit code)
	EndReset       = "reset"       // the guest asked for a reboot
	EndLimit       = "limit"       // the step limit was reached
	EndInterrupted = "interrupted" // the user stopped the session
)

// Record is one entry of a recording.
type Record struct {
	Tag  uint8
	Step uint64
	// Input: the bytes.
	Data []byte
	// Checkpoint and End.
	Instret uint64
	Hashes  vm.Hashes
	// End.
	Reason string
	Code   uint64
}

// Writer appends records to a recording file. It implements vm.Recorder.
type Writer struct {
	f    *os.File
	w    *bufio.Writer
	last uint64
	buf  []byte
}

// Create starts a recording at path.
func Create(path string, h *Header) (*Writer, error) {
	hdr, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, w: bufio.NewWriter(f)}
	var pre [14]byte
	copy(pre[:], magic)
	binary.LittleEndian.PutUint16(pre[8:], formatVersion)
	binary.LittleEndian.PutUint32(pre[10:], uint32(len(hdr)))
	w.w.Write(pre[:])
	w.w.Write(hdr)
	if err := w.w.Flush(); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

func (w *Writer) record(tag uint8, step uint64, payload []byte) error {
	if step < w.last {
		return fmt.Errorf("replay: record at step %d after step %d", step, w.last)
	}
	b := append(w.buf[:0], tag)
	b = binary.AppendUvarint(b, step-w.last)
	b = binary.AppendUvarint(b, uint64(len(payload)))
	b = append(b, payload...)
	w.buf = b
	w.last = step
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	return w.w.Flush()
}

// Input implements vm.Recorder.
func (w *Writer) Input(step uint64, data []byte) error { return w.record(TagInput, step, data) }

// InputEOF implements vm.Recorder.
func (w *Writer) InputEOF(step uint64) error { return w.record(TagInputEOF, step, nil) }

func hashPayload(instret uint64, h vm.Hashes) []byte {
	p := make([]byte, 32)
	binary.LittleEndian.PutUint64(p[0:], instret)
	binary.LittleEndian.PutUint64(p[8:], h.Core)
	binary.LittleEndian.PutUint64(p[16:], h.RAM)
	binary.LittleEndian.PutUint64(p[24:], h.VMM)
	return p
}

// Checkpoint records the state hashes at step.
func (w *Writer) Checkpoint(step, instret uint64, h vm.Hashes) error {
	return w.record(TagCheckpoint, step, hashPayload(instret, h))
}

// End records how the session ended and its final hashes.
func (w *Writer) End(step, instret uint64, h vm.Hashes, reason string, code uint64) error {
	p := hashPayload(instret, h)
	p = binary.LittleEndian.AppendUint64(p, code)
	p = append(p, reason...)
	return w.record(TagEnd, step, p)
}

// Close flushes and closes the file.
func (w *Writer) Close() error {
	err := w.w.Flush()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ErrTruncated reports a recording that ends mid-record (for example, the
// recorder crashed). The records before it are still returned.
var ErrTruncated = errors.New("replay: recording is truncated")

// Read parses a recording.
func Read(r io.Reader) (*Header, []Record, error) {
	br := bufio.NewReader(r)
	var pre [14]byte
	if _, err := io.ReadFull(br, pre[:]); err != nil {
		return nil, nil, fmt.Errorf("replay: reading the file header: %w", err)
	}
	if string(pre[:8]) != magic {
		return nil, nil, errors.New("replay: not an rvsim recording")
	}
	if v := binary.LittleEndian.Uint16(pre[8:]); v != formatVersion {
		return nil, nil, fmt.Errorf("replay: recording format version %d, this rvsim reads %d", v, formatVersion)
	}
	n := binary.LittleEndian.Uint32(pre[10:])
	if n > maxHeader {
		return nil, nil, fmt.Errorf("replay: header of %d bytes", n)
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, nil, fmt.Errorf("replay: reading the header: %w", err)
	}
	h := new(Header)
	if err := json.Unmarshal(hdr, h); err != nil {
		return nil, nil, fmt.Errorf("replay: header: %w", err)
	}

	var recs []Record
	var step uint64
	for {
		tag, err := br.ReadByte()
		if err == io.EOF {
			return h, recs, nil
		}
		rec, err := readRecord(br, tag, &step)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return h, recs, ErrTruncated
			}
			return h, recs, err
		}
		recs = append(recs, rec)
	}
}

func readRecord(br *bufio.Reader, tag uint8, step *uint64) (Record, error) {
	delta, err := binary.ReadUvarint(br)
	if err != nil {
		return Record{}, err
	}
	n, err := binary.ReadUvarint(br)
	if err != nil {
		return Record{}, err
	}
	if n > maxPayload {
		return Record{}, fmt.Errorf("replay: record of %d bytes", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(br, p); err != nil {
		return Record{}, err
	}
	*step += delta
	rec := Record{Tag: tag, Step: *step}
	switch tag {
	case TagInput:
		rec.Data = p
	case TagInputEOF:
	case TagCheckpoint, TagEnd:
		if len(p) < 32 || (tag == TagEnd && len(p) < 40) {
			return Record{}, fmt.Errorf("replay: short record (tag %#x) at step %d", tag, *step)
		}
		rec.Instret = binary.LittleEndian.Uint64(p[0:])
		rec.Hashes = vm.Hashes{
			Core: binary.LittleEndian.Uint64(p[8:]),
			RAM:  binary.LittleEndian.Uint64(p[16:]),
			VMM:  binary.LittleEndian.Uint64(p[24:]),
		}
		if tag == TagEnd {
			rec.Code = binary.LittleEndian.Uint64(p[32:])
			rec.Reason = string(p[40:])
		}
	default:
		return Record{}, fmt.Errorf("replay: unknown record tag %#x at step %d", tag, *step)
	}
	return rec, nil
}
