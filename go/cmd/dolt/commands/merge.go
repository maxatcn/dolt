// Copyright 2019 Dolthub, Inc.
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

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/fatih/color"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"

	"github.com/dolthub/dolt/go/cmd/dolt/cli"
	"github.com/dolthub/dolt/go/cmd/dolt/commands/engine"
	"github.com/dolthub/dolt/go/cmd/dolt/errhand"
	eventsapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/eventsapi/v1alpha1"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/merge"
	"github.com/dolthub/dolt/go/libraries/utils/argparser"
	"github.com/dolthub/dolt/go/store/util/outputpager"
)

var mergeDocs = cli.CommandDocumentationContent{
	ShortDesc: "Join two or more development histories together",
	LongDesc: `Incorporates changes from the named commits (since the time their histories diverged from the current branch) into the current branch.

The second syntax ({{.LessThan}}dolt merge --abort{{.GreaterThan}}) can only be run after the merge has resulted in conflicts. dolt merge {{.EmphasisLeft}}--abort{{.EmphasisRight}} will abort the merge process and try to reconstruct the pre-merge state. However, if there were uncommitted changes when the merge started (and especially if those changes were further modified after the merge was started), dolt merge {{.EmphasisLeft}}--abort{{.EmphasisRight}} will in some cases be unable to reconstruct the original (pre-merge) changes. Therefore: 

{{.LessThan}}Warning{{.GreaterThan}}: Running dolt merge with non-trivial uncommitted changes is discouraged: while possible, it may leave you in a state that is hard to back out of in the case of a conflict.
`,

	Synopsis: []string{
		"[--squash] {{.LessThan}}branch{{.GreaterThan}}",
		"--no-ff [-m message] {{.LessThan}}branch{{.GreaterThan}}",
		"--abort",
	},
}

type MergeCmd struct{}

// Name returns the name of the Dolt cli command. This is what is used on the command line to invoke the command
func (cmd MergeCmd) Name() string {
	return "merge"
}

// Description returns a description of the command
func (cmd MergeCmd) Description() string {
	return "Merge a branch."
}

func (cmd MergeCmd) Docs() *cli.CommandDocumentation {
	ap := cli.CreateMergeArgParser()
	return cli.NewCommandDocumentation(mergeDocs, ap)
}

func (cmd MergeCmd) ArgParser() *argparser.ArgParser {
	return cli.CreateMergeArgParser()
}

// EventType returns the type of the event to log
func (cmd MergeCmd) EventType() eventsapi.ClientEventType {
	return eventsapi.ClientEventType_MERGE
}

func (cmd MergeCmd) RequiresRepo() bool {
	return false
}

