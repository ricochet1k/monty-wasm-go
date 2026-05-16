package monty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type CallLocation struct {
	FileName     string
	FunctionName *string
}

type Call struct {
	Name         string
	Args         []Value
	Kwargs       []KeywordArg
	MethodCall   bool
	Location     *CallLocation
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
	Location     *CallLocation
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

// Dumps bytes that must be loaded by Runtime.LoadPendingFutures, you will need PendingFutures.PendingIDs too.
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

// LoadPendingFutures recreates a PendingFutures from a PendingFutures.Dump
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
