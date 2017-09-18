// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package aggregation

import (
	log "github.com/Sirupsen/logrus"
	"github.com/juju/errors"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util/types"
)

type maxMinFunction struct {
	aggFunction
	isMax bool
}

// Clone implements Aggregation interface.
func (mmf *maxMinFunction) Clone() Aggregation {
	nf := *mmf
	for i, arg := range mmf.Args {
		nf.Args[i] = arg.Clone()
	}
	nf.resultMapper = make(aggCtxMapper)
	return &nf
}

// CalculateDefaultValue implements Aggregation interface.
func (mmf *maxMinFunction) CalculateDefaultValue(schema *expression.Schema, ctx context.Context) (d types.Datum, valid bool) {
	arg := mmf.Args[0]
	result, err := expression.EvaluateExprWithNull(ctx, schema, arg)
	if err != nil {
		log.Warnf("Evaluate expr with null failed in function %s, err msg is %s", mmf, err.Error())
		return d, false
	}
	if con, ok := result.(*expression.Constant); ok {
		return con.Value, true
	}
	return d, false
}

// GetType implements Aggregation interface.
func (mmf *maxMinFunction) GetType() *types.FieldType {
	return mmf.Args[0].GetType()
}

// GetGroupResult implements Aggregation interface.
func (mmf *maxMinFunction) GetGroupResult(groupKey []byte) (d types.Datum) {
	return mmf.getContext(groupKey).Value
}

// GetPartialResult implements Aggregation interface.
func (mmf *maxMinFunction) GetPartialResult(groupKey []byte) []types.Datum {
	return []types.Datum{mmf.GetGroupResult(groupKey)}
}

// GetStreamResult implements Aggregation interface.
func (mmf *maxMinFunction) GetStreamResult() (d types.Datum) {
	if mmf.streamCtx == nil {
		return
	}
	d = mmf.streamCtx.Value
	mmf.streamCtx = nil
	return
}

// Update implements Aggregation interface.
func (mmf *maxMinFunction) Update(row []types.Datum, groupKey []byte, sc *variable.StatementContext) error {
	ctx := mmf.getContext(groupKey)
	if len(mmf.Args) != 1 {
		return errors.New("Wrong number of args for AggFuncMaxMin")
	}
	a := mmf.Args[0]
	value, err := a.Eval(row)
	if err != nil {
		return errors.Trace(err)
	}
	if ctx.Value.IsNull() {
		ctx.Value = value
	}
	if value.IsNull() {
		return nil
	}
	var c int
	c, err = ctx.Value.CompareDatum(sc, value)
	if err != nil {
		return errors.Trace(err)
	}
	if (mmf.isMax && c == -1) || (!mmf.isMax && c == 1) {
		ctx.Value = value
	}
	return nil
}

// StreamUpdate implements Aggregation interface.
func (mmf *maxMinFunction) StreamUpdate(row []types.Datum, sc *variable.StatementContext) error {
	ctx := mmf.getStreamedContext()
	if len(mmf.Args) != 1 {
		return errors.New("Wrong number of args for AggFuncMaxMin")
	}
	a := mmf.Args[0]
	value, err := a.Eval(row)
	if err != nil {
		return errors.Trace(err)
	}
	if ctx.Value.IsNull() {
		ctx.Value = value
	}
	if value.IsNull() {
		return nil
	}
	var c int
	c, err = ctx.Value.CompareDatum(sc, value)
	if err != nil {
		return errors.Trace(err)
	}
	if (mmf.isMax && c == -1) || (!mmf.isMax && c == 1) {
		ctx.Value = value
	}
	return nil
}