// Exec executes the command
func (cmd MergeCmd) Exec(ctx context.Context, commandStr string, args []string, dEnv *env.DoltEnv, cliCtx cli.CliContext) int {
	ap := cli.CreateMergeArgParser()
	help, usage := cli.HelpAndUsagePrinters(cli.CommandDocsForCommandString(commandStr, mergeDocs, ap))
	apr := cli.ParseArgsOrDie(ap, args, help)

	queryist, sqlCtx, closeFunc, err := cliCtx.QueryEngine(ctx)
	if err != nil {
		cli.Println(err.Error())
		return 1
	}
	if closeFunc != nil {
		defer closeFunc()
	}

	if _, ok := queryist.(*engine.SqlEngine); !ok {
		msg := fmt.Sprintf(cli.RemoteUnsupportedMsg, commandStr)
		cli.Println(msg)
		return 1
	}

	ok := validateDoltMergeArgs(apr, usage, cliCtx)
	if ok != 0 {
		return 1
	}

	// allows merges that create conflicts to stick
	_, _, err = queryist.Query(sqlCtx, "set @@dolt_force_transaction_commit = 1")
	if err != nil {
		cli.Println(err.Error())
		return 1
	}

	query, err := constructInterpolatedDoltMergeQuery(apr, cliCtx)
	if err != nil {
		cli.Println(err.Error())
		return 1
	}
	schema, rowIter, err := queryist.Query(sqlCtx, query)
	if err != nil {
		cli.Println(err.Error())
		return 1
	}
	// if merge is called with '--no-commit', we need to commit the sql transaction or the staged changes will be lost
	_, _, err = queryist.Query(sqlCtx, "COMMIT")
	if err != nil {
		cli.Println(err.Error())
		return 1
	}

	// check for fast-forward merge
	rows, err := sql.RowIterToRows(sqlCtx, schema, rowIter)
	if err != nil {
		cli.Println("merge finished, but failed to check for fast-forward")
		cli.Println(err.Error())
		return 0
	}
	if rows != nil && rows[0][1].(int64) == 1 {
		cli.Println("Fast-forward")
	}

	// calculate merge stats
	if !apr.Contains(cli.AbortParam) {
		//todo: refs with the `remotes/` prefix will fail to get a hash
		mergeHash, mergeHashErr := getHashOf(queryist, sqlCtx, apr.Arg(0))
		if mergeHashErr != nil {
			cli.Println("merge finished, but failed to get hash of merge ref")
			cli.Println(mergeHashErr.Error())
		}
		headHash, headhHashErr := getHashOf(queryist, sqlCtx, "HEAD")
		if headhHashErr != nil {
			cli.Println("merge finished, but failed to get hash of HEAD")
			cli.Println(headhHashErr.Error())
		}
		if mergeHashErr == nil && headhHashErr == nil {
			cli.Println("Updating", headHash+".."+mergeHash)
		}

		if apr.Contains(cli.SquashParam) {
			cli.Println("Squash commit -- not updating HEAD")
		}

		if apr.Contains(cli.NoCommitFlag) {
			cli.Println("Automatic merge went well; stopped before committing as requested")
		}

		mergeStats := make(map[string]*merge.MergeStats)
		mergeStats, noConflicts, err := calculateMergeConflicts(queryist, sqlCtx, mergeStats)
		if err != nil {
			cli.Println("merge finished, but could not calculate conflicts")
			cli.Println(err.Error())
			return 1
		}

		if noConflicts {
			if apr.Contains(cli.NoCommitFlag) {
				mergeStats, err = calculateMergeStats(queryist, sqlCtx, mergeStats, "HEAD", "STAGED")
			} else {
				mergeStats, err = calculateMergeStats(queryist, sqlCtx, mergeStats, "HEAD^1", "HEAD")
			}
			if err != nil {
				if err.Error() == "Already up to date." || err.Error() == "error: unable to get diff summary from HEAD^1 to HEAD: invalid ancestor spec" {
					cli.Println("Already up to date.")
					return 0
				}
				cli.Println("merge successful, but could not calculate stats")
				cli.Println(err.Error())
				return 1
			}
		}

		if !apr.Contains(cli.NoCommitFlag) && !apr.Contains(cli.NoFFParam) {
			commit, err := getCommitInfo(queryist, sqlCtx, "HEAD")
			if err != nil {
				cli.Println("merge finished, but failed to get commit info")
				cli.Println(err.Error())
				return 0
			}
			if cli.ExecuteWithStdioRestored != nil {
				cli.ExecuteWithStdioRestored(func() {
					pager := outputpager.Start()
					defer pager.Stop()

					PrintCommitInfo(pager, 0, false, "auto", commit)
				})
			}
		}

		hasConflicts, hasConstraintViolations := printSuccessStats(mergeStats)
		return handleMergeErr(sqlCtx, queryist, nil, hasConflicts, hasConstraintViolations, usage)
	}

	return 0
}

