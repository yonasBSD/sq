package json

// xmlud.go contains functionality common to the
// various JSON import mechanisms.

import (
	"bytes"
	"context"
	stdj "encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/neilotoole/sq/libsq/core/errz"
	"github.com/neilotoole/sq/libsq/core/ioz/checksum"
	"github.com/neilotoole/sq/libsq/core/kind"
	"github.com/neilotoole/sq/libsq/core/lg"
	"github.com/neilotoole/sq/libsq/core/lg/lga"
	"github.com/neilotoole/sq/libsq/core/record"
	"github.com/neilotoole/sq/libsq/core/schema"
	"github.com/neilotoole/sq/libsq/core/sqlz"
	"github.com/neilotoole/sq/libsq/core/stringz"
	"github.com/neilotoole/sq/libsq/driver"
	"github.com/neilotoole/sq/libsq/files"
	"github.com/neilotoole/sq/libsq/source"
)

// ingestJob describes a single ingest job, where the JSON
// at fromSrc is read via newRdrFn and the resulting records
// are written to destGrip.
type ingestJob struct {
	destGrip driver.Grip

	fromSrc  *source.Source
	newRdrFn files.NewReaderFunc

	stmtCache map[string]*driver.StmtExecer

	// sampleSize is the maximum number of values to
	// sample to determine the kind of an element.
	sampleSize int

	// flatten specifies that the fields of nested JSON objects are
	// imported as fields of the single top-level table, with a
	// scoped column name.
	//
	// TODO: flatten should come from src.Options
	flatten bool
}

// Close closes the ingestJob. In particular, it closes any cached statements.
func (jb *ingestJob) Close() error {
	var err error
	for _, stmt := range jb.stmtCache {
		err = errz.Append(err, stmt.Close())
	}
	return err
}

// execInsertions performs db INSERT for each of the insertions.
// The caller must ensure that ingestJob.Close is eventually called.
func (jb *ingestJob) execInsertions(ctx context.Context, drvr driver.SQLDriver,
	db sqlz.DB, insertions []*insertion,
) error {
	// TODO: Although we cache the insert statements (driver.StmtExecer), this
	// is still pretty inefficient. We should be able to use driver.BatchInsert.
	// But that requires interaction with execSchemaDelta, so that we
	// can create a new BatchInsert instance if the schema changes.
	// And execSchemaDelta is not yet fully implemented.

	var (
		execer *driver.StmtExecer
		ok     bool
		err    error
	)

	for _, insert := range insertions {
		if execer, ok = jb.stmtCache[insert.stmtHash]; !ok {
			if execer, err = drvr.PrepareInsertStmt(ctx, db, insert.tbl, insert.cols, 1); err != nil {
				return err
			}

			// Note that we don't close the execer here, because we cache it.
			// It will be closed via ingestJob.Close.
			jb.stmtCache[insert.stmtHash] = execer
		}

		if err = execer.Munge(insert.vals); err != nil {
			return err
		}

		if _, err = execer.Exec(ctx, insert.vals...); err != nil {
			return err
		}
	}

	return nil
}

type ingestFunc func(ctx context.Context, job *ingestJob) error

var (
	_ ingestFunc = ingestJSON
	_ ingestFunc = ingestJSONA
	_ ingestFunc = ingestJSONL
)

// getRecMeta returns record.Meta to use with RecordWriter.Open.
func getRecMeta(ctx context.Context, grip driver.Grip, tblDef *schema.Table) (record.Meta, error) {
	db, err := grip.DB(ctx)
	if err != nil {
		return nil, err
	}

	colTypes, err := grip.SQLDriver().TableColumnTypes(ctx, db, tblDef.Name, tblDef.ColNames())
	if err != nil {
		return nil, err
	}

	destMeta, _, err := grip.SQLDriver().RecordMeta(ctx, colTypes)
	if err != nil {
		return nil, err
	}

	return destMeta, nil
}

const (
	leftBrace    = stdj.Delim('{')
	rightBrace   = stdj.Delim('}')
	leftBracket  = stdj.Delim('[')
	rightBracket = stdj.Delim(']')

	// colScopeSep is used when generating flat column names. Thus
	// an entity "name.first" becomes "name_first".
	colScopeSep = "_"
)

// objectValueSet is the set of values for each of the fields of
// a top-level JSON object. It is a map of entity to a map
// of fieldName:fieldValue. For a nested JSON object, the value set
// may refer to several entities, and thus may decompose into
// insertions to several tables.
type objectValueSet map[*entity]map[string]any

