package monty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero/api"
)

// ============================================================
// REPL types
// ============================================================

// ReplContinuationMode indicates whether code is complete or needs more input.
type ReplContinuationMode int

const (
	ReplComplete ReplContinuationMode = iota
	ReplIncompleteImplicit
	ReplIncompleteBlock
)

// ReplProgressKind indicates the type of REPL progress.
type ReplProgressKind int

const (
	ReplKindComplete ReplProgressKind = iota
	ReplKindFunctionCall
	ReplKindOsCall
	ReplKindNameLookup
	ReplKindResolveFutures
)

// ReplProgress represents the result of a REPL execution.
// Use a type switch or type assertion to access the specific variant:
//
//	switch p := repl.Start(...) (type) {
//	case *ReplSnippetComplete:
//	case *ReplFunctionCall:
//	case *ReplOsCall:
//	case *ReplNameLookup:
//	case *ReplResolveFutures:
//	}
type ReplProgress interface {
	Kind() ReplProgressKind
	replProgressPrivate()
}

// ReplSnippetComplete represents the result when a REPL snippet completes.
type ReplSnippetComplete struct {
	Repl   *Repl
	Result Value
}

func (s *ReplSnippetComplete) Kind() ReplProgressKind { return ReplKindComplete }
func (s *ReplSnippetComplete) replProgressPrivate()   {}

// Resume resumes execution with a result.
//
// For ReplSnippetComplete, this is a no-op and returns the progress unchanged.
// For other kinds, it resumes the suspended operation with the given result.
// The result can be a string, []byte, or any value that can be JSON-encoded.
//
// The returned ReplProgress represents the next state of execution.
func (s *ReplSnippetComplete) Resume(ctx context.Context, rt *Runtime, result any) (ReplProgress, error) {
	return s, nil
}

// ReplFunctionCall represents a suspended external function call.
type ReplFunctionCall struct {
	FunctionName string
	Args         []Value
	Kwargs       []KeywordArg
	CallID       uint32
	MethodCall   bool
	Location     *CallLocation
	snapshotID   uint64
	rt           *Runtime
}

func (c *ReplFunctionCall) Kind() ReplProgressKind { return ReplKindFunctionCall }
func (c *ReplFunctionCall) replProgressPrivate()   {}

// Dump serializes the function call snapshot.
func (c *ReplFunctionCall) Dump(ctx context.Context) ([]byte, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return nil, errors.New("monty: function call not resumable")
	}
	blobID, err := c.rt.callID(ctx, c.rt.fnReplSnapshotDump, c.snapshotID)
	if err != nil {
		return nil, err
	}
	return c.rt.readBlob(ctx, blobID)
}

// Close releases the function call snapshot.
func (c *ReplFunctionCall) Close(ctx context.Context) {
	if c != nil && c.rt != nil && c.snapshotID != 0 {
		c.rt.fnReplSnapshotFree.Call(ctx, c.snapshotID)
		c.snapshotID = 0
	}
}

// Return resumes the function call with a return value.
func (c *ReplFunctionCall) Return(ctx context.Context, result any) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return nil, errors.New("monty: function call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":  "return",
		"value": result,
	})
	if err != nil {
		return nil, fmt.Errorf("monty: encode function call result: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 0, arg.ptr, arg.len)
}

// Throw resumes the function call with an error.
func (c *ReplFunctionCall) Throw(ctx context.Context, message string) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return nil, errors.New("monty: function call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":    "error",
		"message": message,
	})
	if err != nil {
		return nil, fmt.Errorf("monty: encode function call error: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 0, arg.ptr, arg.len)
}

// ResumePending resumes execution by pushing an ExternalFuture for async resolution.
//
// This is used when a function call occurs inside an async context (e.g., inside
// asyncio.gather()) and the result should be tracked as a pending future rather
// than returned immediately. The call_id is available in the ReplResolveFutures
// progress that follows.
func (c *ReplFunctionCall) ResumePending(ctx context.Context) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return nil, errors.New("monty: function call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type": "future",
	})
	if err != nil {
		return nil, fmt.Errorf("monty: encode function call future: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 0, arg.ptr, arg.len)
}

// ReplOsCall represents a suspended OS call.
type ReplOsCall struct {
	OSFunction string
	Args       []Value
	Kwargs     []KeywordArg
	CallID     uint32
	Location   *CallLocation
	snapshotID uint64
	rt         *Runtime
}

func (c *ReplOsCall) Kind() ReplProgressKind { return ReplKindOsCall }
func (c *ReplOsCall) replProgressPrivate()   {}

// Dump serializes the OS call snapshot.
func (c *ReplOsCall) Dump(ctx context.Context) ([]byte, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return nil, errors.New("monty: OS call not resumable")
	}
	blobID, err := c.rt.callID(ctx, c.rt.fnReplSnapshotDump, c.snapshotID)
	if err != nil {
		return nil, err
	}
	return c.rt.readBlob(ctx, blobID)
}

