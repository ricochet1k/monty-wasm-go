SHELL := /bin/sh

WASM_CRATE := rust/monty-wasm
WASM_TARGET := wasm32-wasip1
WASM_BUILD := $(WASM_CRATE)/target/$(WASM_TARGET)/release/monty_wasm.wasm
WASM_EMBED := pkg/monty/wasm/monty.wasm

.PHONY: wasm test fmt clean

wasm:
	rustup target add $(WASM_TARGET)
	cargo build --release --target $(WASM_TARGET) --manifest-path $(WASM_CRATE)/Cargo.toml
	mkdir -p $(dir $(WASM_EMBED))
	cp $(WASM_BUILD) $(WASM_EMBED)

test:
	go test ./...

fmt:
	cargo fmt --manifest-path $(WASM_CRATE)/Cargo.toml
	go fmt ./...

clean:
	rm -rf $(WASM_CRATE)/target
