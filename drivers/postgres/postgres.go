// Package postgres implements the sq driver for postgres.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	// Import jackc/pgx, which is our postgres driver.
	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/neilotoole/lg"

	"github.com/neilotoole/sq/libsq/driver"
	"github.com/neilotoole/sq/libsq/errz"
	"github.com/neilotoole/sq/libsq/source"
	"github.com/neilotoole/sq/libsq/sqlbuilder"
	"github.com/neilotoole/sq/libsq/sqlmodel"
	"github.com/neilotoole/sq/libsq/sqlz"
	"github.com/neilotoole/sq/libsq/stringz"
)

const (
	// Type is the postgres source driver type.
	Type = source.Type("postgres")

	// dbDrvr is the backing postgres SQL driver impl name.
	dbDrvr = "pgx"
)

// Provider is the postgres implementation of driver.Provider.
type Provider struct {
	Log lg.Log
}

// DriverFor implements driver.Provider.
func (p *Provider) DriverFor(typ source.Type) (driver.Driver, error) {
	if typ != Type {
		return nil, errz.Errorf("unsupported driver type %q", typ)
	}

	return &Driver{log: p.Log}, nil
}

// Driver is the postgres implementation of driver.Driver.
type Driver struct {
	log lg.Log
}

// DriverMetadata implements driver.Driver.
func (d *Driver) DriverMetadata() driver.Metadata {
	return driver.Metadata{
		Type:        Type,
		Description: "PostgreSQL",
		Doc:         "https://github.com/jackc/pgx",
		IsSQL:       true,
	}
}

// Dialect implements driver.SQLDriver.
func (d *Driver) Dialect() driver.Dialect {
	return driver.Dialect{
		Type:           Type,
		Placeholders:   placeholders,
		Quote:          '"',
		MaxBatchValues: 1000,
	}
}

func placeholders(numCols, numRows int) string {
	rows := make([]string, numRows)

	n := 1
	var sb strings.Builder
	for i := 0; i < numRows; i++ {
		sb.Reset()
		sb.WriteRune('(')
		for j := 1; j <= numCols; j++ {
			sb.WriteRune('$')
			sb.WriteString(strconv.Itoa(n))
			n++
			if j < numCols {
				sb.WriteString(driver.Comma)
			}
		}
		sb.WriteRune(')')
		rows[i] = sb.String()
	}

	return strings.Join(rows, driver.Comma)
}

// SQLBuilder implements driver.SQLDriver.
func (d *Driver) SQLBuilder() (sqlbuilder.FragmentBuilder, sqlbuilder.QueryBuilder) {
	return newFragmentBuilder(d.log), &sqlbuilder.BaseQueryBuilder{}
}

// Open implements driver.Driver.
func (d *Driver) Open(ctx context.Context, src *source.Source) (driver.Database, error) {
	db, err := sql.Open(dbDrvr, src.Location)
	if err != nil {
		return nil, errz.Err(err)
	}

	return &database{log: d.log, db: db, src: src, drvr: d}, nil
}

// ValidateSource implements driver.Driver.
func (d *Driver) ValidateSource(src *source.Source) (*source.Source, error) {
	if src.Type != Type {
		return nil, errz.Errorf("expected source type %q but got %q", Type, src.Type)
	}
	return src, nil
}

// Ping implements driver.Driver.
func (d *Driver) Ping(ctx context.Context, src *source.Source) error {
	dbase, err := d.Open(context.TODO(), src)
	if err != nil {
		return err
	}

	defer d.log.WarnIfCloseError(dbase.DB())

	return dbase.DB().Ping()
}

// Truncate implements driver.Driver.
func (d *Driver) Truncate(ctx context.Context, src *source.Source, tbl string, reset bool) (affected int64, err error) {
	// https://www.postgresql.org/docs/9.1/sql-truncate.html

	// RESTART IDENTITY and CASCADE/RESTRICT are from pg 8.2 onwards
	// TODO: should first check the pg version for < pg8.2 support

	db, err := sql.Open(dbDrvr, src.Location)
	if err != nil {
		return affected, errz.Err(err)
	}

	query := fmt.Sprintf("TRUNCATE TABLE %q", tbl)
	if reset {
		// if reset & src.DBVersion >= 8.2
		query += " RESTART IDENTITY" // default is CONTINUE IDENTITY
	}
	// We could add RESTRICT here; alternative is CASCADE

	affected, err = sqlz.ExecResult(ctx, db, query)
	return affected, err
}

// CreateTable implements driver.SQLDriver.
func (d *Driver) CreateTable(ctx context.Context, db sqlz.DB, tblDef *sqlmodel.TableDef) error {
	stmt := buildCreateTableStmt(tblDef)

	_, err := db.ExecContext(ctx, stmt)
	return errz.Err(err)
}

