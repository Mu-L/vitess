// Copyright 2014, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tabletserver

import (
	"fmt"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/hack"
	mproto "github.com/youtube/vitess/go/mysql/proto"
	"github.com/youtube/vitess/go/sqltypes"
	"github.com/youtube/vitess/go/vt/callinfo"
	"github.com/youtube/vitess/go/vt/schema"
	"github.com/youtube/vitess/go/vt/sqlparser"
	"github.com/youtube/vitess/go/vt/tabletserver/planbuilder"
	"golang.org/x/net/context"
)

// QueryExecutor is used for executing a query request.
type QueryExecutor struct {
	query         string
	bindVars      map[string]interface{}
	transactionID int64
	plan          *ExecPlan
	ctx           context.Context
	logStats      *SQLQueryStats
	qe            *QueryEngine
}

// poolConn is the interface implemented by users of this specialized pool.
type poolConn interface {
	Exec(ctx context.Context, query string, maxrows int, wantfields bool) (*mproto.QueryResult, error)
}

// Execute performs a non-streaming query execution.
func (qre *QueryExecutor) Execute() (reply *mproto.QueryResult) {
	qre.logStats.OriginalSql = qre.query
	qre.logStats.BindVariables = qre.bindVars
	qre.logStats.TransactionID = qre.transactionID
	planName := qre.plan.PlanId.String()
	qre.logStats.PlanType = planName
	defer func(start time.Time) {
		duration := time.Now().Sub(start)
		qre.qe.queryServiceStats.QueryStats.Add(planName, duration)
		if reply == nil {
			qre.plan.AddStats(1, duration, 0, 1)
			return
		}
		qre.plan.AddStats(1, duration, int64(reply.RowsAffected), 0)
		qre.logStats.RowsAffected = int(reply.RowsAffected)
		qre.logStats.Rows = reply.Rows
		qre.qe.queryServiceStats.ResultStats.Add(int64(len(reply.Rows)))
	}(time.Now())

	qre.checkPermissions()

	if qre.plan.PlanId == planbuilder.PLAN_DDL {
		return qre.execDDL()
	}

	if qre.transactionID != 0 {
		// Need upfront connection for DMLs and transactions
		conn := qre.qe.txPool.Get(qre.transactionID)
		defer conn.Recycle()
		conn.RecordQuery(qre.query)
		var invalidator CacheInvalidator
		if qre.plan.TableInfo != nil && qre.plan.TableInfo.CacheType != schema.CACHE_NONE {
			invalidator = conn.DirtyKeys(qre.plan.TableName)
		}
		switch qre.plan.PlanId {
		case planbuilder.PLAN_PASS_DML:
			if qre.qe.strictMode.Get() != 0 {
				panic(NewTabletError(ErrFail, "DML too complex"))
			}
			reply = qre.directFetch(conn, qre.plan.FullQuery, qre.bindVars, nil)
		case planbuilder.PLAN_INSERT_PK:
			reply = qre.execInsertPK(conn)
		case planbuilder.PLAN_INSERT_SUBQUERY:
			reply = qre.execInsertSubquery(conn)
		case planbuilder.PLAN_DML_PK:
			reply = qre.execDMLPK(conn, invalidator)
		case planbuilder.PLAN_DML_SUBQUERY:
			reply = qre.execDMLSubquery(conn, invalidator)
		case planbuilder.PLAN_OTHER:
			reply = qre.execSQL(conn, qre.query, true)
		default: // select or set in a transaction, just count as select
			reply = qre.execDirect(conn)
		}
	} else {
		switch qre.plan.PlanId {
		case planbuilder.PLAN_PASS_SELECT:
			if qre.plan.Reason == planbuilder.REASON_LOCK {
				panic(NewTabletError(ErrFail, "Disallowed outside transaction"))
			}
			reply = qre.execSelect()
		case planbuilder.PLAN_PK_IN:
			reply = qre.execPKIN()
		case planbuilder.PLAN_SELECT_SUBQUERY:
			reply = qre.execSubquery()
		case planbuilder.PLAN_SET:
			reply = qre.execSet()
		case planbuilder.PLAN_OTHER:
			conn := qre.getConn(qre.qe.connPool)
			defer conn.Recycle()
			reply = qre.execSQL(conn, qre.query, true)
		default:
			if !qre.qe.enableAutoCommit {
				panic(NewTabletError(ErrFatal, "unsupported query: %s", qre.query))
			}
			reply = qre.execDmlAutoCommit()
		}
	}
	return reply
}

