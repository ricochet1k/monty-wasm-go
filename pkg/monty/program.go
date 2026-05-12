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

type Program struct {
	rt *Runtime
	id uint64
}

func (p *Program) Start(ctx context.Context, inputs ...any) (Progress, error) {
	if p == nil || p.rt == nil || p.id == 0 {
		return Progress{}, errors.New("monty: closed program")
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return Progress{}, fmt.Errorf("monty: encode inputs: %w", err)
	}
	arg, done, err := p.rt.arg(ctx, encoded)
	if err != nil {
		return Progress{}, err
	}
	defer done()
	return p.rt.callProgress(ctx, p.rt.fnRunStart, p.id, arg.ptr, arg.len)
}

func (p *Program) Run(ctx context.Context, inputs ...any) (Value, error) {
	progress, err := p.Start(ctx, inputs...)
	if err != nil {
		return nil, err
	}
	if progress.Kind != KindComplete {
		return nil, fmt.Errorf("monty: execution paused with kind %v", progress.Kind)
	}
	return progress.Result, nil
}

func (p *Program) Dump(ctx context.Context) ([]byte, error) {
	if p == nil || p.rt == nil || p.id == 0 {
		return nil, errors.New("monty: closed program")
	}
	blobID, err := p.rt.callID(ctx, p.rt.fnRunDump, p.id)
	if err != nil {
		return nil, err
	}
	return p.rt.readBlob(ctx, blobID)
}

func (p *Program) Close(ctx context.Context) {
	if p != nil && p.rt != nil && p.id != 0 {
		p.rt.fnRunFree.Call(ctx, p.id)
		p.id = 0
	}
}

type Call struct {
	Name         string
	Args         []Value
	Kwargs       []KeywordArg
	MethodCall   bool
	snapshotType string
	resume       *snapshotResume
}

func (c *Call) Dump(ctx context.Context) ([]byte, error) {
	if c == nil || c.resume == nil {
		return nil, errors.New("monty: call not resumable")
	}
	return c.resume.dump(ctx)
}

func (c *Call) Close(ctx context.Context) {
	if c != nil && c.resume != nil {
		c.resume.close(ctx)
	}
}

func (c *Call) Return(ctx context.Context, result any) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: call not resumable")
	}
	return c.resume.resumeResult(ctx, result)
}

func (c *Call) Throw(ctx context.Context, message string) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: call not resumable")
	}
	return c.resume.resumeError(ctx, message)
}

func (c *Call) Defer(ctx context.Context) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: call not resumable")
	}
	return c.resume.resumeFuture(ctx)
}

type OSCall struct {
	Name         string
	Args         []Value
	Kwargs       []KeywordArg
	snapshotType string
	resume       *snapshotResume
}

func (c *OSCall) Dump(ctx context.Context) ([]byte, error) {
	if c == nil || c.resume == nil {
		return nil, errors.New("monty: os call not resumable")
	}
	return c.resume.dump(ctx)
}

func (c *OSCall) Close(ctx context.Context) {
	if c != nil && c.resume != nil {
		c.resume.close(ctx)
	}
}

type Continuation struct {
	resume *snapshotResume
}

func (r *Runtime) LoadContinuation(ctx context.Context, data []byte) (*Continuation, error) {
	arg, done, err := r.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	id, err := r.callID(ctx, r.fnSnapshotLoad, arg.ptr, arg.len)
	if err != nil {
		return nil, err
	}
	return &Continuation{resume: &snapshotResume{rt: r, snapshotID: id}}, nil
}

func (c *Continuation) Return(ctx context.Context, result any) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: continuation not resumable")
	}
	return c.resume.resumeResult(ctx, result)
}

func (c *Continuation) Throw(ctx context.Context, message string) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: continuation not resumable")
	}
	return c.resume.resumeError(ctx, message)
}

func (c *Continuation) Defer(ctx context.Context) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: continuation not resumable")
	}
	return c.resume.resumeFuture(ctx)
}

func (c *Continuation) Close(ctx context.Context) {
	if c != nil && c.resume != nil {
		c.resume.close(ctx)
	}
}

