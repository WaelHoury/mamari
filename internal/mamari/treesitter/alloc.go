package treesitter

// go-tree-sitter's own package init() calls its Go-level SetAllocator(nil,
// nil, nil, nil), which looks like "use the library default" but is not:
// it always registers non-NULL C trampoline functions with tree-sitter's
// real ts_set_allocator, and those trampolines call back into Go (the
// exported go_malloc/go_calloc/go_realloc/go_free functions) only to turn
// around and call the same C.malloc/calloc/realloc/free anyway. The result
// is a full C-to-Go cgo callback — measurably more expensive than a normal
// Go-to-C call, since it additionally has to acquire an OS thread — for
// *every single allocation tree-sitter's C parser makes while building a
// parse tree, with no way to avoid it through go-tree-sitter's own Go API
// (every code path through SetAllocator still installs those same
// trampolines; only the Go-side function the trampoline calls changes).
//
// Confirmed via CPU profiling a 10,000-line real-world Scala file: 55% of
// total time in runtime.cgocall, ~47% cumulative in
// runtime.cgocallbackg/cgocallbackg1, for a file an order of magnitude
// larger than typical but otherwise unremarkable — parsed in well under a
// second by tools with no such per-allocation indirection.
//
// tree-sitter's own C API doc for ts_set_allocator is explicit: "If you
// pass NULL for any parameter, Tree-sitter will switch back to its
// default implementation" — i.e. plain libc malloc/calloc/realloc/free,
// no indirection of any kind. That default is only reachable by calling
// the real C function with true NULLs, which go-tree-sitter's Go-level
// wrapper never does. This file calls it directly, after go-tree-sitter's
// own init() has already run (Go guarantees an imported package's init()
// completes before the importing package's init() runs), restoring
// tree-sitter's actual zero-overhead allocator for the lifetime of the
// process.
//
// No header dependency on go-tree-sitter's own (version-pinned,
// module-cache-pathed) C sources is needed: the function is already
// linked into the same binary by go-tree-sitter's own cgo build, so a
// bare forward declaration matching the public C API below is enough —
// only the cgo preamble comment immediately below (no blank line, as cgo
// requires) is actually fed to the C compiler.
//
// The declaration uses __SIZE_TYPE__ (a GCC/Clang builtin macro that
// always expands to whatever size_t actually is on the target) rather
// than hand-picking a C integer type for it: size_t is 64-bit unsigned on
// every platform mamari currently builds for (macOS/Linux, both LP64,
// where "unsigned long" happens to match), but "unsigned long" is only
// 32-bit on Windows (LLP64) — a real ABI mismatch waiting to surprise a
// future Windows build, caught by checking this file specifically while
// verifying mamari's other recent cgo-adjacent changes (the chunked
// parser reader, the per-language Language/Query cache) on Linux for the
// first time, not by Windows actually being a build target today.

/*
extern void ts_set_allocator(
    void *(*new_malloc)(__SIZE_TYPE__),
    void *(*new_calloc)(__SIZE_TYPE__, __SIZE_TYPE__),
    void *(*new_realloc)(void *, __SIZE_TYPE__),
    void (*new_free)(void *)
);
*/
import "C"

func init() {
	C.ts_set_allocator(nil, nil, nil, nil)
}