// validateDoltMergeArgs checks if the arguments passed to 'dolt merge' are valid
func validateDoltMergeArgs(apr *argparser.ArgParseResults, usage cli.UsagePrinter, cliCtx cli.CliContext) int {
	if apr.ContainsAll(cli.SquashParam, cli.NoFFParam) {
		cli.PrintErrf("error: Flags '--%s' and '--%s' cannot be used together.\n", cli.SquashParam, cli.NoFFParam)
		return 1
	}

	// This command may create a commit, so we need user identity
	if !cli.CheckUserNameAndEmail(cliCtx.Config()) {
		bdr := errhand.BuildDError("Could not determine name and/or email.")
		bdr.AddDetails("Log into DoltHub: dolt login")
		bdr.AddDetails("OR add name to config: dolt config [--global|--local] --add %[1]s \"FIRST LAST\"", env.UserNameKey)
		bdr.AddDetails("OR add email to config: dolt config [--global|--local] --add %[1]s \"EMAIL_ADDRESS\"", env.UserEmailKey)

		return HandleVErrAndExitCode(bdr.Build(), usage)
	}

	if apr.Contains(cli.SquashParam) {
		if apr.NArg() != 1 {
			usage()
			return 1
		}
	} else if apr.Contains(cli.NoFFParam) {
		if apr.NArg() == 0 || apr.NArg() > 2 {
			usage()
			return 1
		}
	} else if apr.Contains(cli.AbortParam) {
		if apr.NArg() != 0 {
			usage()
			return 1
		}
	}

	if apr.ContainsAll(cli.CommitFlag, cli.NoCommitFlag) {
		return HandleVErrAndExitCode(errhand.BuildDError("cannot define both 'commit' and 'no-commit' flags at the same time").Build(), usage)
	}
	if !apr.Contains(cli.AbortParam) && apr.NArg() == 0 {
		usage()
		return 1
	}

	return 0
}

// constructInterpolatedDoltMergeQuery generates the sql query necessary to call the DOLT_MERGE() stored procedure.
// Also interpolates this query to prevent sql injection.
func constructInterpolatedDoltMergeQuery(apr *argparser.ArgParseResults, cliCtx cli.CliContext) (string, error) {
	var params []interface{}

	var buffer bytes.Buffer
	var first bool
	first = true
	buffer.WriteString("CALL DOLT_MERGE(")

	writeToBuffer := func(s string, param bool) {
		if !first {
			buffer.WriteString(", ")
		}
		if !param {
			buffer.WriteString("'")
		}
		buffer.WriteString(s)
		if !param {
			buffer.WriteString("'")
		}
		first = false
	}

	if apr.Contains(cli.SquashParam) {
		writeToBuffer("--squash", false)
		writeToBuffer("?", true)
		params = append(params, apr.Arg(0))
	} else if apr.Contains(cli.NoFFParam) {
		writeToBuffer("--no-ff", false)
	} else if apr.Contains(cli.AbortParam) {
		writeToBuffer("--abort", false)
	}

	if apr.Contains(cli.CommitFlag) {
		writeToBuffer("--commit", false)
	}
	if apr.Contains(cli.NoCommitFlag) {
		writeToBuffer("--no-commit", false)
	}
	if apr.Contains(cli.NoEditFlag) {
		writeToBuffer("--no-edit", false)
	}

	writeToBuffer("--author", false)
	var author string
	if apr.Contains(cli.AuthorParam) {
		author, _ = apr.GetValue(cli.AuthorParam)
	} else {
		name, email, err := env.GetNameAndEmail(cliCtx.Config())
		if err != nil {
			return "", err
		}
		author = name + " <" + email + ">"
	}
	writeToBuffer("?", true)
	params = append(params, author)

	if apr.Contains(cli.DateParam) {
		writeToBuffer("--date", false)
		writeToBuffer("?", true)
		date, ok := apr.GetValue(cli.DateParam)
		if !ok {
			return "", errors.New("Could not retrieve date")
		}
		params = append(params, date)
	}
	if apr.Contains(cli.MessageArg) {
		writeToBuffer("-m", false)
		writeToBuffer("?", true)
		msg, ok := apr.GetValue(cli.MessageArg)
		if !ok {
			return "", errors.New("Could not retrieve message")
		}
		params = append(params, msg)
	}

	if !apr.Contains(cli.AbortParam) && !apr.Contains(cli.SquashParam) {
		writeToBuffer("?", true)
		params = append(params, apr.Arg(0))
	}

	buffer.WriteString(")")

	interpolatedQuery, err := dbr.InterpolateForDialect(buffer.String(), params, dialect.MySQL)
	if err != nil {
		return "", err
	}

	return interpolatedQuery, nil
}