func (c *OSCall) Return(ctx context.Context, result any) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: os call not resumable")
	}
	return c.resume.resumeResult(ctx, result)
}

func (c *OSCall) Throw(ctx context.Context, message string) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: os call not resumable")
	}
	return c.resume.resumeError(ctx, message)
}

func (c *OSCall) Defer(ctx context.Context) (Progress, error) {
	if c == nil || c.resume == nil {
		return Progress{}, errors.New("monty: os call not resumable")
	}
	return c.resume.resumeFuture(ctx)
}

type PendingFutures struct {
	PendingIDs []uint32
	rt         *Runtime
	snapshotID uint64
}

func (f *PendingFutures) Resume(ctx context.Context, results []FutureResult) (Progress, error) {
	if f == nil || f.snapshotID == 0 || f.rt == nil {
		return Progress{}, errors.New("monty: futures not resumable")
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
		return Progress{}, fmt.Errorf("monty: encode future results: %w", err)
	}
	arg, done, err := f.rt.arg(ctx, data)
	if err != nil {
		return Progress{}, err
	}
	defer done()
	progress, err := f.rt.callProgress(ctx, f.rt.fnFutureSnapshotRun, f.snapshotID, arg.ptr, arg.len)
	if err != nil {
		return Progress{}, err
	}
	f.snapshotID = 0
	return progress, nil
}

func (f *PendingFutures) Dump(ctx context.Context) ([]byte, error) {
	if f == nil || f.snapshotID == 0 || f.rt == nil {
		return nil, errors.New("monty: closed future snapshot")
	}
	blobID, err := f.rt.callID(ctx, f.rt.fnFutureSnapshotDump, f.snapshotID)
	if err != nil {
		return nil, err
	}
	return f.rt.readBlob(ctx, blobID)
}

func (f *PendingFutures) Close(ctx context.Context) {
	if f != nil && f.rt != nil && f.snapshotID != 0 {
		f.rt.fnFutureSnapshotFree.Call(ctx, f.snapshotID)
		f.snapshotID = 0
	}
}

func (r *Runtime) LoadPendingFutures(ctx context.Context, data []byte, pendingIDs []uint32) (*PendingFutures, error) {
	arg, done, err := r.arg(ctx, data)
	if err != nil {
		return nil, err
	}
	defer done()
	id, err := r.callID(ctx, r.fnFutureSnapshotLoad, arg.ptr, arg.len)
	if err != nil {
		return nil, err
	}
	return &PendingFutures{rt: r, snapshotID: id, PendingIDs: append([]uint32(nil), pendingIDs...)}, nil
}

type snapshotResume struct {
	rt           *Runtime
	snapshotID   uint64
	callID       uint32
	snapshotType string
}

func (s *snapshotResume) dump(ctx context.Context) ([]byte, error) {
	if s.snapshotID == 0 {
		return nil, errors.New("monty: closed snapshot")
	}
	blobID, err := s.rt.callID(ctx, s.rt.fnSnapshotDump, s.snapshotID)
	if err != nil {
		return nil, err
	}
	return s.rt.readBlob(ctx, blobID)
}

func (s *snapshotResume) close(ctx context.Context) {
	if s.snapshotID != 0 {
		s.rt.fnSnapshotFree.Call(ctx, s.snapshotID)
		s.snapshotID = 0
	}
}

func (s *snapshotResume) resumeResult(ctx context.Context, result any) (Progress, error) {
	if s.snapshotID == 0 {
		return Progress{}, errors.New("monty: closed snapshot")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return Progress{}, fmt.Errorf("monty: encode result: %w", err)
	}
	arg, done, err := s.rt.arg(ctx, data)
	if err != nil {
		return Progress{}, err
	}
	defer done()
	typeJSON := fmt.Sprintf(`"%s"`, s.snapshotType)
	typeArg, typeDone, err := s.rt.arg(ctx, []byte(typeJSON))
	if err != nil {
		return Progress{}, err
	}
	defer typeDone()
	progress, err := s.rt.callProgress(ctx, s.rt.fnSnapshotResume, s.snapshotID, uint64(s.callID), 0, arg.ptr, arg.len, 0, 0, typeArg.ptr, typeArg.len)
	if err != nil {
		return Progress{}, err
	}
	s.snapshotID = 0
	return progress, nil
}

