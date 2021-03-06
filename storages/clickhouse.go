package storages

import (
	"context"
	"fmt"
	"github.com/hashicorp/go-multierror"
	"github.com/ksensehq/eventnative/adapters"
	"github.com/ksensehq/eventnative/appconfig"
	"github.com/ksensehq/eventnative/appstatus"
	"github.com/ksensehq/eventnative/events"
	"github.com/ksensehq/eventnative/schema"
	"log"
	"math/rand"
)

const clickHouseStorageType = "ClickHouse"

//Store files to ClickHouse in two modes:
//batch: (1 file = 1 transaction)
//stream: (1 object = 1 transaction)
type ClickHouse struct {
	name            string
	adapters        []*adapters.ClickHouse
	tableHelpers    []*TableHelper
	schemaProcessor *schema.Processor
	eventQueue      *events.PersistentQueue
	breakOnError    bool
}

func NewClickHouse(ctx context.Context, name, fallbackDir string, config *adapters.ClickHouseConfig, processor *schema.Processor,
	breakOnError, streamMode bool) (*ClickHouse, error) {
	tableStatementFactory, err := adapters.NewTableStatementFactory(config)
	if err != nil {
		return nil, err
	}

	var eventQueue *events.PersistentQueue
	if streamMode {
		var err error
		queueName := fmt.Sprintf("%s-%s", appconfig.Instance.ServerName, name)
		eventQueue, err = events.NewPersistentQueue(queueName, fallbackDir)
		if err != nil {
			return nil, err
		}
	}

	//put default values and values from config
	nonNullFields := map[string]bool{"eventn_ctx_event_id": true, "_timestamp": true}
	if config.Engine != nil {
		for _, fieldName := range config.Engine.NonNullFields {
			nonNullFields[fieldName] = true
		}
	}

	monitorKeeper := NewMonitorKeeper()

	var chAdapters []*adapters.ClickHouse
	var tableHelpers []*TableHelper
	for _, dsn := range config.Dsns {
		adapter, err := adapters.NewClickHouse(ctx, dsn, config.Database, config.Cluster, config.Tls, tableStatementFactory, nonNullFields)
		if err != nil {
			//close all previous created adapters
			for _, toClose := range chAdapters {
				toClose.Close()
			}
			return nil, err
		}

		chAdapters = append(chAdapters, adapter)
		tableHelpers = append(tableHelpers, NewTableHelper(adapter, monitorKeeper, clickHouseStorageType))
	}

	ch := &ClickHouse{
		name:            name,
		adapters:        chAdapters,
		tableHelpers:    tableHelpers,
		schemaProcessor: processor,
		eventQueue:      eventQueue,
		breakOnError:    breakOnError,
	}

	adapter, _ := ch.getAdapters()
	err = adapter.CreateDB(config.Database)
	if err != nil {
		//close all previous created adapters
		for _, toClose := range chAdapters {
			toClose.Close()
		}
		return nil, err
	}

	if streamMode {
		ch.startStreamingConsumer()
	}

	return ch, nil
}

func (ch *ClickHouse) Name() string {
	return ch.name
}

func (ch *ClickHouse) Type() string {
	return clickHouseStorageType
}

//Consume events.Fact and enqueue it
func (ch *ClickHouse) Consume(fact events.Fact) {
	if err := ch.eventQueue.Enqueue(fact); err != nil {
		logSkippedEvent(fact, err)
	}
}

//Run goroutine to:
//1. read from queue
//2. insert in ClickHouse
func (ch *ClickHouse) startStreamingConsumer() {
	go func() {
		for {
			if appstatus.Instance.Idle {
				break
			}
			fact, err := ch.eventQueue.DequeueBlock()
			if err != nil {
				log.Println("Error reading event fact from clickhouse queue", err)
				continue
			}

			dataSchema, flattenObject, err := ch.schemaProcessor.ProcessFact(fact)
			if err != nil {
				log.Printf("Unable to process object %v: %v", fact, err)
				continue
			}

			//don't process empty object
			if !dataSchema.Exists() {
				continue
			}

			if err := ch.insert(dataSchema, flattenObject); err != nil {
				log.Printf("Error inserting to clickhouse table [%s]: %v", dataSchema.Name, err)
				continue
			}
		}
	}()
}

//insert fact in ClickHouse
func (ch *ClickHouse) insert(dataSchema *schema.Table, fact events.Fact) (err error) {
	adapter, tableHelper := ch.getAdapters()

	dbSchema, err := tableHelper.EnsureTable(dataSchema)
	if err != nil {
		return err
	}

	if err := ch.schemaProcessor.ApplyDBTypingToObject(dbSchema, fact); err != nil {
		return err
	}

	return adapter.Insert(dataSchema, fact)
}

//Store file payload to ClickHouse with processing
func (ch *ClickHouse) Store(fileName string, payload []byte) error {
	flatData, err := ch.schemaProcessor.ProcessFilePayload(fileName, payload, ch.breakOnError)
	if err != nil {
		return err
	}

	adapter, tableHelper := ch.getAdapters()
	//process db tables & schema
	for _, fdata := range flatData {
		dbSchema, err := tableHelper.EnsureTable(fdata.DataSchema)
		if err != nil {
			return err
		}

		if err := ch.schemaProcessor.ApplyDBTyping(dbSchema, fdata); err != nil {
			return err
		}
	}

	//insert all data in one transaction
	tx, err := adapter.OpenTx()
	if err != nil {
		return fmt.Errorf("Error opening clickhouse transaction: %v", err)
	}

	for _, fdata := range flatData {
		for _, object := range fdata.GetPayload() {
			if err := adapter.InsertInTransaction(tx, fdata.DataSchema, object); err != nil {
				if ch.breakOnError {
					tx.Rollback()
					return err
				} else {
					log.Printf("Warn: unable to insert object %v reason: %v. This line will be skipped", object, err)
				}
			}
		}
	}

	return tx.DirectCommit()
}

//Close adapters.ClickHouse
func (ch *ClickHouse) Close() (multiErr error) {
	for i, adapter := range ch.adapters {
		if err := adapter.Close(); err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("Error closing clickhouse datasource[%d]: %v", i, err))
		}
	}

	if ch.eventQueue != nil {
		if err := ch.eventQueue.Close(); err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("Error closing clickhouse event queue: %v", err))
		}
	}

	return multiErr
}

//assume that adapters quantity == tableHelpers quantity
func (ch *ClickHouse) getAdapters() (*adapters.ClickHouse, *TableHelper) {
	num := rand.Intn(len(ch.adapters))
	return ch.adapters[num], ch.tableHelpers[num]
}
