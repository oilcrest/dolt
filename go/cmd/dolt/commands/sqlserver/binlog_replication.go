// Copyright 2022 Dolthub, Inc.
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

package sqlserver

import (
	"context"
	"fmt"
	"github.com/dolthub/dolt/go/cmd/dolt/commands/engine"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/globalstate"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/writer"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/vt/proto/query"
	"io"
	"strconv"
	"strings"
	"time"
)

// TODO: Move these into a struct to track?
var format mysql.BinlogFormat
var tableMapsById = make(map[uint64]*mysql.TableMap)

// TODO: Look at configuration interfaces for other replication options and naming patterns
type replicaConfiguration struct {
	sourceServerUuid string
	connectionParams *mysql.ConnParams
}

// NewReplicaConfiguration creates a new replica configuration for the server with a UUID of |sourceServerUuid|
// (found from the @@server_uuid variable on the source server) and |connectionParams| indicating how to connect
// to the source server.
func NewReplicaConfiguration(sourceServerUuid string, connectionParams *mysql.ConnParams) *replicaConfiguration {
	return &replicaConfiguration{
		sourceServerUuid: sourceServerUuid,
		connectionParams: connectionParams,
	}
}

// TODO: Turn this into a struct with an API that can be called
//	"replicationController" or something similar to match "clusterController"?

func replicaBinlogEventHandler(basicCtx context.Context, replicaConfiguration *replicaConfiguration, mrEnv *env.MultiRepoEnv, engine *engine.SqlEngine) error {
	// TODO: hardcoded replica configuration for now...
	replicaConfiguration = NewReplicaConfiguration(
		"748445ca-7d3b-11ec-b443-af8075c99077",
		&mysql.ConnParams{
			Host:  "localhost",
			Port:  3306,
			Uname: "root",
			Pass:  "",
		})

	// TODO: Should probably pass a sql.Context into this method to clean this up...?
	sqlCtx, err := engine.NewContext(basicCtx)
	if err != nil {
		return err
	}
	// TODO: Move this into test code and out of main library code
	sqlCtx.Session.SetClient(sql.Client{User: "root", Address: "%", Capabilities: 0})

	// Connect to the MySQL Replication Source
	// NOTE: Our fork of Vitess currently only supports mysql_native_password auth. The latest code in the main
	//       Vitess repo supports the current MySQL default auth plugin, caching_sha2_password.
	//       https://dev.mysql.com/blog-archive/upgrading-to-mysql-8-0-default-authentication-plugin-considerations/
	//       To work around this limitation, add the following to your /etc/my.cnf:
	//           [mysqld]
	//           default-authentication-plugin=mysql_native_password
	//       or start mysqld with:
	//           --default-authentication-plugin=mysql_native_password
	conn, err := mysql.Connect(basicCtx, replicaConfiguration.connectionParams)
	if err != nil {
		return err
	}

	// Request binlog events to start
	err = startReplicationEventStream(replicaConfiguration, conn)
	if err != nil {
		return err
	}

	// Process binlog events
	for {
		// TODO: How do we configure network timeouts?
		event, err := conn.ReadBinlogEvent()
		if err != nil {
			if sqlError, isSqlError := err.(*mysql.SQLError); isSqlError {
				if sqlError.Message == io.EOF.Error() {
					fmt.Printf("No more binlog messages; retrying in 1s...\n")
					// TODO: Use a channel for receiving signal to stop polling for events
					time.Sleep(1 * time.Second)
					continue
				} else if strings.Contains(sqlError.Message, "can not handle replication events with the checksum") {
					// For now, just ignore any errors about checksums
					fmt.Printf("!!! received checksum error message !!!\n")
					continue
				}
			}

			// otherwise, return the error if it's something we don't expect
			return err
		}

		err = processBinlogEvent(sqlCtx, mrEnv, engine, event)
		if err != nil {
			return err
		}
	}

	return nil
}

