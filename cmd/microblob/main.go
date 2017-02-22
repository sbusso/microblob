package main

import (
	"bufio"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/syndtr/goleveldb/leveldb"
)

// Entry associates a key with a section in a file specified by offset and length.
type Entry struct {
	Key    string `json:"k"`
	Offset int64  `json:"o"`
	Length int64  `json:"l"`
}

// KeyFunc extracts a key from a blob.
type KeyFunc func([]byte) (string, error)

// EntryWriter writes entries to some storage, e.g. a file or a database.
type EntryWriter func(entries []Entry) error

// LineProcessor read a line, extracts the key and writes entries.
type LineProcessor struct {
	r io.Reader   // input data
	f KeyFunc     // extracts a string key from a byte blob
	w EntryWriter // serializes entries
}

// Run start processing the input.
func (p LineProcessor) Run() error {
	bw := bufio.NewReader(p.r)
	for {
		b, err := bw.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		key, err := p.f(b)
		if err != nil {
			return err
		}
		entry := Entry{Key: key}
		if err := p.w([]Entry{entry}); err != nil {
			return err
		}
	}
	return nil
}

// workPackage is a unit of work handed to a worker.
type workPackage struct {
	docs   [][]byte // list of documents to work on
	offset int64    // offset to start with
}

// RunWithWorkers start processing the input, uses multiple workers.
func (p LineProcessor) RunWithWorkers() error {

	// workerErr is set, when a worker fails. Winds down processing.
	var processingErr error

	// Setup communication channels.
	work := make(chan workPackage)
	updates := make(chan []Entry)
	done := make(chan bool)

	// collector runs the EntryWriter on all incoming batches.
	collector := func(ch chan []Entry, done chan bool) {
		for batch := range ch {
			if err := p.w(batch); err != nil {
				processingErr = err
				break
			}
		}
		done <- true
	}

	// worker takes a workPackage, creates Entries from bytes and sends the result
	// down the sink.
	worker := func(queue chan workPackage, wg *sync.WaitGroup) {
		defer wg.Done()
		for pkg := range queue {
			offset := pkg.offset
			var entries []Entry
			for _, b := range pkg.docs {
				key, err := p.f(b)
				if err != nil {
					processingErr = err
					break
				}
				length := int64(len(b))
				entries = append(entries, Entry{key, offset, length})
				offset += length
			}

			updates <- entries
			if processingErr != nil {
				break
			}
		}
	}

	var wg sync.WaitGroup

	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go worker(work, &wg)
	}

	go collector(updates, done)

	br := bufio.NewReader(p.r)
	var offset, blen int64
	batch := [][]byte{}

	for {
		b, err := br.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(batch) == 50000 {
			bb := make([][]byte, len(batch))
			copy(bb, batch)
			work <- workPackage{docs: bb, offset: offset}
			offset += blen
			blen, batch = 0, nil
		}
		batch = append(batch, b)
		blen += int64(len(b))
	}

	bb := make([][]byte, len(batch))
	copy(bb, batch)
	work <- workPackage{docs: bb, offset: offset}

	close(work)
	wg.Wait()
	close(updates)
	<-done

	return processingErr
}

// renderString tries various ways to get a string out of a given type.
func renderString(v interface{}) (s string, err error) {
	switch w := v.(type) {
	case string:
		s = w
	case int:
		s = fmt.Sprintf("%d", w)
	case float64:
		s = fmt.Sprintf("%0d", int(w))
	case fmt.Stringer:
		s = fmt.Sprintf("%s", w)
	case time.Time:
		s = w.Format(time.RFC3339)
	default:
		err = fmt.Errorf("unsupported type: %v", reflect.TypeOf(w))
	}
	return
}

var key = "finc.record_id"
var keyPattern = regexp.MustCompile(`ai-[\d]+-[\w]+`)

// regexpExtractor extracts the key via regular expression. Quite fast.
func regexpExtractor(b []byte) (string, error) {
	return string(keyPattern.Find(b)), nil
}

// parsingExtractor unmarshals JSON. Slower, but might be tweakable.
func parsingExtractor(b []byte) (string, error) {
	dst := make(map[string]interface{})
	if err := json.Unmarshal(b, &dst); err != nil {
		return "", err
	}
	if _, ok := dst[key]; !ok {
		return "", fmt.Errorf("key %s not found in: %s", key, string(b))
	}
	s, err := renderString(dst[key])
	if err != nil {
		return "", err
	}
	return s, nil
}

// leveldbWriter writes entries into leveldb.
type leveldbWriter struct {
	Filename string
	db       *leveldb.DB
}

func (w *leveldbWriter) Close() error {
	if w.db != nil {
		return w.db.Close()
	}
	return nil
}

func (w *leveldbWriter) WriteEntries(entries []Entry) error {
	if w.db == nil {
		db, err := leveldb.OpenFile(w.Filename, nil)
		if err != nil {
			return err
		}
		w.db = db
	}
	batch := new(leveldb.Batch)
	for _, entry := range entries {
		offset, length := make([]byte, 8), make([]byte, 8)
		binary.PutVarint(offset, entry.Offset)
		binary.PutVarint(length, entry.Length)
		value := append(offset, length...)
		batch.Put([]byte(entry.Key), value)
	}
	return w.db.Write(batch, nil)
}

// sqliteWriter writes entries into a sqlite file
type sqliteWriter struct {
	Filename string
	db       *sql.DB
}

func (w *sqliteWriter) WriteEntries(entries []Entry) error {
	// create table if necessary
	if w.db == nil {
		db, err := sql.Open("sqlite3", w.Filename)
		if err != nil {
			return err
		}
		w.db = db
	}

	init := `
	CREATE TABLE IF NOT EXISTS blob (
		key    TEXT    NOT NULL PRIMARY KEY,
		offset INTEGER NOT NULL,
		length INTEGER NOT NULL
	);`

	_, err := w.db.Exec(init)
	if err != nil {
		return fmt.Errorf("%q: %s", err, init)
	}

	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("insert into blob(key, offset, length) values(?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, entry := range entries {
		if _, err = stmt.Exec(entry.Key, entry.Offset, entry.Length); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (w *sqliteWriter) Close() error {
	if w.db != nil {
		return w.db.Close()
	}
	return nil
}

func main() {
	// writer := leveldbWriter{Filename: "hello.ldb"}
	writer := sqliteWriter{Filename: "hello.db"}
	defer writer.Close()
	processor := LineProcessor{
		r: os.Stdin,
		// f: parsingExtractor,
		f: regexpExtractor,
		// w: func(entries []Entry) error {
		// 	for _, e := range entries {
		// 		fmt.Printf("%s\t%d\t%d\n", e.Key, e.Offset, e.Length)
		// 	}
		// 	return nil
		// }}
		w: writer.WriteEntries,
	}
	if err := processor.RunWithWorkers(); err != nil {
		log.Fatal(err)
	}
}
