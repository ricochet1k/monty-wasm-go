package monty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type Kind int

const (
	KindComplete Kind = iota
	KindFunctionCall
	KindOSCall
	KindResolveFutures
	KindNameLookup
)

type Value []byte

func (v Value) Decode(target any) error {
	if len(v) == 0 {
		return fmt.Errorf("monty: empty value")
	}
	return json.Unmarshal(v, target)
}

type KeywordArg struct {
	Key   Value
	Value Value
}

type Progress struct {
	Kind       Kind
	Result     Value
	Call       *Call
	OSCall     *OSCall
	Futures    *PendingFutures
	NameLookup *NameLookup
	rawCallID  uint32
}

type FutureResult struct {
	CallID uint32
	Result any
	Err    string
}

type CompileOptions struct {
	ScriptName string
	InputNames []string
}

type NameLookup struct {
	Name         string
	snapshotID   uint64
	rt           *Runtime
	snapshotType string
}

func (n *NameLookup) Dump(ctx context.Context) ([]byte, error) {
	if n == nil || n.snapshotID == 0 || n.rt == nil {
		return nil, errors.New("monty: name lookup not resumable")
	}
	blobID, err := n.rt.callID(ctx, n.rt.fnNameLookupDump, n.snapshotID)
	if err != nil {
		return nil, err
	}
	return n.rt.readBlob(ctx, blobID)
}

func (n *NameLookup) Close(ctx context.Context) {
	if n != nil && n.rt != nil && n.snapshotID != 0 {
		n.rt.fnNameLookupFree.Call(ctx, n.snapshotID)
		n.snapshotID = 0
	}
}

func (n *NameLookup) Resume(ctx context.Context, value any) (Progress, error) {
	if n == nil || n.snapshotID == 0 || n.rt == nil {
		return Progress{}, errors.New("monty: name lookup not resumable")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return Progress{}, fmt.Errorf("monty: encode name lookup result: %w", err)
	}
	arg, done, err := n.rt.arg(ctx, data)
	if err != nil {
		return Progress{}, err
	}
	defer done()
	typeJSON := fmt.Sprintf(`"%s"`, n.snapshotType)
	typeArg, typeDone, err := n.rt.arg(ctx, []byte(typeJSON))
	if err != nil {
		return Progress{}, err
	}
	defer typeDone()
	progress, err := n.rt.callProgress(ctx, n.rt.fnSnapshotResume, n.snapshotID, 0, 0, arg.ptr, arg.len, 0, 0, typeArg.ptr, typeArg.len)
	if err != nil {
		return Progress{}, err
	}
	n.snapshotID = 0
	return progress, nil
}

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
	ReplProgressComplete ReplProgressKind = iota
	ReplProgressFunctionCall
	ReplProgressOsCall
	ReplProgressNameLookup
	ReplProgressResolveFutures
)

// ReplProgress is the result of Repl.Start().
type ReplProgress struct {
	Kind       ReplProgressKind
	Result     Value
	Call       *ReplFunctionCall
	OS         *ReplOsCall
	NameLookup *ReplNameLookup
	Futures    *ReplResolveFutures
}

// ReplFunctionCall represents a suspended external function call.
type ReplFunctionCall struct {
	FunctionName string
	Args         []Value
	Kwargs       []KeywordArg
	CallID       uint32
	MethodCall   bool
	snapshotID   uint64
	rt           *Runtime
}

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
		return ReplProgress{}, errors.New("monty: function call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":  "return",
		"value": result,
	})
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: encode function call result: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return ReplProgress{}, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 0, arg.ptr, arg.len)
}

// Throw resumes the function call with an error.
func (c *ReplFunctionCall) Throw(ctx context.Context, message string) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return ReplProgress{}, errors.New("monty: function call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":    "error",
		"message": message,
	})
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: encode function call error: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return ReplProgress{}, err
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
	snapshotID uint64
	rt         *Runtime
}

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
		return ReplProgress{}, errors.New("monty: OS call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":  "return",
		"value": result,
	})
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: encode OS call result: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return ReplProgress{}, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 1, arg.ptr, arg.len)
}

// Throw resumes the OS call with an error.
func (c *ReplOsCall) Throw(ctx context.Context, message string) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return ReplProgress{}, errors.New("monty: OS call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":    "error",
		"message": message,
	})
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: encode OS call error: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return ReplProgress{}, err
	}
	defer done()
	return c.rt.callReplProgress(ctx, c.rt.fnReplResume, c.snapshotID, 1, arg.ptr, arg.len)
}

// Defer resumes the OS call as a future.
func (c *ReplOsCall) Defer(ctx context.Context) (ReplProgress, error) {
	if c == nil || c.snapshotID == 0 || c.rt == nil {
		return ReplProgress{}, errors.New("monty: OS call not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":    "future",
		"call_id": c.CallID,
	})
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: encode OS call future: %w", err)
	}
	arg, done, err := c.rt.arg(ctx, data)
	if err != nil {
		return ReplProgress{}, err
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
		return ReplProgress{}, errors.New("monty: name lookup not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type":  "value",
		"value": value,
	})
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: encode name lookup result: %w", err)
	}
	arg, done, err := n.rt.arg(ctx, data)
	if err != nil {
		return ReplProgress{}, err
	}
	defer done()
	return n.rt.callReplProgress(ctx, n.rt.fnReplResume, n.snapshotID, 2, arg.ptr, arg.len)
}

// Undefined resumes the name lookup as undefined.
func (n *ReplNameLookup) Undefined(ctx context.Context) (ReplProgress, error) {
	if n == nil || n.snapshotID == 0 || n.rt == nil {
		return ReplProgress{}, errors.New("monty: name lookup not resumable")
	}
	data, err := json.Marshal(map[string]any{
		"type": "undefined",
	})
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: encode name lookup undefined: %w", err)
	}
	arg, done, err := n.rt.arg(ctx, data)
	if err != nil {
		return ReplProgress{}, err
	}
	defer done()
	return n.rt.callReplProgress(ctx, n.rt.fnReplResume, n.snapshotID, 2, arg.ptr, arg.len)
}

// ReplResolveFutures represents suspended async futures.
type ReplResolveFutures struct {
	PendingCallIDs []uint32
	snapshotID     uint64
	rt             *Runtime
}

// Resume resolves the futures.
func (f *ReplResolveFutures) Resume(ctx context.Context, results []FutureResult) (ReplProgress, error) {
	if f == nil || f.snapshotID == 0 || f.rt == nil {
		return ReplProgress{}, errors.New("monty: futures not resumable")
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
		return ReplProgress{}, fmt.Errorf("monty: encode future results: %w", err)
	}
	arg, done, err := f.rt.arg(ctx, data)
	if err != nil {
		return ReplProgress{}, err
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