func processBinlogEvent(ctx *sql.Context, mrEnv *env.MultiRepoEnv, engine *engine.SqlEngine, event mysql.BinlogEvent) error {
	var err error

	switch {
	case event.IsRand():
		// A RAND_EVENT contains two seed values that set the rand_seed1 and rand_seed2 system variables that are
		// used to compute the random number. For more details, see: https://mariadb.com/kb/en/rand_event/
		// Note: it is written only before a QUERY_EVENT and is NOT used with row-based logging.
		fmt.Printf("Received: Rand event\n")

	case event.IsXID():
		// An XID event is generated for a COMMIT of a transaction that modifies one or more tables of an
		// XA-capable storage engine. For more details, see: https://mariadb.com/kb/en/xid_event/
		fmt.Printf("Received: XID event\n")
		// TODO: parse XID transaction number and perform a commit?
		//       gtid, b, err := event.GTID(format)

	case event.IsQuery():
		// A Query event represents a statement executed on the source server that should be executed on the
		// replica. Used for all statements with statement-based replication, DDL statements with row-based replication
		// as well as COMMITs for non-transactional engines such as MyISAM.
		// For more details, see: https://mariadb.com/kb/en/query_event/
		fmt.Printf("Received: Query event\n")
		query, err := event.Query(format)
		if err != nil {
			return err
		}
		fmt.Printf(" - %s \n", query.String())
		ctx.SetCurrentDatabase(query.Database)
		executeQueryWithEngine(ctx, engine, query.SQL)

	case event.IsRotate():
		// When a binary log file exceeds the configured size limit, a ROTATE_EVENT is written at the end of the file,
		// pointing to the next file in the sequence. ROTATE_EVENT is generated locally and written to the binary log
		// on the source server and it's also written when a FLUSH LOGS statement occurs on the source server.
		// For more details, see: https://mariadb.com/kb/en/rotate_event/
		fmt.Printf("Received: Rotate event\n")
		// TODO: What action do we take at rotate?

	case event.IsPreviousGTIDs():
		// Logged in every binlog to record the current replication state. Consists of the last GTID seen for each
		// replication domain. For more details, see: https://mariadb.com/kb/en/gtid_list_event/
		fmt.Printf("Received: PreviousGTIDs event\n")
		// TODO: Is there an action we should take here?

	case event.IsFormatDescription():
		// This is a descriptor event that is written to the beginning of a binary log file, at position 4 (after
		// the 4 magic number bytes). For more details, see: https://mariadb.com/kb/en/format_description_event/
		fmt.Printf("Received: FormatDescription event\n")
		format, err = event.Format()
		if err != nil {
			return err
		}

	case event.IsGTID():
		// For global transaction ID, used to start a new transaction event group, instead of the old BEGIN query event,
		// and also to mark stand-alone (ddl). For more details, see: https://mariadb.com/kb/en/gtid_event/
		fmt.Printf("Received: GTID event\n")
		// TODO: Does this mean we should perform a commit?
		// TODO: Read MariaDB KB docs on GTID: https://mariadb.com/kb/en/gtid/
		gtid, isBegin, err := event.GTID(format)
		fmt.Printf(" - %v (isBegin: %t) \n", gtid, isBegin)
		if err != nil {
			return err
		}

	case event.IsTableMap():
		// Used for row-based binary logging beginning (binlog_format=ROW or MIXED). This event precedes each row
		// operation event and maps a table definition to a number, where the table definition consists of database
		// and table names. For more details, see: https://mariadb.com/kb/en/table_map_event/
		fmt.Printf("Received: TableMap event\n")
		tableId := event.TableID(format)
		tableMap, err := event.TableMap(format)
		if err != nil {
			return err
		}
		tableMapsById[tableId] = tableMap
		fmt.Printf(" - tableMap: %v \n", formatTableMapAsString(tableId, tableMap))
		// TODO: Will these be resent before each row event, like the documentation seems to indicate? If so, that
		//       seems to remove the requirement to make this metadata durable between server restarts.

	case event.IsDeleteRows():
		// A ROWS_EVENT is written for row based replication if data is inserted, deleted or updated.
		// For more details, see: https://mariadb.com/kb/en/rows_event_v1v2-rows_compressed_event_v1/
		fmt.Printf("Received: DeleteRows event")
		tableId := event.TableID(format)
		tableMap, ok := tableMapsById[tableId]
		if !ok {
			return fmt.Errorf("unable to find replication metadata for table ID: %d", tableId)
		}
		rows, err := event.Rows(format, tableMap)
		if err != nil {
			return err
		}
		schema, err := getTableSchema(ctx, engine, tableMap.Name, tableMap.Database)
		if err != nil {
			return err
		}

		fmt.Printf(" - Deleted Rows (table: %s)\n", tableMap.Name)
		for _, row := range rows.Rows {
			deletedRow, err := parseRow(tableMap, schema, rows.IdentifyColumns, row.Identify)
			if err != nil {
				return err
			}
			fmt.Printf("     - Identify: %v \n", sql.FormatRow(deletedRow))

			writeSession, tableWriter, err := getTableWriter(ctx, tableMap.Name, tableMap.Database, mrEnv)
			if err != nil {
				return err
			}

			err = tableWriter.Delete(ctx, deletedRow)
			if err != nil {
				return err
			}

			err = closeWriteSession(ctx, tableMap.Database, writeSession, mrEnv)
			if err != nil {
				return err
			}
		}

	case event.IsWriteRows():
		// A ROWS_EVENT is written for row based replication if data is inserted, deleted or updated.
		// For more details, see: https://mariadb.com/kb/en/rows_event_v1v2-rows_compressed_event_v1/
		fmt.Printf("Received: WriteRows event\n")
		tableId := event.TableID(format)
		tableMap, ok := tableMapsById[tableId]
		if !ok {
			return fmt.Errorf("unable to find replication metadata for table ID: %d", tableId)
		}
		rows, err := event.Rows(format, tableMap)
		if err != nil {
			return err
		}
		schema, err := getTableSchema(ctx, engine, tableMap.Name, tableMap.Database)
		if err != nil {
			return err
		}

		fmt.Printf(" - New Rows (table: %s)\n", tableMap.Name)
		for _, row := range rows.Rows {
			newRow, err := parseRow(tableMap, schema, rows.DataColumns, row.Data)
			if err != nil {
				return err
			}
			fmt.Printf("     - Data: %v \n", sql.FormatRow(newRow))

			writeSession, tableWriter, err := getTableWriter(ctx, tableMap.Name, tableMap.Database, mrEnv)
			if err != nil {
				return err
			}

			err = tableWriter.Insert(ctx, newRow)
			if err != nil {
				return err
			}

			err = closeWriteSession(ctx, tableMap.Database, writeSession, mrEnv)
			if err != nil {
				return err
			}
		}

	case event.IsUpdateRows():
		// A ROWS_EVENT is written for row based replication if data is inserted, deleted or updated.
		// For more details, see: https://mariadb.com/kb/en/rows_event_v1v2-rows_compressed_event_v1/
		fmt.Printf("Received: UpdateRows event\n")
		tableId := event.TableID(format)
		tableMap, ok := tableMapsById[tableId]
		if !ok {
			return fmt.Errorf("unable to find replication metadata for table ID: %d", tableId)
		}
		rows, err := event.Rows(format, tableMap)
		if err != nil {
			return err
		}
		schema, err := getTableSchema(ctx, engine, tableMap.Name, tableMap.Database)
		if err != nil {
			return err
		}

		// TODO: do we need to process rows.Flags for anything?

		fmt.Printf(" - Updated Rows (table: %s)\n", tableMap.Name)
		for _, row := range rows.Rows {
			identifyRow, err := parseRow(tableMap, schema, rows.IdentifyColumns, row.Identify)
			if err != nil {
				return err
			}
			updatedRow, err := parseRow(tableMap, schema, rows.DataColumns, row.Data)
			if err != nil {
				return err
			}
			fmt.Printf("     - Identify: %v Data: %v \n", sql.FormatRow(identifyRow), sql.FormatRow(updatedRow))

			writeSession, tableWriter, err := getTableWriter(ctx, tableMap.Name, tableMap.Database, mrEnv)
			if err != nil {
				return err
			}

			err = tableWriter.Update(ctx, identifyRow, updatedRow)
			if err != nil {
				return err
			}

			err = closeWriteSession(ctx, tableMap.Database, writeSession, mrEnv)
			if err != nil {
				return err
			}
		}

	//case event.IsStop():
	// The primary server writes a STOP event to the binary log when it shuts down or when resuming after a mysqld
	// process crash. A new binary log file is always created but there is no ROTATE_EVENT. STOP_EVENT is then the
	// last written event after clean shutdown or resuming a crash.
	// NOTE: this event is NEVER sent to replica servers!

	default:
		// TODO: we can't access the bytes directly because the non-interface types are not exposed
		//       having a Bytes() or Type() method on the interface would let us clean this up.
		byteString := fmt.Sprintf("%v", event)
		if strings.HasPrefix(byteString, "{[0 0 0 0 27 ") {
			// Type 27 is a Heartbeat event. This event does not appear in the binary log. It's only sent over the
			// network by a primary to a replica to let it know that the primary is still alive, and is only sent
			// when the primary has no binlog events to send to replica servers.
			// For more details, see: https://mariadb.com/kb/en/heartbeat_log_event/
			fmt.Printf("Received: Heartbeat event\n")
		} else {
			return fmt.Errorf("received unknown event: %v", event)
		}
	}

	return nil
}

