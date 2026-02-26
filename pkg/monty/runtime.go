package monty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	_ "embed"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed wasm/monty.wasm
var wasmBinary []byte

type Runtime struct {
	rt     wazero.Runtime
	mod    api.Module
	memory api.Memory

	fnAlloc              api.Function
	fnFree               api.Function
	fnLastErrorLen       api.Function
	fnLastErrorPtr       api.Function
	fnRunNew             api.Function
	fnRunStart           api.Function
	fnSnapshotResume     api.Function
	fnFutureSnapshotRun  api.Function
	fnRunDump            api.Function
	fnRunLoad            api.Function
	fnSnapshotDump       api.Function
	fnSnapshotLoad       api.Function
	fnFutureSnapshotDump api.Function
	fnFutureSnapshotLoad api.Function
	fnRunFree            api.Function
	fnSnapshotFree       api.Function
	fnFutureSnapshotFree api.Function
	fnBlobPtr            api.Function
	fnBlobLen            api.Function
	fnBlobFree           api.Function
}

func NewRuntime(ctx context.Context) (*Runtime, error) {
	rt := wazero.NewRuntime(ctx)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("monty: instantiate WASI: %w", err)
	}

	compiled, err := rt.CompileModule(ctx, wasmBinary)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("monty: compile wasm: %w", err)
	}

	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("monty: instantiate module: %w", err)
	}

	r := &Runtime{rt: rt, mod: mod, memory: mod.Memory()}
	if err := r.cacheFunctions(); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	return r, nil
}

func (r *Runtime) Close(ctx context.Context) error {
	if r == nil || r.rt == nil {
		return nil
	}
	return r.rt.Close(ctx)
}

func (r *Runtime) Compile(ctx context.Context, code string, options CompileOptions) (*Program, error) {
	if options.ScriptName == "" {
		options.ScriptName = "script.py"
	}
	inputNames := options.InputNames
	if inputNames == nil {
		inputNames = []string{}
	}
	externalFunctions := options.ExternalFunctions
	if externalFunctions == nil {
		externalFunctions = []string{}
	}

	inputJSON, err := json.Marshal(inputNames)
	if err != nil {
		return nil, fmt.Errorf("monty: encode input names: %w", err)
	}
	extJSON, err := json.Marshal(externalFunctions)
	if err != nil {
		return nil, fmt.Errorf("monty: encode external funcs: %w", err)
	}

	codeArg, freeCode, err := r.arg(ctx, []byte(code))
	if err != nil {
		return nil, err
	}
	defer freeCode()
	scriptArg, freeScript, err := r.arg(ctx, []byte(options.ScriptName))
	if err != nil {
		return nil, err
	}
	defer freeScript()
	inputsArg, freeInputs, err := r.arg(ctx, inputJSON)
	if err != nil {
		return nil, err
	}
	defer freeInputs()
	extArg, freeExt, err := r.arg(ctx, extJSON)
	if err != nil {
		return nil, err
	}
	defer freeExt()

	runID, err := r.callID(ctx, r.fnRunNew, codeArg.ptr, codeArg.len, scriptArg.ptr, scriptArg.len, inputsArg.ptr, inputsArg.len, extArg.ptr, extArg.len)
	if err != nil {
		return nil, err
	}
	return &Program{rt: r, id: runID}, nil
}