// Stream performs a streaming query execution.
func (qre *QueryExecutor) Stream(sendReply func(*mproto.QueryResult) error) {
	qre.logStats.OriginalSql = qre.query
	qre.logStats.PlanType = qre.plan.PlanId.String()
	defer qre.qe.queryServiceStats.QueryStats.Record(qre.plan.PlanId.String(), time.Now())

	qre.checkPermissions()

	conn := qre.getConn(qre.qe.streamConnPool)
	defer conn.Recycle()

	qd := NewQueryDetail(qre.logStats.ctx, conn)
	qre.qe.streamQList.Add(qd)
	defer qre.qe.streamQList.Remove(qd)

	qre.fullStreamFetch(conn, qre.plan.FullQuery, qre.bindVars, nil, sendReply)
}

func (qre *QueryExecutor) execDmlAutoCommit() (reply *mproto.QueryResult) {
	transactionID := qre.qe.txPool.Begin(qre.ctx)
	qre.logStats.AddRewrittenSql("begin", time.Now())
	defer func() {
		err := recover()
		if err == nil {
			qre.qe.Commit(qre.ctx, qre.logStats, transactionID)
			qre.logStats.AddRewrittenSql("commit", time.Now())
		} else {
			qre.qe.txPool.Rollback(qre.ctx, transactionID)
			qre.logStats.AddRewrittenSql("rollback", time.Now())
			panic(err)
		}
	}()
	conn := qre.qe.txPool.Get(transactionID)
	defer conn.Recycle()
	var invalidator CacheInvalidator
	if qre.plan.TableInfo != nil && qre.plan.TableInfo.CacheType != schema.CACHE_NONE {
		invalidator = conn.DirtyKeys(qre.plan.TableName)
	}
	switch qre.plan.PlanId {
	case planbuilder.PLAN_PASS_DML:
		if qre.qe.strictMode.Get() != 0 {
			panic(NewTabletError(ErrFail, "DML too complex"))
		}
		reply = qre.directFetch(conn, qre.plan.FullQuery, qre.bindVars, nil)
	case planbuilder.PLAN_INSERT_PK:
		reply = qre.execInsertPK(conn)
	case planbuilder.PLAN_INSERT_SUBQUERY:
		reply = qre.execInsertSubquery(conn)
	case planbuilder.PLAN_DML_PK:
		reply = qre.execDMLPK(conn, invalidator)
	case planbuilder.PLAN_DML_SUBQUERY:
		reply = qre.execDMLSubquery(conn, invalidator)
	default:
		panic(NewTabletError(ErrFatal, "unsupported query: %s", qre.query))
	}
	return reply
}

func (qre *QueryExecutor) checkPermissions() {
	// Skip permissions check if we have a background context.
	if qre.ctx == context.Background() {
		return
	}

	// Blacklist
	remoteAddr := ""
	username := ""
	ci, ok := callinfo.FromContext(qre.ctx)
	if ok {
		remoteAddr = ci.RemoteAddr()
		username = ci.Username()
	}
	action, desc := qre.plan.Rules.getAction(remoteAddr, username, qre.bindVars)
	switch action {
	case QR_FAIL:
		panic(NewTabletError(ErrFail, "Query disallowed due to rule: %s", desc))
	case QR_FAIL_RETRY:
		panic(NewTabletError(ErrRetry, "Query disallowed due to rule: %s", desc))
	}

	// Perform table ACL check if it is enabled
	if qre.plan.Authorized != nil && !qre.plan.Authorized.IsMember(username) {
		errStr := fmt.Sprintf("table acl error: %q cannot run %v on table %q", username, qre.plan.PlanId, qre.plan.TableName)
		// Raise error if in strictTableAcl mode, else just log an error
		if qre.qe.strictTableAcl {
			panic(NewTabletError(ErrFail, "%s", errStr))
		}
		qre.qe.accessCheckerLogger.Errorf("%s", errStr)
	}
}