// closeWriteSession flushes and closes the specified |writeSession| and returns an error if anything failed.
func closeWriteSession(ctx *sql.Context, database string, writeSession writer.WriteSession, mrEnv *env.MultiRepoEnv) error {
	newWorkingSet, err := writeSession.Flush(ctx)
	if err != nil {
		return err
	}

	doltEnv := mrEnv.GetEnv(database)
	if doltEnv == nil {
		return fmt.Errorf("couldn't find a dolt environment named %q", database)
	}
	return doltEnv.UpdateWorkingSet(ctx, newWorkingSet)
}

// getTableSchema returns a sql.Schema for the specified table in the specified database.
func getTableSchema(ctx *sql.Context, engine *engine.SqlEngine, tableName, databaseName string) (sql.Schema, error) {
	database, err := engine.GetUnderlyingEngine().Analyzer.Catalog.Database(ctx, databaseName)
	if err != nil {
		return nil, err
	}
	table, ok, err := database.GetTableInsensitive(ctx, tableName)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("unable to find table %q", tableName)
	}

	return table.Schema(), nil
}

// getTableWriter returns a WriteSession and a TableWriter for writing to the specified |table| in the specified |database|.
func getTableWriter(ctx *sql.Context, table, database string, mrEnv *env.MultiRepoEnv) (writer.WriteSession, writer.TableWriter, error) {
	// TODO: This won't detect new databases created during replication!
	doltEnv := mrEnv.GetEnv(database)
	if doltEnv == nil {
		return nil, nil, fmt.Errorf("couldn't find a dolt environment named %q", database)
	}

	ws, err := doltEnv.WorkingSet(ctx)
	if err != nil {
		return nil, nil, err
	}

	// TODO: Does this work correctly?
	tracker, err := globalstate.NewAutoIncrementTracker(ctx, ws)
	if err != nil {
		return nil, nil, err
	}

	// TODO: plug in correct editor.Options
	writeSession := writer.NewWriteSession(doltEnv.DoltDB.Format(), ws, tracker, editor.Options{})

	tableWriter, err := writeSession.GetTableWriter(ctx, table, database, nil, false)
	if err != nil {
		return nil, nil, err
	}

	return writeSession, tableWriter, nil
}

