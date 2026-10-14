// Package gdb is a GDB Remote Serial Protocol stub (ADR-030): registers
// (with a RISC-V target description), memory through the MMU, software
// breakpoints, single-step, continue, Ctrl-C and, on replays, reverse
// execution.
package gdb

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Server speaks RSP to one debugger over conn.
type Server struct {
	t      Target
	r      *bufio.Reader
	w      io.Writer
	noAck  bool
	in     chan []byte // packets, or {0x03} for an interrupt
	rerr   error
	Logf   func(format string, args ...any) // optional protocol log
	exited bool
}

// Ending is how a debugging session ended.
type Ending int

const (
	Detached     Ending = iota // the debugger detached: the target may run on
	Killed                     // the debugger killed the target
	Disconnected               // the connection closed
)

// Serve runs the protocol until the debugger detaches or kills the target,
// or the connection closes. It drives t on the calling goroutine.
func Serve(conn io.ReadWriter, t Target, logf func(string, ...any)) (Ending, error) {
	s := &Server{t: t, r: bufio.NewReader(conn), w: conn, in: make(chan []byte, 16), Logf: logf}
	go s.readLoop()
	for p := range s.in {
		if len(p) == 1 && p[0] == 0x03 {
			continue // an interrupt while stopped: nothing to do
		}
		end, done, err := s.handle(string(p))
		if err != nil || done {
			return end, err
		}
	}
	if s.rerr == io.EOF {
		return Disconnected, nil
	}
	return Disconnected, s.rerr
}

// readLoop turns the byte stream into packets (acknowledging them) and raw
// interrupts. It never touches the target.
func (s *Server) readLoop() {
	defer close(s.in)
	for {
		c, err := s.r.ReadByte()
		if err != nil {
			s.rerr = err
			return
		}
		switch c {
		case '+', '-':
			continue
		case 0x03:
			s.in <- []byte{0x03}
		case '$':
			body, err := s.r.ReadString('#')
			if err != nil {
				s.rerr = err
				return
			}
			var sum [2]byte
			if _, err := io.ReadFull(s.r, sum[:]); err != nil {
				s.rerr = err
				return
			}
			body = body[:len(body)-1]
			want, _ := strconv.ParseUint(string(sum[:]), 16, 8)
			if !s.noAck {
				if checksum(body) != uint8(want) {
					s.w.Write([]byte("-"))
					continue
				}
				s.w.Write([]byte("+"))
			}
			s.in <- []byte(unescape(body))
		}
	}
}

func checksum(s string) uint8 {
	var c uint8
	for i := 0; i < len(s); i++ {
		c += s[i]
	}
	return c
}