// PrepareInsertStmt implements driver.SQLDriver.
func (d *Driver) PrepareInsertStmt(ctx context.Context, db sqlz.DB, destTbl string, destColNames []string, numRows int) (*driver.StmtExecer, error) {
	// Note that the pgx driver doesn't support res.LastInsertId.
	// https://github.com/jackc/pgx/issues/411

	destColsMeta, err := d.getTableRecordMeta(ctx, db, destTbl, destColNames)
	if err != nil {
		return nil, err
	}

	stmt, err := driver.PrepareInsertStmt(ctx, d, db, destTbl, destColsMeta.Names(), numRows)
	if err != nil {
		return nil, err
	}

	execer := driver.NewStmtExecer(stmt, driver.DefaultInsertMungeFunc(destTbl, destColsMeta), newStmtExecFunc(stmt), destColsMeta)
	return execer, nil
}

// PrepareUpdateStmt implements driver.SQLDriver.
func (d *Driver) PrepareUpdateStmt(ctx context.Context, db sqlz.DB, destTbl string, destColNames []string, where string) (*driver.StmtExecer, error) {
	destColsMeta, err := d.getTableRecordMeta(ctx, db, destTbl, destColNames)
	if err != nil {
		return nil, err
	}

	query, err := buildUpdateStmt(destTbl, destColNames, where)
	if err != nil {
		return nil, err
	}

	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}

	execer := driver.NewStmtExecer(stmt, driver.DefaultInsertMungeFunc(destTbl, destColsMeta), newStmtExecFunc(stmt), destColsMeta)
	return execer, nil
}

func newStmtExecFunc(stmt *sql.Stmt) driver.StmtExecFunc {
	return func(ctx context.Context, args ...interface{}) (int64, error) {
		res, err := stmt.ExecContext(ctx, args...)
		if err != nil {
			return 0, errz.Err(err)
		}
		affected, err := res.RowsAffected()
		return affected, errz.Err(err)
	}
}

// CopyTable implements driver.SQLDriver.
func (d *Driver) CopyTable(ctx context.Context, db sqlz.DB, fromTable, toTable string, copyData bool) (int64, error) {
	stmt := fmt.Sprintf("CREATE TABLE %q AS TABLE %q", toTable, fromTable)

	if !copyData {
		stmt += " WITH NO DATA"
	}

	affected, err := sqlz.ExecResult(ctx, db, stmt)
	if err != nil {
		return 0, errz.Err(err)
	}

	return affected, nil
}

// DropTable implements driver.SQLDriver.
func (d *Driver) DropTable(ctx context.Context, db sqlz.DB, tbl string, ifExists bool) error {
	var stmt string

	if ifExists {
		stmt = fmt.Sprintf("DROP TABLE IF EXISTS %q RESTRICT", tbl)
	} else {
		stmt = fmt.Sprintf("DROP TABLE %q RESTRICT", tbl)
	}

	_, err := db.ExecContext(ctx, stmt)
	return errz.Err(err)
}

// TableColumnTypes implements driver.SQLDriver.
func (d *Driver) TableColumnTypes(ctx context.Context, db sqlz.DB, tblName string, colNames []string) ([]*sql.ColumnType, error) {
	// We have to do some funky stuff to get the column types
	// from when the table has no rows.
	// https://stackoverflow.com/questions/8098795/return-a-value-if-no-record-is-found

	// If tblName is "person" and we want cols "username"
	// and "email", the query will look like:
	//
	// SELECT
	// 	(SELECT username FROM person LIMIT 1) AS username,
	// 	(SELECT email FROM person LIMIT 1) AS email
	// LIMIT 1;
	quote := string(d.Dialect().Quote)
	tblNameQuoted := stringz.Surround(tblName, quote)

	var query string

	if len(colNames) == 0 {
		// When the table is empty, and colNames are not provided,
		// then we need to fetch the table col names independently.
		var err error
		colNames, err = getTableColumnNames(ctx, d.log, db, tblName)
		if err != nil {
			return nil, err
		}
	}

	var sb strings.Builder
	sb.WriteString("SELECT\n")
	for i, colName := range colNames {
		colNameQuoted := stringz.Surround(colName, quote)
		sb.WriteString(fmt.Sprintf("  (SELECT %s FROM %s LIMIT 1) AS %s", colNameQuoted, tblNameQuoted, colNameQuoted))
		if i < len(colNames)-1 {
			sb.WriteRune(',')
		}
		sb.WriteString("\n")
	}
	sb.WriteString("LIMIT 1")
	query = sb.String()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, errz.Err(err)
	}

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		d.log.WarnIfFuncError(rows.Close)
		return nil, errz.Err(err)
	}

	err = rows.Err()
	if err != nil {
		d.log.WarnIfFuncError(rows.Close)
		return nil, errz.Err(err)
	}

	err = rows.Close()
	if err != nil {
		return nil, errz.Err(err)
	}

	return colTypes, nil
}