// calculateMergeConflicts calculates the count of conflicts that occurred during the merge. Returns a map of table name to MergeStats,
// a bool indicating whether there were any conflicts, and a bool indicating whether calculation was successful.
func calculateMergeConflicts(queryist cli.Queryist, sqlCtx *sql.Context, mergeStats map[string]*merge.MergeStats) (map[string]*merge.MergeStats, bool, error) {
	dataConflicts, err := GetRowsForSql(queryist, sqlCtx, "SELECT * FROM dolt_conflicts")
	if err != nil {
		return nil, false, err
	}
	for _, conflict := range dataConflicts {
		tableName := conflict[0].(string)
		if ok := mergeStats[tableName]; ok != nil {
			mergeStats[tableName].DataConflicts = int(conflict[1].(uint64))
		} else {
			mergeStats[tableName] = &merge.MergeStats{DataConflicts: int(conflict[1].(uint64))}
		}
	}

	schemaConflicts, err := GetRowsForSql(queryist, sqlCtx, "SELECT * FROM dolt_schema_conflicts")
	if err != nil {
		return nil, false, err
	}
	for _, conflict := range schemaConflicts {
		tableName := conflict[0].(string)
		if ok := mergeStats[tableName]; ok != nil {
			mergeStats[tableName].SchemaConflicts = 1
		} else {
			mergeStats[tableName] = &merge.MergeStats{SchemaConflicts: 1}
		}
	}

	constraintViolations, err := GetRowsForSql(queryist, sqlCtx, "SELECT * FROM dolt_constraint_violations")
	if err != nil {
		return nil, false, err
	}
	for _, conflict := range constraintViolations {
		tableName := conflict[0].(string)
		if ok := mergeStats[tableName]; ok != nil {
			mergeStats[tableName].ConstraintViolations = int(conflict[1].(uint64))
		} else {
			mergeStats[tableName] = &merge.MergeStats{ConstraintViolations: int(conflict[1].(uint64))}
		}
	}

	return mergeStats, dataConflicts == nil && schemaConflicts == nil && constraintViolations == nil, nil
}

// calculateMergeStats calculates the table operations and row operations that occurred during the merge. Returns a map of
// table name to MergeStats, and a bool indicating whether calculation was successful.
func calculateMergeStats(queryist cli.Queryist, sqlCtx *sql.Context, mergeStats map[string]*merge.MergeStats, fromRef, toRef string) (map[string]*merge.MergeStats, error) {
	diffSummaries, err := getDiffSummariesBetweenRefs(queryist, sqlCtx, fromRef, toRef)
	if err != nil {
		return nil, err
	}

	diffStats := make(map[string]diffStatistics)

	var allUnmodified = true
	// get table operations
	for _, summary := range diffSummaries {
		// We want to ignore all statistics for Full-Text tables
		if doltdb.IsFullTextTable(summary.TableName) {
			continue
		}
		if summary.DiffType == "added" {
			allUnmodified = false
			mergeStats[summary.TableName] = &merge.MergeStats{
				Operation: merge.TableAdded,
			}
		} else if summary.DiffType == "dropped" {
			allUnmodified = false
			mergeStats[summary.TableName] = &merge.MergeStats{
				Operation: merge.TableRemoved,
			}
		} else if summary.DiffType == "modified" || summary.DiffType == "renamed" {
			allUnmodified = false
			mergeStats[summary.TableName] = &merge.MergeStats{
				Operation: merge.TableModified,
			}
			tableStats, err := getTableDiffStats(queryist, sqlCtx, summary.TableName, fromRef, toRef)
			if err != nil {
				return nil, err
			}
			if tableStats != nil && len(tableStats) > 0 {
				diffStats[tableStats[0].TableName] = tableStats[0]
			}
		} else {
			mergeStats[summary.TableName] = &merge.MergeStats{
				Operation: merge.TableUnmodified,
			}
		}
	}

	if allUnmodified {
		return nil, errors.New("Already up to date.")
	}

	// get row stats
	for tableName, diffStat := range diffStats {
		mergeStats[tableName].Adds = int(diffStat.RowsAdded)
		mergeStats[tableName].Deletes = int(diffStat.RowsDeleted)
		mergeStats[tableName].Modifications = int(diffStat.RowsModified)
	}

	return mergeStats, nil
}

