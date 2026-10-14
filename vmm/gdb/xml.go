package gdb

import _ "embed"

// targetXML describes the target to GDB: RV64 integer registers and pc, the
// CSRs rvsim implements (GDB numbers CSR c as register 65+c), and the
// privilege level ("priv", register 4161). There are no FP registers: rvsim
// has no F or D.
//
//go:embed target.xml
var targetXML string