func (d *Driver) getTableRecordMeta(ctx context.Context, db sqlz.DB, tblName string, colNames []string) (sqlz.RecordMeta, error) {
	colTypes, err := d.TableColumnTypes(ctx, db, tblName, colNames)
	if err != nil {
		return nil, err
	}

	destCols, _, err := d.RecordMeta(colTypes)
	if err != nil {
		return nil, err
	}

	return destCols, nil
}

// getTableColumnNames consults postgres's information_schema.columns table,
// returning the names of the table's columns in oridinal order.
func getTableColumnNames(ctx context.Context, log lg.Log, db sqlz.DB, tblName string) ([]string, error) {
	const query = `SELECT column_name FROM information_schema.columns
	WHERE table_schema = current_schema()
	AND table_name = $1
	ORDER BY ordinal_position`

	rows, err := db.QueryContext(ctx, query, tblName)
	if err != nil {
		return nil, errz.Err(err)
	}

	var colNames []string
	var colName string

	for rows.Next() {
		err = rows.Scan(&colName)
		if err != nil {
			log.WarnIfCloseError(rows)
			return nil, errz.Err(err)
		}

		colNames = append(colNames, colName)
	}

	if rows.Err() != nil {
		log.WarnIfCloseError(rows)
		return nil, errz.Err(err)
	}

	err = rows.Close()
	if err != nil {
		return nil, errz.Err(err)
	}

	return colNames, nil
}

// RecordMeta implements driver.SQLDriver.
func (d *Driver) RecordMeta(colTypes []*sql.ColumnType) (sqlz.RecordMeta, driver.NewRecordFunc, error) {
	// The jackc/pgx driver doesn't report nullability (sql.ColumnType)
	// Apparently this is due to what postgres sends over the wire.
	// See https://github.com/jackc/pgx/issues/276#issuecomment-526831493
	// So, we'll set the scan type for each column to the nullable
	// version below.

	recMeta := make(sqlz.RecordMeta, len(colTypes))
	for i, colType := range colTypes {
		kind := kindFromDBTypeName(d.log, colType.Name(), colType.DatabaseTypeName())
		colTypeData := sqlz.NewColumnTypeData(colType, kind)
		setScanType(d.log, colTypeData, kind)
		recMeta[i] = sqlz.NewFieldMeta(colTypeData)
	}

	mungeFn := func(vals []interface{}) (sqlz.Record, error) {
		// postgres doesn't need to do any special munging, so we
		// just use the default munging.
		rec, skipped := driver.NewRecordFromScanRow(recMeta, vals, nil)
		if len(skipped) > 0 {
			var skippedDetails []string

			for _, skip := range skipped {
				meta := recMeta[skip]
				skippedDetails = append(skippedDetails,
					fmt.Sprintf("[%d] %s: db(%s) --> kind(%s) --> scan(%s)",
						skip, meta.Name(), meta.DatabaseTypeName(), meta.Kind(), meta.ScanType()))
			}

			return nil, errz.Errorf("expected zero skipped cols but have %d:\n  %s",
				skipped, strings.Join(skippedDetails, "\n  "))
		}
		return rec, nil
	}

	return recMeta, mungeFn, nil
}

// database is the postgres implementation of driver.Database.
type database struct {
	log  lg.Log
	drvr *Driver
	db   *sql.DB
	src  *source.Source
}

// DB implements driver.Database.
func (d *database) DB() *sql.DB {
	return d.db
}

// SQLDriver implements driver.Database.
func (d *database) SQLDriver() driver.SQLDriver {
	return d.drvr
}

// Source implements driver.Database.
func (d *database) Source() *source.Source {
	return d.src
}

// TableMetadata implements driver.Database.
func (d *database) TableMetadata(ctx context.Context, tblName string) (*source.TableMetadata, error) {
	return getTableMetadata(ctx, d.log, d.DB(), tblName)
}

// Metadata implements driver.Database.
func (d *database) SourceMetadata(ctx context.Context) (*source.Metadata, error) {
	return getSourceMetadata(ctx, d.log, d.src, d.DB())
}

// Close implements driver.Database.
func (d *database) Close() error {
	d.log.Debugf("Close database: %s", d.src)

	err := d.db.Close()
	if err != nil {
		return errz.Err(err)
	}
	return nil
}