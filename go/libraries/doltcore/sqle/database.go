// Copyright 2019-2020 Dolthub, Inc.
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

package sqle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/fulltext"
	"github.com/dolthub/go-mysql-server/sql/types"
	"github.com/dolthub/vitess/go/vt/sqlparser"
	"gopkg.in/src-d/go-errors.v1"

	"github.com/dolthub/dolt/go/libraries/doltcore/branch_control"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions/commitwalk"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dtables"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/globalstate"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/sqlutil"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
	"github.com/dolthub/dolt/go/store/hash"
)

var ErrInvalidTableName = errors.NewKind("Invalid table name %s.")
var ErrReservedTableName = errors.NewKind("Invalid table name %s. Table names beginning with `dolt_` are reserved for internal use")
var ErrSystemTableAlter = errors.NewKind("Cannot alter table %s: system tables cannot be dropped or altered")

// Database implements sql.Database for a dolt DB.
type Database struct {
	baseName      string
	requestedName string
	ddb           *doltdb.DoltDB
	rsr           env.RepoStateReader
	rsw           env.RepoStateWriter
	gs            dsess.GlobalStateImpl
	editOpts      editor.Options
	revision      string
	revType       dsess.RevisionType
}

var _ dsess.SqlDatabase = Database{}
var _ dsess.RevisionDatabase = Database{}
var _ globalstate.GlobalStateProvider = Database{}
var _ sql.CollatedDatabase = Database{}
var _ sql.Database = Database{}
var _ sql.StoredProcedureDatabase = Database{}
var _ sql.TableCreator = Database{}
var _ sql.IndexedTableCreator = Database{}
var _ sql.TableDropper = Database{}
var _ sql.TableRenamer = Database{}
var _ sql.TemporaryTableCreator = Database{}
var _ sql.TemporaryTableDatabase = Database{}
var _ sql.TriggerDatabase = Database{}
var _ sql.VersionedDatabase = Database{}
var _ sql.ViewDatabase = Database{}
var _ sql.EventDatabase = Database{}
var _ sql.AliasedDatabase = Database{}
var _ fulltext.Database = Database{}

type ReadOnlyDatabase struct {
	Database
}

var _ sql.ReadOnlyDatabase = ReadOnlyDatabase{}
var _ dsess.SqlDatabase = ReadOnlyDatabase{}

func (r ReadOnlyDatabase) IsReadOnly() bool {
	return true
}

func (r ReadOnlyDatabase) InitialDBState(ctx *sql.Context) (dsess.InitialDbState, error) {
	return initialDBState(ctx, r, r.revision)
}

func (r ReadOnlyDatabase) WithBranchRevision(requestedName string, branchSpec dsess.SessionDatabaseBranchSpec) (dsess.SqlDatabase, error) {
	revDb, err := r.Database.WithBranchRevision(requestedName, branchSpec)
	if err != nil {
		return nil, err
	}

	r.Database = revDb.(Database)
	return r, nil
}

func (db Database) WithBranchRevision(requestedName string, branchSpec dsess.SessionDatabaseBranchSpec) (dsess.SqlDatabase, error) {
	db.rsr, db.rsw = branchSpec.RepoState, branchSpec.RepoState
	db.revision = branchSpec.Branch
	db.revType = dsess.RevisionTypeBranch
	db.requestedName = requestedName

	return db, nil
}

// Revision implements dsess.RevisionDatabase
func (db Database) Revision() string {
	return db.revision
}

func (db Database) Versioned() bool {
	return true
}

func (db Database) RevisionType() dsess.RevisionType {
	return db.revType
}

func (db Database) EditOptions() editor.Options {
	return db.editOpts
}

func (db Database) DoltDatabases() []*doltdb.DoltDB {
	return []*doltdb.DoltDB{db.ddb}
}

// NewDatabase returns a new dolt database to use in queries.
func NewDatabase(ctx context.Context, name string, dbData env.DbData, editOpts editor.Options) (Database, error) {
	globalState, err := dsess.NewGlobalStateStoreForDb(ctx, name, dbData.Ddb)
	if err != nil {
		return Database{}, err
	}

	return Database{
		baseName:      name,
		requestedName: name,
		ddb:           dbData.Ddb,
		rsr:           dbData.Rsr,
		rsw:           dbData.Rsw,
		gs:            globalState,
		editOpts:      editOpts,
	}, nil
}

// initialDBState returns the InitialDbState for |db|. Other implementations of SqlDatabase outside this file should
// implement their own method for an initial db state and not rely on this method.
func initialDBState(ctx *sql.Context, db dsess.SqlDatabase, branch string) (dsess.InitialDbState, error) {
	if len(db.Revision()) > 0 {
		return initialStateForRevisionDb(ctx, db)
	}

	return initialDbState(ctx, db, branch)
}

func (db Database) InitialDBState(ctx *sql.Context) (dsess.InitialDbState, error) {
	return initialDBState(ctx, db, db.revision)
}

// Name returns the name of this database, set at creation time.
func (db Database) Name() string {
	return db.RequestedName()
}

// AliasedName is what allows databases named e.g. `mydb/b1` to work with the grant and info schema tables that expect
// a base (no revision qualifier) db name
func (db Database) AliasedName() string {
	return db.baseName
}

// RevisionQualifiedName returns the name of this database including its revision qualifier, if any. This method should
// be used whenever accessing internal state of a database and its tables.
func (db Database) RevisionQualifiedName() string {
	if db.revision == "" {
		return db.baseName
	}
	return db.baseName + dsess.DbRevisionDelimiter + db.revision
}

func (db Database) RequestedName() string {
	return db.requestedName
}

// GetDoltDB gets the underlying DoltDB of the Database
func (db Database) GetDoltDB() *doltdb.DoltDB {
	return db.ddb
}

// GetStateReader gets the RepoStateReader for a Database
func (db Database) GetStateReader() env.RepoStateReader {
	return db.rsr
}

// GetStateWriter gets the RepoStateWriter for a Database
func (db Database) GetStateWriter() env.RepoStateWriter {
	return db.rsw
}

func (db Database) DbData() env.DbData {
	return env.DbData{
		Ddb: db.ddb,
		Rsw: db.rsw,
		Rsr: db.rsr,
	}
}

func (db Database) GetGlobalState() globalstate.GlobalState {
	return db.gs
}