// Close releases the OS call snapshot.
func (c *ReplOsCall) Close(ctx context.Context) {
	if c != nil && c.rt != nil && c.snapshotID != 0 {
		c.rt.fnReplSnapshotFree.Call(ctx, c.snapshotID)
		c.snapshotID = 0
	}
}

// Return resumes the OS call with a return value.
func (c *ReplOsCall) Return(ctx context.Context, result any) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return nil, errors.New("monty: OS call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":  "return",
		"value": result,
	})
	if err != nil {
		return nil, fmt.Errorf("monty: encode OS call result: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 1, arg.ptr, arg.len)
}

// Throw resumes the OS call with an error.
func (c *ReplOsCall) Throw(ctx context.Context, message string) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return nil, errors.New("monty: OS call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":    "error",
		"message": message,
	})
	if err != nil {
		return nil, fmt.Errorf("monty: encode OS call error: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 1, arg.ptr, arg.len)
}

// Defer resumes the OS call as a future.
func (c *ReplOsCall) Defer(ctx context.Context) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return nil, errors.New("monty: OS call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":    "future",
		"call_id": c.CallID,
	})
	if err != nil {
		return nil, fmt.Errorf("monty: encode OS call future: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 1, arg.ptr, arg.len)
}

// ReplNameLookup represents a suspended name lookup.
type ReplNameLookup struct {
	Name          string
	NamespaceSlot uint16
	IsGlobal      bool
	snapshotID    uint64
	rt            *Runtime
}

func (n *ReplNameLookup) Kind() ReplProgressKind { return ReplKindNameLookup }
func (n *ReplNameLookup) replProgressPrivate()   {}

// Dump serializes the name lookup snapshot.
func (n *ReplNameLookup) Dump(ctx context.Context) ([]byte, error) {
	if n == nil || n.snapshotID == 0 || n.rt == nil {
		return nil, errors.New("monty: name lookup not resumable")
	}
	blobID, err := n.rt.callID(ctx, n.rt.fnReplSnapshotDump, n.snapshotID)
	if err != nil {
		return nil, err
	}
	return n.rt.readBlob(ctx, blobID)
}

// Close releases the name lookup snapshot.
func (n *ReplNameLookup) Close(ctx context.Context) {
	if n != nil && n.rt != nil && n.snapshotID != 0 {
		n.rt.fnReplSnapshotFree.Call(ctx, n.snapshotID)
		n.snapshotID = 0
	}
}

// Return resumes the name lookup with a value.
func (n *ReplNameLookup) Return(ctx context.Context, value any) (ReplProgress, error) {
	if n == nil || n.snapshotID == 0 || n.rt == nil {
		return nil, errors.New("monty: name lookup not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":  "value",
		"value": value,
	})
	if err != nil {
		return nil, fmt.Errorf("monty: encode name lookup result: %w", err)
	}
	arg, done, err := n.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return n.rt.callReplProgress(ctx, n.rt.fnReplResume, n.snapshotID, 2, arg.ptr, arg.len)
}

// Undefined resumes the name lookup as undefined.
func (n *ReplNameLookup) Undefined(ctx context.Context) (ReplProgress, error) {
	if n == nil || n.snapshotID == 0 || n.rt == nil {
		return nil, errors.New("monty: name lookup not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type": "undefined",
	})
	if err != nil {
		return nil, fmt.Errorf("monty: encode name lookup undefined: %w", err)
	}
	arg, done, err := n.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return n.rt.callReplProgress(ctx, n.rt.fnReplResume, n.snapshotID, 2, arg.ptr, arg.len)
}

// ReplResolveFutures represents suspended async futures.
//
// This progress type is returned when the VM encounters external function calls
// inside an async context (e.g., inside asyncio.gather()). The PendingCallIDs
// contain the call IDs that need to be resolved before execution can continue.
//
// To resolve the futures:
//  1. Call Dump() to serialize the snapshot for later loading
//  2. Provide results for each call_id via Resume()
//  3. The VM will continue execution with the resolved values
//
// See ReplFunctionCall.ResumePending() for how to track function calls as pending futures.
type ReplResolveFutures struct {
	PendingCallIDs []uint32
	snapshotID     uint64
	rt             *Runtime
}

func (f *ReplResolveFutures) Kind() ReplProgressKind { return ReplKindResolveFutures }
func (f *ReplResolveFutures) replProgressPrivate()   {}

// Resume resolves the futures by providing results for each pending call.
//
// The results slice must contain one entry for each call_id in PendingCallIDs.
// Each entry can either have a Result (for successful completion) or an Err
// (for failures). The order of entries doesn't matter - they are matched by
// call_id.
//
// After all futures are resolved, execution continues and returns the next
// progress state (typically Complete).
func (f *ReplResolveFutures) Resume(ctx context.Context, results []FutureResult) (ReplProgress, error) {
	if f == nil || f.snapshotID == 0 || f.rt == nil {
		return nil, errors.New("monty: futures not resumable")
	}
	payload := make([]map[string]any, 0, len(results))
	for _, entry := range results {
		item := map[string]any{"call_id": entry.CallID}
		if entry.Err != "" {
			item["error"] = entry.Err
		} else if entry.Result != nil {
			item["result"] = entry.Result
		}
		payload = append(payload, item)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("monty: encode future results: %w", err)
	}
	arg, done, err := f.rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	return f.rt.callReplProgress(ctx, f.rt.fnReplResume, f.snapshotID, 3, arg.ptr, arg.len)
}

// Dump serializes the futures snapshot.
func (f *ReplResolveFutures) Dump(ctx context.Context) ([]byte, error) {
	if f == nil || f.snapshotID == 0 || f.rt == nil {
		return nil, errors.New("monty: closed future snapshot")
	}
	blobID, err := f.rt.callID(ctx, f.rt.fnReplSnapshotDump, f.snapshotID)
	if err != nil {
		return nil, err
	}
	return f.rt.readBlob(ctx, blobID)
}

// Close releases the futures snapshot.
func (f *ReplResolveFutures) Close(ctx context.Context) {
	if f != nil && f.rt != nil && f.snapshotID != 0 {
		f.rt.fnReplSnapshotFree.Call(ctx, f.snapshotID)
		f.snapshotID = 0
	}
}

// ReplStartError represents a Python exception raised during Repl.Start().
type ReplStartError struct {
	Message string
	ReplID  uint64
}

func (e *ReplStartError) Error() string {
	return e.Message
}

// ============================================================
// REPL type
// ============================================================

// Repl provides a stateful incremental REPL with async suspension support.
type Repl struct {
	rt *Runtime
	id uint64
}

// NewRepl creates a new REPL session.
func NewRepl(ctx context.Context, rt *Runtime, scriptName string) (*Repl, error) {
	if scriptName == "" {
		scriptName = "repl.py"
	}
	scriptArg, freeScript, err := rt.arg(ctx, []byte(scriptName))
	if err != nil {
		return nil, err
	}
	defer freeScript()
	id, err := rt.callID(ctx, rt.fnReplNew, scriptArg.ptr, scriptArg.len)
	if err != nil {
		return nil, err
	}
	return &Repl{rt: rt, id: id}, nil
}

// Start executes code with suspension support.
//
// Start runs the provided code and returns a ReplProgress that represents the
// result. If the code completes without suspending, the returned progress will
// have Kind == ReplProgressComplete with the result in the Result field.
//
// If the code suspends (e.g., due to an external function call, OS call, or
// name lookup), the returned progress will have a non-nil field indicating the
// type of suspension. The caller can resume execution by:
//
//   - Calling ReplProgress.Resume() with a result value
//   - Calling the specific suspension type's method (e.g., ReplFunctionCall.Return())
//
// After calling Start, the Repl is consumed and cannot be used for further
// Start calls. To continue executing code, use the returned ReplProgress or
// create a new REPL.
func (r *Repl) Start(ctx context.Context, code string, inputs ...any) (ReplProgress, error) {
	if r == nil || r.rt == nil || r.id == 0 {
		return nil, errors.New("monty: repl already consumed by Start — call Start only once per REPL; to continue, use the returned ReplProgress or create a new REPL")
	}
	// Encode code string
	codeArg, doneCode, err := r.rt.arg(ctx, []byte(code))
	if err != nil {
		return nil, err
	}
	defer doneCode()
	var encoded []byte
	if inputs != nil {
		var err error
		encoded, err = json.Marshal(inputs)
		if err != nil {
			return nil, fmt.Errorf("monty: encode inputs: %w", err)
		}
	}
	inputArg, done, err := r.rt.arg(ctx, encoded)
	if err != nil {
		return nil, err
	}
	defer done()
	// Use empty input names (positional inputs)
	inputNamesJSON := "[]"
	namesArg, doneNames, err := r.rt.arg(ctx, []byte(inputNamesJSON))
	if err != nil {
		return nil, err
	}
	defer doneNames()
	id, err := r.rt.callID(ctx, r.rt.fnReplStart, r.id, codeArg.ptr, codeArg.len, namesArg.ptr, namesArg.len, inputArg.ptr, inputArg.len)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		// Check for ReplStartError - the repl_id is stored in last_error
		msg, readErr := r.rt.lastError(ctx)
		if readErr != nil {
			return nil, readErr
		}
		if msg == "" {
			msg = "unknown error"
		}
		// Try to extract new repl_id from the error message (format: "error (repl_id=123)")
		newReplID := r.id
		if idx := strings.LastIndex(msg, "repl_id="); idx != -1 {
			if endIdx := strings.Index(msg[idx+8:], ")"); endIdx != -1 {
				if idStr := msg[idx+8 : idx+8+endIdx]; idStr != "" {
					if parsed, err := strconv.ParseUint(idStr, 10, 64); err == nil {
						newReplID = parsed
					}
				}
			}
		}
		return nil, &ReplStartError{Message: msg, ReplID: newReplID}
	}
	r.id = 0
	return r.rt.DecodeReplProgressFromBlob(ctx, id)
}