func (qre *QueryExecutor) execDDL() *mproto.QueryResult {
	ddlPlan := planbuilder.DDLParse(qre.query)
	if ddlPlan.Action == "" {
		panic(NewTabletError(ErrFail, "DDL is not understood"))
	}

	txid := qre.qe.txPool.Begin(qre.ctx)
	defer qre.qe.txPool.SafeCommit(qre.ctx, txid)

	// Stolen from Execute
	conn := qre.qe.txPool.Get(txid)
	defer conn.Recycle()
	result := qre.execSQL(conn, qre.query, false)

	if ddlPlan.TableName != "" && ddlPlan.TableName != ddlPlan.NewName {
		// It's a drop or rename.
		qre.qe.schemaInfo.DropTable(ddlPlan.TableName)
	}
	if ddlPlan.NewName != "" {
		qre.qe.schemaInfo.CreateOrUpdateTable(qre.ctx, ddlPlan.NewName)
	}
	return result
}

func (qre *QueryExecutor) execPKIN() (result *mproto.QueryResult) {
	pkRows, err := buildValueList(qre.plan.TableInfo, qre.plan.PKValues, qre.bindVars)
	if err != nil {
		panic(err)
	}
	return qre.fetchMulti(pkRows, getLimit(qre.plan.Limit, qre.bindVars))
}

func (qre *QueryExecutor) execSubquery() (result *mproto.QueryResult) {
	innerResult := qre.qFetch(qre.logStats, qre.plan.Subquery, qre.bindVars)
	return qre.fetchMulti(innerResult.Rows, -1)
}

func (qre *QueryExecutor) fetchMulti(pkRows [][]sqltypes.Value, limit int64) (result *mproto.QueryResult) {
	if qre.plan.Fields == nil {
		panic("unexpected")
	}
	result = &mproto.QueryResult{Fields: qre.plan.Fields}
	if len(pkRows) == 0 || limit == 0 {
		return
	}

	tableInfo := qre.plan.TableInfo
	keys := make([]string, len(pkRows))
	for i, pk := range pkRows {
		keys[i] = buildKey(pk)
	}
	rcresults := tableInfo.Cache.Get(qre.ctx, keys)
	rows := make([][]sqltypes.Value, 0, len(pkRows))
	missingRows := make([][]sqltypes.Value, 0, len(pkRows))
	var hits, absent, misses int64
	for i, pk := range pkRows {
		rcresult := rcresults[keys[i]]
		if rcresult.Row != nil {
			if qre.mustVerify() {
				qre.spotCheck(rcresult, pk)
			}
			rows = append(rows, applyFilter(qre.plan.ColumnNumbers, rcresult.Row))
			hits++
		} else {
			missingRows = append(missingRows, pk)
		}
	}
	if len(missingRows) != 0 {
		bv := map[string]interface{}{
			"#pk": sqlparser.TupleEqualityList{
				Columns: qre.plan.TableInfo.Indexes[0].Columns,
				Rows:    missingRows,
			},
		}
		resultFromdb := qre.qFetch(qre.logStats, qre.plan.OuterQuery, bv)
		misses = int64(len(resultFromdb.Rows))
		absent = int64(len(pkRows)) - hits - misses
		for _, row := range resultFromdb.Rows {
			rows = append(rows, applyFilter(qre.plan.ColumnNumbers, row))
			key := buildKey(applyFilter(qre.plan.TableInfo.PKColumns, row))
			tableInfo.Cache.Set(qre.ctx, key, row, rcresults[key].Cas)
		}
	}

	qre.logStats.CacheHits = hits
	qre.logStats.CacheAbsent = absent
	qre.logStats.CacheMisses = misses

	qre.logStats.QuerySources |= QuerySourceRowcache

	tableInfo.hits.Add(hits)
	tableInfo.absent.Add(absent)
	tableInfo.misses.Add(misses)
	result.RowsAffected = uint64(len(rows))
	result.Rows = rows
	// limit == 0 is already addressed upfront.
	if limit > 0 && len(result.Rows) > int(limit) {
		result.Rows = result.Rows[:limit]
		result.RowsAffected = uint64(limit)
	}
	return result
}