// processor process JSON objects.
type processor struct {
	root      *entity
	curSchema *ingestSchema

	// schemaDirtyEntities tracks entities whose structure have been modified.
	schemaDirtyEntities map[*entity]struct{}

	curObjVals objectValueSet

	colNamesOrdered []string

	unwrittenObjVals []objectValueSet
	// if flattened is true, the JSON object will be flattened into a single table.
	flatten bool
}

func newProcessor(flatten bool) *processor {
	return &processor{
		flatten:   flatten,
		curSchema: nil,
		root: &entity{
			name:      source.MonotableName,
			detectors: map[string]*kind.Detector{},
			kinds:     map[string]kind.Kind{},
		},
		schemaDirtyEntities: map[*entity]struct{}{},
	}
}

func (p *processor) markSchemaDirty(e *entity) {
	p.schemaDirtyEntities[e] = struct{}{}
}

func (p *processor) markSchemaClean() {
	for k := range p.schemaDirtyEntities {
		delete(p.schemaDirtyEntities, k)
	}
}

// calcColName calculates the appropriate DB column name from
// a field. The result is different if p.flatten is true (in which
// case the column name may have a prefix derived from the entity's
// parent).
func (p *processor) calcColName(ent *entity, fieldName string) string {
	if !p.flatten {
		return fieldName
	}

	// Otherwise we namespace the column name.
	if ent.parent == nil {
		return fieldName
	}

	colName := ent.name + colScopeSep + fieldName
	return p.calcColName(ent.parent, colName)
}

// buildSchemaFlat currently only builds a flat (single table) schema.
func (p *processor) buildSchemaFlat() (*ingestSchema, error) {
	tblDef := &schema.Table{
		Name: source.MonotableName,
	}

	var colDefs []*schema.Column

	schma := &ingestSchema{
		colMungeFns: map[*schema.Column]kind.MungeFunc{},
		entityTbls:  map[*entity]*schema.Table{},
		tblDefs:     []*schema.Table{tblDef}, // Single table only because flat
	}

	visitFn := func(e *entity) error {
		schma.entityTbls[e] = tblDef

		for _, field := range e.fieldNames {
			if detector, ok := e.detectors[field]; ok {
				// If it has a detector, it's a regular field
				k, mungeFn, err := detector.Detect()
				if err != nil {
					return errz.Err(err)
				}

				if k == kind.Null {
					k = kind.Text
				}

				colDef := &schema.Column{
					Name:  p.calcColName(e, field),
					Table: tblDef,
					Kind:  k,
				}

				colDefs = append(colDefs, colDef)
				if mungeFn != nil {
					schma.colMungeFns[colDef] = mungeFn
				}
				continue
			}
		}

		return nil
	}

	err := walkEntity(p.root, visitFn)
	if err != nil {
		return nil, err
	}

	// Add the column names, in the correct order
	for _, colName := range p.colNamesOrdered {
		for j := range colDefs {
			if colDefs[j].Name == colName {
				tblDef.Cols = append(tblDef.Cols, colDefs[j])
			}
		}
	}

	return schma, nil
}

// processObject processes the parsed JSON object m. If the structure
// of the ingestSchema changes due to this object, dirtySchema returns true.
func (p *processor) processObject(m map[string]any, chunk []byte) (dirtySchema bool, err error) {
	p.curObjVals = make(objectValueSet)
	err = p.doAddObject(p.root, m)
	dirtySchema = len(p.schemaDirtyEntities) > 0
	if err != nil {
		return dirtySchema, err
	}

	p.unwrittenObjVals = append(p.unwrittenObjVals, p.curObjVals)

	p.curObjVals = nil
	if dirtySchema {
		err = p.updateColNames(chunk)
	}

	return dirtySchema, err
}

func (p *processor) updateColNames(chunk []byte) error {
	colNames, err := columnOrderFlat(chunk)
	if err != nil {
		return err
	}

	for _, colName := range colNames {
		if !stringz.InSlice(p.colNamesOrdered, colName) {
			p.colNamesOrdered = append(p.colNamesOrdered, colName)
		}
	}

	return nil
}