func validateMergeSpec(ctx context.Context, spec *merge.MergeSpec) errhand.VerboseError {
	if spec.HeadH == spec.MergeH {
		//TODO - why is this different for merge/pull?
		// cli.Println("Already up to date.")
		cli.Println("Everything up-to-date.")
		return nil

	}
	cli.Println("Updating", spec.HeadH.String()+".."+spec.MergeH.String())

	if spec.Squash {
		cli.Println("Squash commit -- not updating HEAD")
	}
	if len(spec.StompedTblNames) != 0 {
		bldr := errhand.BuildDError("error: Your local changes to the following tables would be overwritten by merge:")
		for _, tName := range spec.StompedTblNames {
			bldr.AddDetails(tName)
		}
		bldr.AddDetails("Please commit your changes before you merge.")
		return bldr.Build()
	}

	if ok, err := spec.HeadC.CanFastForwardTo(ctx, spec.MergeC); ok {
		ancRoot, err := spec.HeadC.GetRootValue(ctx)
		if err != nil {
			return errhand.VerboseErrorFromError(err)
		}
		mergedRoot, err := spec.MergeC.GetRootValue(ctx)
		if err != nil {
			return errhand.VerboseErrorFromError(err)
		}
		if _, err := merge.MayHaveConstraintViolations(ctx, ancRoot, mergedRoot); err != nil {
			return errhand.VerboseErrorFromError(err)
		}
		if !spec.Noff {
			cli.Println("Fast-forward")
		}
	} else if err == doltdb.ErrUpToDate || err == doltdb.ErrIsAhead {
		cli.Println("Already up to date.")
	}
	return nil
}

func abortMerge(ctx context.Context, doltEnv *env.DoltEnv) errhand.VerboseError {
	roots, err := doltEnv.Roots(ctx)
	if err != nil {
		return errhand.VerboseErrorFromError(err)
	}

	roots, err = actions.CheckoutAllTables(ctx, roots)
	if err == nil {
		err = doltEnv.AbortMerge(ctx)

		if err == nil {
			return nil
		}
	}

	err = doltEnv.UpdateWorkingRoot(ctx, roots.Working)
	if err != nil {
		return errhand.VerboseErrorFromError(err)
	}

	return errhand.BuildDError("fatal: failed to revert changes").AddCause(err).Build()
}

// printSuccessStats returns whether there are conflicts or constraint violations.
func printSuccessStats(tblToStats map[string]*merge.MergeStats) (conflicts bool, constraintViolations bool) {
	printModifications(tblToStats)
	printAdditions(tblToStats)
	printDeletions(tblToStats)
	return printConflictsAndViolations(tblToStats)
}

func printAdditions(tblToStats map[string]*merge.MergeStats) {
	for tblName, stats := range tblToStats {
		if stats.Operation == merge.TableAdded {
			cli.Println(tblName, "added")
		}
	}
}

func printDeletions(tblToStats map[string]*merge.MergeStats) {
	for tblName, stats := range tblToStats {
		if stats.Operation == merge.TableRemoved {
			cli.Println(tblName, "deleted")
		}
	}
}

