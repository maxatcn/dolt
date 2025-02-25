// Copyright 2020 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dtables

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/rowconv"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/index"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/sqlutil"
	"github.com/dolthub/dolt/go/store/types"
)

var ErrExactlyOneToCommit = errors.New("dolt_commit_diff_* tables must be filtered to a single 'to_commit'")
var ErrExactlyOneFromCommit = errors.New("dolt_commit_diff_* tables must be filtered to a single 'from_commit'")
var ErrInvalidCommitDiffTableArgs = errors.New("commit_diff_<table> requires one 'to_commit' and one 'from_commit'")

var _ sql.Table = (*CommitDiffTable)(nil)

type CommitDiffTable struct {
	name        string
	ddb         *doltdb.DoltDB
	joiner      *rowconv.Joiner
	sqlSch      sql.PrimaryKeySchema
	workingRoot *doltdb.RootValue
	// toCommit and fromCommit are set via the
	// sql.IndexAddressable interface
	toCommit          string
	fromCommit        string
	requiredFilterErr error
	targetSchema      schema.Schema
}

func NewCommitDiffTable(ctx *sql.Context, tblName string, ddb *doltdb.DoltDB, root *doltdb.RootValue) (sql.Table, error) {
	diffTblName := doltdb.DoltCommitDiffTablePrefix + tblName

	table, _, ok, err := root.GetTableInsensitive(ctx, tblName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, sql.ErrTableNotFound.New(diffTblName)
	}

	sch, err := table.GetSchema(ctx)
	if err != nil {
		return nil, err
	}

	diffTableSchema, j, err := GetDiffTableSchemaAndJoiner(ddb.Format(), sch, sch)
	if err != nil {
		return nil, err
	}

	sqlSch, err := sqlutil.FromDoltSchema(diffTblName, diffTableSchema)
	if err != nil {
		return nil, err
	}

	return &CommitDiffTable{
		name:         tblName,
		ddb:          ddb,
		workingRoot:  root,
		joiner:       j,
		sqlSch:       sqlSch,
		targetSchema: sch,
	}, nil
}

func (dt *CommitDiffTable) Name() string {
	return doltdb.DoltCommitDiffTablePrefix + dt.name
}

func (dt *CommitDiffTable) String() string {
	return doltdb.DoltCommitDiffTablePrefix + dt.name
}

func (dt *CommitDiffTable) Schema() sql.Schema {
	return dt.sqlSch.Schema
}

// Collation implements the sql.Table interface.
func (dt *CommitDiffTable) Collation() sql.CollationID {
	return sql.Collation_Default
}

// GetIndexes implements sql.IndexAddressable
func (dt *CommitDiffTable) GetIndexes(ctx *sql.Context) ([]sql.Index, error) {
	return []sql.Index{index.DoltToFromCommitIndex(dt.name)}, nil
}

// IndexedAccess implements sql.IndexAddressable
func (dt *CommitDiffTable) IndexedAccess(lookup sql.IndexLookup) sql.IndexedTable {
	nt := *dt
	return &nt
}

func (dt *CommitDiffTable) Partitions(ctx *sql.Context) (sql.PartitionIter, error) {
	return nil, fmt.Errorf("error querying table %s: %w", dt.Name(), ErrExactlyOneToCommit)
}