// GetTableInsensitive is used when resolving tables in queries. It returns a best-effort case-insensitive match for
// the table name given.
func (db Database) GetTableInsensitive(ctx *sql.Context, tblName string) (sql.Table, bool, error) {
	// We start by first checking whether the input table is a temporary table. Temporary tables with name `x` take
	// priority over persisted tables of name `x`.
	ds := dsess.DSessFromSess(ctx.Session)
	if tbl, ok := ds.GetTemporaryTable(ctx, db.Name(), tblName); ok {
		return tbl, ok, nil
	}

	root, err := db.GetRoot(ctx)
	if err != nil {
		return nil, false, err
	}

	tbl, ok, err := db.getTableInsensitive(ctx, nil, ds, root, tblName)
	if err != nil {
		return nil, false, err
	}

	if !ok {
		return nil, false, nil
	}

	return tbl, true, nil
}

// GetTableInsensitiveAsOf implements sql.VersionedDatabase
func (db Database) GetTableInsensitiveAsOf(ctx *sql.Context, tableName string, asOf interface{}) (sql.Table, bool, error) {
	if asOf == nil {
		return db.GetTableInsensitive(ctx, tableName)
	}
	head, root, err := resolveAsOf(ctx, db, asOf)
	if err != nil {
		return nil, false, err
	} else if root == nil {
		return nil, false, nil
	}

	sess := dsess.DSessFromSess(ctx.Session)

	table, ok, err := db.getTableInsensitive(ctx, head, sess, root, tableName)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}

	if doltdb.IsReadOnlySystemTable(tableName) {
		// currently, system tables do not need to be "locked to root"
		//  see comment below in getTableInsensitive
		return table, ok, nil
	}

	switch table := table.(type) {
	case *DoltTable:
		tbl, err := table.LockedToRoot(ctx, root)
		if err != nil {
			return nil, false, err
		}
		return tbl, true, nil
	case *AlterableDoltTable:
		tbl, err := table.LockedToRoot(ctx, root)
		if err != nil {
			return nil, false, err
		}
		return tbl, true, nil
	case *WritableDoltTable:
		tbl, err := table.LockedToRoot(ctx, root)
		if err != nil {
			return nil, false, err
		}
		return tbl, true, nil
	default:
		panic(fmt.Sprintf("unexpected table type %T", table))
	}
}

func (db Database) getTableInsensitive(ctx *sql.Context, head *doltdb.Commit, ds *dsess.DoltSession, root *doltdb.RootValue, tblName string) (sql.Table, bool, error) {
	lwrName := strings.ToLower(tblName)

	// TODO: these tables that cache a root value at construction time should not, they need to get it from the session
	//  at runtime
	switch {
	case strings.HasPrefix(lwrName, doltdb.DoltDiffTablePrefix):
		if head == nil {
			var err error
			head, err = ds.GetHeadCommit(ctx, db.RevisionQualifiedName())

			if err != nil {
				return nil, false, err
			}
		}

		tableName := tblName[len(doltdb.DoltDiffTablePrefix):]
		dt, err := dtables.NewDiffTable(ctx, tableName, db.ddb, root, head)
		if err != nil {
			return nil, false, err
		}
		return dt, true, nil

	case strings.HasPrefix(lwrName, doltdb.DoltCommitDiffTablePrefix):
		suffix := tblName[len(doltdb.DoltCommitDiffTablePrefix):]
		dt, err := dtables.NewCommitDiffTable(ctx, suffix, db.ddb, root)
		if err != nil {
			return nil, false, err
		}
		return dt, true, nil

	case strings.HasPrefix(lwrName, doltdb.DoltHistoryTablePrefix):
		baseTableName := tblName[len(doltdb.DoltHistoryTablePrefix):]
		baseTable, ok, err := db.getTable(ctx, root, baseTableName)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}

		if head == nil {
			var err error
			head, err = ds.GetHeadCommit(ctx, db.RevisionQualifiedName())
			if err != nil {
				return nil, false, err
			}
		}

		return NewHistoryTable(baseTable.(*AlterableDoltTable).DoltTable, db.ddb, head), true, nil

	case strings.HasPrefix(lwrName, doltdb.DoltConfTablePrefix):
		suffix := tblName[len(doltdb.DoltConfTablePrefix):]
		srcTable, ok, err := db.getTableInsensitive(ctx, head, ds, root, suffix)
		if err != nil {
			return nil, false, err
		} else if !ok {
			return nil, false, nil
		}
		dt, err := dtables.NewConflictsTable(ctx, suffix, srcTable, root, dtables.RootSetter(db))
		if err != nil {
			return nil, false, err
		}
		return dt, true, nil

	case strings.HasPrefix(lwrName, doltdb.DoltConstViolTablePrefix):
		suffix := tblName[len(doltdb.DoltConstViolTablePrefix):]
		dt, err := dtables.NewConstraintViolationsTable(ctx, suffix, root, dtables.RootSetter(db))
		if err != nil {
			return nil, false, err
		}
		return dt, true, nil
	}

	var dt sql.Table
	found := false
	switch lwrName {
	case doltdb.LogTableName:
		if head == nil {
			var err error
			head, err = ds.GetHeadCommit(ctx, db.RevisionQualifiedName())
			if err != nil {
				return nil, false, err
			}
		}

		dt, found = dtables.NewLogTable(ctx, db.ddb, head), true
	case doltdb.DiffTableName:
		if head == nil {
			var err error
			head, err = ds.GetHeadCommit(ctx, db.RevisionQualifiedName())
			if err != nil {
				return nil, false, err
			}
		}

		dt, found = dtables.NewUnscopedDiffTable(ctx, db.RevisionQualifiedName(), db.ddb, head), true
	case doltdb.ColumnDiffTableName:
		if head == nil {
			var err error
			head, err = ds.GetHeadCommit(ctx, db.RevisionQualifiedName())
			if err != nil {
				return nil, false, err
			}
		}

		dt, found = dtables.NewColumnDiffTable(ctx, db.RevisionQualifiedName(), db.ddb, head), true
	case doltdb.TableOfTablesInConflictName:
		dt, found = dtables.NewTableOfTablesInConflict(ctx, db.RevisionQualifiedName(), db.ddb), true
	case doltdb.TableOfTablesWithViolationsName:
		dt, found = dtables.NewTableOfTablesConstraintViolations(ctx, root), true
	case doltdb.SchemaConflictsTableName:
		dt, found = dtables.NewSchemaConflictsTable(ctx, db.RevisionQualifiedName(), db.ddb), true
	case doltdb.BranchesTableName:
		dt, found = dtables.NewBranchesTable(ctx, db), true
	case doltdb.RemoteBranchesTableName:
		dt, found = dtables.NewRemoteBranchesTable(ctx, db), true
	case doltdb.RemotesTableName:
		dt, found = dtables.NewRemotesTable(ctx, db.ddb), true
	case doltdb.CommitsTableName:
		dt, found = dtables.NewCommitsTable(ctx, db.ddb), true
	case doltdb.CommitAncestorsTableName:
		dt, found = dtables.NewCommitAncestorsTable(ctx, db.ddb), true
	case doltdb.StatusTableName:
		sess := dsess.DSessFromSess(ctx.Session)
		adapter := dsess.NewSessionStateAdapter(
			sess, db.RevisionQualifiedName(),
			map[string]env.Remote{},
			map[string]env.BranchConfig{},
			map[string]env.Remote{})
		ws, err := sess.WorkingSet(ctx, db.RevisionQualifiedName())
		if err != nil {
			return nil, false, err
		}
		dt, found = dtables.NewStatusTable(ctx, db.ddb, ws, adapter), true
	case doltdb.MergeStatusTableName:
		dt, found = dtables.NewMergeStatusTable(db.RevisionQualifiedName()), true
	case doltdb.TagsTableName:
		dt, found = dtables.NewTagsTable(ctx, db.ddb), true
	case dtables.AccessTableName:
		basCtx := branch_control.GetBranchAwareSession(ctx)
		if basCtx != nil {
			if controller := basCtx.GetController(); controller != nil {
				dt, found = dtables.NewBranchControlTable(controller.Access), true
			}
		}
	case dtables.NamespaceTableName:
		basCtx := branch_control.GetBranchAwareSession(ctx)
		if basCtx != nil {
			if controller := basCtx.GetController(); controller != nil {
				dt, found = dtables.NewBranchNamespaceControlTable(controller.Namespace), true
			}
		}
	case doltdb.IgnoreTableName:
		backingTable, _, err := db.getTable(ctx, root, doltdb.IgnoreTableName)
		if err != nil {
			return nil, false, err
		}
		dt, found = dtables.NewIgnoreTable(ctx, db.ddb, backingTable), true
	}

	if found {
		return dt, found, nil
	}

	// TODO: this should reuse the root, not lookup the db state again
	return db.getTable(ctx, root, tblName)
}