func printConflictsAndViolations(tblToStats map[string]*merge.MergeStats) (conflicts bool, constraintViolations bool) {
	hasConflicts := false
	hasConstraintViolations := false
	for tblName, stats := range tblToStats {
		if stats.HasArtifacts() {
			cli.Println("Auto-merging", tblName)
			if stats.HasDataConflicts() {
				cli.Println("CONFLICT (content): Merge conflict in", tblName)
				hasConflicts = true
			}
			if stats.HasSchemaConflicts() {
				cli.Println("CONFLICT (schema): Merge conflict in", tblName)
				hasConflicts = true
			}
			if stats.HasConstraintViolations() {
				cli.Println("CONSTRAINT VIOLATION (content): Merge created constraint violation in", tblName)
				hasConstraintViolations = true
			}
		}
	}

	return hasConflicts, hasConstraintViolations
}

func printModifications(tblToStats map[string]*merge.MergeStats) {
	maxNameLen := 0
	maxModCount := 0
	rowsAdded := 0
	rowsDeleted := 0
	rowsChanged := 0
	var tbls []string
	for tblName, stats := range tblToStats {
		if stats.Operation == merge.TableModified && stats.DataConflicts == 0 && stats.ConstraintViolations == 0 {
			tbls = append(tbls, tblName)
			nameLen := len(tblName)
			modCount := stats.Adds + stats.Modifications + stats.Deletes + stats.DataConflicts

			if nameLen > maxNameLen {
				maxNameLen = nameLen
			}

			if modCount > maxModCount {
				maxModCount = modCount
			}

			rowsAdded += stats.Adds
			rowsChanged += stats.Modifications + stats.DataConflicts
			rowsDeleted += stats.Deletes
		}
	}

	if len(tbls) == 0 {
		return
	}

	sort.Strings(tbls)
	modCountStrLen := len(strconv.FormatInt(int64(maxModCount), 10))
	format := fmt.Sprintf("%%-%ds | %%-%ds %%s", maxNameLen, modCountStrLen)

	for _, tbl := range tbls {
		stats := tblToStats[tbl]
		if stats.Operation == merge.TableModified {
			modCount := stats.Adds + stats.Modifications + stats.Deletes + stats.DataConflicts
			modCountStr := strconv.FormatInt(int64(modCount), 10)
			visualizedChanges := visualizeChangeTypes(stats, maxModCount)

			cli.Println(fmt.Sprintf(format, tbl, modCountStr, visualizedChanges))
		}
	}

	details := fmt.Sprintf("%d tables changed, %d rows added(+), %d rows modified(*), %d rows deleted(-)", len(tbls), rowsAdded, rowsChanged, rowsDeleted)
	cli.Println(details)
}

func visualizeChangeTypes(stats *merge.MergeStats, maxMods int) string {
	const maxVisLen = 30 //can be a bit longer due to min len and rounding

	resultStr := ""
	if stats.Adds > 0 {
		addLen := int(maxVisLen * (float64(stats.Adds) / float64(maxMods)))
		if addLen > stats.Adds {
			addLen = stats.Adds
		}
		addStr := fillStringWithChar('+', addLen)
		resultStr += color.GreenString(addStr)
	}

	if stats.Modifications > 0 {
		modLen := int(maxVisLen * (float64(stats.Modifications) / float64(maxMods)))
		if modLen > stats.Modifications {
			modLen = stats.Modifications
		}
		modStr := fillStringWithChar('*', modLen)
		resultStr += color.YellowString(modStr)
	}

	if stats.Deletes > 0 {
		delLen := int(maxVisLen * (float64(stats.Deletes) / float64(maxMods)))
		if delLen > stats.Deletes {
			delLen = stats.Deletes
		}
		delStr := fillStringWithChar('-', delLen)
		resultStr += color.GreenString(delStr)
	}

	return resultStr
}

func fillStringWithChar(ch rune, strLen int) string {
	if strLen == 0 {
		strLen = 1
	}

	runes := make([]rune, strLen)
	for i := 0; i < strLen; i++ {
		runes[i] = ch
	}

	return string(runes)
}

