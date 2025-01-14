package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/aoldershaw/eventsource"
	"github.com/pkg/errors"
)

const (
	insertSQL = `INSERT INTO ${TABLE} (aggregate_id, data, version) VALUES (?, ?, ?)`
	selectSQL = `SELECT data, version FROM ${TABLE} WHERE aggregate_id = ? AND version >= ? AND version <= ? ORDER BY version ASC`
	readSQL   = `SELECT id, aggregate_id, data, version FROM ${TABLE} WHERE id >= ? ORDER BY ID LIMIT ?`
)

// DB provides a smaller surface area for the db calls used; Exec is only used by the create function
type DB interface {
	// Exec is implemented by *sql.DB and *sql.Tx
	Exec(query string, args ...interface{}) (sql.Result, error)
	// PrepareContext is implemented by *sql.DB and *sql.Tx
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	// Query is implemented by *sql.DB and *sql.Tx
	Query(query string, args ...interface{}) (*sql.Rows, error)
}

// Accessor provides a standard interface to allow the store to obtain a db connection
type Accessor interface {
	// Open requests a new connection
	Open(ctx context.Context) (DB, error)

	// Close will be called when the store is finished with the connection
	Close(DB) error
}

// Store provides an eventsource.Store implementation backed by mysql
type Store struct {
	tableName string
	accessor  Accessor
}

func (s *Store) expand(statement string) string {
	return strings.Replace(statement, "${TABLE}", s.tableName, -1)
}

// Save the provided serialized records to the store
func (s *Store) Save(ctx context.Context, aggregateID string, records ...eventsource.Record) error {
	if len(records) == 0 {
		return nil
	}

	db, err := s.accessor.Open(ctx)
	if err != nil {
		return errors.Wrap(err, "save failed; unable to connect to db")
	}
	defer s.accessor.Close(db)

	stmt, err := db.PrepareContext(ctx, s.expand(insertSQL))
	if err != nil {
		return errors.Wrap(err, "unable to prepare statement")
	}
	defer stmt.Close()

	for _, record := range records {
		_, err = stmt.Exec(aggregateID, record.Data, record.Version)
		if err != nil {
			return s.isIdempotent(ctx, db, aggregateID, records...)
		}
	}

	return nil
}

func (s *Store) isIdempotent(ctx context.Context, db DB, aggregateID string, records ...eventsource.Record) error {
	segments := eventsource.History(records)
	sort.Sort(segments)

	fromVersion := segments[0].Version
	toVersion := segments[len(segments)-1].Version
	loaded, err := s.doLoad(ctx, db, aggregateID, fromVersion, toVersion)
	if err != nil {
		return fmt.Errorf("unable to retrieve version %v-%v for aggregate, %v", fromVersion, toVersion, aggregateID)
	}

	if !reflect.DeepEqual(segments, loaded) {
		return fmt.Errorf("unable to save records; conflicting records detected for aggregate, %v", aggregateID)
	}

	return nil
}

// Load the history of events up to the version specified; when version is
// 0, all events will be loaded
func (s *Store) Load(ctx context.Context, aggregateID string, fromVersion, toVersion int) (eventsource.History, error) {
	db, err := s.accessor.Open(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "load failed; unable to connect to db")
	}
	defer s.accessor.Close(db)

	return s.doLoad(ctx, db, aggregateID, fromVersion, toVersion)
}

func (s *Store) doLoad(ctx context.Context, db DB, aggregateID string, initialVersion, version int) (eventsource.History, error) {
	if version == 0 {
		version = math.MaxInt32
	}

	rows, err := db.Query(s.expand(selectSQL), aggregateID, initialVersion, version)
	if err != nil {
		return nil, errors.Wrap(err, "load failed; unable to query rows")
	}

	history := eventsource.History{}
	for rows.Next() {
		record := eventsource.Record{}
		if err := rows.Scan(&record.Data, &record.Version); err != nil {
			return nil, errors.Wrap(err, "load failed; unable to parse row")
		}
		history = append(history, record)
	}

	return history, nil
}

func (s *Store) Read(ctx context.Context, startingOffset uint64, recordCount int) ([]eventsource.StreamRecord, error) {
	db, err := s.accessor.Open(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "load failed; unable to connect to db")
	}
	defer s.accessor.Close(db)

	records := make([]eventsource.StreamRecord, 0, recordCount)
	rows, err := db.Query(s.expand(readSQL), startingOffset, recordCount)
	if err != nil {
		return nil, errors.Wrap(err, "read failed; unable to read records from db")
	}
	defer rows.Close()

	for rows.Next() {
		record := eventsource.StreamRecord{}
		if err := rows.Scan(&record.Offset, &record.AggregateID, &record.Data, &record.Version); err != nil {
			return nil, errors.Wrapf(err, "failed to scan stream record from db")
		}
		records = append(records, record)
	}

	return records, nil
}

// New returns a new postgres backed eventsource.Store
func New(tableName string, accessor Accessor) (*Store, error) {
	store := &Store{
		tableName: tableName,
		accessor:  accessor,
	}

	return store, nil
}