// resolveAsOf resolves given expression to a commit, if one exists.
func resolveAsOf(ctx *sql.Context, db Database, asOf interface{}) (*doltdb.Commit, *doltdb.RootValue, error) {
	head, err := db.rsr.CWBHeadRef()
	if err != nil {
		return nil, nil, err
	}
	switch x := asOf.(type) {
	case time.Time:
		return resolveAsOfTime(ctx, db.ddb, head, x)
	case string:
		return resolveAsOfCommitRef(ctx, db, head, x)
	default:
		return nil, nil, fmt.Errorf("unsupported AS OF type %T", asOf)
	}
}

func resolveAsOfTime(ctx *sql.Context, ddb *doltdb.DoltDB, head ref.DoltRef, asOf time.Time) (*doltdb.Commit, *doltdb.RootValue, error) {
	cs, err := doltdb.NewCommitSpec("HEAD")
	if err != nil {
		return nil, nil, err
	}

	cm, err := ddb.Resolve(ctx, cs, head)
	if err != nil {
		return nil, nil, err
	}

	h, err := cm.HashOf()
	if err != nil {
		return nil, nil, err
	}

	cmItr, err := commitwalk.GetTopologicalOrderIterator(ctx, ddb, []hash.Hash{h}, nil)
	if err != nil {
		return nil, nil, err
	}

	for {
		_, curr, err := cmItr.Next(ctx)
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, err
		}

		meta, err := curr.GetCommitMeta(ctx)
		if err != nil {
			return nil, nil, err
		}

		if meta.Time().Equal(asOf) || meta.Time().Before(asOf) {
			root, err := curr.GetRootValue(ctx)
			if err != nil {
				return nil, nil, err
			}
			return curr, root, nil
		}
	}
	return nil, nil, nil
}

func resolveAsOfCommitRef(ctx *sql.Context, db Database, head ref.DoltRef, commitRef string) (*doltdb.Commit, *doltdb.RootValue, error) {
	ddb := db.ddb

	if commitRef == doltdb.Working || commitRef == doltdb.Staged {
		sess := dsess.DSessFromSess(ctx.Session)
		root, _, _, err := sess.ResolveRootForRef(ctx, ctx.GetCurrentDatabase(), commitRef)
		if err != nil {
			return nil, nil, err
		}

		cm, err := ddb.ResolveCommitRef(ctx, head)
		if err != nil {
			return nil, nil, err
		}
		return cm, root, nil
	}

	cs, err := doltdb.NewCommitSpec(commitRef)

	if err != nil {
		return nil, nil, err
	}

	nomsRoot, err := dsess.TransactionRoot(ctx, db)
	if err != nil {
		return nil, nil, err
	}

	cm, err := ddb.ResolveByNomsRoot(ctx, cs, head, nomsRoot)
	if err != nil {
		return nil, nil, err
	}

	root, err := cm.GetRootValue(ctx)
	if err != nil {
		return nil, nil, err
	}

	return cm, root, nil
}

// GetTableNamesAsOf implements sql.VersionedDatabase
func (db Database) GetTableNamesAsOf(ctx *sql.Context, time interface{}) ([]string, error) {
	_, root, err := resolveAsOf(ctx, db, time)
	if err != nil {
		return nil, err
	} else if root == nil {
		return nil, nil
	}

	tblNames, err := root.GetTableNames(ctx)
	if err != nil {
		return nil, err
	}
	return filterDoltInternalTables(tblNames), nil
}