func handleMergeErr(sqlCtx *sql.Context, queryist cli.Queryist, mergeErr error, hasConflicts, hasConstraintViolations bool, usage cli.UsagePrinter) int {
	unmergedTables, err := GetRowsForSql(queryist, sqlCtx, "select unmerged_tables from dolt_merge_status")
	if err != nil {
		cli.PrintErrln(err.Error())
		return 1
	}
	unmergedCnt := 0
	if unmergedTables[0][0] != nil {
		tableNames := unmergedTables[0][0].(string)
		unmergedCnt = len(strings.Split(tableNames, ", "))
	}

	if hasConflicts && hasConstraintViolations {
		cli.Printf("Automatic merge failed; %d table(s) are unmerged.\n"+
			"Fix conflicts and constraint violations and then commit the result.\n"+
			"Use 'dolt conflicts' to investigate and resolve conflicts.\n", unmergedCnt)
	} else if hasConflicts {
		cli.Printf("Automatic merge failed; %d table(s) are unmerged.\n"+
			"Use 'dolt conflicts' to investigate and resolve conflicts.\n", unmergedCnt)
	} else if hasConstraintViolations {
		cli.Printf("Automatic merge failed; %d table(s) are unmerged.\n"+
			"Fix constraint violations and then commit the result.\n"+
			"Constraint violations for the working set may be viewed using the 'dolt_constraint_violations' system table.\n"+
			"They may be queried and removed per-table using the 'dolt_constraint_violations_TABLENAME' system table.\n", unmergedCnt)
	}

	if mergeErr != nil {
		var verr errhand.VerboseError
		switch mergeErr {
		case doltdb.ErrIsAhead:
			verr = nil
		default:
			verr = errhand.VerboseErrorFromError(mergeErr)
			cli.Println("Unable to stage changes: add and commit to finish merge")
		}
		return handleCommitErr(sqlCtx, queryist, verr, usage)
	}

	return 0
}

// performMerge applies a merge spec, potentially fast-forwarding the current branch HEAD, and returns a MergeStats object.
// If the merge can be applied as a fast-forward merge, no commit is needed.
// If the merge is a fast-forward merge, but --no-ff has been supplied, the ExecNoFFMerge function will call
// commit after merging. If the merge is not fast-forward, the --no-commit flag is not defined, and there are
// no conflicts and/or constraint violations, this function will call commit after merging.
// TODO: forcing a commit with a constraint violation should warn users that subsequent
//
//	FF merges will not surface constraint violations on their own; constraint verify --all
//	is required to reify violations.
func performMerge(ctx context.Context, sqlCtx *sql.Context, queryist cli.Queryist, dEnv *env.DoltEnv, spec *merge.MergeSpec, suggestedMsg string, cliCtx cli.CliContext) (map[string]*merge.MergeStats, error) {
	if ok, err := spec.HeadC.CanFastForwardTo(ctx, spec.MergeC); err != nil && !errors.Is(err, doltdb.ErrUpToDate) {
		return nil, err
	} else if ok {
		if spec.Noff {
			return executeNoFFMergeAndCommit(ctx, sqlCtx, queryist, dEnv, spec, suggestedMsg, cliCtx)
		}
		return nil, merge.ExecuteFFMerge(ctx, dEnv, spec)
	}
	return executeMergeAndCommit(ctx, sqlCtx, queryist, dEnv, spec, suggestedMsg, cliCtx)
}