func (s *snapshotResume) resumeError(ctx context.Context, message string) (Progress, error) {
	if s.snapshotID == 0 {
		return Progress{}, errors.New("monty: closed snapshot")
	}
	arg, done, err := s.rt.arg(ctx, []byte(message))
	if err != nil {
		return Progress{}, err
	}
	defer done()
	typeJSON := fmt.Sprintf(`"%s"`, s.snapshotType)
	typeArg, typeDone, err := s.rt.arg(ctx, []byte(typeJSON))
	if err != nil {
		return Progress{}, err
	}
	defer typeDone()
	progress, err := s.rt.callProgress(ctx, s.rt.fnSnapshotResume, s.snapshotID, uint64(s.callID), 1, 0, 0, arg.ptr, arg.len, typeArg.ptr, typeArg.len)
	if err != nil {
		return Progress{}, err
	}
	s.snapshotID = 0
	return progress, nil
}

func (s *snapshotResume) resumeFuture(ctx context.Context) (Progress, error) {
	if s.snapshotID == 0 {
		return Progress{}, errors.New("monty: closed snapshot")
	}
	typeJSON := fmt.Sprintf(`"%s"`, s.snapshotType)
	typeArg, typeDone, err := s.rt.arg(ctx, []byte(typeJSON))
	if err != nil {
		return Progress{}, err
	}
	defer typeDone()
	progress, err := s.rt.callProgress(ctx, s.rt.fnSnapshotResume, s.snapshotID, uint64(s.callID), 2, 0, 0, 0, 0, typeArg.ptr, typeArg.len)
	if err != nil {
		return Progress{}, err
	}
	s.snapshotID = 0
	return progress, nil
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
func (r *Repl) Start(ctx context.Context, code string, inputs ...any) (ReplProgress, error) {
	if r == nil || r.rt == nil || r.id == 0 {
		return ReplProgress{}, errors.New("monty: closed repl")
	}
	// Encode code string
	codeArg, doneCode, err := r.rt.arg(ctx, []byte(code))
	if err != nil {
		return ReplProgress{}, err
	}
	defer doneCode()
	var encoded []byte
	if inputs != nil {
		var err error
		encoded, err = json.Marshal(inputs)
		if err != nil {
			return ReplProgress{}, fmt.Errorf("monty: encode inputs: %w", err)
		}
	}
	inputArg, done, err := r.rt.arg(ctx, encoded)
	if err != nil {
		return ReplProgress{}, err
	}
	defer done()
	// Use empty input names (positional inputs)
	inputNamesJSON := "[]"
	namesArg, doneNames, err := r.rt.arg(ctx, []byte(inputNamesJSON))
	if err != nil {
		return ReplProgress{}, err
	}
	defer doneNames()
	id, err := r.rt.callID(ctx, r.rt.fnReplStart, r.id, codeArg.ptr, codeArg.len, namesArg.ptr, namesArg.len, inputArg.ptr, inputArg.len)
	if err != nil {
		return ReplProgress{}, err
	}
	if id == 0 {
		// Check for ReplStartError - the repl_id is stored in last_error
		msg, readErr := r.rt.lastError(ctx)
		if readErr != nil {
			return ReplProgress{}, readErr
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
		return ReplProgress{}, &ReplStartError{Message: msg, ReplID: newReplID}
	}
	r.id = 0
	return r.rt.decodeReplProgressFromBlob(id)
}

// Resume resumes from a progress snapshot.
func (r *Repl) Resume(ctx context.Context, progress ReplProgress, result any) (ReplProgress, error) {
	if r == nil || r.rt == nil {
		return ReplProgress{}, errors.New("monty: closed repl")
	}
	if progress.Kind == ReplProgressComplete {
		return progress, nil
	}
	var resultJSON []byte
	switch result := result.(type) {
	case string:
		resultJSON = []byte(result)
	case []byte:
		resultJSON = result
	default:
		var err error
		resultJSON, err = json.Marshal(result)
		if err != nil {
			return ReplProgress{}, fmt.Errorf("monty: encode result: %w", err)
		}
	}
	arg, done, err := r.rt.arg(ctx, resultJSON)
	if err != nil {
		return ReplProgress{}, err
	}
	defer done()
	var snapshotID uint64
	switch progress.Kind {
	case ReplProgressFunctionCall:
		if progress.Call != nil {
			snapshotID = progress.Call.snapshotID
		}
	case ReplProgressOsCall:
		if progress.OS != nil {
			snapshotID = progress.OS.snapshotID
		}
	case ReplProgressNameLookup:
		if progress.NameLookup != nil {
			snapshotID = progress.NameLookup.snapshotID
		}
	case ReplProgressResolveFutures:
		if progress.Futures != nil {
			snapshotID = progress.Futures.snapshotID
		}
	}
	id, err := r.rt.callID(ctx, r.rt.fnReplResume, snapshotID, arg.ptr, arg.len)
	if err != nil {
		return ReplProgress{}, err
	}
	if id == 0 {
		msg, _ := r.rt.lastError(ctx)
		return ReplProgress{}, &ReplStartError{Message: msg, ReplID: r.id}
	}
	return r.rt.decodeReplProgressFromBlob(id)
}

// Feed executes code synchronously (no suspension).
func (r *Repl) Feed(ctx context.Context, code string) (Value, error) {
	if r == nil || r.rt == nil || r.id == 0 {
		return nil, errors.New("monty: closed repl")
	}
	codeArg, done, err := r.rt.arg(ctx, []byte(code))
	if err != nil {
		return nil, err
	}
	defer done()
	id, err := r.rt.callID(ctx, r.rt.fnReplFeed, r.id, codeArg.ptr, codeArg.len)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, nil // None result
	}
	buf, err := r.rt.readBlob(ctx, id)
	if err != nil {
		return nil, err
	}
	return Value(buf), nil
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

// Load deserializes REPL state from bytes.
func (r *Repl) Load(ctx context.Context, data []byte) error {
	if r == nil || r.rt == nil {
		return errors.New("monty: nil runtime")
	}
	arg, done, err := r.rt.arg(ctx, data)
	if err != nil {
		return err
	}
	defer done()
	id, err := r.rt.callID(ctx, r.rt.fnReplLoad, arg.ptr, arg.len)
	if err != nil {
		return err
	}
	r.id = id
	return nil
}

// Close releases the REPL.
func (r *Repl) Close() {
	if r != nil && r.rt != nil && r.id != 0 {
		r.rt.fnReplFree.Call(context.Background(), r.id)
		r.id = 0
	}
}

// callUint64 calls a function and returns the result as uint64.
func (r *Runtime) callUint64(fn api.Function, params ...uint64) (uint64, error) {
	res, err := fn.Call(context.Background(), params...)
	if err != nil {
		return 0, err
	}
	return res[0], nil
}

// decodeReplProgressFromBlob reads a blob and decodes it as ReplProgress.
func (r *Runtime) decodeReplProgressFromBlob(blobID uint64) (ReplProgress, error) {
	defer r.fnBlobFree.Call(context.Background(), blobID)
	ptrRes, err := r.fnBlobPtr.Call(context.Background(), blobID)
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: blob ptr call: %w", err)
	}
	lenRes, err := r.fnBlobLen.Call(context.Background(), blobID)
	if err != nil {
		return ReplProgress{}, fmt.Errorf("monty: blob len call: %w", err)
	}
	ptr := uint32(ptrRes[0])
	length := uint32(lenRes[0])
	if length == 0 {
		return ReplProgress{}, errors.New("monty: empty repl progress blob")
	}
	data, ok := r.memory.Read(ptr, length)
	if !ok {
		return ReplProgress{}, errors.New("monty: failed reading repl progress memory")
	}
	return r.decodeReplProgress(data)
}