// getTable returns the user table with the given baseName from the root given
func (db Database) getTable(ctx *sql.Context, root *doltdb.RootValue, tableName string) (sql.Table, bool, error) {
	sess := dsess.DSessFromSess(ctx.Session)
	dbState, ok, err := sess.LookupDbState(ctx, db.RevisionQualifiedName())
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, fmt.Errorf("no state for database %s", db.RevisionQualifiedName())
	}

	key, err := doltdb.NewDataCacheKey(root)
	if err != nil {
		return nil, false, err
	}

	cachedTable, ok := dbState.SessionCache().GetCachedTable(key, tableName)
	if ok {
		return cachedTable, true, nil
	}

	tableNames, err := getAllTableNames(ctx, root)
	if err != nil {
		return nil, true, err
	}

	tableName, ok = sql.GetTableNameInsensitive(tableName, tableNames)
	if !ok {
		return nil, false, nil
	}

	tbl, ok, err := root.GetTable(ctx, tableName)
	if err != nil {
		return nil, false, err
	} else if !ok {
		// Should be impossible
		return nil, false, doltdb.ErrTableNotFound
	}

	sch, err := tbl.GetSchema(ctx)
	if err != nil {
		return nil, false, err
	}

	var table sql.Table

	readonlyTable, err := NewDoltTable(tableName, sch, tbl, db, db.editOpts)
	if err != nil {
		return nil, false, err
	}
	if doltdb.IsReadOnlySystemTable(tableName) {
		table = readonlyTable
	} else if doltdb.HasDoltPrefix(tableName) && !doltdb.IsFullTextTable(tableName) {
		table = &WritableDoltTable{DoltTable: readonlyTable, db: db}
	} else {
		table = &AlterableDoltTable{WritableDoltTable{DoltTable: readonlyTable, db: db}}
	}

	dbState.SessionCache().CacheTable(key, tableName, table)

	return table, true, nil
}

// GetTableNames returns the names of all user tables. System tables in user space (e.g. dolt_docs, dolt_query_catalog)
// are filtered out. This method is used for queries that examine the schema of the database, e.g. show tables. Table
// name resolution in queries is handled by GetTableInsensitive. Use GetAllTableNames for an unfiltered list of all
// tables in user space.
func (db Database) GetTableNames(ctx *sql.Context) ([]string, error) {
	tblNames, err := db.GetAllTableNames(ctx)
	if err != nil {
		return nil, err
	}
	return filterDoltInternalTables(tblNames), nil
}

// GetAllTableNames returns all user-space tables, including system tables in user space
// (e.g. dolt_docs, dolt_query_catalog).
func (db Database) GetAllTableNames(ctx *sql.Context) ([]string, error) {
	root, err := db.GetRoot(ctx)

	if err != nil {
		return nil, err
	}

	return getAllTableNames(ctx, root)
}

func getAllTableNames(ctx context.Context, root *doltdb.RootValue) ([]string, error) {
	return root.GetTableNames(ctx)
}

func filterDoltInternalTables(tblNames []string) []string {
	result := []string{}
	for _, tbl := range tblNames {
		if !doltdb.HasDoltPrefix(tbl) {
			result = append(result, tbl)
		}
	}
	return result
}

// GetRoot returns the root value for this database session
func (db Database) GetRoot(ctx *sql.Context) (*doltdb.RootValue, error) {
	sess := dsess.DSessFromSess(ctx.Session)
	dbState, ok, err := sess.LookupDbState(ctx, db.RevisionQualifiedName())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no root value found in session")
	}

	return dbState.WorkingRoot(), nil
}

// GetWorkingSet gets the current working set for the database.
// If there is no working set (most likely because the DB is in Detached Head mode, return an error.
// If a command needs to work while in Detached Head, that command should call sess.LookupDbState directly.
// TODO: This is a temporary measure to make sure that new commands that call GetWorkingSet don't unexpectedly receive
// a null pointer. In the future, we should replace all uses of dbState.WorkingSet, including this, with a new interface
// where users avoid handling the WorkingSet directly.
func (db Database) GetWorkingSet(ctx *sql.Context) (*doltdb.WorkingSet, error) {
	sess := dsess.DSessFromSess(ctx.Session)
	dbState, ok, err := sess.LookupDbState(ctx, db.RevisionQualifiedName())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no root value found in session")
	}
	if dbState.WorkingSet() == nil {
		return nil, doltdb.ErrOperationNotSupportedInDetachedHead
	}
	return dbState.WorkingSet(), nil
}

// SetRoot should typically be called on the Session, which is where this state lives. But it's available here as a
// convenience.
func (db Database) SetRoot(ctx *sql.Context, newRoot *doltdb.RootValue) error {
	sess := dsess.DSessFromSess(ctx.Session)
	return sess.SetRoot(ctx, db.RevisionQualifiedName(), newRoot)
}

// GetHeadRoot returns root value for the current session head
func (db Database) GetHeadRoot(ctx *sql.Context) (*doltdb.RootValue, error) {
	sess := dsess.DSessFromSess(ctx.Session)
	head, err := sess.GetHeadCommit(ctx, db.RevisionQualifiedName())
	if err != nil {
		return nil, err
	}
	return head.GetRootValue(ctx)
}

// DropTable drops the table with the name given.
// The planner returns the correct case sensitive name in tableName
func (db Database) DropTable(ctx *sql.Context, tableName string) error {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}
	if doltdb.IsNonAlterableSystemTable(tableName) {
		return ErrSystemTableAlter.New(tableName)
	}

	return db.dropTable(ctx, tableName)
}

// dropTable drops the table with the baseName given, without any business logic checks
func (db Database) dropTable(ctx *sql.Context, tableName string) error {
	ds := dsess.DSessFromSess(ctx.Session)
	if _, ok := ds.GetTemporaryTable(ctx, db.Name(), tableName); ok {
		ds.DropTemporaryTable(ctx, db.Name(), tableName)
		return nil
	}

	ws, err := db.GetWorkingSet(ctx)
	if err != nil {
		return err
	}

	root := ws.WorkingRoot()
	tbl, tableExists, err := root.GetTable(ctx, tableName)
	if err != nil {
		return err
	}

	if !tableExists {
		return sql.ErrTableNotFound.New(tableName)
	}

	newRoot, err := root.RemoveTables(ctx, true, false, tableName)
	if err != nil {
		return err
	}

	sch, err := tbl.GetSchema(ctx)
	if err != nil {
		return err
	}

	if schema.HasAutoIncrement(sch) {
		ddb, _ := ds.GetDoltDB(ctx, db.RevisionQualifiedName())
		err = db.removeTableFromAutoIncrementTracker(ctx, tableName, ddb, ws.Ref())
		if err != nil {
			return err
		}
	}

	return db.SetRoot(ctx, newRoot)
}

