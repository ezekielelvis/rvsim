// Package uart implements a 16550A-compatible UART ("ns16550a") with byte-
// wide registers at consecutive offsets, as on QEMU virt.
//
// Transmission is instantaneous: a write to THR goes straight to the output
// writer and THR is empty again at once. Received bytes are delivered only
// by Receive, which the run loop calls from scheduled events, so input
// reaches the guest at a deterministic point in virtual time.
package uart

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/ezekielelvis/rvsim/vmm/devices"
)

// Size is the UART's MMIO window.
const Size = 0x100

const fifoSize = 16

// Register offsets (DLAB = 0 unless noted).
const (
	regRBR = 0 // read: receive buffer; write: THR; DLAB=1: DLL
	regIER = 1 // DLAB=1: DLM
	regIIR = 2 // read: interrupt identification; write: FCR
	regLCR = 3
	regMCR = 4
	regLSR = 5
	regMSR = 6
	regSCR = 7
)

// Register bits.
const (
	ierRDA  = 1 << 0
	ierTHRE = 1 << 1
	ierRLS  = 1 << 2

	iirNone    = 0x01
	iirRLS     = 0x06
	iirRDA     = 0x04
	iirTimeout = 0x0c
	iirTHRE    = 0x02
	iirFIFO    = 0xc0

	fcrEnable  = 1 << 0
	fcrClearRX = 1 << 1

	lcrDLAB = 1 << 7

	mcrLoop = 1 << 4

	lsrDR   = 1 << 0
	lsrOE   = 1 << 1
	lsrTHRE = 1 << 5
	lsrTEMT = 1 << 6

	// DCD, DSR and CTS asserted, as QEMU reports for a connected chardev.
	msrConnected = 0xb0
)

var triggerLevels = [4]int{1, 4, 8, 14}

// state is everything a snapshot must preserve (fixed-size for encoding/binary).
type state struct {
	IER, LCR, MCR, LSR, SCR, FCR uint8
	DLL, DLM                     uint8
	ThrPending                   bool // THRE interrupt raised and not yet acknowledged
	RxLen                        uint8
	RxHead                       uint8
	Rx                           [fifoSize]byte
}

// UART is the device.
type UART struct {
	s       state
	out     io.Writer
	irq     devices.IRQLine
	irqHigh bool
	txBuf   [1]byte
}

// New returns a UART writing output to out and signalling irq.
func New(out io.Writer, irq devices.IRQLine) *UART {
	u := &UART{out: out, irq: irq}
	u.Reset()
	return u
}

// SetOutput redirects transmitted bytes to out.
func (u *UART) SetOutput(out io.Writer) { u.out = out }

// Reset implements devices.Device.
func (u *UART) Reset() {
	u.s = state{LSR: lsrTHRE | lsrTEMT}
	u.update()
}

// Receive delivers one byte from the host side. If the receive FIFO is full
// the byte is lost and the overrun flag is set, as on real hardware.
func (u *UART) Receive(b byte) {
	if int(u.s.RxLen) == u.rxCapacity() {
		u.s.LSR |= lsrOE
	} else {
		u.s.Rx[(int(u.s.RxHead)+int(u.s.RxLen))%fifoSize] = b
		u.s.RxLen++
		u.s.LSR |= lsrDR
	}
	u.update()
}

// RxSpace reports how many more bytes Receive can accept without overrun.
func (u *UART) RxSpace() int { return u.rxCapacity() - int(u.s.RxLen) }

func (u *UART) rxCapacity() int {
	if u.s.FCR&fcrEnable != 0 {
		return fifoSize
	}
	return 1
}

func (u *UART) dlab() bool { return u.s.LCR&lcrDLAB != 0 }

// Read implements devices.Device. The UART is byte-wide; wider accesses
// read the addressed byte, as QEMU does.
func (u *UART) Read(off uint64, size int) uint64 {
	switch off {
	case regRBR:
		if u.dlab() {
			return uint64(u.s.DLL)
		}
		return uint64(u.popRx())
	case regIER:
		if u.dlab() {
			return uint64(u.s.DLM)
		}
		return uint64(u.s.IER)
	case regIIR:
		iir := u.iir()
		if iir == iirTHRE {
			// Reading IIR acknowledges a THRE interrupt (16550A datasheet).
			u.s.ThrPending = false
			u.update()
		}
		if u.s.FCR&fcrEnable != 0 {
			iir |= iirFIFO
		}
		return uint64(iir)
	case regLCR:
		return uint64(u.s.LCR)
	case regMCR:
		return uint64(u.s.MCR)
	case regLSR:
		lsr := u.s.LSR
		u.s.LSR &^= lsrOE // reading LSR clears the error bits
		u.update()
		return uint64(lsr)
	case regMSR:
		if u.s.MCR&mcrLoop != 0 {
			// Loopback: RTS->CTS, DTR->DSR, OUT1->RI, OUT2->DCD.
			m := u.s.MCR
			return uint64((m&0x02)<<3 | (m&0x01)<<5 | (m&0x04)<<4 | (m&0x08)<<4)
		}
		return msrConnected
	case regSCR:
		return uint64(u.s.SCR)
	}
	return 0
}