// Feed executes code with suspension support.
func (r *Repl) Feed(ctx context.Context, code string) (ReplProgress, error) {
	if r == nil || r.rt == nil || r.id == 0 {
		return nil, errors.New("monty: closed repl")
	}
	codeArg, done, err := r.rt.arg(ctx, []byte(code))
	if err != nil {
		return nil, err
	}
	defer done()
	id, err := r.rt.callID(ctx, r.rt.fnReplFeedProgress, r.id, codeArg.ptr, codeArg.len)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		// Check for ReplStartError - the repl_id is stored in last_error
		msg, readErr := r.rt.lastError(ctx)
		if readErr != nil {
			return nil, readErr
		}
		if msg == "" {
			msg = "unknown error"
		}
		// Try to extract new repl_id from the error message (format: "error (repl_id=123)")
		newReplID := r.id
		if idx := strings.LastIndex(msg, "repl_id="); idx != -1 {
			if endIdx := strings.Index(msg[idx+8:], ")"); endIdx != -1 {
				if idStr := msg[idx+8 : idx+8+endIdx]; idStr != "" {
					if parsed, err := strconv.ParseUint(idStr, 10, 64); err == nil {
						newReplID = parsed
					}
				}
			}
		}
		return nil, &ReplStartError{Message: msg, ReplID: newReplID}
	}
	r.id = 0
	return r.rt.DecodeReplProgressFromBlob(ctx, id)
}

