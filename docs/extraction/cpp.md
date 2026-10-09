# C / C++ — what enola extracts

Parsed with tree-sitter-c and tree-sitter-cpp. Detected by a C source (`.c`), a C++
source (`.cpp`/`.cc`/`.cxx`/`.hpp`/…), or a build file (`CMakeLists.txt`, `Makefile`,
`meson.build`, `*.vcxproj`) plus any header. Language is recorded **per fact**, so a mixed
tree reports `c` and `cpp` separately rather than collapsing to one.

Fixture: [`cpp_sample`](../../internal/engine/testdata/repos/cpp_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a source directory | one module per directory | `module` |
| `int foo(void) { … }` | a symbol with `has_body=true` | `symbol` |
| `static int foo(void)` | the same, with `static=true`, `exported=false` | `symbol` |
| a declaration in a header + definition in a `.c` | **one** merged symbol, not two | `symbol` |
| `#define X`, `const int` | a `constant` symbol | `symbol` |
| `gc->set = xlp_gpio_set;` | a call edge to the assigned function | relation |
| `.lock = fn` in a compound literal | a call edge to `fn` | relation |
| a function name inside a `#define` body | a call edge recovered by a macro pre-pass | relation |
| a token-pasted callback (`_pfx##_name##_show`) | a call edge to the pasted name | relation |
| a call inside a `for`/`while` | `calls_in_loop` / `calls_in_scaling_loop` | props |

## Symbols, and why `has_body` matters

```c
static int omap_reserve(void) { … }
```

```
symbol drivers.omap_reserve   drivers/board.c:12
       props: symbol_kind=function, language=c, static=true, exported=false,
              has_body=true, cyclomatic=1
```

A header declaration and its definition are the same entity. Merging them means the
symbol has one location — the definition — and one set of callers, instead of a phantom
zero-caller declaration sitting next to the real thing.

## The three ways a C callback gets its only caller

C code wires behaviour through function pointers, and none of it looks like a call. All
three forms are recovered, because otherwise most of a driver reads as dead code.

**1. Function-pointer field assignment**

```c
static int probe(struct gpio_chip *gc)
{
	gc->set = xlp_gpio_set;          /* plain */
	gc->get = &xlp_gpio_get;         /* address-of */
	ct->chip.irq_mask = mvebu_mask;  /* nested field */
	gc->ngpio = 32;                  /* plain data — must NOT create an edge */
}
```

The first three create a `calls` edge from `probe` to the assigned function. The fourth
assigns an integer and creates nothing — the discriminator is whether the right-hand side
names a known function, not whether the statement is an assignment.

**2. Compound-literal designated initializers**

```c
cfg = (struct regmap_config) {
	.reg_bits = 8,
	.lock     = dio48e_regmap_lock,
	.unlock   = dio48e_regmap_unlock,
};
```

`dio48e_regmap_lock` and `dio48e_regmap_unlock` get inbound edges from the enclosing
function. `.reg_bits = 8` does not.

**3. References that exist only inside macro bodies**

A function named only in a `#define` replacement list is invisible to the AST — the
preprocessor would have to run first. A project-wide macro pre-pass recovers both the call
position and the value position:

```c
#define ATTR_PERM(_pfx, _name, _perm)                        \
	static struct configfs_attribute _pfx##attr_##_name = {  \
		.show  = _pfx##_name##_show,                         \
		.store = _pfx##_name##_store,                        \
	}
```

The pre-pass is **project-wide, not include-scoped**, so the pasted `cfg_label_show` /
`cfg_label_store` callbacks are recovered even though the invoking file does not literally
`#include` the header that defines the macro. Following `#include` graphs exactly would
lose these, and the kernel's sysfs and configfs attribute surfaces are built almost
entirely this way.

## Loops

```
symbol drivers.constant_loop   drivers/loops.cpp:12
       props: loop_count=1, loop_depth=1, scaling_loop_depth=0,
              calls_in_loop=[drivers.step], calls_in_scaling_loop=[]
```

Same model as [Go](go.md#loops-for-n1-hunting) and [Ruby](ruby.md): a constant-bounded
loop records the call but keeps the scaling set empty rather than absent.

A `for` is constant-bounded only when it both starts and stops at a constant. `for (int i
= n - 1; i >= 0; i--)` compares against a literal and walks all *n*, so it scales.
`calls_in_scaling_loop_depth` gives the scaling depth each in-loop call sits at.

## C++ specifics

Namespaces, templates and class methods are extracted, with header and source methods
merged into one symbol as above.

> **A note on `override`.** The C++ corpus used to validate this extractor
> (getdp, gmsh, the Linux kernel) is pre-C++11 and never uses the `override`
> specifier, so the code path that consumes it has zero exercise on real code.
> `virtual` is the specifier that is actually in play there.

## What is deliberately not extracted

- **Preprocessor evaluation.** `#if`/`#ifdef` branches are all parsed; enola does not pick
  a configuration, so facts from mutually exclusive branches can coexist.
- **Template instantiation.** A template is one symbol, not one per instantiation.
- **Virtual dispatch.** A call through a base-class pointer resolves to the declared
  method, not to every override.
- **Linker-level symbol resolution.** Two static functions with the same name in different
  translation units are distinguished by module, not by object file.

---

Measured on real C/C++ repositories — including the Linux kernel: [BENCHMARKS.md](../BENCHMARKS.md).