func (qre *QueryExecutor) mustVerify() bool {
	return (Rand() % spotCheckMultiplier) < qre.qe.spotCheckFreq.Get()
}

func (qre *QueryExecutor) spotCheck(rcresult RCResult, pk []sqltypes.Value) {
	qre.qe.queryServiceStats.SpotCheckCount.Add(1)
	bv := map[string]interface{}{
		"#pk": sqlparser.TupleEqualityList{
			Columns: qre.plan.TableInfo.Indexes[0].Columns,
			Rows:    [][]sqltypes.Value{pk},
		},
	}
	resultFromdb := qre.qFetch(qre.logStats, qre.plan.OuterQuery, bv)
	var dbrow []sqltypes.Value
	if len(resultFromdb.Rows) != 0 {
		dbrow = resultFromdb.Rows[0]
	}
	if dbrow == nil || !rowsAreEqual(rcresult.Row, dbrow) {
		qre.qe.Launch(func() { qre.recheckLater(rcresult, dbrow, pk) })
	}
}

func (qre *QueryExecutor) recheckLater(rcresult RCResult, dbrow []sqltypes.Value, pk []sqltypes.Value) {
	time.Sleep(10 * time.Second)
	keys := make([]string, 1)
	keys[0] = buildKey(pk)
	reloaded := qre.plan.TableInfo.Cache.Get(context.Background(), keys)[keys[0]]
	// If reloaded row is absent or has changed, we're good
	if reloaded.Row == nil || reloaded.Cas != rcresult.Cas {
		return
	}
	log.Warningf("query: %v", qre.plan.FullQuery)
	log.Warningf("mismatch for: %v\ncache: %v\ndb:    %v", pk, rcresult.Row, dbrow)
	qre.qe.queryServiceStats.InternalErrors.Add("Mismatch", 1)
}

// execDirect always sends the query to mysql
func (qre *QueryExecutor) execDirect(conn poolConn) (result *mproto.QueryResult) {
	if qre.plan.Fields != nil {
		result = qre.directFetch(conn, qre.plan.FullQuery, qre.bindVars, nil)
		result.Fields = qre.plan.Fields
		return
	}
	result = qre.fullFetch(conn, qre.plan.FullQuery, qre.bindVars, nil)
	return
}

// execSelect sends a query to mysql only if another identical query is not running. Otherwise, it waits and
// reuses the result. If the plan is missng field info, it sends the query to mysql requesting full info.
func (qre *QueryExecutor) execSelect() (result *mproto.QueryResult) {
	if qre.plan.Fields != nil {
		result = qre.qFetch(qre.logStats, qre.plan.FullQuery, qre.bindVars)
		result.Fields = qre.plan.Fields
		return
	}
	conn := qre.getConn(qre.qe.connPool)
	defer conn.Recycle()
	return qre.fullFetch(conn, qre.plan.FullQuery, qre.bindVars, nil)
}

func (qre *QueryExecutor) execInsertPK(conn poolConn) (result *mproto.QueryResult) {
	pkRows, err := buildValueList(qre.plan.TableInfo, qre.plan.PKValues, qre.bindVars)
	if err != nil {
		panic(err)
	}
	return qre.execInsertPKRows(conn, pkRows)
}

