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
	fnNameLookupDump     api.Function
	fnNameLookupLoad     api.Function
	fnNameLookupFree     api.Function
	fnBlobPtr            api.Function
	fnBlobLen            api.Function
	fnBlobFree           api.Function
	fnBlobStore          api.Function

	// REPL functions
	fnReplCheckContinuation api.Function
	fnReplNew               api.Function
	fnReplStart             api.Function
	fnReplResume            api.Function
	fnReplFeed              api.Function
	fnReplFeedProgress      api.Function
	fnReplDump              api.Function
	fnReplLoad              api.Function
	fnReplFree              api.Function
	fnReplProgressFree      api.Function
	fnReplSnapshotDump      api.Function
	fnReplSnapshotLoad      api.Function
	fnReplSnapshotFree      api.Function
	fnReplSnapshotLoadInfo  api.Function
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

	inputJSON, err := json.Marshal(inputNames)
	if err != nil {
		return nil, fmt.Errorf("monty: encode input names: %w", err)
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

	runID, err := r.callID(ctx, r.fnRunNew, codeArg.ptr, codeArg.len, scriptArg.ptr, scriptArg.len, inputsArg.ptr, inputsArg.len)
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
		return nil, err
	}
	buf, err := r.readBlob(ctx, id)
	if err != nil {
		return nil, err
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
		Name             string              `json:"name"`
		Location         *CallLocation       `json:"location"`
		ReplID           uint64              `json:"repl_id"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("monty: decode progress: %w", err)
	}

	switch raw.Kind {
	case "complete":
		return &ProgressComplete{Result: append(Value{}, raw.Result...)}, nil
	case "function_call":
		return &Call{
			Name:         raw.FunctionName,
			Args:         rawToValues(raw.Args),
			Kwargs:       rawToKwargs(raw.Kwargs),
			MethodCall:   raw.MethodCall,
			Location:     raw.Location,
			snapshotType: "function_call",
			resume:       &snapshotResume{rt: r, snapshotID: raw.SnapshotID, callID: raw.CallID, snapshotType: "function_call"},
		}, nil
	case "os_call":
		return &OSCall{
			Name:         raw.OSFunction,
			Args:         rawToValues(raw.Args),
			Kwargs:       rawToKwargs(raw.Kwargs),
			Location:     raw.Location,
			snapshotType: "os_call",
			resume:       &snapshotResume{rt: r, snapshotID: raw.SnapshotID, callID: raw.CallID, snapshotType: "os_call"},
		}, nil
	case "resolve_futures":
		return &PendingFutures{
			PendingIDs: append([]uint32(nil), raw.PendingCallIDs...),
			rt:         r,
			snapshotID: raw.FutureSnapshotID,
		}, nil
	case "name_lookup":
		return &NameLookup{
			Name:       raw.Name,
			snapshotID: raw.SnapshotID,
			rt:         r,
		}, nil
	default:
		return nil, fmt.Errorf("monty: unknown progress kind %q", raw.Kind)
	}
}

// decodeReplProgress decodes REPL progress from JSON.
func (r *Runtime) decodeReplProgress(payload []byte) (ReplProgress, error) {
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
		Name             string              `json:"name"`
		Location         *CallLocation       `json:"location"`
		ReplID           uint64              `json:"repl_id"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("monty: decode repl progress: %w", err)
	}

	switch raw.Kind {
	case "complete":
		return &ReplSnippetComplete{
			Repl:   &Repl{rt: r, id: raw.ReplID},
			Result: append(Value{}, raw.Result...),
		}, nil
	case "function_call":
		return &ReplFunctionCall{
			FunctionName: raw.FunctionName,
			Args:         rawToValues(raw.Args),
			Kwargs:       rawToKwargs(raw.Kwargs),
			CallID:       raw.CallID,
			MethodCall:   raw.MethodCall,
			Location:     raw.Location,
			snapshotID:   raw.SnapshotID,
			rt:           r,
		}, nil
	case "os_call":
		return &ReplOsCall{
			OSFunction: raw.OSFunction,
			Args:       rawToValues(raw.Args),
			Kwargs:     rawToKwargs(raw.Kwargs),
			CallID:     raw.CallID,
			Location:   raw.Location,
			snapshotID: raw.SnapshotID,
			rt:         r,
		}, nil
	case "resolve_futures":
		return &ReplResolveFutures{
			PendingCallIDs: append([]uint32(nil), raw.PendingCallIDs...),
			snapshotID:     raw.FutureSnapshotID,
			rt:             r,
		}, nil
	case "name_lookup":
		return &ReplNameLookup{
			Name:       raw.Name,
			snapshotID: raw.SnapshotID,
			rt:         r,
		}, nil
	default:
		return nil, fmt.Errorf("monty: unknown repl progress kind %q", raw.Kind)
	}
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
		"monty_name_lookup_dump":       &r.fnNameLookupDump,
		"monty_name_lookup_load":       &r.fnNameLookupLoad,
		"monty_name_lookup_free":       &r.fnNameLookupFree,
		"monty_blob_ptr":               &r.fnBlobPtr,
		"monty_blob_len":               &r.fnBlobLen,
		"monty_blob_free":              &r.fnBlobFree,
		"monty_blob_store":             &r.fnBlobStore,
		// REPL functions
		"monty_repl_check_continuation": &r.fnReplCheckContinuation,
		"monty_repl_new":                &r.fnReplNew,
		"monty_repl_start":              &r.fnReplStart,
		"monty_repl_resume":             &r.fnReplResume,
		"monty_repl_feed":               &r.fnReplFeed,
		"monty_repl_feed_progress":      &r.fnReplFeedProgress,
		"monty_repl_dump":               &r.fnReplDump,
		"monty_repl_load":               &r.fnReplLoad,
		"monty_repl_free":               &r.fnReplFree,
		"monty_repl_progress_free":      &r.fnReplProgressFree,
		"monty_repl_snapshot_dump":      &r.fnReplSnapshotDump,
		"monty_repl_snapshot_load":      &r.fnReplSnapshotLoad,
		"monty_repl_snapshot_free":      &r.fnReplSnapshotFree,
		"monty_repl_snapshot_load_info": &r.fnReplSnapshotLoadInfo,
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

