package trace

import (
	"bytes"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/machine"
)

// Expected lines are copied verbatim from Spike's --log-commits output.
func TestSpikeFormat(t *testing.T) {
	tests := []struct {
		name string
		rec  machine.TraceRecord
		want string
	}{
		{
			"no writes",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 3, PC: 0x80000042, Insn: 0xbfc5, InsnLen: 2},
			"core   0: 3 0x0000000080000042 (0xbfc5)",
		},
		{
			"one-digit register",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 3, PC: 0x80000088, Insn: 0x00000297, InsnLen: 4,
				Flags: machine.TraceFlagRd, Rd: 5, RdValue: 0x80000088},
			"core   0: 3 0x0000000080000088 (0x00000297) x5  0x0000000080000088",
		},
		{
			"two-digit register in U-mode",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 0, PC: 0x80000136, Insn: 0x4581, InsnLen: 2,
				Flags: machine.TraceFlagRd, Rd: 11},
			"core   0: 0 0x0000000080000136 (0x4581) x11 0x0000000000000000",
		},
		{
			"load",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 0, PC: 0x80000140, Insn: 0x00010703, InsnLen: 4,
				Flags: machine.TraceFlagRd | machine.TraceFlagLoad, Rd: 14, RdValue: ^uint64(0), MemAddr: 0x80002000, MemSize: 1},
			"core   0: 0 0x0000000080000140 (0x00010703) x14 0xffffffffffffffff mem 0x0000000080002000",
		},
		{
			"byte store",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 3, PC: 0x80000020, Insn: 0x00628023, InsnLen: 4,
				Flags: machine.TraceFlagStore, MemAddr: 0x80001000, MemSize: 1, MemValue: 1},
			"core   0: 3 0x0000000080000020 (0x00628023) mem 0x0000000080001000 0x01",
		},
		{
			"halfword store",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 3, PC: 0x80000024, Insn: 0x00629123, InsnLen: 4,
				Flags: machine.TraceFlagStore, MemAddr: 0x80001002, MemSize: 2, MemValue: 1},
			"core   0: 3 0x0000000080000024 (0x00629123) mem 0x0000000080001002 0x0001",
		},
		{
			"doubleword store",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 0, PC: 0x8000015a, Insn: 0xe006, InsnLen: 2,
				Flags: machine.TraceFlagStore, MemAddr: 0x80002000, MemSize: 8, MemValue: 0x00aa00aa00aa00aa},
			"core   0: 0 0x000000008000015a (0xe006) mem 0x0000000080002000 0x00aa00aa00aa00aa",
		},
		{
			"csr write",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 3, PC: 0x80000090, Insn: 0x30529073, InsnLen: 4,
				Flags: machine.TraceFlagCSR, CSR: 0x305, CSRValue: 0x80000098},
			"core   0: 3 0x0000000080000090 (0x30529073) c773_mtvec 0x0000000080000098",
		},
		{
			"register and csr",
			machine.TraceRecord{Kind: machine.TraceCommit, Priv: 3, PC: 0x80000008, Insn: 0x34029373, InsnLen: 4,
				Flags: machine.TraceFlagRd | machine.TraceFlagCSR, Rd: 6, CSR: 0x340, CSRValue: 0x1888},
			"core   0: 3 0x0000000080000008 (0x34029373) x6  0x0000000000000000 c832_mscratch 0x0000000000001888",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewSpikeWriter(&buf)
			if err := w.Write([]machine.TraceRecord{tt.rec}); err != nil {
				t.Fatal(err)
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want+"\n" {
				t.Fatalf("got  %q\nwant %q", got, tt.want+"\n")
			}
		})
	}
}

func TestSpikeWriterSkipsTraps(t *testing.T) {
	var buf bytes.Buffer
	w := NewSpikeWriter(&buf)
	recs := []machine.TraceRecord{
		{Kind: machine.TraceTrap, PC: 0x80000000, RdValue: 2},
		{Kind: machine.TraceCommit, Priv: 3, PC: 0x80000004, Insn: 0x13, InsnLen: 4},
	}
	if err := w.Write(recs); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if got, want := buf.String(), "core   0: 3 0x0000000080000004 (0x00000013)\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCSRName(t *testing.T) {
	for csr, want := range map[uint16]string{0x300: "mstatus", 0x3a0: "pmpcfg0", 0x3ef: "pmpaddr63", 0xc03: "hpmcounter3", 0x7ff: "unknown"} {
		if got := CSRName(csr); got != want {
			t.Errorf("CSRName(%#x) = %q, want %q", csr, got, want)
		}
	}
}
