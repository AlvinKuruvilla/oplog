// Package oplog provides a generic oplog/replication system for REST APIs.
//
// Most of the time, the oplog service is used thru the oplogd agent which uses this
// package. But in the case your application is written in Go, you may want to integrate
// at the code level.
package oplog

import (
	"fmt"
	"io"
	"strconv"
	"time"

	log "github.com/Sirupsen/logrus"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

type OpLog struct {
	s     *mgo.Session
	Stats *Stats
}

type OpLogFilter struct {
	Types   []string
	Parents []string
}

type GenericEvent interface {
	io.WriterTo
	GetEventId() string
}

// OpLogEvent is used to send "technical" events with no data like "reset" or "live"
type OpLogEvent struct {
	Id    string
	Event string
}

type OperationDataMap map[string]OperationData

// parseObjectId returns a bson.ObjectId from an hex representation of an object id or nil
// if an empty string is passed or if the format of the id wasn't valid
func parseObjectId(id string) *bson.ObjectId {
	if id != "" && bson.IsObjectIdHex(id) {
		oid := bson.ObjectIdHex(id)
		return &oid
	}
	return nil
}

// parseTimestampId try to find a millisecond timestamp in the string and return it or return
// false as second value if can be parsed
func parseTimestampId(id string) (ts int64, ok bool) {
	ts = -1
	ok = false
	if len(id) <= 13 {
		if i, err := strconv.ParseInt(id, 10, 64); err == nil {
			ts = i
			ok = true
		}
	}
	return
}

// GetEventId returns an SSE event id
func (e OpLogEvent) GetEventId() string {
	return e.Id
}

// WriteTo serializes an event as a SSE compatible message
func (e OpLogEvent) WriteTo(w io.Writer) (int64, error) {
	n, err := fmt.Fprintf(w, "id: %s\nevent: %s\n\n", e.GetEventId(), e.Event)
	return int64(n), err
}

// New returns an OpLog connected to the given provided mongo URL.
// If the capped collection does not exists, it will be created with the max
// size defined by maxBytes parameter.
func New(mongoURL string, maxBytes int) (*OpLog, error) {
	session, err := mgo.Dial(mongoURL)
	if err != nil {
		return nil, err
	}
	stats := NewStats()
	oplog := &OpLog{session, &stats}
	oplog.init(maxBytes)
	return oplog, nil
}

// DB returns the Mongo database object used by the oplog
func (oplog *OpLog) DB() *mgo.Database {
	return oplog.s.Copy().DB("")
}

// init creates capped collection if it does not exists.
func (oplog *OpLog) init(maxBytes int) {
	oplogExists := false
	objectsExists := false
	names, _ := oplog.s.DB("").CollectionNames()
	for _, name := range names {
		switch name {
		case "oplog":
			oplogExists = true
		case "objects":
			objectsExists = true
		}
	}
	if !oplogExists {
		log.Info("OPLOG creating capped collection")
		err := oplog.s.DB("").C("oplog").Create(&mgo.CollectionInfo{
			Capped:   true,
			MaxBytes: maxBytes,
		})
		if err != nil {
			log.Fatal(err)
		}
	}
	if !objectsExists {
		log.Info("OPLOG creating objects index")
		if err := oplog.s.DB("").C("objects").EnsureIndexKey("event", "data.ts"); err != nil {
			log.Fatal(err)
		}
	}
}

// Ingest appends an operation into the OpLog thru a channel
func (oplog *OpLog) Ingest(ops <-chan *Operation) {
	db := oplog.DB()
	for {
		select {
		case op := <-ops:
			oplog.Stats.QueueSize.Set(len(ops))
			oplog.Append(op, db)
		}
	}
}

// Append appends an operation into the OpLog
//
// If the db parameter is not nil, the passed db connection is used. In case of
// error, the db pointer may be replaced by a new alive session.
func (oplog *OpLog) Append(op *Operation, db *mgo.Database) {
	if db == nil {
		db = oplog.DB()
	}
	log.Debugf("OPLOG ingest operation: %#v", op.Info())
	for {
		if err := db.C("oplog").Insert(op); err != nil {
			log.Warnf("OPLOG can't insert operation, try to reconnect: %s", err)
			// Try to reconnect
			time.Sleep(time.Second)
			db.Session.Close()
			db = oplog.DB()
			continue
		}
		break
	}
	// Apply the operation on the state collection
	o := ObjectState{
		Id:    op.Data.GetId(),
		Event: op.Event,
		Data:  op.Data,
	}
	for {
		if _, err := db.C("objects").Upsert(bson.M{"_id": o.Id}, o); err != nil {
			log.Warnf("OPLOG can't upsert object, try to reconnect: %s", err)
			// Try to reconnect
			time.Sleep(time.Second)
			db.Session.Close()
			db = oplog.DB()
			continue
		}
		break
	}
	oplog.Stats.EventsIngested.Incr()
}

// Diff finds which objects must be created or deleted in order to fix the delta
//
// The createMap is a map pointing to all objects present in the source database.
// The function search of differences between the passed map and the oplog database and
// remove objects identical in both sides from the createMap and populate the deleteMap
// with objects that are present in the oplog database but not in the source database.
// If an object is present in both createMap and the oplog database but timestamp of the
// oplog object is earlier than createMap's, the object is added to the updateMap.
func (oplog *OpLog) Diff(createMap OperationDataMap, updateMap OperationDataMap, deleteMap OperationDataMap) error {
	db := oplog.DB()
	defer db.Session.Close()

	// Find the most recent timestamp
	dumpTime := time.Unix(0, 0)
	for _, obd := range createMap {
		if obd.Timestamp.After(dumpTime) {
			dumpTime = obd.Timestamp
		}
	}

	obs := ObjectState{}
	defer db.Session.Close()
	iter := db.C("objects").Find(bson.M{"event": bson.M{"$ne": "delete"}}).Iter()
	for iter.Next(&obs) {
		if opd, ok := createMap[obs.Id]; ok {
			// Object exists on both sides, remove it from the create map
			delete(createMap, obs.Id)
			// If the dump object is newer than oplog's, add it to the update map
			if obs.Data.Timestamp.Before(opd.Timestamp) {
				updateMap[obs.Id] = opd
			}
		} else {
			// The object only exists in the oplog db, add it to the delete map
			// if the timestamp of the found object is older than oldest object
			// in the dump in order to ensure we don't delete an object which
			// have been created between the dump creation and the sync.
			if obs.Data.Timestamp.Before(dumpTime) {
				deleteMap[obs.Id] = *obs.Data
				delete(createMap, obs.Id)
			}
		}
	}
	if iter.Err() != nil {
		return iter.Err()
	}

	// For all object present in the dump but not in the oplog, ensure objects
	// haven't been deleted between the dump creation and the sync
	for id, obd := range createMap {
		err := db.C("objects").FindId(id).One(&obs)
		if err == mgo.ErrNotFound {
			continue
		}
		if err != nil {
			return err
		}
		if obd.Timestamp.Before(obs.Data.Timestamp) {
			delete(createMap, obs.Id)
		}
	}

	return nil
}

// HasId checks if an operation id is present in the capped collection.
func (oplog *OpLog) HasId(id string) bool {
	_, ok := parseTimestampId(id)
	if ok {
		// Id is a timestamp, timestamp are always valid
		return true
	}

	oid := parseObjectId(id)
	if oid == nil {
		return false
	}

	db := oplog.DB()
	defer db.Session.Close()
	count, err := db.C("oplog").FindId(oid).Count()
	if err != nil {
		return false
	}
	return count != 0
}

// LastId returns the most recently inserted operation id if any or "" if oplog is empty
func (oplog *OpLog) LastId() string {
	db := oplog.DB()
	defer db.Session.Close()
	operation := &Operation{}
	err := db.C("oplog").Find(nil).Sort("-$natural").One(operation)
	if err != mgo.ErrNotFound && operation.Id != nil {
		return operation.Id.Hex()
	}
	return ""
}

// iter creates either an iterator on the objects collection if the lastId is a timestamp
// or tailable cursor on the oplog capped collection otherwise
func (oplog *OpLog) iter(db *mgo.Database, lastId string, filter OpLogFilter) (iter *mgo.Iter, streaming bool) {
	query := bson.M{}
	if len(filter.Types) > 0 {
		query["data.t"] = bson.M{"$in": filter.Types}
	}
	if len(filter.Parents) > 0 {
		query["data.p"] = bson.M{"$in": filter.Parents}
	}

	if ts, ok := parseTimestampId(lastId); ok {
		if ts > 0 {
			// Id is a timestamp, timestamp are always valid
			query["data.ts"] = bson.M{"$gte": time.Unix(0, ts*1000000)}
		}
		query["event"] = bson.M{"$ne": "delete"}
		iter = db.C("objects").Find(query).Sort("data.ts").Iter()
		streaming = false
	} else {
		oid := parseObjectId(lastId)
		if oid == nil {
			// If no last id provided, find the last operation id in the colleciton
			oid = parseObjectId(oplog.LastId())
		}
		if oid != nil {
			query["_id"] = bson.M{"$gt": oid}
		}
		iter = db.C("oplog").Find(query).Sort("$natural").Tail(-1)
		streaming = true
	}
	return
}

// Tail tails all the new operations in the oplog and send the operation in
// the given channel. If the lastId parameter is given, all operation posted after
// this event will be returned.
//
// If the lastId is a unix timestamp in milliseconds, the tailing will start by replicating
// all the objects last updated after the timestamp.
//
// Giving a lastId of 0 mean replicating all the stored objects before tailing the live updates.
//
// The filter argument can be used to filter on some type of objects or objects with given parrents.
//
// The create, update, delete events are streamed back to the sender thru the out channel with error
// sent thru the err channel.
func (oplog *OpLog) Tail(lastId string, filter OpLogFilter, out chan<- io.WriterTo, err chan<- error) {
	db := oplog.DB()
	var lastEv GenericEvent

	if lastId == "0" {
		// When full replication is requested, start by sending a "reset" event to instruct
		// the consumer to reset its database before processing further operations.
		// The id is 1 so if connection is lost after this event and consumer processed the event,
		// the connection recover won't trigger a second "reset" event.
		out <- &OpLogEvent{
			Id:    "1",
			Event: "reset",
		}
	}

	for {
		iter, streaming := oplog.iter(db, lastId, filter)

		if streaming {
			log.Debug("OPLOG start live updates")

			operation := Operation{}
			for iter.Next(&operation) {
				out <- operation
				lastEv = operation
			}

			if iter.Err() != nil {
				log.Warnf("OPLOG tail failed with error, try to reconnect: %s", iter.Err())
			}
		} else {
			log.Debug("OPLOG start replication")
			// Capture the current oplog position in order to resume at this position
			// once initial replication is done
			replicationFallbackId := oplog.LastId()

			object := ObjectState{}
			for iter.Next(&object) {
				out <- object
				lastEv = object
			}

			if iter.Err() != nil {
				log.Warnf("OPLOG replication failed with error, try to reconnect: %s", iter.Err())
			} else {
				// Replication is done, notify and swtich to live event stream
				//
				// Send a "live" operation to inform the consumer it is no live event stream.
				// We use the last event id here in order to ensure the consumer will resume
				// the replication starting at this point in time in case of a failure after
				// the "live" event.
				out <- &OpLogEvent{
					Id:    lastEv.GetEventId(),
					Event: "live",
				}
				// Switch to live update at the last operation id inserted before the replication
				// was started
				lastId = replicationFallbackId
				lastEv = nil
			}
		}

		// Prepare for reconnect
		iter.Close()
		db.Session.Close()
		db = oplog.DB()
		if lastEv != nil {
			lastId = lastEv.GetEventId()
		}
		time.Sleep(time.Second)
	}
}
