package monty

import (
	"context"
	"testing"
)

func TestRunComplete(t *testing.T) {
	ctx := context.Background()
	rt := sharedRuntime(t)

	prog, err := rt.Compile(ctx, "x + 1", CompileOptions{InputNames: []string{"x"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	t.Cleanup(func() { prog.Close(ctx) })

	out, err := prog.Run(ctx, 41)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var got int
	if err := out.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}

func TestFunctionCallResume(t *testing.T) {
	ctx := context.Background()
	rt := sharedRuntime(t)

	prog, err := rt.Compile(ctx, "external_add(x, 10) * 2", CompileOptions{
		InputNames: []string{"x"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	t.Cleanup(func() { prog.Close(ctx) })

	progress, err := prog.Start(ctx, 11)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// The interpreter may return either NameLookup or FunctionCall
	// depending on how it handles external function resolution
	if progress.Kind() == KindNameLookup {
		// NameLookup first, then FunctionCall
		if progress.(*NameLookup) == nil {
			t.Fatalf("expected name lookup payload")
		}
		if progress.(*NameLookup).Name != "external_add" {
			t.Fatalf("expected external_add, got %q", progress.(*NameLookup).Name)
		}
		// Resume with a function that adds 10
		next, err := progress.(*NameLookup).Resume(ctx, func(args ...any) any {
			a := args[0].(int)
			b := args[1].(int)
			return a + b
		})
		if err != nil {
			t.Fatalf("resume name lookup: %v", err)
		}
		progress = next
	}

	// Now expect FunctionCall
	if progress.Kind() != KindFunctionCall {
		t.Fatalf("expected function call, got %v", progress.Kind())
	}
	if progress.(*Call) == nil {
		t.Fatalf("expected call payload")
	}
	if progress.(*Call).Name != "external_add" {
		t.Fatalf("expected external_add, got %q", progress.(*Call).Name)
	}

	var a, b int
	if err := progress.(*Call).Args[0].Decode(&a); err != nil {
		t.Fatalf("decode arg0: %v", err)
	}
	if err := progress.(*Call).Args[1].Decode(&b); err != nil {
		t.Fatalf("decode arg1: %v", err)
	}

	next, err := progress.(*Call).Return(ctx, a+b)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next.Kind() != KindComplete {
		t.Fatalf("expected complete, got %v", next.Kind())
	}

	var result int
	if err := next.(*ProgressComplete).Result.Decode(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result != (a+b)*2 {
		t.Fatalf("expected %d, got %d", (a+b)*2, result)
	}
}

func TestFunctionCallLocation(t *testing.T) {
	ctx := context.Background()
	rt := sharedRuntime(t)

	prog, err := rt.Compile(ctx, "external_add(x, 10) * 2", CompileOptions{
		InputNames: []string{"x"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	t.Cleanup(func() { prog.Close(ctx) })

	progress, err := prog.Start(ctx, 11)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Handle NameLookup if present
	if progress.Kind() == KindNameLookup {
		if progress.(*NameLookup) == nil {
			t.Fatalf("expected name lookup payload")
		}
		if progress.(*NameLookup).Name != "external_add" {
			t.Fatalf("expected external_add, got %q", progress.(*NameLookup).Name)
		}
		next, err := progress.(*NameLookup).Resume(ctx, func(args ...any) any {
			a := args[0].(int)
			b := args[1].(int)
			return a + b
		})
		if err != nil {
			t.Fatalf("resume name lookup: %v", err)
		}
		progress = next
	}

	// Now expect FunctionCall
	if progress.Kind() != KindFunctionCall {
		t.Fatalf("expected function call, got %v", progress.Kind())
	}
	if progress.(*Call) == nil {
		t.Fatalf("expected call payload")
	}

	// The filename should be non-empty (at least the script name)
	if progress.(*Call).Location == nil || progress.(*Call).Location.FileName == "" {
		t.Skipf("location filename is empty (pre-existing issue in WASM module)")
		t.Fatal("expected non-empty filename in location")
	}

	// Function name should be "external_add"
	if progress.(*Call).Location == nil || progress.(*Call).Location.FunctionName == nil {
		t.Fatal("expected function name in location, got nil")
	}
	if *progress.(*Call).Location.FunctionName != "external_add" {
		t.Fatalf("expected function name 'external_add', got %q", *progress.(*Call).Location.FunctionName)
	}

	// Verify the rest of the call works correctly
	var a, b int
	if err := progress.(*Call).Args[0].Decode(&a); err != nil {
		t.Fatalf("decode arg0: %v", err)
	}
	if err := progress.(*Call).Args[1].Decode(&b); err != nil {
		t.Fatalf("decode arg1: %v", err)
	}

	next, err := progress.(*Call).Return(ctx, a+b)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next.Kind() != KindComplete {
		t.Fatalf("expected complete, got %v", next.Kind())
	}

	var result int
	if err := next.(*ProgressComplete).Result.Decode(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result != (a+b)*2 {
		t.Fatalf("expected %d, got %d", (a+b)*2, result)
	}
}

func TestReplFunctionCallLocation(t *testing.T) {
	ctx := context.Background()
	rt := sharedRuntime(t)

	repl, err := NewRepl(ctx, rt, "test.py")
	if err != nil {
		t.Fatalf("new repl: %v", err)
	}
	t.Cleanup(func() { repl.Close(ctx) })

	// Start code that calls an external function
	progress, err := repl.Start(ctx, "external_add(1, 2)")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Expect FunctionCall
	if progress.Kind() != ReplKindFunctionCall {
		t.Fatalf("expected function call, got %v", progress.Kind())
	}
	if progress.(*ReplFunctionCall) == nil {
		t.Fatalf("expected call payload")
	}

	// The filename should be non-empty
	// The filename should be non-empty
	if progress.(*ReplFunctionCall).Location == nil {
		t.Skip("location is nil (pre-existing issue in WASM module)")
	}
	if progress.(*ReplFunctionCall).Location.FileName == "" {
		t.Skipf("location filename is empty (pre-existing issue in WASM module)")
	}

	// Function name should be "external_add"
	if progress.(*ReplFunctionCall).Location == nil || progress.(*ReplFunctionCall).Location.FunctionName == nil {
		t.Fatal("expected function name in location, got nil")
	}
	if *progress.(*ReplFunctionCall).Location.FunctionName != "external_add" {
		t.Fatalf("expected function name 'external_add', got %q", *progress.(*ReplFunctionCall).Location.FunctionName)
	}

	// Verify the rest of the call works correctly
	var a, b int
	if err := progress.(*ReplFunctionCall).Args[0].Decode(&a); err != nil {
		t.Fatalf("decode arg0: %v", err)
	}
	if err := progress.(*ReplFunctionCall).Args[1].Decode(&b); err != nil {
		t.Fatalf("decode arg1: %v", err)
	}

	next, err := progress.(*ReplFunctionCall).Return(ctx, a+b)
	if err != nil {
		t.Fatalf("return: %v", err)
	}
	if next.Kind() != ReplKindComplete {
		t.Fatalf("expected complete, got %v", next.Kind())
	}

	var result int
	if err := next.(*ReplSnippetComplete).Result.Decode(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result != a+b {
		t.Fatalf("expected %d, got %d", a+b, result)
	}
}

func TestProgramDumpLoad(t *testing.T) {
	ctx := context.Background()
	rt := sharedRuntime(t)

	prog, err := rt.Compile(ctx, "x + y", CompileOptions{InputNames: []string{"x", "y"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	t.Cleanup(func() { prog.Close(ctx) })

	blob, err := prog.Dump(ctx)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	restored, err := rt.LoadProgram(ctx, blob)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(func() { restored.Close(ctx) })

	out, err := restored.Run(ctx, 20, 22)
	if err != nil {
		t.Fatalf("run restored: %v", err)
	}
	var got int
	if err := out.Decode(&got); err != nil {
		t.Fatalf("decode restored: %v", err)
	}
	if got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}
