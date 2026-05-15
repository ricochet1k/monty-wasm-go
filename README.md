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
- [x] Resume pending futures via `ReplResolveFutures.Resume()`.
- [x] Dump/load program state bytes.
- [x] Async execution with external async functions.
- [x] REPL state persistence across snippets.


## Common workflows

```bash
# build/rebuild embedded wasm
make wasm

# run go tests
make test

# format rust + go code
make fmt
```

## Async Execution

The REPL supports async/await with external async functions. When Monty code calls an
external function inside an async context (e.g., `await foo()` or `asyncio.gather(foo())`), the VM
suspends and yields `ReplProgressResolveFutures` with pending call IDs.

### Flow

```
1. repl.Start() executes code
2. For undefined names, VM yields ReplProgressNameLookup
   → Host calls NameLookup.Return() with {"type": "function", "name": "<name>"}
3. External call inside async context yields ReplProgressFunctionCall
   → Host calls Call.ResumePending() to track as pending future
4. VM yields ReplProgressResolveFutures with pending call IDs
   → Host can Dump() the snapshot, load it later, then Resume() with results
```

### Example

```go
// Start async code that will suspend on external calls
progress, err := repl.Start(ctx, `
import asyncio

async def main():
    result = await foo()
    return result

await main()
`)

// Drive through NameLookup yields
for {
    switch progress.Kind {
    case ReplProgressNameLookup:
        // Provide function implementations
        result, _ = progress.NameLookup.Return(ctx, map[string]any{
            "type": "function", "name": progress.NameLookup.Name,
        })
        progress = result
    case ReplProgressFunctionCall:
        // Track as pending future (not immediate return)
        result, _ = progress.Call.ResumePending(ctx)
        progress = result
    case ReplProgressResolveFutures:
        // Got pending futures to resolve
        snapshot, _ := progress.Futures.Dump(ctx)
        // ... later, load and resume ...
        loaded, _ := repl.rt.LoadSnapshot(ctx, snapshot)
        next, _ := loaded.Futures.Resume(ctx, []monty.FutureResult{
            {CallID: loaded.Futures.PendingCallIDs[0], Result: 42},
        })
        progress = next
    case ReplProgressComplete:
        // Done!
        var result int
        progress.Result.Decode(&result)
        // result = 42
    }
}
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
