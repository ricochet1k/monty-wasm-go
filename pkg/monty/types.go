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
