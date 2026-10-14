// Package gdbtest finds a GDB that can debug RISC-V targets and runs it in
// batch mode against a stub, for integration tests.
package gdbtest

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/gdb"
	"github.com/ezekielelvis/rvsim/vmm/internal/rvasm"
	"github.com/ezekielelvis/rvsim/vmm/replay"
)

// Find returns a GDB binary with RISC-V support, or skips the test (fails
// it when RVSIM_REQUIRE_GDB=1).
func Find(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"riscv64-unknown-elf-gdb", "riscv64-linux-gnu-gdb", "gdb-multiarch", "gdb"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if exec.Command(path, "-batch", "-nx", "-ex", "set architecture riscv:rv64").Run() == nil {
			return path
		}
	}
	if os.Getenv("RVSIM_REQUIRE_GDB") == "1" {
		t.Fatal("no GDB with RISC-V support found")
	}
	t.Skip("no GDB with RISC-V support (install gdb-multiarch or riscv64-unknown-elf-gdb)")
	return ""
}

// Run serves tgt on a loopback port and runs GDB in batch mode with args,
// whose "target remote" commands should use the placeholder address
// returned to argsFor. It returns GDB's combined output.
func Run(t *testing.T, gdbPath string, tgt gdb.Target, argsFor func(addr string) []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			served <- err
			return
		}
		defer conn.Close()
		_, err = gdb.Serve(conn, tgt, nil)
		served <- err
	}()
	args := append([]string{"-batch", "-nx"}, argsFor(ln.Addr().String())...)
	out, err := exec.Command(gdbPath, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("gdb: %v\n%s", err, out)
	}
	if err := <-served; err != nil {
		t.Fatalf("stub: %v\n%s", err, out)
	}
	return string(out)
}

// Echo polls the UART and echoes each byte (at echoPC), then spins ~4,000
// steps; it powers off after 'q'. EchoPC is its echoing store.
var Echo = rvasm.Bytes(
	rvasm.Lui(rvasm.T0, 0x10000),
	rvasm.Auipc(rvasm.T2, 0x10),
	rvasm.Lui(rvasm.S0, 0x100),
	rvasm.Lbu(rvasm.T1, rvasm.T0, 5),
	rvasm.Andi(rvasm.T1, rvasm.T1, 1),
	rvasm.Beq(rvasm.T1, rvasm.Zero, -8),
	rvasm.Lbu(rvasm.A0, rvasm.T0, 0),
	rvasm.Sb(rvasm.A0, rvasm.T0, 0), // echoPC
	rvasm.Sb(rvasm.A0, rvasm.T2, 0),
	rvasm.Addi(rvasm.T2, rvasm.T2, 1),
	rvasm.Addi(rvasm.S1, rvasm.Zero, 2000),
	rvasm.Addi(rvasm.S1, rvasm.S1, -1),
	rvasm.Bne(rvasm.S1, rvasm.Zero, -4),
	rvasm.Addi(rvasm.T1, rvasm.Zero, 'q'),
	rvasm.Bne(rvasm.A0, rvasm.T1, -44),
	rvasm.Lui(rvasm.T1, 5),
	rvasm.Addi(rvasm.T1, rvasm.T1, 0x555),
	rvasm.Sw(rvasm.T1, rvasm.S0, 0),
	rvasm.Self,
)

// EchoPC is the address of the echoing store.
const EchoPC = 0x8000_0000 + 7*4

// RecordEcho records a session of Echo receiving text and returns the
// recording's path.
func RecordEcho(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	fw := filepath.Join(dir, "fw.bin")
	kernel := filepath.Join(dir, "Image")
	os.WriteFile(fw, Echo, 0o644)
	os.WriteFile(kernel, make([]byte, 4096), 0o644)
	path := filepath.Join(dir, "s.rvrec")
	in := make(chan byte, len(text))
	for i := range len(text) {
		in <- text[i]
	}
	close(in)
	s, err := replay.StartRecording(replay.Spec{Firmware: fw, Kernel: kernel, RAMSize: 64 << 20, CheckpointEvery: 5_000},
		path, &bytes.Buffer{}, in)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for {
		res, err := s.Run(1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if res.Exited {
			if err := s.Finish(res, false); err != nil {
				t.Fatal(err)
			}
			return path
		}
	}
}