// Write implements devices.Device.
func (u *UART) Write(off uint64, size int, v uint64) {
	b := uint8(v)
	switch off {
	case regRBR:
		if u.dlab() {
			u.s.DLL = b
			return
		}
		u.transmit(b)
	case regIER:
		if u.dlab() {
			u.s.DLM = b
			return
		}
		was := u.s.IER
		u.s.IER = b & 0x0f
		// Enabling the THRE interrupt while THR is empty raises it at once.
		if was&ierTHRE == 0 && u.s.IER&ierTHRE != 0 {
			u.s.ThrPending = true
		}
	case regIIR: // FCR
		if b&fcrEnable != (u.s.FCR & fcrEnable) {
			u.clearRx() // switching FIFO mode clears the FIFOs
		}
		if b&fcrClearRX != 0 {
			u.clearRx()
		}
		u.s.FCR = b & 0xc1 // keep the enable and trigger bits
	case regLCR:
		u.s.LCR = b
	case regMCR:
		u.s.MCR = b & 0x1f
	case regSCR:
		u.s.SCR = b
	}
	u.update()
}

func (u *UART) transmit(b byte) {
	if u.s.MCR&mcrLoop != 0 {
		u.Receive(b)
	} else if u.out != nil {
		u.txBuf[0] = b
		_, _ = u.out.Write(u.txBuf[:])
	}
	// THR is empty again immediately; a THRE interrupt follows if enabled.
	u.s.ThrPending = u.s.IER&ierTHRE != 0
}

func (u *UART) popRx() byte {
	if u.s.RxLen == 0 {
		return 0
	}
	b := u.s.Rx[u.s.RxHead]
	u.s.RxHead = (u.s.RxHead + 1) % fifoSize
	u.s.RxLen--
	if u.s.RxLen == 0 {
		u.s.LSR &^= lsrDR
	}
	u.update()
	return b
}

func (u *UART) clearRx() {
	u.s.RxLen, u.s.RxHead = 0, 0
	u.s.LSR &^= lsrDR
}

// iir returns the highest-priority pending interrupt (16550A table 4):
// line status, received data / character timeout, THR empty, modem status.
// Data below the FIFO trigger level reports as a character timeout
// immediately rather than after four character times, which keeps the
// UART free of wall-clock timers; drivers treat the two the same way.
func (u *UART) iir() uint8 {
	switch {
	case u.s.IER&ierRLS != 0 && u.s.LSR&lsrOE != 0:
		return iirRLS
	case u.s.IER&ierRDA != 0 && u.s.RxLen > 0:
		if u.s.FCR&fcrEnable != 0 && int(u.s.RxLen) < triggerLevels[u.s.FCR>>6] {
			return iirTimeout
		}
		return iirRDA
	case u.s.IER&ierTHRE != 0 && u.s.ThrPending:
		return iirTHRE
	}
	// The modem status lines never change, so there is no MS interrupt.
	return iirNone
}

func (u *UART) update() {
	high := u.iir() != iirNone
	if high != u.irqHigh && u.irq != nil {
		u.irq.SetLevel(high)
	}
	u.irqHigh = high
}

// Snapshot implements devices.Device.
func (u *UART) Snapshot() []byte {
	var b bytes.Buffer
	b.WriteByte(1) // format version
	_ = binary.Write(&b, binary.LittleEndian, &u.s)
	return b.Bytes()
}

// Restore implements devices.Device.
func (u *UART) Restore(data []byte) error {
	if len(data) != 1+binary.Size(state{}) || data[0] != 1 {
		return fmt.Errorf("uart: bad snapshot (%d bytes)", len(data))
	}
	if err := binary.Read(bytes.NewReader(data[1:]), binary.LittleEndian, &u.s); err != nil {
		return err
	}
	// Re-drive the interrupt line to match the restored state.
	u.irqHigh = u.iir() != iirNone
	if u.irq != nil {
		u.irq.SetLevel(u.irqHigh)
	}
	return nil
}
