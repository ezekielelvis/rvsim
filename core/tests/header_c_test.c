/* rv.h must compile as plain C11: that is how cgo consumes it. The layout
 * static assertions in the header run in this translation unit too. */
#include "rv.h"

unsigned rv_header_c_abi_version(void) { return RV_ABI_VERSION; }