func unescape(s string) string {
	if !strings.Contains(s, "}") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '}' && i+1 < len(s) {
			i++
			b.WriteByte(s[i] ^ 0x20)
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (s *Server) send(data string) error {
	if s.Logf != nil {
		s.Logf("<- %s", data)
	}
	_, err := fmt.Fprintf(s.w, "$%s#%02x", data, checksum(data))
	return err
}

// interrupted reports (and consumes) a pending Ctrl-C, without blocking.
// Packets that arrive while running are not expected (all-stop mode).
func (s *Server) interrupted() bool {
	select {
	case p, ok := <-s.in:
		return !ok || (len(p) == 1 && p[0] == 0x03)
	default:
		return false
	}
}

func hex64(v uint64) string {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return hex.EncodeToString(b[:])
}

func parseHex64LE(s string) (uint64, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) > 8 {
		return 0, errors.New("bad register value")
	}
	var w [8]byte
	copy(w[:], b)
	return binary.LittleEndian.Uint64(w[:]), nil
}

func (s *Server) stopReply(st Stop, err error) error {
	if err != nil {
		if errors.Is(err, ErrNoReverse) {
			return s.send("E01")
		}
		if s.Logf != nil {
			s.Logf("target error: %v", err)
		}
		s.exited = true
		return s.send("X06") // SIGABRT: the emulator stopped with an error
	}
	switch st.Reason {
	case StopExited:
		s.exited = true
		return s.send(fmt.Sprintf("W%02x", min(st.Code, 255)))
	case StopInterrupt:
		return s.send("T02thread:1;")
	case StopBreakpoint:
		return s.send("T05swbreak:;thread:1;")
	case StopHistory:
		return s.send("T05replaylog:begin;thread:1;")
	case StopEnd:
		return s.send("T05replaylog:end;thread:1;")
	}
	return s.send("T05thread:1;")
}

// handle processes one packet. It returns done when the session is over.
func (s *Server) handle(p string) (Ending, bool, error) {
	done, err := s.dispatch(p)
	if done {
		if p == "k" {
			return Killed, true, err
		}
		return Detached, true, err
	}
	return 0, false, err
}

func (s *Server) dispatch(p string) (bool, error) {
	if s.Logf != nil {
		s.Logf("-> %s", p)
	}
	t := s.t
	switch {
	case p == "?":
		return false, s.send("T05thread:1;")
	case strings.HasPrefix(p, "qSupported"):
		return false, s.send("PacketSize=4000;qXfer:features:read+;swbreak+;QStartNoAckMode+;vContSupported+;ReverseStep+;ReverseContinue+")
	case p == "QStartNoAckMode":
		err := s.send("OK")
		s.noAck = true
		return false, err
	case strings.HasPrefix(p, "qXfer:features:read:target.xml:"):
		return false, s.xfer(targetXML, strings.TrimPrefix(p, "qXfer:features:read:target.xml:"))
	case p == "qAttached":
		return false, s.send("1")
	case p == "qC":
		return false, s.send("QC1")
	case p == "qfThreadInfo":
		return false, s.send("m1")
	case p == "qsThreadInfo":
		return false, s.send("l")
	case strings.HasPrefix(p, "H"), strings.HasPrefix(p, "T"):
		return false, s.send("OK")
	case p == "vCont?":
		return false, s.send("vCont;c;C;s;S")
	case p == "g":
		var b strings.Builder
		for n := 0; n <= regPC; n++ {
			v, err := t.ReadReg(n)
			if err != nil {
				return false, s.send("E01")
			}
			b.WriteString(hex64(v))
		}
		return false, s.send(b.String())
	case strings.HasPrefix(p, "G"):
		data := p[1:]
		for n := 0; n <= regPC && len(data) >= 16; n++ {
			v, err := parseHex64LE(data[:16])
			if err != nil || t.WriteReg(n, v) != nil {
				return false, s.send("E01")
			}
			data = data[16:]
		}
		return false, s.send("OK")
	case strings.HasPrefix(p, "p"):
		n, err := strconv.ParseUint(p[1:], 16, 32)
		if err != nil {
			return false, s.send("E01")
		}
		v, err := t.ReadReg(int(n))
		if err != nil {
			return false, s.send("xxxxxxxxxxxxxxxx") // unavailable
		}
		return false, s.send(hex64(v))
	case strings.HasPrefix(p, "P"):
		reg, val, ok := strings.Cut(p[1:], "=")
		n, err := strconv.ParseUint(reg, 16, 32)
		v, verr := parseHex64LE(val)
		if !ok || err != nil || verr != nil || t.WriteReg(int(n), v) != nil {
			return false, s.send("E01")
		}
		return false, s.send("OK")
	case strings.HasPrefix(p, "m"):
		addr, n, err := addrLen(p[1:])
		if err != nil || n > 4096 {
			return false, s.send("E01")
		}
		b := make([]byte, n)
		if err := t.ReadMem(addr, b); err != nil {
			return false, s.send("E14") // EFAULT
		}
		return false, s.send(hex.EncodeToString(b))
	case strings.HasPrefix(p, "M"):
		head, data, ok := strings.Cut(p[1:], ":")
		addr, n, err := addrLen(head)
		b, herr := hex.DecodeString(data)
		if !ok || err != nil || herr != nil || uint64(len(b)) != n {
			return false, s.send("E01")
		}
		if err := t.WriteMem(addr, b); err != nil {
			return false, s.send("E14")
		}
		return false, s.send("OK")
	case strings.HasPrefix(p, "Z0,"), strings.HasPrefix(p, "Z1,"), strings.HasPrefix(p, "z0,"), strings.HasPrefix(p, "z1,"):
		addr, _, err := addrLen(p[3:])
		if err != nil {
			return false, s.send("E01")
		}
		if p[0] == 'Z' {
			err = t.AddBreakpoint(addr)
		} else {
			err = t.RemoveBreakpoint(addr)
		}
		if err != nil {
			return false, s.send("E01")
		}
		return false, s.send("OK")
	case p == "c" || strings.HasPrefix(p, "c") || p == "vCont;c" || strings.HasPrefix(p, "vCont;c") || strings.HasPrefix(p, "vCont;C"):
		if s.exited {
			return false, s.send("W00")
		}
		return false, s.stopReply(t.Continue(s.interrupted))
	case p == "s" || strings.HasPrefix(p, "s") || strings.HasPrefix(p, "vCont;s") || strings.HasPrefix(p, "vCont;S"):
		if s.exited {
			return false, s.send("W00")
		}
		return false, s.stopReply(t.Step())
	case p == "bs":
		return false, s.stopReply(t.ReverseStep())
	case p == "bc":
		return false, s.stopReply(t.ReverseContinue(s.interrupted))
	case p == "D" || strings.HasPrefix(p, "D;"):
		return true, s.send("OK")
	case p == "k":
		return true, nil
	}
	return false, s.send("") // unsupported
}

func addrLen(s string) (uint64, uint64, error) {
	a, l, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, errors.New("bad address")
	}
	addr, err := strconv.ParseUint(a, 16, 64)
	if err != nil {
		return 0, 0, err
	}
	n, err := strconv.ParseUint(l, 16, 64)
	return addr, n, err
}

// xfer serves a qXfer read of doc at "offset,length".
func (s *Server) xfer(doc, args string) error {
	off, n, err := addrLen(args)
	if err != nil {
		return s.send("E01")
	}
	if off >= uint64(len(doc)) {
		return s.send("l")
	}
	end := min(off+n, uint64(len(doc)))
	prefix := "m"
	if end == uint64(len(doc)) {
		prefix = "l"
	}
	return s.send(prefix + doc[off:end])
}