// parseRow parses the binary row data from a MySQL binlog event and converts it into a go-mysql-server Row.
func parseRow(tableMap *mysql.TableMap, schema sql.Schema, bitmap mysql.Bitmap, data []byte) (sql.Row, error) {
	var parsedRow sql.Row
	pos := 0

	for i, typ := range tableMap.Types {
		column := schema[i]

		if bitmap.Bit(i) == false {
			parsedRow = append(parsedRow, nil)
			continue
		}

		// TODO: Plug in correct type (just needs to show signed/unsigned; why doesn't typ show that?)
		// TODO: Handle null cols
		value, length, err := mysql.CellValue(data, pos, typ, tableMap.Metadata[i], query.Type_INT8)
		if err != nil {
			fmt.Printf(" - !!! ERROR: %v \n", err)
			continue
		}
		pos += length

		// TODO: Seems like there should be a better way to convert the sqltypes.Value to the type
		//       GMS needs. Converting to a string and then converting again seems inefficient.
		var convertedValue interface{}
		switch {
		case sql.IsEnum(column.Type), sql.IsSet(column.Type):
			atoi, err := strconv.Atoi(value.ToString())
			if err != nil {
				return nil, err
			}
			convertedValue, err = column.Type.Convert(atoi)
		default:
			// TODO: Why does (10, "b") appear in table t1?
			//       It is inserted, but then the table is dropped, and recreated with new rows inserted,
			//       but the old (10, "b") row still appears in it.
			convertedValue, err = column.Type.Convert(value.ToString())
		}
		if err != nil {
			return nil, fmt.Errorf("unable to convert value %q: %v", value, err.Error())
		}
		parsedRow = append(parsedRow, convertedValue)
	}

	return parsedRow, nil
}