func (dt *CommitDiffTable) LookupPartitions(ctx *sql.Context, i sql.IndexLookup) (sql.PartitionIter, error) {
	if len(i.Ranges) != 1 || len(i.Ranges[0]) != 2 {
		return nil, ErrInvalidCommitDiffTableArgs
	}
	to := i.Ranges[0][0]
	from := i.Ranges[0][1]
	switch to.UpperBound.(type) {
	case sql.Above, sql.Below:
	default:
		return nil, ErrInvalidCommitDiffTableArgs
	}
	switch from.UpperBound.(type) {
	case sql.Above, sql.Below:
	default:
		return nil, ErrInvalidCommitDiffTableArgs
	}
	toCommit, _, err := to.Typ.Convert(sql.GetRangeCutKey(to.UpperBound))
	if err != nil {
		return nil, err
	}
	var ok bool
	dt.toCommit, ok = toCommit.(string)
	if !ok {
		return nil, fmt.Errorf("to_commit must be string, found %T", toCommit)
	}
	fromCommit, _, err := from.Typ.Convert(sql.GetRangeCutKey(from.UpperBound))
	if err != nil {
		return nil, err
	}
	dt.fromCommit, ok = fromCommit.(string)
	if !ok {
		return nil, fmt.Errorf("from_commit must be string, found %T", fromCommit)
	}

	toRoot, toHash, toDate, err := dt.rootValForHash(ctx, dt.toCommit)
	if err != nil {
		return nil, err
	}

	fromRoot, fromHash, fromDate, err := dt.rootValForHash(ctx, dt.fromCommit)
	if err != nil {
		return nil, err
	}

	toTable, _, _, err := toRoot.GetTableInsensitive(ctx, dt.name)
	if err != nil {
		return nil, err
	}

	fromTable, _, _, err := fromRoot.GetTableInsensitive(ctx, dt.name)
	if err != nil {
		return nil, err
	}

	dp := DiffPartition{
		to:       toTable,
		from:     fromTable,
		toName:   toHash,
		fromName: fromHash,
		toDate:   toDate,
		fromDate: fromDate,
		toSch:    dt.targetSchema,
		fromSch:  dt.targetSchema,
	}

	isDiffable, err := dp.isDiffablePartition(ctx)
	if err != nil {
		return nil, err
	}

	if !isDiffable {
		ctx.Warn(PrimaryKeyChangeWarningCode, fmt.Sprintf(PrimaryKeyChangeWarning, dp.fromName, dp.toName))
		return NewSliceOfPartitionsItr([]sql.Partition{}), nil
	}

	return NewSliceOfPartitionsItr([]sql.Partition{dp}), nil
}

type SliceOfPartitionsItr struct {
	partitions []sql.Partition
	i          int
	mu         *sync.Mutex
}

func NewSliceOfPartitionsItr(partitions []sql.Partition) *SliceOfPartitionsItr {
	return &SliceOfPartitionsItr{
		partitions: partitions,
		mu:         &sync.Mutex{},
	}
}

func (itr *SliceOfPartitionsItr) Next(*sql.Context) (sql.Partition, error) {
	itr.mu.Lock()
	defer itr.mu.Unlock()

	if itr.i >= len(itr.partitions) {
		return nil, io.EOF
	}

	next := itr.partitions[itr.i]
	itr.i++

	return next, nil
}

func (itr *SliceOfPartitionsItr) Close(*sql.Context) error {
	return nil
}

func (dt *CommitDiffTable) rootValForHash(ctx *sql.Context, hashStr string) (*doltdb.RootValue, string, *types.Timestamp, error) {
	var root *doltdb.RootValue
	var commitTime *types.Timestamp
	if strings.ToLower(hashStr) == "working" {
		root = dt.workingRoot
	} else {
		cs, err := doltdb.NewCommitSpec(hashStr)

		if err != nil {
			return nil, "", nil, err
		}

		cm, err := dt.ddb.Resolve(ctx, cs, nil)

		if err != nil {
			return nil, "", nil, err
		}

		root, err = cm.GetRootValue(ctx)

		if err != nil {
			return nil, "", nil, err
		}

		meta, err := cm.GetCommitMeta(ctx)

		if err != nil {
			return nil, "", nil, err
		}

		t := meta.Time()
		commitTime = (*types.Timestamp)(&t)
	}

	return root, hashStr, commitTime, nil
}

func (dt *CommitDiffTable) PartitionRows(ctx *sql.Context, part sql.Partition) (sql.RowIter, error) {
	dp := part.(DiffPartition)
	return dp.GetRowIter(ctx, dt.ddb, dt.joiner, sql.IndexLookup{})
}
