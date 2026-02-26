# monty-wasm-go

Go bindings for [pydantic/monty](https://github.com/pydantic/monty) using an embedded Wasm module executed by [wazero](https://github.com/tetratelabs/wazero).

Unlike `monty-go` (cgo + static Rust archive), this package:

- embeds a `monty.wasm` binary directly in the Go package,
- runs it through wazero (pure Go runtime),
- exposes an idiomatic Go continuation API for paused execution.

## Status

- [x] Compile Monty programs from Python code.
- [x] Start execution with JSON-serializable inputs.
- [x] Handle external/OS pauses via `Call.Return`, `Call.Throw`, `Call.Defer`.
- [x] Resume pending futures.
- [x] Dump/load program state bytes.

## Common workflows

```bash
# build/rebuild embedded wasm
make wasm

# run go tests
make test

# format rust + go code
make fmt
```

## Build the Wasm module manually

You only need this when updating the Rust bridge or Monty version.

```bash
rustup target add wasm32-wasip1
cargo build --release --target wasm32-wasip1 --manifest-path rust/monty-wasm/Cargo.toml
cp rust/monty-wasm/target/wasm32-wasip1/release/monty_wasm.wasm pkg/monty/wasm/monty.wasm
```

## Usage

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ricochet1k/monty-wasm-go/pkg/monty"
)

func main() {
	ctx := context.Background()
	rt, err := monty.NewRuntime(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)

	prog, err := rt.Compile(ctx, "external_add(x, 10) * 2", monty.CompileOptions{
		InputNames:        []string{"x"},
		ExternalFunctions: []string{"external_add"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer prog.Close(ctx)

	step, err := prog.Start(ctx, 11)
	if err != nil {
		log.Fatal(err)
	}
	if step.Call == nil {
		log.Fatalf("expected function call, got kind=%v", step.Kind)
	}

	var x, y int
	_ = step.Call.Args[0].Decode(&x)
	_ = step.Call.Args[1].Decode(&y)

	next, err := step.Call.Return(ctx, x+y)
	if err != nil {
		log.Fatal(err)
	}

	var out int
	if err := next.Result.Decode(&out); err != nil {
		log.Fatal(err)
	}
	fmt.Println(out)
}
```
