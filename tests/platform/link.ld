/* Bare-metal layout for rvsim's default memory map: RAM at 0x8000_0000. */
OUTPUT_ARCH("riscv")
ENTRY(_start)
SECTIONS
{
  . = 0x80000000;
  .text.init : { *(.text.init) }
  .text : { *(.text) }
  .rodata : { *(.rodata) }
  .data : { *(.data) }
  .tohost : { *(.tohost) }
  .bss : { *(.bss) }
}