func (p *processor) doAddObject(ent *entity, m map[string]any) error {
	var err error

	for fieldName, val := range m {
		switch val := val.(type) {
		case map[string]any:
			// time to recurse
			child := ent.getChild(fieldName)
			if child == nil {
				p.markSchemaDirty(ent)

				if !stringz.InSlice(ent.fieldNames, fieldName) {
					// The field name could already exist (even without
					// the child existing) if we encountered
					// the field before but it was nil
					ent.fieldNames = append(ent.fieldNames, fieldName)
				}

				child = &entity{
					name:      fieldName,
					parent:    ent,
					detectors: map[string]*kind.Detector{},
					kinds:     map[string]kind.Kind{},
				}
				ent.children = append(ent.children, child)
			} else if child.isArray {
				// Child already exists
				// Safety check
				return errz.Errorf("JSON entity {%s} previously detected as array, but now detected as object",
					ent.String())
			}

			if err = p.doAddObject(child, val); err != nil {
				return err
			}

		case []any:
			if !stringz.InSlice(ent.fieldNames, fieldName) {
				ent.fieldNames = append(ent.fieldNames, fieldName)
			}
		default:
			// It's a regular value
			detector, ok := ent.detectors[fieldName]
			if !ok {
				p.markSchemaDirty(ent)
				if stringz.InSlice(ent.fieldNames, fieldName) {
					return errz.Errorf("JSON field {%s} was previously detected as a nested field (object or array)",
						fieldName)
				}

				ent.fieldNames = append(ent.fieldNames, fieldName)

				detector = kind.NewDetector()
				ent.detectors[fieldName] = detector
			}

			entVals := p.curObjVals[ent]
			if entVals == nil {
				entVals = map[string]any{}
				p.curObjVals[ent] = entVals
			}

			colName := p.calcColName(ent, fieldName)
			entVals[colName] = val

			colDef := p.getColDef(ent, colName)

			if colDef == nil && val != nil {
				val = maybeFloatToInt(val)
				// We don't need to keep sampling after we've detected the kind.
				detector.Sample(val)
			} else
			// REVISIT: We don't need to hold onto the samples after we've detected
			// the kind, it's just holding onto memory. We should probably nil out
			// the detector.

			// The column is already defined. Check if the value is allowed.
			if !p.fieldValAllowed(detector, colDef, val) {
				p.markSchemaDirty(ent)
			}
		}
	}

	return nil
}

func (p *processor) fieldValAllowed(detector *kind.Detector, col *schema.Column, val any) bool {
	if val == nil || col == nil {
		return true
	}

	if col.Kind == kind.Null || col.Kind == kind.Unknown || col.Kind == kind.Text {
		return true
	}

	detector.Sample(val)
	k, _, err := detector.Detect()
	if err != nil || k != col.Kind {
		return false
	}

	return true
}

// getColDef returns the schema.Column, or nil if not existing.
func (p *processor) getColDef(ent *entity, colName string) *schema.Column {
	if p == nil || p.curSchema == nil || p.curSchema.entityTbls == nil {
		return nil
	}

	tblDef, ok := p.curSchema.entityTbls[ent]
	if !ok || tblDef == nil {
		return nil
	}

	colDef, err := tblDef.FindCol(colName)
	if err != nil {
		return nil
	}

	return colDef
}

// buildInsertionsFlat builds a set of DB insertions from the
// processor's unwrittenObjVals. After a non-error return, unwrittenObjVals
// is empty.
func (p *processor) buildInsertionsFlat(schma *ingestSchema) ([]*insertion, error) {
	if len(schma.tblDefs) != 1 {
		return nil, errz.Errorf("expected 1 table for flat JSON processing but got %d", len(schma.tblDefs))
	}

	tblDef := schma.tblDefs[0]
	var insertions []*insertion

	// Each of unwrittenObjVals is effectively an INSERT row
	for _, objValSet := range p.unwrittenObjVals {
		var colNames []string
		colVals := map[string]any{}

		for ent, fieldVals := range objValSet {
			// For each entity, we get its values and add them to colVals.
			for colName, val := range fieldVals {
				if _, ok := colVals[colName]; ok {
					return nil, errz.Errorf("column {%s} already exists, but found column with same name in {%s}",
						colName, ent)
				}

				colVals[colName] = val
				colNames = append(colNames, colName)
			}
		}

		sort.Strings(colNames)
		vals := make([]any, len(colNames))
		for i, colName := range colNames {
			vals[i] = colVals[colName]
		}
		insertions = append(insertions, newInsertion(tblDef.Name, colNames, vals))
	}

	p.unwrittenObjVals = p.unwrittenObjVals[:0]

	return insertions, nil
}