// removeTableFromAutoIncrementTracker updates the global auto increment tracking as necessary to deal with the table
// given being dropped or truncated. The auto increment value for this table after this operation will either be reset
// back to 1 if this table only exists in the working set given, or to the highest value in all other working sets
// otherwise. This operation is expensive if the
func (db Database) removeTableFromAutoIncrementTracker(
	ctx *sql.Context,
	tableName string,
	ddb *doltdb.DoltDB,
	ws ref.WorkingSetRef,
) error {
	branches, err := ddb.GetBranches(ctx)
	if err != nil {
		return err
	}

	var wses []*doltdb.WorkingSet
	for _, b := range branches {
		wsRef, err := ref.WorkingSetRefForHead(b)
		if err != nil {
			return err
		}

		if wsRef == ws {
			// skip this branch, we've deleted it here
			continue
		}

		ws, err := ddb.ResolveWorkingSet(ctx, wsRef)
		if err == doltdb.ErrWorkingSetNotFound {
			// skip, continue working on other branches
			continue
		} else if err != nil {
			return err
		}

		wses = append(wses, ws)
	}

	ait, err := db.gs.AutoIncrementTracker(ctx)
	if err != nil {
		return err
	}

	err = ait.DropTable(ctx, tableName, wses...)
	if err != nil {
		return err
	}

	return nil
}

// CreateTable creates a table with the name and schema given.
func (db Database) CreateTable(ctx *sql.Context, tableName string, sch sql.PrimaryKeySchema, collation sql.CollationID) error {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}
	if strings.ToLower(tableName) == doltdb.DocTableName {
		// validate correct schema
		if !dtables.DoltDocsSqlSchema.Equals(sch.Schema) && !dtables.OldDoltDocsSqlSchema.Equals(sch.Schema) {
			return fmt.Errorf("incorrect schema for dolt_docs table")
		}
	} else if doltdb.HasDoltPrefix(tableName) && !doltdb.IsFullTextTable(tableName) {
		return ErrReservedTableName.New(tableName)
	}

	if !doltdb.IsValidTableName(tableName) {
		return ErrInvalidTableName.New(tableName)
	}

	return db.createSqlTable(ctx, tableName, sch, collation)
}

// CreateIndexedTable creates a table with the name and schema given.
func (db Database) CreateIndexedTable(ctx *sql.Context, tableName string, sch sql.PrimaryKeySchema, idxDef sql.IndexDef, collation sql.CollationID) error {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}
	if strings.ToLower(tableName) == doltdb.DocTableName {
		// validate correct schema
		if !dtables.DoltDocsSqlSchema.Equals(sch.Schema) && !dtables.OldDoltDocsSqlSchema.Equals(sch.Schema) {
			return fmt.Errorf("incorrect schema for dolt_docs table")
		}
	} else if doltdb.HasDoltPrefix(tableName) {
		return ErrReservedTableName.New(tableName)
	}

	if !doltdb.IsValidTableName(tableName) {
		return ErrInvalidTableName.New(tableName)
	}

	return db.createIndexedSqlTable(ctx, tableName, sch, idxDef, collation)
}

// CreateFulltextTableNames returns a set of names that will be used to create Full-Text pseudo-index tables.
func (db Database) CreateFulltextTableNames(ctx *sql.Context, parentTableName string, parentIndexName string) (fulltext.IndexTableNames, error) {
	allTableNames, err := db.GetAllTableNames(ctx)
	if err != nil {
		return fulltext.IndexTableNames{}, err
	}
	var tablePrefix string
OuterLoop:
	for i := uint64(0); true; i++ {
		tablePrefix = strings.ToLower(fmt.Sprintf("dolt_%s_%s_%d", parentTableName, parentIndexName, i))
		for _, tableName := range allTableNames {
			if strings.HasPrefix(strings.ToLower(tableName), tablePrefix) {
				continue OuterLoop
			}
		}
		break
	}
	return fulltext.IndexTableNames{
		Config:      fmt.Sprintf("dolt_%s_fts_config", parentTableName),
		Position:    fmt.Sprintf("%s_fts_position", tablePrefix),
		DocCount:    fmt.Sprintf("%s_fts_doc_count", tablePrefix),
		GlobalCount: fmt.Sprintf("%s_fts_global_count", tablePrefix),
		RowCount:    fmt.Sprintf("%s_fts_row_count", tablePrefix),
	}, nil
}

// createSqlTable is the private version of CreateTable. It doesn't enforce any table name checks.
func (db Database) createSqlTable(ctx *sql.Context, tableName string, sch sql.PrimaryKeySchema, collation sql.CollationID) error {
	ws, err := db.GetWorkingSet(ctx)
	if err != nil {
		return err
	}
	root := ws.WorkingRoot()

	if exists, err := root.HasTable(ctx, tableName); err != nil {
		return err
	} else if exists {
		return sql.ErrTableAlreadyExists.New(tableName)
	}

	headRoot, err := db.GetHeadRoot(ctx)
	if err != nil {
		return err
	}

	doltSch, err := sqlutil.ToDoltSchema(ctx, root, tableName, sch, headRoot, collation)
	if err != nil {
		return err
	}

	// Prevent any tables that use Spatial Types as Primary Key from being created
	if schema.IsUsingSpatialColAsKey(doltSch) {
		return schema.ErrUsingSpatialKey.New(tableName)
	}

	// Prevent any tables that use BINARY, CHAR, VARBINARY, VARCHAR prefixes

	if schema.HasAutoIncrement(doltSch) {
		ait, err := db.gs.AutoIncrementTracker(ctx)
		if err != nil {
			return err
		}
		ait.AddNewTable(tableName)
	}

	return db.createDoltTable(ctx, tableName, root, doltSch)
}