// CheckContinuation checks if code is complete or needs more input.
func (r *Repl) CheckContinuation(code string) ReplContinuationMode {
	if r == nil || r.rt == nil {
		return ReplComplete
	}
	codeArg, done, err := r.rt.arg(context.Background(), []byte(code))
	if err != nil {
		return ReplComplete
	}
	defer done()
	mode, err := r.rt.callUint64(r.rt.fnReplCheckContinuation, codeArg.ptr, codeArg.len)
	if err != nil {
		return ReplComplete
	}
	return ReplContinuationMode(mode)
}

// Dump serializes the REPL state.
func (r *Repl) Dump(ctx context.Context) ([]byte, error) {
	if r == nil || r.rt == nil || r.id == 0 {
		return nil, errors.New("monty: closed repl")
	}
	blobID, err := r.rt.callID(ctx, r.rt.fnReplDump, r.id)
	if err != nil {
		return nil, err
	}
	return r.rt.readBlob(ctx, blobID)
}

// LoadRepl loads a REPL state from Repl.Dump-ed data
func (rt *Runtime) LoadRepl(ctx context.Context, data []byte) (*Repl, error) {
	arg, done, err := rt.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	id, err := rt.callID(ctx, rt.fnReplLoad, arg.ptr, arg.len)
	if err != nil {
		return nil, err
	}
	return &Repl{rt: rt, id: id}, nil
}

// Close releases the REPL resources.
func (r *Repl) Close(ctx context.Context) {
	if r != nil && r.rt != nil && r.id != 0 {
		r.rt.fnReplFree.Call(ctx, r.id)
		r.id = 0
	}
}

// ============================================================
// Helper functions for REPL
// ============================================================

// DecodeReplProgressFromBlob reads a blob and decodes it as ReplProgress.
func (r *Runtime) DecodeReplProgressFromBlob(ctx context.Context, blobID uint64) (ReplProgress, error) {
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
		return nil, errors.New("monty: empty repl progress blob")
	}
	data, ok := r.memory.Read(ptr, length)
	if !ok {
		return nil, errors.New("monty: failed reading repl progress memory")
	}
	return r.decodeReplProgress(data)
}

// DecodeReplProgress decodes REPL progress from JSON data.
func (r *Runtime) DecodeReplProgress(ctx context.Context, data []byte) (ReplProgress, error) {
	return r.decodeReplProgress(data)
}

// callReplProgress calls a REPL function and decodes the result as ReplProgress.
func (r *Runtime) callReplProgress(ctx context.Context, fn api.Function, progressID uint64, progressType uint32, params ...uint64) (ReplProgress, error) {
	id, err := r.callID(ctx, fn, append([]uint64{progressID, uint64(progressType)}, params...)...)
	if err != nil {
		return nil, err
	}
	buf, err := r.readBlob(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.decodeReplProgress(buf)
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