// entity models the structure of a JSON entity, either an object or an array.
type entity struct {
	parent *entity

	// detectors holds a kind detector for each non-entity field
	// of entity. That is, it holds a detector for each string or number
	// field etc, but not for an object or array field.
	detectors map[string]*kind.Detector

	// kinds is the sibling of detectors, holding a kind.Kind for each field,
	// once the detector has detected the kind.
	kinds map[string]kind.Kind

	name     string
	children []*entity

	// fieldName holds the names of each field. This includes simple
	// fields (such as a number or string) and nested types like
	// object or array.
	fieldNames []string

	// isArray is true if the entity is an array, false if an object.
	isArray bool
}

func (e *entity) String() string {
	name := e.name
	if name == "" {
		name = source.MonotableName
	}

	parent := e.parent
	for parent != nil {
		name = parent.String() + "." + name
		parent = parent.parent
	}

	return name
}

// fqFieldName returns the fully-qualified field name, such
// as "data.name.first_name".
func (e *entity) fqFieldName(field string) string { //nolint:unused
	return e.String() + "." + field
}

// getChild returns the named child, or nil.
func (e *entity) getChild(name string) *entity {
	for _, child := range e.children {
		if child.name == name {
			return child
		}
	}
	return nil
}

func walkEntity(ent *entity, visitFn func(*entity) error) error {
	err := visitFn(ent)
	if err != nil {
		return err
	}

	for _, child := range ent.children {
		err = walkEntity(child, visitFn)
		if err != nil {
			return err
		}
	}

	return nil
}

// ingestSchema encapsulates the table definitions that
// the JSON is ingested to.
type ingestSchema struct {
	colMungeFns map[*schema.Column]kind.MungeFunc

	// entityTbls is a mapping of entity to the table in which
	// the entity's fields will be inserted.
	entityTbls map[*entity]*schema.Table
	tblDefs    []*schema.Table
}

func (s *ingestSchema) getTable(name string) *schema.Table {
	if s == nil {
		return nil
	}

	for _, tbl := range s.tblDefs {
		if tbl.Name == name {
			return tbl
		}
	}
	return nil
}

// execSchemaDelta executes the schema delta between curSchema and newSchema.
// That is, if curSchema is nil, then newSchema is created in the DB; if
// newSchema has additional tables or columns, then those are created in the DB.
func execSchemaDelta(ctx context.Context, drvr driver.SQLDriver, db sqlz.DB,
	curSchema, newSchema *ingestSchema,
) error {
	log := lg.FromContext(ctx)
	var err error

	if curSchema == nil {
		for _, tbl := range newSchema.tblDefs {
			err = drvr.CreateTable(ctx, db, tbl)
			if err != nil {
				return err
			}

			log.Debug("Created table", lga.Table, tbl.Name)
		}
		return nil
	}

	var alterTbls []*schema.Table
	var createTbls []*schema.Table

	for _, newTbl := range newSchema.tblDefs {
		oldTbl := curSchema.getTable(newTbl.Name)
		if oldTbl == nil {
			createTbls = append(createTbls, newTbl)
		} else if !oldTbl.Equal(newTbl) {
			alterTbls = append(alterTbls, newTbl)
		}
	}

	for _, wantTbl := range alterTbls {
		oldTbl := curSchema.getTable(wantTbl.Name)
		if err = execMaybeAlterTable(ctx, drvr, db, oldTbl, wantTbl); err != nil {
			return err
		}
	}

	for _, wantTbl := range createTbls {
		err = drvr.CreateTable(ctx, db, wantTbl)
		if err != nil {
			return err
		}

		log.Debug("Created table", lga.Table, wantTbl.Name)
	}

	return nil
}

func execMaybeAlterTable(ctx context.Context, drvr driver.SQLDriver, db sqlz.DB,
	oldTbl, newTbl *schema.Table,
) error {
	log := lg.FromContext(ctx)
	if newTbl == nil {
		return nil
	}
	if oldTbl == nil {
		return drvr.CreateTable(ctx, db, newTbl)
	}

	if oldTbl.Equal(newTbl) {
		return nil
	}

	tblName := newTbl.Name

	var createCols []*schema.Column
	var wantAlterColNames []string
	var wantAlterColKinds []kind.Kind

	for _, newCol := range newTbl.Cols {
		oldCol, err := oldTbl.FindCol(newCol.Name)
		if err != nil {
			createCols = append(createCols, newCol)
		} else if newCol.Kind != oldCol.Kind {
			wantAlterColNames = append(wantAlterColNames, newCol.Name)
			wantAlterColKinds = append(wantAlterColKinds, newCol.Kind)
		}
	}

	if len(wantAlterColNames) > 0 {
		err := drvr.AlterTableColumnKinds(ctx, db, tblName, wantAlterColNames, wantAlterColKinds)
		if err != nil {
			return err
		}
	}

	for _, col := range createCols {
		if err := drvr.AlterTableAddColumn(ctx, db, tblName, col.Name, col.Kind); err != nil {
			return err
		}
		log.Debug("Added column", lga.Table, newTbl.Name, lga.Col, col.Name)
	}

	return nil
}

