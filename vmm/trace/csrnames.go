package trace

import "strconv"

// csrNames uses Spike's spelling so commit logs compare equal.
var csrNames = map[uint16]string{
	0x001: "fflags", 0x002: "frm", 0x003: "fcsr",
	0x100: "sstatus", 0x104: "sie", 0x105: "stvec", 0x106: "scounteren", 0x10a: "senvcfg",
	0x140: "sscratch", 0x141: "sepc", 0x142: "scause", 0x143: "stval", 0x144: "sip",
	0x180: "satp",
	0x300: "mstatus", 0x301: "misa", 0x302: "medeleg", 0x303: "mideleg", 0x304: "mie",
	0x305: "mtvec", 0x306: "mcounteren", 0x30a: "menvcfg", 0x320: "mcountinhibit",
	0x340: "mscratch", 0x341: "mepc", 0x342: "mcause", 0x343: "mtval", 0x344: "mip",
	0x744: "mnstatus",
	0xb00: "mcycle", 0xb02: "minstret",
	0xc00: "cycle", 0xc01: "time", 0xc02: "instret",
	0xf11: "mvendorid", 0xf12: "marchid", 0xf13: "mimpid", 0xf14: "mhartid", 0xf15: "mconfigptr",
}

func init() {
	for i := uint16(0); i < 16; i++ {
		csrNames[0x3a0+i] = "pmpcfg" + strconv.Itoa(int(i))
	}
	for i := uint16(0); i < 64; i++ {
		csrNames[0x3b0+i] = "pmpaddr" + strconv.Itoa(int(i))
	}
	for i := uint16(3); i < 32; i++ {
		csrNames[0xb00+i] = "mhpmcounter" + strconv.Itoa(int(i))
		csrNames[0xc00+i] = "hpmcounter" + strconv.Itoa(int(i))
		csrNames[0x320+i] = "mhpmevent" + strconv.Itoa(int(i))
	}
}

// CSRName returns Spike's name for csr, or "unknown".
func CSRName(csr uint16) string {
	if n, ok := csrNames[csr]; ok {
		return n
	}
	return "unknown"
}