func (r *Runtime) LoadProgram(ctx context.Context, payload []byte) (*Program, error) {
	arg, done, err := r.arg(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer done()
	runID, err := r.callID(ctx, r.fnRunLoad, arg.ptr, arg.len)
	if err != nil {
		return nil, err
	}
	return &Program{rt: r, id: runID}, nil
}

func (r *Runtime) callProgress(ctx context.Context, fn api.Function, params ...uint64) (Progress, error) {
	id, err := r.callID(ctx, fn, params...)
	if err != nil {
		return Progress{}, err
	}
	buf, err := r.readBlob(ctx, id)
	if err != nil {
		return Progress{}, err
	}
	return r.decodeProgress(buf)
}

func (r *Runtime) decodeProgress(payload []byte) (Progress, error) {
	var raw struct {
		Kind             string              `json:"kind"`
		Result           json.RawMessage     `json:"result"`
		FunctionName     string              `json:"function_name"`
		OSFunction       string              `json:"os_function"`
		Args             []json.RawMessage   `json:"args"`
		Kwargs           [][]json.RawMessage `json:"kwargs"`
		CallID           uint32              `json:"call_id"`
		MethodCall       bool                `json:"method_call"`
		SnapshotID       uint64              `json:"snapshot_id"`
		PendingCallIDs   []uint32            `json:"pending_call_ids"`
		FutureSnapshotID uint64              `json:"future_snapshot_id"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Progress{}, fmt.Errorf("monty: decode progress: %w", err)
	}

	progress := Progress{rawCallID: raw.CallID}
	switch raw.Kind {
	case "complete":
		progress.Kind = KindComplete
		progress.Result = append(Value{}, raw.Result...)
	case "function_call":
		progress.Kind = KindFunctionCall
		progress.Call = &Call{
			Name:       raw.FunctionName,
			Args:       rawToValues(raw.Args),
			Kwargs:     rawToKwargs(raw.Kwargs),
			MethodCall: raw.MethodCall,
			resume:     &snapshotResume{rt: r, snapshotID: raw.SnapshotID, callID: raw.CallID},
		}
	case "os_call":
		progress.Kind = KindOSCall
		progress.OSCall = &OSCall{
			Name:   raw.OSFunction,
			Args:   rawToValues(raw.Args),
			Kwargs: rawToKwargs(raw.Kwargs),
			resume: &snapshotResume{rt: r, snapshotID: raw.SnapshotID, callID: raw.CallID},
		}
	case "resolve_futures":
		progress.Kind = KindResolveFutures
		progress.Futures = &PendingFutures{
			PendingIDs: append([]uint32(nil), raw.PendingCallIDs...),
			rt:         r,
			snapshotID: raw.FutureSnapshotID,
		}
	default:
		return Progress{}, fmt.Errorf("monty: unknown progress kind %q", raw.Kind)
	}
	return progress, nil
}

func rawToValues(items []json.RawMessage) []Value {
	out := make([]Value, len(items))
	for i, item := range items {
		out[i] = append(Value{}, item...)
	}
	return out
}

func rawToKwargs(items [][]json.RawMessage) []KeywordArg {
	out := make([]KeywordArg, 0, len(items))
	for _, pair := range items {
		if len(pair) != 2 {
			continue
		}
		out = append(out, KeywordArg{Key: append(Value{}, pair[0]...), Value: append(Value{}, pair[1]...)})
	}
	return out
}

func (r *Runtime) readBlob(ctx context.Context, blobID uint64) ([]byte, error) {
	defer r.fnBlobFree.Call(ctx, blobID)
	ptrRes, err := r.fnBlobPtr.Call(ctx, blobID)
	if err != nil {
		return nil, fmt.Errorf("monty: blob ptr call: %w", err)
	}
	lenRes, err := r.fnBlobLen.Call(ctx, blobID)
	if err != nil {
		return nil, fmt.Errorf("monty: blob len call: %w", err)
	}
	ptr := uint32(ptrRes[0])
	length := uint32(lenRes[0])
	if length == 0 {
		return []byte{}, nil
	}
	data, ok := r.memory.Read(ptr, length)
	if !ok {
		return nil, errors.New("monty: failed reading blob memory")
	}
	return append([]byte(nil), data...), nil
}

func (r *Runtime) callID(ctx context.Context, fn api.Function, params ...uint64) (uint64, error) {
	res, err := fn.Call(ctx, params...)
	if err != nil {
		return 0, err
	}
	id := res[0]
	if id != 0 {
		return id, nil
	}
	msg, readErr := r.lastError(ctx)
	if readErr != nil {
		return 0, readErr
	}
	if msg == "" {
		msg = "unknown wasm error"
	}
	return 0, errors.New("monty: " + msg)
}

func (r *Runtime) lastError(ctx context.Context) (string, error) {
	lenRes, err := r.fnLastErrorLen.Call(ctx)
	if err != nil {
		return "", fmt.Errorf("monty: read error len: %w", err)
	}
	length := uint32(lenRes[0])
	if length == 0 {
		return "", nil
	}
	ptrRes, err := r.fnLastErrorPtr.Call(ctx)
	if err != nil {
		return "", fmt.Errorf("monty: read error ptr: %w", err)
	}
	ptr := uint32(ptrRes[0])
	buf, ok := r.memory.Read(ptr, length)
	if !ok {
		return "", errors.New("monty: failed reading error bytes")
	}
	return string(buf), nil
}

type wasmArg struct {
	ptr uint64
	len uint64
}

func (r *Runtime) arg(ctx context.Context, data []byte) (wasmArg, func(), error) {
	if len(data) == 0 {
		return wasmArg{}, func() {}, nil
	}
	res, err := r.fnAlloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return wasmArg{}, nil, fmt.Errorf("monty: alloc: %w", err)
	}
	ptr := uint32(res[0])
	if ptr == 0 {
		return wasmArg{}, nil, errors.New("monty: alloc returned zero pointer")
	}
	if ok := r.memory.Write(ptr, data); !ok {
		r.fnFree.Call(ctx, uint64(ptr), uint64(len(data)))
		return wasmArg{}, nil, errors.New("monty: failed writing arg bytes")
	}
	cleanup := func() {
		r.fnFree.Call(ctx, uint64(ptr), uint64(len(data)))
	}
	return wasmArg{ptr: uint64(ptr), len: uint64(len(data))}, cleanup, nil
}

func (r *Runtime) cacheFunctions() error {
	required := map[string]*api.Function{
		"monty_alloc":                  &r.fnAlloc,
		"monty_free":                   &r.fnFree,
		"monty_last_error_len":         &r.fnLastErrorLen,
		"monty_last_error_ptr":         &r.fnLastErrorPtr,
		"monty_run_new":                &r.fnRunNew,
		"monty_run_start":              &r.fnRunStart,
		"monty_snapshot_resume":        &r.fnSnapshotResume,
		"monty_future_snapshot_resume": &r.fnFutureSnapshotRun,
		"monty_run_dump":               &r.fnRunDump,
		"monty_run_load":               &r.fnRunLoad,
		"monty_snapshot_dump":          &r.fnSnapshotDump,
		"monty_snapshot_load":          &r.fnSnapshotLoad,
		"monty_future_snapshot_dump":   &r.fnFutureSnapshotDump,
		"monty_future_snapshot_load":   &r.fnFutureSnapshotLoad,
		"monty_run_free":               &r.fnRunFree,
		"monty_snapshot_free":          &r.fnSnapshotFree,
		"monty_future_snapshot_free":   &r.fnFutureSnapshotFree,
		"monty_blob_ptr":               &r.fnBlobPtr,
		"monty_blob_len":               &r.fnBlobLen,
		"monty_blob_free":              &r.fnBlobFree,
	}
	for name, dest := range required {
		fn := r.mod.ExportedFunction(name)
		if fn == nil {
			return fmt.Errorf("monty: missing wasm export %s", name)
		}
		*dest = fn
	}
	return nil
}