func (qre *QueryExecutor) execInsertSubquery(conn poolConn) (result *mproto.QueryResult) {
	innerResult := qre.directFetch(conn, qre.plan.Subquery, qre.bindVars, nil)
	innerRows := innerResult.Rows
	if len(innerRows) == 0 {
		return &mproto.QueryResult{RowsAffected: 0}
	}
	if len(qre.plan.ColumnNumbers) != len(innerRows[0]) {
		panic(NewTabletError(ErrFail, "Subquery length does not match column list"))
	}
	pkRows := make([][]sqltypes.Value, len(innerRows))
	for i, innerRow := range innerRows {
		pkRows[i] = applyFilterWithPKDefaults(qre.plan.TableInfo, qre.plan.SubqueryPKColumns, innerRow)
	}
	// Validating first row is sufficient
	if err := validateRow(qre.plan.TableInfo, qre.plan.TableInfo.PKColumns, pkRows[0]); err != nil {
		panic(err)
	}

	qre.bindVars["#values"] = innerRows
	return qre.execInsertPKRows(conn, pkRows)
}

func (qre *QueryExecutor) execInsertPKRows(conn poolConn, pkRows [][]sqltypes.Value) (result *mproto.QueryResult) {
	secondaryList, err := buildSecondaryList(qre.plan.TableInfo, pkRows, qre.plan.SecondaryPKValues, qre.bindVars)
	if err != nil {
		panic(err)
	}
	bsc := buildStreamComment(qre.plan.TableInfo, pkRows, secondaryList)
	result = qre.directFetch(conn, qre.plan.OuterQuery, qre.bindVars, bsc)
	return result
}

func (qre *QueryExecutor) execDMLPK(conn poolConn, invalidator CacheInvalidator) (result *mproto.QueryResult) {
	pkRows, err := buildValueList(qre.plan.TableInfo, qre.plan.PKValues, qre.bindVars)
	if err != nil {
		panic(err)
	}
	return qre.execDMLPKRows(conn, pkRows, invalidator)
}

func (qre *QueryExecutor) execDMLSubquery(conn poolConn, invalidator CacheInvalidator) (result *mproto.QueryResult) {
	innerResult := qre.directFetch(conn, qre.plan.Subquery, qre.bindVars, nil)
	return qre.execDMLPKRows(conn, innerResult.Rows, invalidator)
}

func (qre *QueryExecutor) execDMLPKRows(conn poolConn, pkRows [][]sqltypes.Value, invalidator CacheInvalidator) (result *mproto.QueryResult) {
	if len(pkRows) == 0 {
		return &mproto.QueryResult{RowsAffected: 0}
	}
	secondaryList, err := buildSecondaryList(qre.plan.TableInfo, pkRows, qre.plan.SecondaryPKValues, qre.bindVars)
	if err != nil {
		panic(err)
	}

	result = &mproto.QueryResult{}
	maxRows := int(qre.qe.maxDMLRows.Get())
	for i := 0; i < len(pkRows); i += maxRows {
		end := i + maxRows
		if end >= len(pkRows) {
			end = len(pkRows)
		}
		pkRows := pkRows[i:end]
		secondaryList := secondaryList
		if secondaryList != nil {
			secondaryList = secondaryList[i:end]
		}
		bsc := buildStreamComment(qre.plan.TableInfo, pkRows, secondaryList)
		qre.bindVars["#pk"] = sqlparser.TupleEqualityList{
			Columns: qre.plan.TableInfo.Indexes[0].Columns,
			Rows:    pkRows,
		}
		r := qre.directFetch(conn, qre.plan.OuterQuery, qre.bindVars, bsc)
		// DMLs should only return RowsAffected.
		result.RowsAffected += r.RowsAffected
	}
	if invalidator == nil {
		return result
	}
	for _, pk := range pkRows {
		key := buildKey(pk)
		invalidator.Delete(key)
	}
	return result
}

