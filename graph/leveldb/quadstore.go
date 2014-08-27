// Copyright 2014 The Cayley Authors. All rights reserved.
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

package leveldb

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sync"

	"github.com/barakmich/glog"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/cache"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"

	"github.com/google/cayley/graph"
	"github.com/google/cayley/graph/iterator"
	"github.com/google/cayley/quad"
)

func init() {
	graph.RegisterQuadStore("leveldb", true, newQuadStore, createNewLevelDB)
}

const (
	DefaultCacheSize       = 2
	DefaultWriteBufferSize = 20
)

var (
	hashPool = sync.Pool{
		New: func() interface{} { return sha1.New() },
	}
	hashSize = sha1.Size
)

type Token []byte

func (t Token) Key() interface{} {
	return string(t)
}

type QuadStore struct {
	dbOpts    *opt.Options
	db        *leveldb.DB
	path      string
	open      bool
	size      int64
	horizon   int64
	writeopts *opt.WriteOptions
	readopts  *opt.ReadOptions
}

func createNewLevelDB(path string, _ graph.Options) error {
	opts := &opt.Options{}
	db, err := leveldb.OpenFile(path, opts)
	if err != nil {
		glog.Errorf("Error: couldn't create database: %v", err)
		return err
	}
	defer db.Close()
	qs := &QuadStore{}
	qs.db = db
	qs.writeopts = &opt.WriteOptions{
		Sync: true,
	}
	qs.Close()
	return nil
}

func newQuadStore(path string, options graph.Options) (graph.QuadStore, error) {
	var qs QuadStore
	var err error
	qs.path = path
	cache_size := DefaultCacheSize
	if val, ok := options.IntKey("cache_size_mb"); ok {
		cache_size = val
	}
	qs.dbOpts = &opt.Options{
		BlockCache: cache.NewLRUCache(cache_size * opt.MiB),
	}
	qs.dbOpts.ErrorIfMissing = true

	write_buffer_mb := DefaultWriteBufferSize
	if val, ok := options.IntKey("write_buffer_mb"); ok {
		write_buffer_mb = val
	}
	qs.dbOpts.WriteBuffer = write_buffer_mb * opt.MiB
	qs.writeopts = &opt.WriteOptions{
		Sync: false,
	}
	qs.readopts = &opt.ReadOptions{}
	db, err := leveldb.OpenFile(qs.path, qs.dbOpts)
	if err != nil {
		glog.Errorln("Error, couldn't open! ", err)
		return nil, err
	}
	qs.db = db
	glog.Infoln(qs.GetStats())
	err = qs.getMetadata()
	if err != nil {
		return nil, err
	}
	return &qs, nil
}

func (qs *QuadStore) GetStats() string {
	out := ""
	stats, err := qs.db.GetProperty("leveldb.stats")
	if err == nil {
		out += fmt.Sprintln("Stats: ", stats)
	}
	out += fmt.Sprintln("Size: ", qs.size)
	return out
}

func (qs *QuadStore) Size() int64 {
	return qs.size
}

func (qs *QuadStore) Horizon() int64 {
	return qs.horizon
}

func (qa *QuadStore) createDeltaKeyFor(d graph.Delta) []byte {
	key := make([]byte, 0, 19)
	key = append(key, 'd')
	key = append(key, []byte(fmt.Sprintf("%018x", d.ID))...)
	return key
}

func hashOf(s string) []byte {
	h := hashPool.Get().(hash.Hash)
	h.Reset()
	defer hashPool.Put(h)
	key := make([]byte, 0, hashSize)
	h.Write([]byte(s))
	key = h.Sum(key)
	return key
}

func (qs *QuadStore) createKeyFor(d [4]quad.Direction, q quad.Quad) []byte {
	key := make([]byte, 0, 2+(hashSize*3))
	// TODO(kortschak) Remove dependence on String() method.
	key = append(key, []byte{d[0].Prefix(), d[1].Prefix()}...)
	key = append(key, hashOf(q.Get(d[0]))...)
	key = append(key, hashOf(q.Get(d[1]))...)
	key = append(key, hashOf(q.Get(d[2]))...)
	key = append(key, hashOf(q.Get(d[3]))...)
	return key
}

func (qs *QuadStore) createValueKeyFor(s string) []byte {
	key := make([]byte, 0, 1+hashSize)
	key = append(key, []byte("z")...)
	key = append(key, hashOf(s)...)
	return key
}

type IndexEntry struct {
	quad.Quad
	History []int64
}

