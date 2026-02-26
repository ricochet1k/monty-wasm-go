package monty

import (
	"encoding/json"
	"fmt"
)

type Kind int

const (
	KindComplete Kind = iota
	KindFunctionCall
	KindOSCall
	KindResolveFutures
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
	Kind      Kind
	Result    Value
	Call      *Call
	OSCall    *OSCall
	Futures   *PendingFutures
	rawCallID uint32
}

type FutureResult struct {
	CallID uint32
	Result any
	Err    string
}

type CompileOptions struct {
	ScriptName        string
	InputNames        []string
	ExternalFunctions []string
}
