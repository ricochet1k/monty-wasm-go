package monty

import (
	"context"
	"testing"
)

func TestRunComplete(t *testing.T) {
	ctx := context.Background()
	rt := newRuntime(t, ctx)

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
	rt := newRuntime(t, ctx)

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
	if progress.Kind == KindNameLookup {
		// NameLookup first, then FunctionCall
		if progress.NameLookup == nil {
			t.Fatalf("expected name lookup payload")
		}
		if progress.NameLookup.Name != "external_add" {
			t.Fatalf("expected external_add, got %q", progress.NameLookup.Name)
		}
		// Resume with a function that adds 10
		next, err := progress.NameLookup.Resume(ctx, func(args ...any) any {
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
	if progress.Kind != KindFunctionCall {
		t.Fatalf("expected function call, got %v", progress.Kind)
	}
	if progress.Call == nil {
		t.Fatalf("expected call payload")
	}
	if progress.Call.Name != "external_add" {
		t.Fatalf("expected external_add, got %q", progress.Call.Name)
	}

	var a, b int
	if err := progress.Call.Args[0].Decode(&a); err != nil {
		t.Fatalf("decode arg0: %v", err)
	}
	if err := progress.Call.Args[1].Decode(&b); err != nil {
		t.Fatalf("decode arg1: %v", err)
	}

	next, err := progress.Call.Return(ctx, a+b)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if next.Kind != KindComplete {
		t.Fatalf("expected complete, got %v", next.Kind)
	}

	var result int
	if err := next.Result.Decode(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result != (a+b)*2 {
		t.Fatalf("expected %d, got %d", (a+b)*2, result)
	}
}

func TestProgramDumpLoad(t *testing.T) {
	ctx := context.Background()
	rt := newRuntime(t, ctx)

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

func newRuntime(t *testing.T, ctx context.Context) *Runtime {
	t.Helper()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(ctx); err != nil {
			t.Fatalf("close runtime: %v", err)
		}
	})
	return rt
}