// createIndexedSqlTable is the private version of createSqlTable. It doesn't enforce any table name checks.
func (db Database) createIndexedSqlTable(ctx *sql.Context, tableName string, sch sql.PrimaryKeySchema, idxDef sql.IndexDef, collation sql.CollationID) error {
	ws, err := db.GetWorkingSet(ctx)
	if err != nil {
		return err
	}
	root := ws.WorkingRoot()

	if exists, err := root.HasTable(ctx, tableName); err != nil {
		return err
	} else if exists {
		return sql.ErrTableAlreadyExists.New(tableName)
	}

	headRoot, err := db.GetHeadRoot(ctx)
	if err != nil {
		return err
	}

	doltSch, err := sqlutil.ToDoltSchema(ctx, root, tableName, sch, headRoot, collation)
	if err != nil {
		return err
	}

	// Prevent any tables that use Spatial Types as Primary Key from being created
	if schema.IsUsingSpatialColAsKey(doltSch) {
		return schema.ErrUsingSpatialKey.New(tableName)
	}

	// Prevent any tables that use BINARY, CHAR, VARBINARY, VARCHAR prefixes in Primary Key
	for _, idxCol := range idxDef.Columns {
		col := sch.Schema[sch.Schema.IndexOfColName(idxCol.Name)]
		if col.PrimaryKey && types.IsText(col.Type) && idxCol.Length > 0 {
			return sql.ErrUnsupportedIndexPrefix.New(col.Name)
		}
	}

	if schema.HasAutoIncrement(doltSch) {
		ait, err := db.gs.AutoIncrementTracker(ctx)
		if err != nil {
			return err
		}
		ait.AddNewTable(tableName)
	}

	return db.createDoltTable(ctx, tableName, root, doltSch)
}

// createDoltTable creates a table on the database using the given dolt schema while not enforcing table baseName checks.
func (db Database) createDoltTable(ctx *sql.Context, tableName string, root *doltdb.RootValue, doltSch schema.Schema) error {
	if exists, err := root.HasTable(ctx, tableName); err != nil {
		return err
	} else if exists {
		return sql.ErrTableAlreadyExists.New(tableName)
	}

	var conflictingTbls []string
	_ = doltSch.GetAllCols().Iter(func(tag uint64, col schema.Column) (stop bool, err error) {
		_, tbl, exists, err := root.GetTableByColTag(ctx, tag)
		if err != nil {
			return true, err
		}
		if exists && tbl != tableName {
			errStr := schema.ErrTagPrevUsed(tag, col.Name, tbl).Error()
			conflictingTbls = append(conflictingTbls, errStr)
		}
		return false, nil
	})

	if len(conflictingTbls) > 0 {
		return fmt.Errorf(strings.Join(conflictingTbls, "\n"))
	}

	newRoot, err := root.CreateEmptyTable(ctx, tableName, doltSch)
	if err != nil {
		return err
	}

	return db.SetRoot(ctx, newRoot)
}

// CreateTemporaryTable creates a table that only exists the length of a session.
func (db Database) CreateTemporaryTable(ctx *sql.Context, tableName string, pkSch sql.PrimaryKeySchema, collation sql.CollationID) error {
	if doltdb.HasDoltPrefix(tableName) {
		return ErrReservedTableName.New(tableName)
	}

	if !doltdb.IsValidTableName(tableName) {
		return ErrInvalidTableName.New(tableName)
	}

	tmp, err := NewTempTable(ctx, db.ddb, pkSch, tableName, db.Name(), db.editOpts, collation)
	if err != nil {
		return err
	}

	ds := dsess.DSessFromSess(ctx.Session)
	ds.AddTemporaryTable(ctx, db.Name(), tmp)
	return nil
}

// RenameTable implements sql.TableRenamer
func (db Database) RenameTable(ctx *sql.Context, oldName, newName string) error {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}
	root, err := db.GetRoot(ctx)

	if err != nil {
		return err
	}

	if doltdb.IsNonAlterableSystemTable(oldName) {
		return ErrSystemTableAlter.New(oldName)
	}

	if doltdb.HasDoltPrefix(newName) {
		return ErrReservedTableName.New(newName)
	}

	if !doltdb.IsValidTableName(newName) {
		return ErrInvalidTableName.New(newName)
	}

	if _, ok, _ := db.GetTableInsensitive(ctx, newName); ok {
		return sql.ErrTableAlreadyExists.New(newName)
	}

	newRoot, err := renameTable(ctx, root, oldName, newName)

	if err != nil {
		return err
	}

	return db.SetRoot(ctx, newRoot)
}

// GetViewDefinition implements sql.ViewDatabase
func (db Database) GetViewDefinition(ctx *sql.Context, viewName string) (sql.ViewDefinition, bool, error) {
	root, err := db.GetRoot(ctx)
	if err != nil {
		return sql.ViewDefinition{}, false, err
	}

	lwrViewName := strings.ToLower(viewName)
	switch {
	case strings.HasPrefix(lwrViewName, doltdb.DoltBlameViewPrefix):
		tableName := lwrViewName[len(doltdb.DoltBlameViewPrefix):]

		blameViewTextDef, err := dtables.NewBlameView(ctx, tableName, root)
		if err != nil {
			return sql.ViewDefinition{}, false, err
		}
		return sql.ViewDefinition{Name: viewName, TextDefinition: blameViewTextDef, CreateViewStatement: fmt.Sprintf("CREATE VIEW %s AS %s", viewName, blameViewTextDef)}, true, nil
	}

	key, err := doltdb.NewDataCacheKey(root)
	if err != nil {
		return sql.ViewDefinition{}, false, err
	}

	ds := dsess.DSessFromSess(ctx.Session)
	dbState, _, err := ds.LookupDbState(ctx, db.RevisionQualifiedName())
	if err != nil {
		return sql.ViewDefinition{}, false, err
	}

	if dbState.SessionCache().ViewsCached(key) {
		view, ok := dbState.SessionCache().GetCachedViewDefinition(key, viewName)
		return view, ok, nil
	}

	tbl, ok, err := db.GetTableInsensitive(ctx, doltdb.SchemasTableName)
	if err != nil {
		return sql.ViewDefinition{}, false, err
	}
	if !ok {
		dbState.SessionCache().CacheViews(key, nil)
		return sql.ViewDefinition{}, false, nil
	}

	views, viewDef, found, err := getViewDefinitionFromSchemaFragmentsOfView(ctx, tbl.(*WritableDoltTable), viewName)
	if err != nil {
		return sql.ViewDefinition{}, false, err
	}

	dbState.SessionCache().CacheViews(key, views)

	return viewDef, found, nil
}

