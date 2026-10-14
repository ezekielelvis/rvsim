package vm

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/machine"
)

func newHTIFVM(t *testing.T, console *bytes.Buffer) (*VM, machine.Config) {
	t.Helper()
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 1 << 20
	cfg.HTIFToHost = 0x8000_1000
	cfg.HTIFFromHost = 0x8000_1040
	v, err := New(Config{Machine: cfg, Console: console, HTIF: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return v, cfg
}

func readU64(t *testing.T, m *machine.Machine, addr uint64) uint64 {
	t.Helper()
	var b [8]byte
	if err := m.ReadPhys(addr, b[:]); err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint64(b[:])
}

func TestHandleHTIF(t *testing.T) {
	tests := []struct {
		name         string
		cmd          uint64
		wantDone     bool
		wantCode     uint64
		wantErr      string
		wantConsole  string
		wantFromHost uint64
	}{
		{name: "pass", cmd: 1, wantDone: true, wantCode: 0},
		{name: "fail case 3", cmd: 3<<1 | 1, wantDone: true, wantCode: 3},
		{name: "console putchar", cmd: 1<<56 | 1<<48 | 'A', wantConsole: "A", wantFromHost: 1<<56 | 1<<48 | 0x100 | 'A'},
		{name: "syscall pointer", cmd: 0x8000_2000, wantErr: "unsupported HTIF command"},
		{name: "unknown device", cmd: 7 << 56, wantErr: "device 7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var console bytes.Buffer
			v, cfg := newHTIFVM(t, &console)
			m := v.Machine()
			if err := v.writeU64(cfg.HTIFToHost, tt.cmd); err != nil {
				t.Fatal(err)
			}
			done, code, err := v.handleHTIF(tt.cmd)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if done != tt.wantDone || code != tt.wantCode {
				t.Fatalf("done, code = %v, %d; want %v, %d", done, code, tt.wantDone, tt.wantCode)
			}
			if console.String() != tt.wantConsole {
				t.Fatalf("console = %q, want %q", console.String(), tt.wantConsole)
			}
			if !done {
				if v := readU64(t, m, cfg.HTIFToHost); v != 0 {
					t.Fatalf("tohost = %#x after acknowledgement, want 0", v)
				}
				if v := readU64(t, m, cfg.HTIFFromHost); v != tt.wantFromHost {
					t.Fatalf("fromhost = %#x, want %#x", v, tt.wantFromHost)
				}
			}
		})
	}
}