func (qre *QueryExecutor) execSet() (result *mproto.QueryResult) {
	switch qre.plan.SetKey {
	case "vt_pool_size":
		qre.qe.connPool.SetCapacity(int(getInt64(qre.plan.SetValue)))
	case "vt_stream_pool_size":
		qre.qe.streamConnPool.SetCapacity(int(getInt64(qre.plan.SetValue)))
	case "vt_transaction_cap":
		qre.qe.txPool.pool.SetCapacity(int(getInt64(qre.plan.SetValue)))
	case "vt_transaction_timeout":
		qre.qe.txPool.SetTimeout(getDuration(qre.plan.SetValue))
	case "vt_schema_reload_time":
		qre.qe.schemaInfo.SetReloadTime(getDuration(qre.plan.SetValue))
	case "vt_query_cache_size":
		qre.qe.schemaInfo.SetQueryCacheSize(int(getInt64(qre.plan.SetValue)))
	case "vt_max_result_size":
		val := getInt64(qre.plan.SetValue)
		if val < 1 {
			panic(NewTabletError(ErrFail, "vt_max_result_size out of range %v", val))
		}
		qre.qe.maxResultSize.Set(val)
	case "vt_max_dml_rows":
		val := getInt64(qre.plan.SetValue)
		if val < 1 {
			panic(NewTabletError(ErrFail, "vt_max_dml_rows out of range %v", val))
		}
		qre.qe.maxDMLRows.Set(val)
	case "vt_stream_buffer_size":
		val := getInt64(qre.plan.SetValue)
		if val < 1024 {
			panic(NewTabletError(ErrFail, "vt_stream_buffer_size out of range %v", val))
		}
		qre.qe.streamBufferSize.Set(val)
	case "vt_query_timeout":
		qre.qe.queryTimeout.Set(getDuration(qre.plan.SetValue))
	case "vt_idle_timeout":
		t := getDuration(qre.plan.SetValue)
		qre.qe.connPool.SetIdleTimeout(t)
		qre.qe.streamConnPool.SetIdleTimeout(t)
		qre.qe.txPool.pool.SetIdleTimeout(t)
	case "vt_spot_check_ratio":
		qre.qe.spotCheckFreq.Set(int64(getFloat64(qre.plan.SetValue) * spotCheckMultiplier))
	case "vt_strict_mode":
		qre.qe.strictMode.Set(getInt64(qre.plan.SetValue))
	case "vt_txpool_timeout":
		t := getDuration(qre.plan.SetValue)
		qre.qe.txPool.SetPoolTimeout(t)
	default:
		conn := qre.getConn(qre.qe.connPool)
		defer conn.Recycle()
		return qre.directFetch(conn, qre.plan.FullQuery, qre.bindVars, nil)
	}
	return &mproto.QueryResult{}
}

func getInt64(v interface{}) int64 {
	if ival, ok := v.(int64); ok {
		return ival
	}
	panic(NewTabletError(ErrFail, "expecting int"))
}

func getFloat64(v interface{}) float64 {
	if ival, ok := v.(int64); ok {
		return float64(ival)
	}
	if fval, ok := v.(float64); ok {
		return fval
	}
	panic(NewTabletError(ErrFail, "expecting number"))
}

func getDuration(v interface{}) time.Duration {
	return time.Duration(getFloat64(v) * 1e9)
}

func rowsAreEqual(row1, row2 []sqltypes.Value) bool {
	if len(row1) != len(row2) {
		return false
	}
	for i := 0; i < len(row1); i++ {
		if row1[i].IsNull() && row2[i].IsNull() {
			continue
		}
		if (row1[i].IsNull() && !row2[i].IsNull()) || (!row1[i].IsNull() && row2[i].IsNull()) || row1[i].String() != row2[i].String() {
			return false
		}
	}
	return true
}

func (qre *QueryExecutor) getConn(pool *ConnPool) *DBConn {
	start := time.Now()
	conn, err := pool.Get(qre.ctx)
	switch err {
	case nil:
		qre.logStats.WaitingForConnection += time.Now().Sub(start)
		return conn
	case ErrConnPoolClosed:
		panic(err)
	}
	panic(NewTabletErrorSql(ErrFatal, err))
}