func getViewDefinitionFromSchemaFragmentsOfView(ctx *sql.Context, tbl *WritableDoltTable, viewName string) ([]sql.ViewDefinition, sql.ViewDefinition, bool, error) {
	fragments, err := getSchemaFragmentsOfType(ctx, tbl, viewFragment)
	if err != nil {
		return nil, sql.ViewDefinition{}, false, err
	}

	var found = false
	var viewDef sql.ViewDefinition
	var views = make([]sql.ViewDefinition, len(fragments))
	for i, fragment := range fragments {
		cv, err := sqlparser.ParseWithOptions(fragments[i].fragment,
			sql.NewSqlModeFromString(fragment.sqlMode).ParserOptions())
		if err != nil {
			return nil, sql.ViewDefinition{}, false, err
		}

		createView, ok := cv.(*sqlparser.DDL)
		if ok {
			selectStr := fragments[i].fragment[createView.SubStatementPositionStart:createView.SubStatementPositionEnd]
			views[i] = sql.ViewDefinition{Name: fragments[i].name, TextDefinition: selectStr,
				CreateViewStatement: fragments[i].fragment, SqlMode: fragment.sqlMode}
		} else {
			views[i] = sql.ViewDefinition{Name: fragments[i].name, TextDefinition: fragments[i].fragment, CreateViewStatement: fmt.Sprintf("CREATE VIEW %s AS %s", fragments[i].name, fragments[i].fragment)}
		}

		if strings.ToLower(fragment.name) == strings.ToLower(viewName) {
			found = true
			viewDef = views[i]
		}
	}

	return views, viewDef, found, nil
}

// AllViews implements sql.ViewDatabase
func (db Database) AllViews(ctx *sql.Context) ([]sql.ViewDefinition, error) {
	tbl, ok, err := db.GetTableInsensitive(ctx, doltdb.SchemasTableName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	views, _, _, err := getViewDefinitionFromSchemaFragmentsOfView(ctx, tbl.(*WritableDoltTable), "")
	if err != nil {
		return nil, err
	}

	return views, nil
}

// CreateView implements sql.ViewCreator. Persists the view in the dolt database, so
// it can exist in a sql session later. Returns sql.ErrExistingView if a view
// with that name already exists.
func (db Database) CreateView(ctx *sql.Context, name string, selectStatement, createViewStmt string) error {
	err := sql.ErrExistingView.New(db.Name(), name)
	return db.addFragToSchemasTable(ctx, "view", name, createViewStmt, time.Unix(0, 0).UTC(), err)
}

// DropView implements sql.ViewDropper. Removes a view from persistence in the
// dolt database. Returns sql.ErrNonExistingView if the view did not
// exist.
func (db Database) DropView(ctx *sql.Context, name string) error {
	err := sql.ErrViewDoesNotExist.New(db.baseName, name)
	return db.dropFragFromSchemasTable(ctx, "view", name, err)
}

// GetTriggers implements sql.TriggerDatabase.
func (db Database) GetTriggers(ctx *sql.Context) ([]sql.TriggerDefinition, error) {
	tbl, ok, err := db.GetTableInsensitive(ctx, doltdb.SchemasTableName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	frags, err := getSchemaFragmentsOfType(ctx, tbl.(*WritableDoltTable), triggerFragment)
	if err != nil {
		return nil, err
	}

	var triggers []sql.TriggerDefinition
	for _, frag := range frags {
		triggers = append(triggers, sql.TriggerDefinition{
			Name:            frag.name,
			CreateStatement: frag.fragment,
			CreatedAt:       frag.created,
			SqlMode:         frag.sqlMode,
		})
	}
	if err != nil {
		return nil, err
	}

	return triggers, nil
}

// CreateTrigger implements sql.TriggerDatabase.
func (db Database) CreateTrigger(ctx *sql.Context, definition sql.TriggerDefinition) error {
	return db.addFragToSchemasTable(ctx,
		"trigger",
		definition.Name,
		definition.CreateStatement,
		definition.CreatedAt,
		fmt.Errorf("triggers `%s` already exists", definition.Name), //TODO: add a sql error and return that instead
	)
}

// DropTrigger implements sql.TriggerDatabase.
func (db Database) DropTrigger(ctx *sql.Context, name string) error {
	//TODO: add a sql error and use that as the param error instead
	return db.dropFragFromSchemasTable(ctx, "trigger", name, sql.ErrTriggerDoesNotExist.New(name))
}

// GetEvent implements sql.EventDatabase.
func (db Database) GetEvent(ctx *sql.Context, name string) (sql.EventDefinition, bool, error) {
	tbl, ok, err := db.GetTableInsensitive(ctx, doltdb.SchemasTableName)
	if err != nil {
		return sql.EventDefinition{}, false, err
	}
	if !ok {
		return sql.EventDefinition{}, false, nil
	}

	frags, err := getSchemaFragmentsOfType(ctx, tbl.(*WritableDoltTable), eventFragment)
	if err != nil {
		return sql.EventDefinition{}, false, err
	}

	for _, frag := range frags {
		if strings.ToLower(frag.name) == strings.ToLower(name) {
			return sql.EventDefinition{
				Name:            frag.name,
				CreateStatement: frag.fragment,
				CreatedAt:       frag.created,
				SqlMode:         frag.sqlMode,
			}, true, nil
		}
	}
	return sql.EventDefinition{}, false, nil
}

// GetEvents implements sql.EventDatabase.
func (db Database) GetEvents(ctx *sql.Context) ([]sql.EventDefinition, error) {
	tbl, ok, err := db.GetTableInsensitive(ctx, doltdb.SchemasTableName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	frags, err := getSchemaFragmentsOfType(ctx, tbl.(*WritableDoltTable), eventFragment)
	if err != nil {
		return nil, err
	}

	var events []sql.EventDefinition
	for _, frag := range frags {
		events = append(events, sql.EventDefinition{
			Name:            frag.name,
			CreateStatement: frag.fragment,
			CreatedAt:       frag.created,
			SqlMode:         frag.sqlMode,
		})
	}
	return events, nil
}

// SaveEvent implements sql.EventDatabase.
func (db Database) SaveEvent(ctx *sql.Context, ed sql.EventDefinition) error {
	return db.addFragToSchemasTable(ctx,
		eventFragment,
		ed.Name,
		ed.CreateStatement,
		ed.CreatedAt,
		sql.ErrEventAlreadyExists.New(ed.Name),
	)
}

// DropEvent implements sql.EventDatabase.
func (db Database) DropEvent(ctx *sql.Context, name string) error {
	return db.dropFragFromSchemasTable(ctx, eventFragment, name, sql.ErrEventDoesNotExist.New(name))
}

// UpdateEvent implements sql.EventDatabase.
func (db Database) UpdateEvent(ctx *sql.Context, originalName string, ed sql.EventDefinition) error {
	// TODO: any EVENT STATUS change should also update the branch-specific event scheduling
	err := db.DropEvent(ctx, originalName)
	if err != nil {
		return err
	}
	return db.SaveEvent(ctx, ed)
}

// GetStoredProcedure implements sql.StoredProcedureDatabase.
func (db Database) GetStoredProcedure(ctx *sql.Context, name string) (sql.StoredProcedureDetails, bool, error) {
	procedures, err := DoltProceduresGetAll(ctx, db, strings.ToLower(name))
	if err != nil {
		return sql.StoredProcedureDetails{}, false, nil
	}
	if len(procedures) == 1 {
		return procedures[0], true, nil
	}
	return sql.StoredProcedureDetails{}, false, nil
}

// GetStoredProcedures implements sql.StoredProcedureDatabase.
func (db Database) GetStoredProcedures(ctx *sql.Context) ([]sql.StoredProcedureDetails, error) {
	return DoltProceduresGetAll(ctx, db, "")
}

// SaveStoredProcedure implements sql.StoredProcedureDatabase.
func (db Database) SaveStoredProcedure(ctx *sql.Context, spd sql.StoredProcedureDetails) error {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}
	return DoltProceduresAddProcedure(ctx, db, spd)
}

// DropStoredProcedure implements sql.StoredProcedureDatabase.
func (db Database) DropStoredProcedure(ctx *sql.Context, name string) error {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}
	return DoltProceduresDropProcedure(ctx, db, name)
}

func (db Database) addFragToSchemasTable(ctx *sql.Context, fragType, name, definition string, created time.Time, existingErr error) (err error) {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}
	tbl, err := getOrCreateDoltSchemasTable(ctx, db)
	if err != nil {
		return err
	}

	_, exists, err := fragFromSchemasTable(ctx, tbl, fragType, name)
	if err != nil {
		return err
	}
	if exists {
		return existingErr
	}

	// Insert the new row into the db
	inserter := tbl.Inserter(ctx)
	defer func() {
		cErr := inserter.Close(ctx)
		if err == nil {
			err = cErr
		}
	}()
	// Encode createdAt time to JSON
	extra := Extra{
		CreatedAt: created.Unix(),
	}
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		return err
	}

	sqlMode := sql.LoadSqlMode(ctx)

	return inserter.Insert(ctx, sql.Row{fragType, name, definition, extraJSON, sqlMode.String()})
}

