package tsutil

/*
#include <stddef.h>
void ts_set_allocator(
	void *(*new_malloc)(size_t),
	void *(*new_calloc)(size_t, size_t),
	void *(*new_realloc)(void *, size_t),
	void (*new_free)(void *));
*/
import "C"

import _ "github.com/enola-labs/enola/internal/treesitter"

func init() {
	C.ts_set_allocator(nil, nil, nil, nil)
}
