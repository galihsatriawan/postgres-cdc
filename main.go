package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgconn"
	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgproto3/v2"
	"github.com/jackc/pgtype"
)

// Note that runtime parameter "replication=database" in connection string is obligatory
// replicaiton slot will not be created if replication=database is omitted

const CONN = "postgres://docker:docker@localhost:5400/postgres?replication=database"
const SLOT_NAME = "second_replication_slot"
const OUTPUT_PLUGIN = "pgoutput"
const TABLE_NAME = "trial"

var INSERT_TEMPLATE = fmt.Sprintf("CREATE TABLE IF NOT EXISTS \"%s\"(id int, name text);", TABLE_NAME)

var Event = struct {
	Relation string
	Columns  []string
}{}

func main() {
	const outputPlugin = "pgoutput"
	conn, err := pgconn.Connect(context.Background(), CONN)
	if err != nil {
		log.Fatalln("failed to connect to PostgreSQL server:", err)
	}
	defer conn.Close(context.Background())

	result := conn.Exec(context.Background(), "DROP PUBLICATION IF EXISTS pglogrepl_demo;")
	_, err = result.ReadAll()
	if err != nil {
		log.Fatalln("drop publication if exists error", err)
	}

	result = conn.Exec(context.Background(), "CREATE PUBLICATION pglogrepl_demo FOR ALL TABLES;")
	_, err = result.ReadAll()
	if err != nil {
		log.Fatalln("create publication error", err)
	}
	log.Println("create publication pglogrepl_demo")

	var pluginArguments []string
	if outputPlugin == "pgoutput" {
		pluginArguments = []string{"proto_version '1'", "publication_names 'pglogrepl_demo'"}
	} else if outputPlugin == "wal2json" {
		pluginArguments = []string{"\"pretty-print\" 'true'"}
	}

	sysident, err := pglogrepl.IdentifySystem(context.Background(), conn)
	if err != nil {
		log.Fatalln("IdentifySystem failed:", err)
	}
	log.Println("SystemID:", sysident.SystemID, "Timeline:", sysident.Timeline, "XLogPos:", sysident.XLogPos, "DBName:", sysident.DBName)

	slotName := "pglogrepl_demo"
	getReplicationSlot := fmt.Sprintf("SELECT * FROM pg_replication_slots WHERE slot_name='%s'", slotName)
	res, err := conn.Exec(context.Background(), getReplicationSlot).ReadAll()
	if err != nil {
		panic(err)
	}
	if len(res) == 0 {
		_, err = pglogrepl.CreateReplicationSlot(context.Background(), conn, slotName, outputPlugin, pglogrepl.CreateReplicationSlotOptions{Temporary: false})
		if err != nil {
			log.Fatalln("CreateReplicationSlot failed:", err)
		}
		log.Println("Created temporary replication slot:", slotName)
	}

	log.Println("Log pos", sysident.XLogPos)
	err = pglogrepl.StartReplication(context.Background(), conn, slotName, sysident.XLogPos, pglogrepl.StartReplicationOptions{PluginArgs: pluginArguments})
	if err != nil {
		log.Fatalln("StartReplication failed:", err)
	}
	log.Println("Logical replication started on slot", slotName)

	clientXLogPos := sysident.XLogPos
	standbyMessageTimeout := time.Second * 10
	nextStandbyMessageDeadline := time.Now().Add(standbyMessageTimeout)
	relations := map[uint32]*pglogrepl.RelationMessage{}
	connInfo := pgtype.NewConnInfo()

	for {
		if time.Now().After(nextStandbyMessageDeadline) {
			err = pglogrepl.SendStandbyStatusUpdate(context.Background(), conn, pglogrepl.StandbyStatusUpdate{
				WALWritePosition: clientXLogPos,
			})
			if err != nil {
				log.Fatalln("SendStandbyStatusUpdate failed:", err)
			}
			log.Println("Sent Standby status message")
			nextStandbyMessageDeadline = time.Now().Add(standbyMessageTimeout)
		}

		ctx, cancel := context.WithDeadline(context.Background(), nextStandbyMessageDeadline)
		rawMsg, err := conn.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			log.Fatalln("ReceiveMessage failed:", err)
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			log.Println(fmt.Errorf("received Postgres WAL error: %+v", errMsg))
			return
		}

		msg, ok := rawMsg.(*pgproto3.CopyData)
		if !ok {
			log.Printf("Received unexpected message: %T\n", rawMsg)
			continue
		}

		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParsePrimaryKeepaliveMessage failed:", err)
			}
			log.Println("Primary Keepalive Message =>", "ServerWALEnd:", pkm.ServerWALEnd, "ServerTime:", pkm.ServerTime, "ReplyRequested:", pkm.ReplyRequested)

			if pkm.ReplyRequested {
				nextStandbyMessageDeadline = time.Time{}
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParseXLogData failed:", err)
			}
			log.Println("XLogData =>", "WALStart", xld.WALStart, "ServerWALEnd", xld.ServerWALEnd, "ServerTime:", xld.ServerTime, "WALData", string(xld.WALData))
			logicalMsg, err := pglogrepl.Parse(xld.WALData)
			if err != nil {
				log.Fatalf("Parse logical replication message: %s", err)
			}
			log.Printf("Receive a logical replication message: %s", logicalMsg.Type())
			switch logicalMsg := logicalMsg.(type) {
			case *pglogrepl.RelationMessage:
				relations[logicalMsg.RelationID] = logicalMsg

			case *pglogrepl.BeginMessage:
				fmt.Println("begin")
				// Indicates the beginning of a group of changes in a transaction. This is only sent for committed transactions. You won't get any events from rolled back transactions.

			case *pglogrepl.CommitMessage:
				fmt.Println("commit")

			case *pglogrepl.InsertMessage:
				rel, ok := relations[logicalMsg.RelationID]
				if !ok {
					log.Fatalf("unknown relation ID %d", logicalMsg.RelationID)
				}
				values := map[string]interface{}{}
				for idx, col := range logicalMsg.Tuple.Columns {
					colName := rel.Columns[idx].Name
					switch col.DataType {
					case 'n': // null
						values[colName] = nil
					case 'u': // unchanged toast
						// This TOAST value was not changed. TOAST values are not stored in the tuple, and logical replication doesn't want to spend a disk read to fetch its value for you.
						fmt.Println(col.Data)
					case 't': //text
						val, err := decodeTextColumnData(connInfo, col.Data, rel.Columns[idx].DataType)
						if err != nil {
							log.Fatalln("error decoding column data:", err)
						}
						values[colName] = val
					case 'b':
						fmt.Println("byte data", col.Data)
					}

				}
				log.Printf("INSERT INTO %s.%s: %v", rel.Namespace, rel.RelationName, values)

			case *pglogrepl.UpdateMessage:
				// ...
				rel, ok := relations[logicalMsg.RelationID]
				if !ok {
					log.Fatalf("unknown relation ID %d", logicalMsg.RelationID)
				}
				oldValues := map[string]interface{}{}
				fmt.Println("oldTuple", logicalMsg.OldTuple)
				if logicalMsg.OldTuple != nil {
					for idx, col := range logicalMsg.OldTuple.Columns {
						colName := rel.Columns[idx].Name
						switch col.DataType {
						case 'n': // null
							oldValues[colName] = nil
						case 'u': // unchanged toast
							// This TOAST value was not changed. TOAST values are not stored in the tuple, and logical replication doesn't want to spend a disk read to fetch its value for you.
							fmt.Println(col.Data)
						case 't': //text
							val, err := decodeTextColumnData(connInfo, col.Data, rel.Columns[idx].DataType)
							if err != nil {
								log.Fatalln("error decoding column data:", err)
							}
							oldValues[colName] = val
						case 'b':
							fmt.Println("byte data", col.Data)
						}

					}
				}
				newValues := map[string]interface{}{}
				for idx, col := range logicalMsg.NewTuple.Columns {
					colName := rel.Columns[idx].Name
					switch col.DataType {
					case 'n': // null
						newValues[colName] = nil
					case 'u': // unchanged toast
						// This TOAST value was not changed. TOAST values are not stored in the tuple, and logical replication doesn't want to spend a disk read to fetch its value for you.
						fmt.Println(col.Data)
					case 't': //text
						val, err := decodeTextColumnData(connInfo, col.Data, rel.Columns[idx].DataType)
						if err != nil {
							log.Fatalln("error decoding column data:", err)
						}
						newValues[colName] = val
					case 'b':
						fmt.Println("byte data", col.Data)
					}

				}

				log.Printf("Update %s.%s: after %v before %v", rel.Namespace, rel.RelationName, newValues, oldValues)

			case *pglogrepl.DeleteMessage:
				// ...
				s, _ := json.Marshal(logicalMsg)
				fmt.Println("delete message", string(s))
			case *pglogrepl.TruncateMessage:
				// ...
				s, _ := json.Marshal(logicalMsg)
				fmt.Println("truncate message", string(s))

			case *pglogrepl.TypeMessage:
				fmt.Println("type message", logicalMsg)
			case *pglogrepl.OriginMessage:
			default:
				log.Printf("Unknown message type in pgoutput stream: %T", logicalMsg)
			}

			clientXLogPos = xld.WALStart + pglogrepl.LSN(len(xld.WALData))
		}
	}
}

func decodeTextColumnData(ci *pgtype.ConnInfo, data []byte, dataType uint32) (interface{}, error) {
	var decoder pgtype.TextDecoder
	if dt, ok := ci.DataTypeForOID(dataType); ok {
		decoder, ok = dt.Value.(pgtype.TextDecoder)
		if !ok {
			decoder = &pgtype.GenericText{}
		}
	} else {
		decoder = &pgtype.GenericText{}
	}
	if err := decoder.DecodeText(ci, data); err != nil {
		return nil, err
	}
	return decoder.(pgtype.Value).Get(), nil
}