// columnOrderFlat parses the json chunk and returns a slice
// containing column names, in the order they appear in chunk.
// Nested fields are flattened, e.g:
//
//	{"a":1, "b": {"c":2, "d":3}}  -->  ["a", "b_c", "b_d"]
func columnOrderFlat(chunk []byte) ([]string, error) {
	dec := stdj.NewDecoder(bytes.NewReader(chunk))

	var (
		cols  []string
		stack []string
		tok   stdj.Token
		err   error
	)

	// Get the opening left-brace
	_, err = requireDelimToken(dec, leftBrace)
	if err != nil {
		return nil, err
	}

loop:
	for {
		// Expect tok to be a field name, or else the terminating right-brace.
		tok, err = dec.Token()
		if err != nil {
			if err == io.EOF { //nolint:errorlint
				break
			}
			return nil, errz.Err(err)
		}

		switch tok := tok.(type) {
		case string:
			// tok is a field name
			stack = append(stack, tok)

		case stdj.Delim:
			if tok == rightBrace {
				if len(stack) == 0 {
					// This is the terminating right-brace
					break loop
				}
				// Else we've come to the end of an object
				stack = stack[:len(stack)-1]
				continue
			}

		default:
			return nil, errz.Errorf("expected string field name but got %T: %s", tok, formatToken(tok))
		}

		// We've consumed the field name above, now let's see what
		// the next token is
		tok, err = dec.Token()
		if err != nil {
			return nil, errz.Err(err)
		}

		switch tok := tok.(type) {
		default:
			// This next token was a regular old value.

			// The field name is already on the stack. We generate
			// the column name...
			cols = append(cols, strings.Join(stack, colScopeSep))

			// And pop the stack.
			stack = stack[0 : len(stack)-1]

		case stdj.Delim:
			// The next token was a delimiter.

			if tok == leftBrace {
				// It's the start of a nested object.
				// Back to the top of the loop we go, so that
				// we can descend into the nested object.
				continue loop
			}

			if tok == leftBracket {
				// It's the start of an array.
				// Note that we don't descend into arrays.

				cols = append(cols, strings.Join(stack, colScopeSep))
				stack = stack[0 : len(stack)-1]

				err = decoderFindArrayClose(dec)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	return cols, nil
}

// decoderFindArrayClose advances dec until a closing
// right-bracket ']' is located at the correct nesting level.
// The most-recently returned decoder token should have been
// the opening left-bracket '['.
func decoderFindArrayClose(dec *stdj.Decoder) error {
	var depth int
	var tok stdj.Token
	var err error

	for {
		tok, err = dec.Token()
		if err != nil {
			break
		}

		if tok == leftBracket {
			// Nested array
			depth++
			continue
		}

		if tok == rightBracket {
			if depth == 0 {
				return nil
			}
			depth--
		}
	}

	return errz.Err(err)
}

type insertion struct {
	// stmtHash is a concatenation of tbl and cols that can
	// uniquely identify a db insert statement.
	stmtHash string

	tbl  string
	cols []string
	vals []any
}

// newInsert should always be used to create an insertion, because
// it initializes insertion.stmtHash.
func newInsertion(tbl string, cols []string, vals []any) *insertion {
	return &insertion{
		stmtHash: checksum.SumAll(tbl, cols...),
		tbl:      tbl,
		cols:     cols,
		vals:     vals,
	}
}

// cannotBeJSON returns true if the input cannot be JSON. This is a quick sanity
// check that returns true if two JSON tokens cannot be decoded from the input.
func cannotBeJSON(r io.Reader) bool {
	// Decode some JSON tokens from reader r. A minimum of two tokens are required
	// to determine if the input is JSON.
	dec := stdj.NewDecoder(r)
	if _, err := dec.Token(); err != nil {
		return true
	}

	if _, err := dec.Token(); err != nil {
		return true
	}

	return false
}
