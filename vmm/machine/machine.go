// Package machine wraps the rvsim C++ core through its C ABI (core/include/rv.h).
//
// A Machine is not safe for concurrent use. All calls must come from the
// single goroutine that owns the run loop (determinism rule 1).
package machine

/*
#cgo CFLAGS: -I${SRCDIR}/../../core/include
#cgo LDFLAGS: -L${SRCDIR}/../../build/core -lrvcore
#cgo darwin LDFLAGS: -lc++
#cgo linux LDFLAGS: -lstdc++ -lm
#include "rv.h"
#include "rv_trace.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// ABIVersion is the rv.h ABI version this package was compiled against.
const ABIVersion = uint32(C.RV_ABI_VERSION)

// coreLibHash is the SHA-256 of the linked librvcore.a, set by the generated
// zz_corehash.go. It also makes Go relink whenever the C++ core changes.
var coreLibHash string

// CoreBuild identifies the linked core library build, or "" if unknown.
func CoreBuild() string { return coreLibHash }

// Status is an rv_status code returned by the core.
type Status int32

const (
	StatusInval       Status = C.RV_E_INVAL
	StatusRange       Status = C.RV_E_RANGE
	StatusState       Status = C.RV_E_STATE
	StatusNoMem       Status = C.RV_E_NOMEM
	StatusABI         Status = C.RV_E_ABI
	StatusSnapVersion Status = C.RV_E_SNAP_VER
	StatusSnapBad     Status = C.RV_E_SNAP_BAD
	StatusBufSize     Status = C.RV_E_BUFSZ
	StatusPoisoned    Status = C.RV_E_POISONED
	StatusUnsupported Status = C.RV_E_UNSUPPORTED
	StatusInternal    Status = C.RV_E_INTERNAL
)

var statusNames = [...]string{
	-C.RV_E_INVAL:       "invalid argument",
	-C.RV_E_RANGE:       "out of range",
	-C.RV_E_STATE:       "invalid state",
	-C.RV_E_NOMEM:       "out of memory",
	-C.RV_E_ABI:         "ABI version mismatch",
	-C.RV_E_SNAP_VER:    "unsupported snapshot version",
	-C.RV_E_SNAP_BAD:    "corrupt snapshot",
	-C.RV_E_BUFSZ:       "buffer too small",
	-C.RV_E_POISONED:    "machine poisoned by earlier error",
	-C.RV_E_UNSUPPORTED: "not implemented",
}

func (s Status) Error() string {
	if s == StatusInternal {
		return "rv: internal error"
	}
	if i := -int(s); i > 0 && i < len(statusNames) && statusNames[i] != "" {
		return "rv: " + statusNames[i]
	}
	return fmt.Sprintf("rv: status %d", int32(s))
}

// ErrClosed is returned by methods called after Close.
var ErrClosed = errors.New("rv: machine closed")

func check(op string, rc C.int) error {
	if rc == C.RV_OK {
		return nil
	}
	return fmt.Errorf("rv_%s: %w", op, Status(rc))
}

// Config flags (RV_CFG_*).
const (
	FlagHWADUpdate   = uint32(C.RV_CFG_HW_AD_UPDATE)
	FlagMisalignedOK = uint32(C.RV_CFG_MISALIGNED_OK)
)

// Config mirrors rv_config.
type Config struct {
	Flags        uint32
	RAMBase      uint64
	RAMSize      uint64
	CLINTBase    uint64
	ResetPC      uint64
	HartID       uint64
	StepsPerTick uint64
	HTIFToHost   uint64 // 0 disables HTIF
	HTIFFromHost uint64
}

// DefaultConfig returns the QEMU-virt-compatible memory map (ADR-002) with
// 256 MiB of RAM and Svadu available (ADR-025).
func DefaultConfig() Config {
	return Config{
		Flags:        FlagHWADUpdate,
		RAMBase:      0x8000_0000,
		RAMSize:      256 << 20,
		CLINTBase:    0x0200_0000,
		ResetPC:      0x8000_0000,
		StepsPerTick: 10,
	}
}

// ExitReason mirrors rv_exit_reason.
type ExitReason uint32

const (
	ExitNone       ExitReason = C.RV_EXIT_NONE
	ExitMMIORead   ExitReason = C.RV_EXIT_MMIO_READ
	ExitMMIOWrite  ExitReason = C.RV_EXIT_MMIO_WRITE
	ExitBudget     ExitReason = C.RV_EXIT_BUDGET
	ExitWFI        ExitReason = C.RV_EXIT_WFI
	ExitBreakpoint ExitReason = C.RV_EXIT_BREAKPOINT
	ExitHalt       ExitReason = C.RV_EXIT_HALT
	ExitTraceFull  ExitReason = C.RV_EXIT_TRACE_FULL
	ExitError      ExitReason = C.RV_EXIT_ERROR
)

var exitNames = [...]string{"none", "mmio-read", "mmio-write", "budget", "wfi", "breakpoint", "halt", "trace-full", "error"}

func (r ExitReason) String() string {
	if int(r) < len(exitNames) {
		return exitNames[r]
	}
	return fmt.Sprintf("exit(%d)", uint32(r))
}

// Exit is a Go copy of rv_exit. See rv.h for per-reason field meanings.
type Exit struct {
	Reason    ExitReason
	Size      uint32
	Addr      uint64
	Value     uint64
	PC        uint64
	Steps     uint64
	Instret   uint64
	TimerStep uint64
}

// TraceRecord mirrors rv_trace_record (core/include/rv_trace.h). Its layout
// is checked against the C struct at compile time below.
type TraceRecord struct {
	PC        uint64
	Insn      uint32
	Kind      uint8 // TraceCommit or TraceTrap
	Priv      uint8
	InsnLen   uint8
	Flags     uint8 // TraceFlag*
	Rd        uint8
	MemSize   uint8
	CSR       uint16
	CSR2      uint16
	_         uint16
	RdValue   uint64 // TraceTrap: cause
	CSRValue  uint64 // TraceTrap: handler pc
	MemAddr   uint64 // TraceTrap: tval
	MemValue  uint64
	CSR2Value uint64
}

// Compile-time checks that TraceRecord matches rv_trace_record: indexing a
// one-element array with a non-zero (or wrapped negative) constant fails to compile.
var (
	_ = [1]struct{}{}[unsafe.Sizeof(TraceRecord{})-unsafe.Sizeof(C.rv_trace_record{})]
	_ = [1]struct{}{}[unsafe.Offsetof(TraceRecord{}.CSR)-unsafe.Offsetof(C.rv_trace_record{}.csr)]
	_ = [1]struct{}{}[unsafe.Offsetof(TraceRecord{}.RdValue)-unsafe.Offsetof(C.rv_trace_record{}.rd_value)]
	_ = [1]struct{}{}[unsafe.Offsetof(TraceRecord{}.MemValue)-unsafe.Offsetof(C.rv_trace_record{}.mem_value)]
	_ = [1]struct{}{}[unsafe.Offsetof(TraceRecord{}.CSR2)-unsafe.Offsetof(C.rv_trace_record{}.csr2)]
	_ = [1]struct{}{}[unsafe.Offsetof(TraceRecord{}.CSR2Value)-unsafe.Offsetof(C.rv_trace_record{}.csr2_value)]
)

// Trace record kinds and flags.
const (
	TraceCommit = uint8(C.RV_TRACE_COMMIT)
	TraceTrap   = uint8(C.RV_TRACE_TRAP)

	TraceFlagRd    = uint8(C.RV_TF_RD)
	TraceFlagCSR   = uint8(C.RV_TF_CSR)
	TraceFlagLoad  = uint8(C.RV_TF_LOAD)
	TraceFlagStore = uint8(C.RV_TF_STORE)
	TraceFlagCSR2  = uint8(C.RV_TF_CSR2)
)

// Register numbers for Reg/SetReg (RV_REG_*). GPRs are 0..31.
const (
	RegPC   = uint32(C.RV_REG_PC)
	RegPriv = uint32(C.RV_REG_PRIV)
)

// RegCSR returns the register number for CSR csr.
func RegCSR(csr uint16) uint32 { return uint32(C.RV_REG_CSR) + uint32(csr&0xfff) }

// External interrupt lines for SetIRQLines.
const (
	IRQSEIP = uint64(C.RV_IRQ_SEIP)
	IRQMEIP = uint64(C.RV_IRQ_MEIP)
)

// Machine owns one rv_machine.
type Machine struct {
	p       *C.rv_machine
	exit    *C.rv_exit // stable C memory (ADR-001), valid until Close
	cleanup runtime.Cleanup
}

// New creates a machine from cfg.
func New(cfg Config) (*Machine, error) {
	c := C.rv_config{
		abi_version:    C.RV_ABI_VERSION,
		flags:          C.uint32_t(cfg.Flags),
		ram_base:       C.uint64_t(cfg.RAMBase),
		ram_size:       C.uint64_t(cfg.RAMSize),
		clint_base:     C.uint64_t(cfg.CLINTBase),
		reset_pc:       C.uint64_t(cfg.ResetPC),
		mhartid:        C.uint64_t(cfg.HartID),
		steps_per_tick: C.uint64_t(cfg.StepsPerTick),
		htif_tohost:    C.uint64_t(cfg.HTIFToHost),
		htif_fromhost:  C.uint64_t(cfg.HTIFFromHost),
	}
	var p *C.rv_machine
	if err := check("create", C.rv_create(&c, &p)); err != nil {
		return nil, err
	}
	m := &Machine{p: p, exit: C.rv_exit_info(p)}
	// Safety net for leaked machines; Close is the normal path.
	m.cleanup = runtime.AddCleanup(m, func(p *C.rv_machine) { C.rv_destroy(p) }, p)
	return m, nil
}

// Close destroys the machine. It is safe to call more than once.
func (m *Machine) Close() {
	if m.p == nil {
		return
	}
	m.cleanup.Stop()
	C.rv_destroy(m.p)
	m.p, m.exit = nil, nil
}

// Reset performs an architectural reset; RAM is preserved.
func (m *Machine) Reset() error {
	if m.p == nil {
		return ErrClosed
	}
	return check("reset", C.rv_reset(m.p))
}

// Run executes up to maxSteps steps and returns why the core stopped.
// It does not allocate.
func (m *Machine) Run(maxSteps uint64) (Exit, error) {
	if m.p == nil {
		return Exit{}, ErrClosed
	}
	if rc := C.rv_run(m.p, C.uint64_t(maxSteps)); rc != C.RV_OK {
		return Exit{}, check("run", rc)
	}
	e := m.exit
	return Exit{
		Reason:    ExitReason(e.reason),
		Size:      uint32(e.size),
		Addr:      uint64(e.addr),
		Value:     uint64(e.value),
		PC:        uint64(e.pc),
		Steps:     uint64(e.steps),
		Instret:   uint64(e.instret),
		TimerStep: uint64(e.timer_step),
	}, nil
}

// CompleteMMIO finishes the pending MMIO access from the last exit.
func (m *Machine) CompleteMMIO(readValue uint64, accessFault bool) error {
	if m.p == nil {
		return ErrClosed
	}
	var f C.int
	if accessFault {
		f = 1
	}
	return check("complete_mmio", C.rv_complete_mmio(m.p, C.uint64_t(readValue), f))
}

// AdvanceIdle skips idle virtual time after an ExitWFI.
func (m *Machine) AdvanceIdle(steps uint64) error {
	if m.p == nil {
		return ErrClosed
	}
	return check("advance_idle", C.rv_advance_idle(m.p, C.uint64_t(steps)))
}

// SetIRQLines sets the levels of the external interrupt lines (IRQSEIP|IRQMEIP).
func (m *Machine) SetIRQLines(lines uint64) error {
	if m.p == nil {
		return ErrClosed
	}
	return check("set_irq_lines", C.rv_set_irq_lines(m.p, C.uint64_t(lines)))
}

// SetIRQMask hides the given mip bits from the interrupt logic (fault injection).
func (m *Machine) SetIRQMask(suppress uint64) error {
	if m.p == nil {
		return ErrClosed
	}
	return check("set_irq_mask", C.rv_set_irq_mask(m.p, C.uint64_t(suppress)))
}

// Reg reads register reg (0..31, RegPC, RegPriv, or RegCSR(n)).
func (m *Machine) Reg(reg uint32) (uint64, error) {
	if m.p == nil {
		return 0, ErrClosed
	}
	var v C.uint64_t
	if err := check("get_reg", C.rv_get_reg(m.p, C.uint32_t(reg), &v)); err != nil {
		return 0, err
	}
	return uint64(v), nil
}

// SetReg writes register reg.
func (m *Machine) SetReg(reg uint32, value uint64) error {
	if m.p == nil {
		return ErrClosed
	}
	return check("set_reg", C.rv_set_reg(m.p, C.uint32_t(reg), C.uint64_t(value)))
}

// ReadPhys copies len(buf) bytes of physical memory at paddr into buf.
func (m *Machine) ReadPhys(paddr uint64, buf []byte) error {
	if m.p == nil {
		return ErrClosed
	}
	if len(buf) == 0 {
		return nil
	}
	return check("read_phys", C.rv_read_phys(m.p, C.uint64_t(paddr), unsafe.Pointer(&buf[0]), C.size_t(len(buf))))
}

// WritePhys copies buf into physical memory at paddr.
func (m *Machine) WritePhys(paddr uint64, buf []byte) error {
	if m.p == nil {
		return ErrClosed
	}
	if len(buf) == 0 {
		return nil
	}
	return check("write_phys", C.rv_write_phys(m.p, C.uint64_t(paddr), unsafe.Pointer(&buf[0]), C.size_t(len(buf))))
}

// RAM returns a view of guest RAM backed by C memory. It is valid until Close,
// its contents are stable only between Run calls, and it must never be
// written: use WritePhys so the core can invalidate its caches.
func (m *Machine) RAM() []byte {
	if m.p == nil {
		return nil
	}
	var n C.uint64_t
	p := C.rv_ram_view(m.p, &n)
	return unsafe.Slice((*byte)(unsafe.Pointer(p)), int(n))
}

// EnableTrace allocates a trace buffer of capacityBytes in the core, or
// disables tracing when capacityBytes is 0.
func (m *Machine) EnableTrace(capacityBytes int) error {
	if m.p == nil {
		return ErrClosed
	}
	return check("trace_enable", C.rv_trace_enable(m.p, C.size_t(capacityBytes)))
}

// TraceRecords returns the buffered trace records. The slice aliases C memory
// and is valid only until the next ConsumeTrace, Run, or Close.
func (m *Machine) TraceRecords() ([]TraceRecord, error) {
	if m.p == nil {
		return nil, ErrClosed
	}
	var data *C.uint8_t
	var n C.size_t
	if err := check("trace_peek", C.rv_trace_peek(m.p, &data, &n)); err != nil {
		return nil, err
	}
	count := int(n) / int(unsafe.Sizeof(TraceRecord{}))
	if count == 0 {
		return nil, nil
	}
	return unsafe.Slice((*TraceRecord)(unsafe.Pointer(data)), count), nil
}

// ConsumeTrace empties the trace buffer.
func (m *Machine) ConsumeTrace() error {
	if m.p == nil {
		return ErrClosed
	}
	return check("trace_consume", C.rv_trace_consume(m.p))
}