func (db Database) dropFragFromSchemasTable(ctx *sql.Context, fragType, name string, missingErr error) error {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}

	stbl, found, err := db.GetTableInsensitive(ctx, doltdb.SchemasTableName)
	if err != nil {
		return err
	}
	if !found {
		return missingErr
	}

	tbl := stbl.(*WritableDoltTable)
	row, exists, err := fragFromSchemasTable(ctx, tbl, fragType, name)
	if err != nil {
		return err
	}
	if !exists {
		return missingErr
	}
	deleter := tbl.Deleter(ctx)
	err = deleter.Delete(ctx, row)
	if err != nil {
		return err
	}

	err = deleter.Close(ctx)
	if err != nil {
		return err
	}

	// If the dolt schemas table is now empty, drop it entirely. This is necessary to prevent the creation and
	// immediate dropping of views or triggers, when none previously existed, from changing the database state.
	return db.dropTableIfEmpty(ctx, doltdb.SchemasTableName)
}

// dropTableIfEmpty drops the table named if it exists and has at least one row.
func (db Database) dropTableIfEmpty(ctx *sql.Context, tableName string) error {
	stbl, found, err := db.GetTableInsensitive(ctx, tableName)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	table, err := stbl.(*WritableDoltTable).DoltTable.DoltTable(ctx)
	if err != nil {
		return err
	}

	rows, err := table.GetRowData(ctx)
	if err != nil {
		return err
	}

	numRows, err := rows.Count()
	if err != nil {
		return err
	}

	if numRows == 0 {
		return db.dropTable(ctx, tableName)
	}

	return nil
}

// GetAllTemporaryTables returns all temporary tables
func (db Database) GetAllTemporaryTables(ctx *sql.Context) ([]sql.Table, error) {
	sess := dsess.DSessFromSess(ctx.Session)
	return sess.GetAllTemporaryTables(ctx, db.Name())
}

// GetCollation implements the interface sql.CollatedDatabase.
func (db Database) GetCollation(ctx *sql.Context) sql.CollationID {
	root, err := db.GetRoot(ctx)
	if err != nil {
		return sql.Collation_Default
	}
	collation, err := root.GetCollation(ctx)
	if err != nil {
		return sql.Collation_Default
	}
	return sql.CollationID(collation)
}

// SetCollation implements the interface sql.CollatedDatabase.
func (db Database) SetCollation(ctx *sql.Context, collation sql.CollationID) error {
	if err := dsess.CheckAccessForDb(ctx, db, branch_control.Permissions_Write); err != nil {
		return err
	}
	if collation == sql.Collation_Unspecified {
		collation = sql.Collation_Default
	}
	root, err := db.GetRoot(ctx)
	if err != nil {
		return err
	}
	newRoot, err := root.SetCollation(ctx, schema.Collation(collation))
	if err != nil {
		return err
	}
	return db.SetRoot(ctx, newRoot)
}

// noopRepoStateWriter is a minimal implementation of RepoStateWriter that does nothing
type noopRepoStateWriter struct{}

func (n noopRepoStateWriter) UpdateStagedRoot(ctx context.Context, newRoot *doltdb.RootValue) error {
	return nil
}

func (n noopRepoStateWriter) UpdateWorkingRoot(ctx context.Context, newRoot *doltdb.RootValue) error {
	return nil
}

func (n noopRepoStateWriter) SetCWBHeadRef(ctx context.Context, marshalableRef ref.MarshalableRef) error {
	return nil
}

func (n noopRepoStateWriter) AddRemote(r env.Remote) error {
	return nil
}

func (n noopRepoStateWriter) AddBackup(r env.Remote) error {
	return nil
}

func (n noopRepoStateWriter) RemoveRemote(ctx context.Context, name string) error {
	return nil
}

func (n noopRepoStateWriter) RemoveBackup(ctx context.Context, name string) error {
	return nil
}

func (n noopRepoStateWriter) TempTableFilesDir() (string, error) {
	return "", nil
}

func (n noopRepoStateWriter) UpdateBranch(name string, new env.BranchConfig) error {
	return nil
}