// loadSnapshot deserializes bytes into a REPL snapshot and returns the snapshot ID.
func (r *Runtime) loadSnapshot(ctx context.Context, data []byte) (uint64, error) {
	arg, done, err := r.arg(ctx, data)
	if err != nil {
		return 0, err
	}
	defer done()
	id, err := r.callUint64(r.fnReplSnapshotLoad, arg.ptr, arg.len)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		msg, _ := r.lastError(ctx)
		return 0, fmt.Errorf("monty: load snapshot: %s", msg)
	}
	return id, nil
}

// LoadSnapshot deserializes bytes to get the kind and snapshot ID,
// then loads the snapshot and returns a ReplProgress with the loaded snapshot ID.
func (r *Runtime) LoadSnapshot(ctx context.Context, data []byte) (ReplProgress, error) {
	// Store the data as a blob
	dataBlobID, err := r.storeBlob(ctx, data)
	if err != nil {
		return nil, err
	}
	defer r.fnBlobFree.Call(context.Background(), dataBlobID)

	// Get the blob pointer and length
	ptrRes, err := r.fnBlobPtr.Call(context.Background(), dataBlobID)
	if err != nil {
		return nil, err
	}
	lenRes, err := r.fnBlobLen.Call(context.Background(), dataBlobID)
	if err != nil {
		return nil, err
	}

	// Call load info with pointer and length to get the JSON info blob
	infoBlobID, err := r.callID(ctx, r.fnReplSnapshotLoadInfo, uint64(ptrRes[0]), uint64(lenRes[0]))
	if err != nil {
		return nil, err
	}

	// Read the JSON info from the blob
	infoBlob, err := r.readBlob(ctx, infoBlobID)
	if err != nil {
		return nil, err
	}

	// Parse the JSON to get kind and snapshot_id
	var info struct {
		Kind       string          `json:"kind"`
		SnapshotID uint64          `json:"snapshot_id"`
		Extra      json.RawMessage `json:"extra,omitempty"`
	}
	if err := json.Unmarshal(infoBlob, &info); err != nil {
		return nil, err
	}

	// Load the actual snapshot
	snapshotID, err := r.loadSnapshot(ctx, data)
	if err != nil {
		return nil, err
	}

	// Build the ReplProgress based on kind
	switch info.Kind {
	case "function_call":
		return &ReplFunctionCall{snapshotID: snapshotID, rt: r}, nil
	case "os_call":
		return &ReplOsCall{snapshotID: snapshotID, rt: r}, nil
	case "name_lookup":
		return &ReplNameLookup{snapshotID: snapshotID, rt: r}, nil
	case "resolve_futures":
		// Parse pending_call_ids from extra field
		var pendingCallIDs []uint32
		if len(info.Extra) > 0 {
			var extra struct {
				PendingCallIDs []uint32 `json:"pending_call_ids"`
			}
			if err := json.Unmarshal(info.Extra, &extra); err == nil {
				pendingCallIDs = extra.PendingCallIDs
			}
		}
		return &ReplResolveFutures{
			PendingCallIDs: pendingCallIDs,
			snapshotID:     snapshotID,
			rt:             r,
		}, nil
	case "complete":
		return &ReplSnippetComplete{}, nil
	default:
		return nil, fmt.Errorf("monty: unknown snapshot kind %q", info.Kind)
	}
}

// kindFromString converts a string kind to ReplProgressKind
func (r *Runtime) kindFromString(kind string) ReplProgressKind {
	switch kind {
	case "complete":
		return ReplKindComplete
	case "function_call":
		return ReplKindFunctionCall
	case "os_call":
		return ReplKindOsCall
	case "name_lookup":
		return ReplKindNameLookup
	case "resolve_futures":
		return ReplKindResolveFutures
	default:
		return ReplKindComplete
	}
}

// storeBlob stores bytes as a blob and returns the blob ID.
func (r *Runtime) storeBlob(ctx context.Context, data []byte) (uint64, error) {
	arg, done, err := r.arg(ctx, data)
	if err != nil {
		return 0, err
	}
	defer done()
	id, err := r.callUint64(r.fnBlobStore, arg.ptr, arg.len)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// callUint64 calls a function and returns the result as uint64.
func (r *Runtime) callUint64(fn api.Function, params ...uint64) (uint64, error) {
	res, err := fn.Call(context.Background(), params...)
	if err != nil {
		return 0, err
	}
	return res[0], nil
}
