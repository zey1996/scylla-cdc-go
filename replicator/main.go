package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"time"

	"github.com/gocql/gocql"
	scylla_cdc "github.com/piodul/scylla-cdc-go"
)

// TODO: Escape field names?
// TODO: Tuple support

var debugQueries = true

func main() {
	var (
		keyspace    string
		table       string
		source      string
		destination string
		consistency string
	)

	flag.StringVar(&keyspace, "keyspace", "", "keyspace name")
	flag.StringVar(&table, "table", "", "table name")
	flag.StringVar(&source, "source", "", "address of a node in source cluster")
	flag.StringVar(&destination, "destination", "", "address of a node in destination cluster")
	flag.StringVar(&consistency, "consistency", "", "consistency level (one, quorum, all)")
	flag.String("mode", "", "mode (ignored)")
	flag.Parse()

	cl := gocql.One
	switch strings.ToLower(consistency) {
	case "one":
		cl = gocql.One
	case "quorum":
		cl = gocql.Quorum
	case "all":
		cl = gocql.All
	}

	adv := scylla_cdc.AdvancedReaderConfig{
		ConfidenceWindowSize:   0,
		ChangeAgeLimit:         24 * time.Hour,
		QueryTimeWindowSize:    24 * time.Hour,
		PostEmptyQueryDelay:    15 * time.Second,
		PostNonEmptyQueryDelay: 5 * time.Second,
		PostFailedQueryDelay:   5 * time.Second,
	}

	reader, err := MakeReplicator(
		source, destination,
		[]string{keyspace + "." + table},
		&adv,
		cl,
	)
	if err != nil {
		log.Fatalln(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// React to Ctrl+C signal, and stop gracefully after the first signal
	// Second signal cancels the context, so that the replicator
	// should stop immediately, but still gracefully
	// The third signal kills the process
	signalC := make(chan os.Signal)
	go func() {
		<-signalC
		now := time.Now()
		log.Printf("stopping at %v", now)
		reader.StopAt(now)

		<-signalC
		log.Printf("stopping now")
		cancel()

		<-signalC
		log.Printf("killing")
		os.Exit(1)
	}()
	signal.Notify(signalC, os.Interrupt)

	if err := reader.Run(ctx); err != nil {
		log.Println(err)
		os.Exit(1)
	}

	log.Println("quitting")
}

func MakeReplicator(
	source, destination string,
	tableNames []string,
	advancedParams *scylla_cdc.AdvancedReaderConfig,
	consistency gocql.Consistency,
) (*scylla_cdc.Reader, error) {
	// Configure a session for the destination cluster
	destinationCluster := gocql.NewCluster(destination)
	destinationSession, err := destinationCluster.CreateSession()
	if err != nil {
		return nil, err
	}

	tracker := scylla_cdc.NewClusterStateTracker(gocql.TokenAwareHostPolicy(gocql.RoundRobinHostPolicy()))

	// Configure a session
	cluster := gocql.NewCluster(source)
	cluster.PoolConfig.HostSelectionPolicy = tracker
	session, err := cluster.CreateSession()
	if err != nil {
		destinationSession.Close()
		return nil, err
	}

	factory := &replicatorFactory{
		destinationSession: destinationSession,
		consistency:        consistency,
	}

	// Configuration for the CDC reader
	cfg := scylla_cdc.NewReaderConfig(
		session,
		factory,
		tableNames...,
	)
	if advancedParams != nil {
		cfg.Advanced = *advancedParams
	}
	cfg.Consistency = consistency
	cfg.ClusterStateTracker = tracker
	cfg.Logger = log.New(os.Stderr, "", log.Ldate|log.Lmicroseconds|log.Lshortfile)

	reader, err := scylla_cdc.NewReader(cfg)
	if err != nil {
		session.Close()
		destinationSession.Close()
		return nil, err
	}

	// TODO: source and destination sessions are leaking
	return reader, nil
}

type replicatorFactory struct {
	destinationSession *gocql.Session
	consistency        gocql.Consistency
}

func (rf *replicatorFactory) CreateChangeConsumer(input scylla_cdc.CreateChangeConsumerInput) (scylla_cdc.ChangeConsumer, error) {
	splitTableName := strings.SplitN(input.TableName, ".", 2)
	if len(splitTableName) < 2 {
		return nil, fmt.Errorf("table name is not fully qualified: %s", input.TableName)
	}

	kmeta, err := rf.destinationSession.KeyspaceMetadata(splitTableName[0])
	if err != nil {
		rf.destinationSession.Close()
		return nil, err
	}
	tmeta, ok := kmeta.Tables[splitTableName[1]]
	if !ok {
		rf.destinationSession.Close()
		return nil, fmt.Errorf("table %s does not exist", input.TableName)
	}

	return NewDeltaReplicator(rf.destinationSession, kmeta, tmeta, rf.consistency)
}

type DeltaReplicator struct {
	session     *gocql.Session
	tableName   string
	consistency gocql.Consistency

	pkColumns    []string
	ckColumns    []string
	otherColumns []string
	columnTypes  map[string]TypeInfo
	allColumns   []string

	rowDeleteQueryStr       string
	partitionDeleteQueryStr string
	rangeDeleteQueryStrs    []string
}

type updateQuerySet struct {
	add    string
	remove string
}

type udtInfo struct {
	setterQuery string
	fields      []string
}

func NewDeltaReplicator(session *gocql.Session, kmeta *gocql.KeyspaceMetadata, meta *gocql.TableMetadata, consistency gocql.Consistency) (*DeltaReplicator, error) {
	var (
		pkColumns    []string
		ckColumns    []string
		otherColumns []string
	)

	for _, name := range meta.OrderedColumns {
		colDesc := meta.Columns[name]
		switch colDesc.Kind {
		case gocql.ColumnPartitionKey:
			pkColumns = append(pkColumns, name)
		case gocql.ColumnClusteringKey:
			ckColumns = append(ckColumns, name)
		default:
			otherColumns = append(otherColumns, name)
		}
	}

	columnTypes := make(map[string]TypeInfo, len(meta.Columns))
	for colName, colMeta := range meta.Columns {
		info := parseType(colMeta.Type)
		columnTypes[colName] = info
	}

	dr := &DeltaReplicator{
		session:     session,
		tableName:   meta.Keyspace + "." + meta.Name,
		consistency: consistency,

		pkColumns:    pkColumns,
		ckColumns:    ckColumns,
		otherColumns: otherColumns,
		columnTypes:  columnTypes,
		allColumns:   append(append(append([]string{}, otherColumns...), pkColumns...), ckColumns...),
	}

	dr.computeRowDeleteQuery()
	dr.computePartitionDeleteQuery()
	dr.computeRangeDeleteQueries()

	return dr, nil
}

func (r *DeltaReplicator) computeRowDeleteQuery() {
	keyColumns := append(append([]string{}, r.pkColumns...), r.ckColumns...)

	r.rowDeleteQueryStr = fmt.Sprintf(
		"DELETE FROM %s WHERE %s",
		r.tableName,
		r.makeBindMarkerAssignments(keyColumns, " AND "),
	)
}

func (r *DeltaReplicator) computePartitionDeleteQuery() {
	r.partitionDeleteQueryStr = fmt.Sprintf(
		"DELETE FROM %s WHERE %s",
		r.tableName,
		r.makeBindMarkerAssignments(r.pkColumns, " AND "),
	)
}

func (r *DeltaReplicator) computeRangeDeleteQueries() {
	r.rangeDeleteQueryStrs = make([]string, 0, 8*len(r.ckColumns))

	prefix := fmt.Sprintf("DELETE FROM %s WHERE ", r.tableName)
	eqConds := r.makeBindMarkerAssignmentList(r.pkColumns)

	for _, ckCol := range r.ckColumns {
		for typ := 0; typ < 8; typ++ {
			startOp := [3]string{">=", ">", ""}[typ%3]
			endOp := [3]string{"<=", "<", ""}[typ/3]
			condsWithBounds := eqConds
			if startOp != "" {
				condsWithBounds = append(
					condsWithBounds,
					fmt.Sprintf("%s %s ?", ckCol, startOp),
				)
			}
			if endOp != "" {
				condsWithBounds = append(
					condsWithBounds,
					fmt.Sprintf("%s %s ?", ckCol, endOp),
				)
			}
			queryStr := prefix + strings.Join(condsWithBounds, " AND ")
			r.rangeDeleteQueryStrs = append(r.rangeDeleteQueryStrs, queryStr)
		}

		eqConds = append(eqConds, ckCol+" = ?")
	}
}

func (r *DeltaReplicator) Consume(c scylla_cdc.Change) error {
	timestamp := c.GetCassandraTimestamp()
	pos := 0

	for pos < len(c.Delta) {
		change := c.Delta[pos]
		var err error
		switch change.GetOperation() {
		case scylla_cdc.Update:
			err = r.processUpdate(timestamp, change)
			pos++

		case scylla_cdc.Insert:
			err = r.processInsert(timestamp, change)
			pos++

		case scylla_cdc.RowDelete:
			err = r.processRowDelete(timestamp, change)
			pos++

		case scylla_cdc.PartitionDelete:
			err = r.processPartitionDelete(timestamp, change)
			pos++

		case scylla_cdc.RangeDeleteStartInclusive, scylla_cdc.RangeDeleteStartExclusive:
			// TODO: Check that we aren't at the end?
			start := change
			end := c.Delta[pos+1]
			err = r.processRangeDelete(timestamp, start, end)
			pos += 2

		default:
			panic("unsupported operation: " + change.GetOperation().String())
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (r *DeltaReplicator) End() {
	// TODO: Take a snapshot here
}

func (r *DeltaReplicator) processUpdate(timestamp int64, c *scylla_cdc.ChangeRow) error {
	return r.processInsertOrUpdate(timestamp, false, c)
}

func (r *DeltaReplicator) processInsert(timestamp int64, c *scylla_cdc.ChangeRow) error {
	return r.processInsertOrUpdate(timestamp, true, c)
}

func (r *DeltaReplicator) processInsertOrUpdate(timestamp int64, isInsert bool, c *scylla_cdc.ChangeRow) error {
	batch := gocql.NewBatch(gocql.UnloggedBatch)

	keyColumns := append(r.pkColumns, r.ckColumns...)

	if isInsert {
		// Insert row to make a row marker
		// The rest of the columns will be set by using UPDATE queries
		var bindMarkers []string
		for _, columnName := range keyColumns {
			bindMarkers = append(bindMarkers, makeBindMarkerForType(r.columnTypes[columnName]))
		}

		insertStr := fmt.Sprintf(
			"INSERT INTO %s (%s) VALUES (%s) USING TTL ?",
			r.tableName, strings.Join(keyColumns, ", "), strings.Join(bindMarkers, ", "),
		)

		var vals []interface{}
		vals = appendKeyValuesToBind(vals, keyColumns, c)
		vals = append(vals, c.GetTTL())
		batch.Query(insertStr, vals...)
	}

	// Precompute the WHERE x = ? AND y = ? ... part
	pkConditions := r.makeBindMarkerAssignments(keyColumns, " AND ")

	for _, colName := range r.otherColumns {
		typ := r.columnTypes[colName]
		isNonFrozenCollection := !typ.IsFrozen() && typ.Type().IsCollection()

		if !isNonFrozenCollection {
			scalarChange := c.GetScalarChange(colName)
			if scalarChange.IsDeleted {
				// Delete the value from the column
				deleteStr := fmt.Sprintf(
					"DELETE %s FROM %s WHERE %s",
					colName, r.tableName, pkConditions,
				)

				var vals []interface{}
				vals = appendKeyValuesToBind(vals, keyColumns, c)
				batch.Query(deleteStr, vals...)
			} else if scalarChange.Value != nil {
				// The column was overwritten
				updateStr := fmt.Sprintf(
					"UPDATE %s USING TTL ? SET %s = %s WHERE %s",
					r.tableName, colName, makeBindMarkerForType(typ), pkConditions,
				)

				var vals []interface{}
				vals = append(vals, c.GetTTL())
				vals = appendValueByType(vals, scalarChange.Value, typ)
				vals = appendKeyValuesToBind(vals, keyColumns, c)
				batch.Query(updateStr, vals...)
			}
		} else if typ.Type() == TypeList {
			listChange := c.GetListChange(colName)
			if listChange.IsReset {
				// We can't just do UPDATE SET l = [...],
				// because we need to precisely control timestamps
				// of the list cells. This can be done only by
				// UPDATE tbl SET l[SCYLLA_TIMEUUID_LIST_INDEX(?)] = ?,
				// which is equivalent to an append of one cell.
				// Hence, the need for clear + append.
				//
				// We clear using a timestamp one-less-than the real
				// timestamp of the write. This is what Cassandra/Scylla
				// does internally, so it's OK to for us to do that.
				deleteStr := fmt.Sprintf(
					"DELETE %s FROM %s USING TIMESTAMP ? WHERE %s",
					colName, r.tableName, pkConditions,
				)

				var vals []interface{}
				clearTimestamp := timestamp
				if listChange.AppendedElements != nil {
					clearTimestamp--
				}
				vals = append(vals, clearTimestamp)
				vals = appendKeyValuesToBind(vals, keyColumns, c)
				batch.Query(deleteStr, vals...)
			}
			if listChange.AppendedElements != nil {
				// TODO: Explain
				setStr := fmt.Sprintf(
					"UPDATE %s USING TTL ? SET %s[SCYLLA_TIMEUUID_LIST_INDEX(?)] = %s WHERE %s",
					r.tableName, colName, makeBindMarkerForType(typ), pkConditions,
				)

				rAppendedElements := reflect.ValueOf(listChange.AppendedElements)
				r := rAppendedElements.MapRange()
				for r.Next() {
					k := r.Key().Interface()
					v := r.Value().Interface()

					var vals []interface{}
					vals = append(vals, c.GetTTL())
					vals = append(vals, k)
					vals = appendValueByType(vals, v, typ)
					vals = appendKeyValuesToBind(vals, keyColumns, c)
					batch.Query(setStr, vals...)
				}
			}
			if listChange.RemovedElements != nil {
				// TODO: Explain
				clearStr := fmt.Sprintf(
					"UPDATE %s SET %s[SCYLLA_TIMEUUID_LIST_INDEX(?)] = null WHERE %s",
					r.tableName, colName, pkConditions,
				)

				rRemovedElements := reflect.ValueOf(listChange.RemovedElements)
				elsLen := rRemovedElements.Len()
				for i := 0; i < elsLen; i++ {
					k := rRemovedElements.Index(i).Interface()

					var vals []interface{}
					vals = append(vals, k)
					vals = appendKeyValuesToBind(vals, keyColumns, c)
					batch.Query(clearStr, vals...)
				}
			}
		} else if typ.Type() == TypeSet || typ.Type() == TypeMap {
			// TODO: Better comment
			// Fortunately, both cases can be handled by the same code
			// by using reflection. We are forced to use reflection anyway.
			var (
				added, removed interface{}
				isReset        bool
			)

			if typ.Type() == TypeSet {
				setChange := c.GetSetChange(colName)
				added = setChange.AddedElements
				removed = setChange.RemovedElements
				isReset = setChange.IsReset
			} else {
				mapChange := c.GetSetChange(colName)
				added = mapChange.AddedElements
				removed = mapChange.RemovedElements
				isReset = mapChange.IsReset
			}

			if isReset {
				// Overwrite the existing value
				setStr := fmt.Sprintf(
					"UPDATE %s USING TTL ? SET %s = ? WHERE %s",
					r.tableName, colName, pkConditions,
				)

				var vals []interface{}
				vals = append(vals, c.GetTTL())
				vals = append(vals, added)
				vals = appendKeyValuesToBind(vals, keyColumns, c)
				batch.Query(setStr, vals...)
			} else {
				if added != nil {
					// Add elements
					addStr := fmt.Sprintf(
						"UPDATE %s USING TTL ? SET %s = %s + ? WHERE %s",
						r.tableName, colName, colName, pkConditions,
					)

					var vals []interface{}
					vals = append(vals, c.GetTTL())
					vals = append(vals, added)
					vals = appendKeyValuesToBind(vals, keyColumns, c)
					batch.Query(addStr, vals...)
				}
				if removed != nil {
					// Removed elements
					remStr := fmt.Sprintf(
						"UPDATE %s USING TTL ? SET %s = %s - ? WHERE %s",
						r.tableName, colName, colName, pkConditions,
					)

					var vals []interface{}
					vals = append(vals, c.GetTTL())
					vals = append(vals, removed)
					vals = appendKeyValuesToBind(vals, keyColumns, c)
					batch.Query(remStr, vals...)
				}
			}
		} else if typ.Type() == TypeUDT {
			udtChange := c.GetUDTChange(colName)
			if udtChange.IsReset {
				// The column was overwritten
				updateStr := fmt.Sprintf(
					"UPDATE %s USING TTL ? SET %s = %s WHERE %s",
					r.tableName, colName, makeBindMarkerForType(typ), pkConditions,
				)

				var vals []interface{}
				vals = append(vals, c.GetTTL())
				vals = appendValueByType(vals, udtChange.AddedFields, typ)
				vals = appendKeyValuesToBind(vals, keyColumns, c)
				batch.Query(updateStr, vals...)
			} else {
				// Overwrite those columns which are non-null in AddedFields,
				// and remove those which are listed in RemovedFields.
				// In order to do this, we need to know the schema
				// of the UDT.

				// TODO: Optimize, this makes processing of the row quadratic
				colInfos := c.Columns()
				var udtInfo gocql.UDTTypeInfo
				for _, colInfo := range colInfos {
					if colInfo.Name == colName {
						udtInfo = colInfo.TypeInfo.(gocql.UDTTypeInfo)
						break
					}
				}

				elementValues := make([]interface{}, len(udtInfo.Elements))

				// Determine which elements to set, which to remove and which to ignore
				for i := range elementValues {
					elementValues[i] = gocql.UnsetValue
				}
				for i, el := range udtInfo.Elements {
					v := udtChange.AddedFields[el.Name]
					// TODO: Do we want to use pointers in maps?
					if v != nil && !reflect.ValueOf(v).IsNil() {
						elementValues[i] = v
					}
				}
				for _, idx := range udtChange.RemovedFields {
					elementValues[idx] = nil
				}

				// Send an individual query for each field that is being updated
				for i, el := range udtInfo.Elements {
					v := elementValues[i]
					if v == gocql.UnsetValue {
						continue
					}

					// fmt.Printf("    %#v\n", v)

					bindValue := "null"
					if v != nil {
						// TODO: This should be "typ" for the UDT element
						bindValue = makeBindMarkerForType(typ)
					}

					updateFieldStr := fmt.Sprintf(
						"UPDATE %s USING TTL ? SET %s.%s = %s WHERE %s",
						r.tableName, colName, el.Name, bindValue, pkConditions,
					)

					var vals []interface{}
					vals = append(vals, c.GetTTL())
					if v != nil {
						vals = appendValueByType(vals, v, typ)
					}
					vals = appendKeyValuesToBind(vals, keyColumns, c)
					batch.Query(updateFieldStr, vals...)
				}
			}
		}
	}

	batch.SetConsistency(r.consistency)
	batch.WithTimestamp(timestamp)

	if debugQueries {
		for _, ent := range batch.Entries {
			fmt.Println(ent.Stmt)
			fmt.Println(ent.Args...)
		}
	}

	err := r.session.ExecuteBatch(batch)
	if err != nil {
		typ := "update"
		if isInsert {
			typ = "insert"
		}
		fmt.Printf("ERROR while trying to %s: %s\n", typ, err)
	}

	return err
}

func (r *DeltaReplicator) processRowDelete(timestamp int64, c *scylla_cdc.ChangeRow) error {
	// TODO: Cache vals?
	vals := make([]interface{}, 0, len(r.pkColumns)+len(r.ckColumns))
	vals = appendKeyValuesToBind(vals, r.pkColumns, c)
	vals = appendKeyValuesToBind(vals, r.ckColumns, c)

	if debugQueries {
		fmt.Println(r.rowDeleteQueryStr)
		fmt.Println(vals...)
	}

	// TODO: Propagate errors
	err := r.session.
		Query(r.rowDeleteQueryStr, vals...).
		Consistency(r.consistency).
		Idempotent(true).
		WithTimestamp(timestamp).
		Exec()
	if err != nil {
		fmt.Printf("ERROR while trying to delete row: %s\n", err)
	}

	return err
}

func (r *DeltaReplicator) processPartitionDelete(timestamp int64, c *scylla_cdc.ChangeRow) error {
	// TODO: Cache vals?
	vals := make([]interface{}, 0, len(r.pkColumns))
	vals = appendKeyValuesToBind(vals, r.pkColumns, c)

	if debugQueries {
		fmt.Println(r.partitionDeleteQueryStr)
		fmt.Println(vals...)
	}

	err := r.session.
		Query(r.partitionDeleteQueryStr, vals...).
		Consistency(r.consistency).
		Idempotent(true).
		WithTimestamp(timestamp).
		Exec()
	if err != nil {
		fmt.Printf("ERROR while trying to delete partition: %s\n", err)
	}

	// TODO: Retries
	return err
}

func (r *DeltaReplicator) processRangeDelete(timestamp int64, start, end *scylla_cdc.ChangeRow) error {
	// TODO: Cache vals?
	vals := make([]interface{}, 0, len(r.pkColumns)+len(r.ckColumns)+1)
	vals = appendKeyValuesToBind(vals, r.pkColumns, start)

	// Find the right query to use
	var (
		prevRight         interface{}
		left, right       interface{}
		hasLeft, hasRight bool
		baseIdx           int = -1
	)

	// TODO: Explain what this loop does
	for i, ckCol := range r.ckColumns {
		left, hasLeft = start.GetValue(ckCol)
		right, hasRight = end.GetValue(ckCol)

		if hasLeft {
			if hasRight {
				// Has both left and right
				// It's either a delete bounded from two sides, or it's an
				// equality condition
				// If it's the last ck column or the next ck column will be null
				// in both start and end, then it's an two-sided bound
				// If not, it's an equality condition
				prevRight = right
				vals = append(vals, left)
				continue
			} else {
				// Bounded from the left
				vals = append(vals, left)
				baseIdx = i
				break
			}
		} else {
			if hasRight {
				// Bounded from the right
				vals = append(vals, right)
				baseIdx = i
				break
			} else {
				// The previous column was a two-sided bound
				// In previous iteration, we appended the left bound
				// Append the right bound
				vals = append(vals, prevRight)
				hasLeft = true
				hasRight = true
				baseIdx = i - 1
				break
			}
		}
	}

	if baseIdx == -1 {
		// It's a two-sided bound
		vals = append(vals, prevRight)
		baseIdx = len(r.ckColumns) - 1
	}

	// Magic... TODO: Make it more clear
	leftOff := 2
	if hasLeft {
		leftOff = int(start.GetOperation() - scylla_cdc.RangeDeleteStartInclusive)
	}
	rightOff := 2
	if hasRight {
		rightOff = int(end.GetOperation() - scylla_cdc.RangeDeleteEndInclusive)
	}
	queryIdx := 8*baseIdx + leftOff + 3*rightOff
	queryStr := r.rangeDeleteQueryStrs[queryIdx]

	if debugQueries {
		fmt.Println(queryStr)
		fmt.Println(vals...)
	}

	err := r.session.
		Query(queryStr, vals...).
		Consistency(r.consistency).
		Idempotent(true).
		WithTimestamp(timestamp).
		Exec()
	if err != nil {
		fmt.Printf("ERROR while trying to delete range: %s\n", err)
	}

	// TODO: Retries
	return err
}

func (r *DeltaReplicator) makeBindMarkerAssignmentList(columnNames []string) []string {
	assignments := make([]string, 0, len(columnNames))
	for _, name := range columnNames {
		assignments = append(assignments, name+" = "+makeBindMarkerForType(r.columnTypes[name]))
	}
	return assignments
}

func (r *DeltaReplicator) makeBindMarkerAssignments(columnNames []string, sep string) string {
	assignments := r.makeBindMarkerAssignmentList(columnNames)
	return strings.Join(assignments, sep)
}

func makeBindMarkerForType(typ TypeInfo) string {
	if typ.Type() != TypeTuple {
		return "?"
	}
	tupleTyp := typ.Unfrozen().(*TupleType)
	vals := make([]string, 0, len(tupleTyp.Elements))
	for range tupleTyp.Elements {
		// vals = append(vals, makeBindMarkerForType(typ))
		vals = append(vals, "?")
	}
	return "(" + strings.Join(vals, ", ") + ")"
}

func appendValueByType(vals []interface{}, v interface{}, typ TypeInfo) []interface{} {
	if typ.Type() == TypeTuple {
		vTup := v.([]interface{})
		vals = append(vals, vTup...)
	} else {
		vals = append(vals, v)
	}
	return vals
}

func appendKeyValuesToBind(
	vals []interface{},
	names []string,
	c *scylla_cdc.ChangeRow,
) []interface{} {
	// No need to handle non-frozen lists here, because they can't appear
	// in either partition or clustering key
	for _, name := range names {
		v, ok := c.GetValue(name)
		if !ok {
			v = gocql.UnsetValue
		}
		vals = append(vals, v)
	}
	return vals
}