func executeNoFFMergeAndCommit(ctx context.Context, sqlCtx *sql.Context, queryist cli.Queryist, dEnv *env.DoltEnv, spec *merge.MergeSpec, suggestedMsg string, cliCtx cli.CliContext) (map[string]*merge.MergeStats, error) {
	tblToStats, err := merge.ExecNoFFMerge(ctx, dEnv, spec)
	if err != nil {
		return tblToStats, err
	}

	if spec.NoCommit {
		cli.Println("Automatic merge went well; stopped before committing as requested")
		return tblToStats, nil
	}

	// Reload roots since the above method writes new values to the working set
	roots, err := dEnv.Roots(ctx)
	if err != nil {
		return tblToStats, err
	}

	ws, err := dEnv.WorkingSet(ctx)
	if err != nil {
		return tblToStats, err
	}

	var mergeParentCommits []*doltdb.Commit
	if ws.MergeActive() {
		mergeParentCommits = []*doltdb.Commit{ws.MergeState().Commit()}
	}

	msg, err := getCommitMsgForMerge(ctx, sqlCtx, queryist, spec.Msg, suggestedMsg, spec.NoEdit, cliCtx)
	if err != nil {
		return tblToStats, err
	}

	pendingCommit, err := actions.GetCommitStaged(ctx, roots, ws, mergeParentCommits, dEnv.DbData().Ddb, actions.CommitStagedProps{
		Message:    msg,
		Date:       spec.Date,
		AllowEmpty: spec.AllowEmpty,
		Force:      spec.Force,
		Name:       spec.Name,
		Email:      spec.Email,
	})

	headRef, err := dEnv.RepoStateReader().CWBHeadRef()
	if err != nil {
		return tblToStats, err
	}

	wsHash, err := ws.HashOf()
	_, err = dEnv.DoltDB.CommitWithWorkingSet(
		ctx,
		headRef,
		ws.Ref(),
		pendingCommit,
		ws.WithStagedRoot(pendingCommit.Roots.Staged).WithWorkingRoot(pendingCommit.Roots.Working).ClearMerge(),
		wsHash,
		dEnv.NewWorkingSetMeta(msg),
		nil,
	)

	if err != nil {
		return tblToStats, fmt.Errorf("%w; failed to commit", err)
	}

	return tblToStats, err
}

func executeMergeAndCommit(ctx context.Context, sqlCtx *sql.Context, queryist cli.Queryist, dEnv *env.DoltEnv, spec *merge.MergeSpec, suggestedMsg string, cliCtx cli.CliContext) (map[string]*merge.MergeStats, error) {
	tblToStats, err := merge.ExecuteMerge(sqlCtx, dEnv, spec)
	if err != nil {
		return tblToStats, err
	}

	if hasConflictOrViolations(tblToStats) {
		return tblToStats, nil
	}

	if spec.NoCommit {
		cli.Println("Automatic merge went well; stopped before committing as requested")
		return tblToStats, nil
	}

	msg, err := getCommitMsgForMerge(ctx, sqlCtx, queryist, spec.Msg, suggestedMsg, spec.NoEdit, cliCtx)
	if err != nil {
		return tblToStats, err
	}

	author := fmt.Sprintf("%s <%s>", spec.Name, spec.Email)

	res, skipped := performCommit(ctx, "commit", []string{"-m", msg, "--author", author}, cliCtx, dEnv)
	if res != 0 || skipped {
		return nil, fmt.Errorf("dolt commit failed after merging")
	}

	return tblToStats, nil
}

// getCommitMsgForMerge returns user defined message if exists; otherwise, get the commit message from editor.
func getCommitMsgForMerge(ctx context.Context, sqlCtx *sql.Context, queryist cli.Queryist, userDefinedMsg, suggestedMsg string, noEdit bool, cliCtx cli.CliContext) (string, error) {
	if userDefinedMsg != "" {
		return userDefinedMsg, nil
	}

	msg, err := getCommitMessageFromEditor(sqlCtx, queryist, suggestedMsg, "", noEdit, cliCtx)
	if err != nil {
		return msg, err
	}

	if msg == "" {
		return msg, fmt.Errorf("error: Empty commit message.\n" +
			"Not committing merge; use 'dolt commit' to complete the merge.")
	}

	return msg, nil
}

// hasConflictOrViolations checks for conflicts or constraint violation regardless of a table being modified
func hasConflictOrViolations(tblToStats map[string]*merge.MergeStats) bool {
	for _, tblStats := range tblToStats {
		if tblStats.HasArtifacts() {
			return true
		}
	}
	return false
}