// Short hand for direction permutations.
var (
	spo = [4]quad.Direction{quad.Subject, quad.Predicate, quad.Object, quad.Label}
	osp = [4]quad.Direction{quad.Object, quad.Subject, quad.Predicate, quad.Label}
	pos = [4]quad.Direction{quad.Predicate, quad.Object, quad.Subject, quad.Label}
	cps = [4]quad.Direction{quad.Label, quad.Predicate, quad.Subject, quad.Object}
)

func (qs *QuadStore) ApplyDeltas(deltas []graph.Delta) error {
	batch := &leveldb.Batch{}
	resizeMap := make(map[string]int64)
	size_change := int64(0)
	for _, d := range deltas {
		bytes, err := json.Marshal(d)
		if err != nil {
			return err
		}
		batch.Put(qs.createDeltaKeyFor(d), bytes)
		err = qs.buildQuadWrite(batch, d.Quad, d.ID, d.Action == graph.Add)
		if err != nil {
			return err
		}
		delta := int64(1)
		if d.Action == graph.Delete {
			delta = int64(-1)
		}
		resizeMap[d.Quad.Subject] += delta
		resizeMap[d.Quad.Predicate] += delta
		resizeMap[d.Quad.Object] += delta
		if d.Quad.Label != "" {
			resizeMap[d.Quad.Label] += delta
		}
		size_change += delta
		qs.horizon = d.ID
	}
	for k, v := range resizeMap {
		if v != 0 {
			err := qs.UpdateValueKeyBy(k, v, batch)
			if err != nil {
				return err
			}
		}
	}
	err := qs.db.Write(batch, qs.writeopts)
	if err != nil {
		glog.Error("Couldn't write to DB for quadset.")
		return err
	}
	qs.size += size_change
	return nil
}

func (qs *QuadStore) buildQuadWrite(batch *leveldb.Batch, q quad.Quad, id int64, isAdd bool) error {
	var entry IndexEntry
	data, err := qs.db.Get(qs.createKeyFor(spo, q), qs.readopts)
	if err != nil && err != leveldb.ErrNotFound {
		glog.Error("Couldn't access DB to prepare index: ", err)
		return err
	}
	if err == nil {
		// We got something.
		err = json.Unmarshal(data, &entry)
		if err != nil {
			return err
		}
	} else {
		entry.Quad = q
	}
	entry.History = append(entry.History, id)

	if isAdd && len(entry.History)%2 == 0 {
		glog.Error("Entry History is out of sync for", entry)
		return errors.New("Odd index history")
	}

	bytes, err := json.Marshal(entry)
	if err != nil {
		glog.Errorf("Couldn't write to buffer for entry %#v: %s", entry, err)
		return err
	}
	batch.Put(qs.createKeyFor(spo, q), bytes)
	batch.Put(qs.createKeyFor(osp, q), bytes)
	batch.Put(qs.createKeyFor(pos, q), bytes)
	if q.Get(quad.Label) != "" {
		batch.Put(qs.createKeyFor(cps, q), bytes)
	}
	return nil
}

type ValueData struct {
	Name string
	Size int64
}

func (qs *QuadStore) UpdateValueKeyBy(name string, amount int64, batch *leveldb.Batch) error {
	value := &ValueData{name, amount}
	key := qs.createValueKeyFor(name)
	b, err := qs.db.Get(key, qs.readopts)

	// Error getting the node from the database.
	if err != nil && err != leveldb.ErrNotFound {
		glog.Errorf("Error reading Value %s from the DB.", name)
		return err
	}

	// Node exists in the database -- unmarshal and update.
	if b != nil && err != leveldb.ErrNotFound {
		err = json.Unmarshal(b, value)
		if err != nil {
			glog.Errorf("Error: couldn't reconstruct value: %v", err)
			return err
		}
		value.Size += amount
	}

	// Are we deleting something?
	if value.Size <= 0 {
		value.Size = 0
	}

	// Repackage and rewrite.
	bytes, err := json.Marshal(&value)
	if err != nil {
		glog.Errorf("Couldn't write to buffer for value %s: %s", name, err)
		return err
	}
	if batch == nil {
		qs.db.Put(key, bytes, qs.writeopts)
	} else {
		batch.Put(key, bytes)
	}
	return nil
}

func (qs *QuadStore) Close() {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.LittleEndian, qs.size)
	if err == nil {
		werr := qs.db.Put([]byte("__size"), buf.Bytes(), qs.writeopts)
		if werr != nil {
			glog.Error("Couldn't write size before closing!")
		}
	} else {
		glog.Errorf("Couldn't convert size before closing!")
	}
	buf.Reset()
	err = binary.Write(buf, binary.LittleEndian, qs.horizon)
	if err == nil {
		werr := qs.db.Put([]byte("__horizon"), buf.Bytes(), qs.writeopts)
		if werr != nil {
			glog.Error("Couldn't write horizon before closing!")
		}
	} else {
		glog.Errorf("Couldn't convert horizon before closing!")
	}
	qs.db.Close()
	qs.open = false
}