func (qre *QueryExecutor) qFetch(logStats *SQLQueryStats, parsedQuery *sqlparser.ParsedQuery, bindVars map[string]interface{}) (result *mproto.QueryResult) {
	sql := qre.generateFinalSql(parsedQuery, bindVars, nil)
	q, ok := qre.qe.consolidator.Create(string(sql))
	if ok {
		defer q.Broadcast()
		waitingForConnectionStart := time.Now()
		conn, err := qre.qe.connPool.Get(qre.ctx)
		logStats.WaitingForConnection += time.Now().Sub(waitingForConnectionStart)
		if err != nil {
			q.Err = NewTabletErrorSql(ErrFatal, err)
		} else {
			defer conn.Recycle()
			q.Result, q.Err = qre.execSQLNoPanic(conn, sql, false)
		}
	} else {
		logStats.QuerySources |= QuerySourceConsolidator
		startTime := time.Now()
		q.Wait()
		qre.qe.queryServiceStats.WaitStats.Record("Consolidations", startTime)
	}
	if q.Err != nil {
		panic(q.Err)
	}
	return q.Result.(*mproto.QueryResult)
}

func (qre *QueryExecutor) directFetch(conn poolConn, parsedQuery *sqlparser.ParsedQuery, bindVars map[string]interface{}, buildStreamComment []byte) (result *mproto.QueryResult) {
	sql := qre.generateFinalSql(parsedQuery, bindVars, buildStreamComment)
	return qre.execSQL(conn, sql, false)
}

// fullFetch also fetches field info
func (qre *QueryExecutor) fullFetch(conn poolConn, parsedQuery *sqlparser.ParsedQuery, bindVars map[string]interface{}, buildStreamComment []byte) (result *mproto.QueryResult) {
	sql := qre.generateFinalSql(parsedQuery, bindVars, buildStreamComment)
	return qre.execSQL(conn, sql, true)
}

func (qre *QueryExecutor) fullStreamFetch(conn *DBConn, parsedQuery *sqlparser.ParsedQuery, bindVars map[string]interface{}, buildStreamComment []byte, callback func(*mproto.QueryResult) error) {
	sql := qre.generateFinalSql(parsedQuery, bindVars, buildStreamComment)
	qre.execStreamSQL(conn, sql, callback)
}

func (qre *QueryExecutor) generateFinalSql(parsedQuery *sqlparser.ParsedQuery, bindVars map[string]interface{}, buildStreamComment []byte) string {
	bindVars["#maxLimit"] = qre.qe.maxResultSize.Get() + 1
	sql, err := parsedQuery.GenerateQuery(bindVars)
	if err != nil {
		panic(NewTabletError(ErrFail, "%s", err))
	}
	if buildStreamComment != nil {
		sql = append(sql, buildStreamComment...)
	}
	// undo hack done by stripTrailing
	sql = restoreTrailing(sql, bindVars)
	return hack.String(sql)
}

func (qre *QueryExecutor) execSQL(conn poolConn, sql string, wantfields bool) *mproto.QueryResult {
	result, err := qre.execSQLNoPanic(conn, sql, true)
	if err != nil {
		panic(err)
	}
	return result
}

func (qre *QueryExecutor) execSQLNoPanic(conn poolConn, sql string, wantfields bool) (*mproto.QueryResult, error) {
	defer qre.logStats.AddRewrittenSql(sql, time.Now())
	return conn.Exec(qre.ctx, sql, int(qre.qe.maxResultSize.Get()), wantfields)
}

func (qre *QueryExecutor) execStreamSQL(conn *DBConn, sql string, callback func(*mproto.QueryResult) error) {
	start := time.Now()
	err := conn.Stream(qre.ctx, sql, callback, int(qre.qe.streamBufferSize.Get()))
	qre.logStats.AddRewrittenSql(sql, start)
	if err != nil {
		panic(NewTabletErrorSql(ErrFail, err))
	}
}