// startReplicationEventStream sends a request over |conn|, the connection to the MySQL source server, to begin
// sending binlog events.
func startReplicationEventStream(replicaConfiguration *replicaConfiguration, conn *mysql.Conn) error {
	sid, err := mysql.ParseSID(replicaConfiguration.sourceServerUuid)
	if err != nil {
		return err
	}
	gtid := mysql.Mysql56GTID{
		Server:   sid,
		Sequence: 1,
	}
	startPosition := mysql.Position{GTIDSet: gtid.GTIDSet()}
	// TODO: unhardcode 1 as the replica's server id
	return conn.SendBinlogDumpCommand(1, startPosition)
}

func formatTableMapAsString(tableId uint64, tableMap *mysql.TableMap) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("ID: %v, ", tableId))
	sb.WriteString(fmt.Sprintf("Name: %s, ", tableMap.Name))
	sb.WriteString(fmt.Sprintf("Database: %s, ", tableMap.Database))
	sb.WriteString(fmt.Sprintf("Flags: %v, ", tableMap.Flags))
	sb.WriteString(fmt.Sprintf("Metadata: %v, ", tableMap.Metadata))
	sb.WriteString(fmt.Sprintf("Types: %v, ", tableMap.Types))

	return sb.String()
}

func executeQueryWithEngine(ctx *sql.Context, engine *engine.SqlEngine, query string) {
	_, iter, err := engine.Query(ctx, query)
	if err != nil {
		fmt.Printf("!!! ERROR executing query: %v \n", err.Error())
		return
	}
	for {
		row, err := iter.Next(ctx)
		if err != nil {
			if err != io.EOF {
				fmt.Printf("!!! ERROR reading query results: %v \n", err.Error())
			}
			return
		}
		fmt.Printf(" row: %s \n", sql.FormatRow(row))
	}
}