func (qs *QuadStore) Quad(k graph.Value) quad.Quad {
	var q quad.Quad
	b, err := qs.db.Get(k.(Token), qs.readopts)
	if err != nil && err != leveldb.ErrNotFound {
		glog.Error("Error: couldn't get quad from DB.")
		return quad.Quad{}
	}
	if err == leveldb.ErrNotFound {
		// No harm, no foul.
		return quad.Quad{}
	}
	err = json.Unmarshal(b, &q)
	if err != nil {
		glog.Error("Error: couldn't reconstruct quad.")
		return quad.Quad{}
	}
	return q
}

func (qs *QuadStore) ValueOf(s string) graph.Value {
	return Token(qs.createValueKeyFor(s))
}

func (qs *QuadStore) valueData(value_key []byte) ValueData {
	var out ValueData
	if glog.V(3) {
		glog.V(3).Infof("%s %v", string(value_key[0]), value_key)
	}
	b, err := qs.db.Get(value_key, qs.readopts)
	if err != nil && err != leveldb.ErrNotFound {
		glog.Errorln("Error: couldn't get value from DB")
		return out
	}
	if b != nil && err != leveldb.ErrNotFound {
		err = json.Unmarshal(b, &out)
		if err != nil {
			glog.Errorln("Error: couldn't reconstruct value")
			return ValueData{}
		}
	}
	return out
}

func (qs *QuadStore) NameOf(k graph.Value) string {
	if k == nil {
		glog.V(2).Info("k was nil")
		return ""
	}
	return qs.valueData(k.(Token)).Name
}

func (qs *QuadStore) SizeOf(k graph.Value) int64 {
	if k == nil {
		return 0
	}
	return int64(qs.valueData(k.(Token)).Size)
}

func (qs *QuadStore) getInt64ForKey(key string, empty int64) (int64, error) {
	var out int64
	b, err := qs.db.Get([]byte(key), qs.readopts)
	if err != nil && err != leveldb.ErrNotFound {
		glog.Errorln("Couldn't read " + key + ": " + err.Error())
		return 0, err
	}
	if err == leveldb.ErrNotFound {
		// Must be a new database. Cool
		return empty, nil
	}
	buf := bytes.NewBuffer(b)
	err = binary.Read(buf, binary.LittleEndian, &out)
	if err != nil {
		glog.Errorln("Error: couldn't parse", key)
		return 0, err
	}
	return out, nil
}

func (qs *QuadStore) getMetadata() error {
	var err error
	qs.size, err = qs.getInt64ForKey("__size", 0)
	if err != nil {
		return err
	}
	qs.horizon, err = qs.getInt64ForKey("__horizon", 0)
	return err
}

func (qs *QuadStore) SizeOfPrefix(pre []byte) (int64, error) {
	limit := make([]byte, len(pre))
	copy(limit, pre)
	end := len(limit) - 1
	limit[end]++
	ranges := make([]util.Range, 1)
	ranges[0].Start = pre
	ranges[0].Limit = limit
	sizes, err := qs.db.SizeOf(ranges)
	if err == nil {
		return (int64(sizes[0]) >> 6) + 1, nil
	}
	return 0, nil
}

func (qs *QuadStore) QuadIterator(d quad.Direction, val graph.Value) graph.Iterator {
	var prefix string
	switch d {
	case quad.Subject:
		prefix = "sp"
	case quad.Predicate:
		prefix = "po"
	case quad.Object:
		prefix = "os"
	case quad.Label:
		prefix = "cp"
	default:
		panic("unreachable " + d.String())
	}
	return NewIterator(prefix, d, val, qs)
}

func (qs *QuadStore) NodesAllIterator() graph.Iterator {
	return NewAllIterator("z", quad.Any, qs)
}

func (qs *QuadStore) QuadsAllIterator() graph.Iterator {
	return NewAllIterator("po", quad.Predicate, qs)
}

func (qs *QuadStore) QuadDirection(val graph.Value, d quad.Direction) graph.Value {
	v := val.(Token)
	offset := PositionOf(v[0:2], d, qs)
	if offset != -1 {
		return Token(append([]byte("z"), v[offset:offset+hashSize]...))
	} else {
		return Token(qs.Quad(val).Get(d))
	}
}

func compareBytes(a, b graph.Value) bool {
	return bytes.Equal(a.(Token), b.(Token))
}

func (qs *QuadStore) FixedIterator() graph.FixedIterator {
	return iterator.NewFixedIteratorWithCompare(compareBytes)
}
