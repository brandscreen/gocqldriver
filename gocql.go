// Copyright (c) 2012 The gocql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The gocql package provides a database/sql driver for CQL, the Cassandra
// query language.
//
// This package requires a recent version of Cassandra (≥ 1.2) that supports
// CQL 3.0 and the new native protocol. The native protocol is still considered
// beta and must be enabled manually in Cassandra 1.2 by setting
// "start_native_transport" to true in conf/cassandra.yaml.
//
// Example Usage:
//
//     db, err := sql.Open("gocql", "localhost:9042 keyspace=system")
//     // ...
//     rows, err := db.Query("SELECT keyspace_name FROM schema_keyspaces")
//     // ...
//     for rows.Next() {
//          var keyspace string
//          err = rows.Scan(&keyspace)
//          // ...
//          fmt.Println(keyspace)
//     }
//     if err := rows.Err(); err != nil {
//         // ...
//     }
//
package gocql

import (
	"bytes"
	"code.google.com/p/snappy-go/snappy"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	protoRequest  byte = 0x01
	protoResponse byte = 0x81

	opError        byte = 0x00
	opStartup      byte = 0x01
	opReady        byte = 0x02
	opAuthenticate byte = 0x03
	opCredentials  byte = 0x04
	opOptions      byte = 0x05
	opSupported    byte = 0x06
	opQuery        byte = 0x07
	opResult       byte = 0x08
	opPrepare      byte = 0x09
	opExecute      byte = 0x0A
	opLAST         byte = 0x0A // not a real opcode -- used to check for valid opcodes

	errorOverloaded   = 0x1001
	errorWriteTimeout = 0x1100
	errorReadTimeout  = 0x1200

	flagCompressed byte = 0x01

	keyVersion     string = "CQL_VERSION"
	keyCompression string = "COMPRESSION"
)

var consistencyLevels = map[string]byte{"any": 0x00, "one": 0x01, "two": 0x02,
	"three": 0x03, "quorum": 0x04, "all": 0x05, "local_quorum": 0x06, "each_quorum": 0x07}

type drv struct{}

func (d drv) Open(name string) (driver.Conn, error) {
	return Open(name)
}

type connection struct {
	c                                 net.Conn
	compression                       string
	readConsistency, writeConsistency byte
	recycle                           time.Time
	retries                           int
}

// dial addresses until we connect
func getConn(addrs []string) (conn net.Conn, err error) {
	for _, i := range rand.Perm(len(addrs)) {
		conn, err = net.Dial("tcp", addrs[i])
		if err == nil {
			break
		}
	}
	return
}

func parseConsistency(cs string) (byte, error) {
	if b, ok := consistencyLevels[strings.ToLower(cs)]; ok {
		return b, nil
	}
	return 0xff, fmt.Errorf("unknown consistency level %q", cs)
}

func Open(name string) (cn *connection, err error) {
	parts := strings.Split(name, " ")

	version := "3.0.0"
	cn = &connection{
		readConsistency:  consistencyLevels["one"],
		writeConsistency: consistencyLevels["one"],
	}
	var keyspace string

	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		splitPart := strings.SplitN(part, "=", 2)
		if len(splitPart) != 2 {
			return nil, fmt.Errorf("missing = in option: %q", part)
		}
		opt, val := splitPart[0], splitPart[1]
		opt = strings.ToLower(opt)
		val = strings.TrimSpace(val)
		switch opt {
		case "keyspace":
			keyspace = val
		case "compression":
			val = strings.ToLower(val)
			if val != "snappy" {
				err = fmt.Errorf("unknown compression algorithm %q", val)
				return
			}
			cn.compression = val
		case "version":
			version = val
		case "consistency":
			consistency, err := parseConsistency(val)
			if err != nil {
				return nil, err
			}
			cn.readConsistency = consistency
			cn.writeConsistency = consistency
		case "writeconsistency":
			cn.writeConsistency, err = parseConsistency(val)
			if err != nil {
				return
			}
		case "readconsistency":
			cn.readConsistency, err = parseConsistency(val)
			if err != nil {
				return
			}
		case "recycle":
			ttl, err := time.ParseDuration(val)
			if err != nil {
				return nil, fmt.Errorf("bad recycle option: %s", err)
			}
			if ttl > 0 {
				cn.recycle = time.Now().Add(ttl)
			} else {
				cn.recycle = time.Time{}
			}
		case "retries":
			i64, err := strconv.ParseInt(val, 0, 0)
			if err != nil {
				return nil, fmt.Errorf("bad retries option: %s", err)
			}
			cn.retries = int(i64)
		default:
			return nil, fmt.Errorf("unsupported option %q", opt)
		}
	}

	cn.c, err = getConn(strings.Split(parts[0], ","))
	if err != nil {
		return nil, err
	}

	b := &bytes.Buffer{}

	if cn.compression != "" {
		binary.Write(b, binary.BigEndian, uint16(2))
	} else {
		binary.Write(b, binary.BigEndian, uint16(1))
	}

	binary.Write(b, binary.BigEndian, uint16(len(keyVersion)))
	b.WriteString(keyVersion)
	binary.Write(b, binary.BigEndian, uint16(len(version)))
	b.WriteString(version)

	if cn.compression != "" {
		binary.Write(b, binary.BigEndian, uint16(len(keyCompression)))
		b.WriteString(keyCompression)
		binary.Write(b, binary.BigEndian, uint16(len(cn.compression)))
		b.WriteString(cn.compression)
	}

	if err := cn.send(opStartup, b.Bytes()); err != nil {
		return nil, err
	}

	opcode, _, err := cn.recv()
	if err != nil {
		return nil, err
	}
	if opcode != opReady {
		return nil, fmt.Errorf("connection not ready")
	}

	if keyspace != "" {
		st, err := cn.Prepare(fmt.Sprintf("USE %s", keyspace))
		if err != nil {
			return nil, err
		}
		if _, err = st.Exec([]driver.Value{}); err != nil {
			return nil, err
		}
	}

	return cn, nil
}

// close a connection actively, typically used when there's an error and we want to ensure
// we don't repeatedly try to use the broken connection
func (cn *connection) close() {
	cn.c.Close()
	cn.c = nil // ensure we generate ErrBadConn when cn gets reused
}

func (cn *connection) send(opcode byte, body []byte) error {
	if cn.c == nil {
		return driver.ErrBadConn
	}
	frame := make([]byte, len(body)+8)
	frame[0] = protoRequest
	frame[1] = 0
	frame[2] = 0
	frame[3] = opcode
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(body)))
	copy(frame[8:], body)
	if _, err := cn.c.Write(frame); err != nil {
		return err
	}
	return nil
}

func (cn *connection) recv() (byte, []byte, error) {
	if cn.c == nil {
		return 0, nil, driver.ErrBadConn
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(cn.c, header); err != nil {
		cn.close() // better assume that the connection is broken (may have read some bytes)
		return 0, nil, err
	}
	// verify that the frame starts with version==1 and req/resp flag==response
	// this may be overly conservative in that future versions may be backwards compatible
	// in that case simply amend the check...
	if header[0] != protoResponse {
		cn.close()
		return 0, nil, fmt.Errorf("unsupported frame version or not a response: 0x%x (header=%v)", header[0], header)
	}
	// verify that the flags field has only a single flag set, again, this may
	// be overly conservative if additional flags are backwards-compatible
	if header[1] > 1 {
		cn.close()
		return 0, nil, fmt.Errorf("unsupported frame flags: 0x%x (header=%v)", header[1], header)
	}
	opcode := header[3]
	if opcode > opLAST {
		cn.close()
		return 0, nil, fmt.Errorf("unknown opcode: 0x%x (header=%v)", opcode, header)
	}
	length := binary.BigEndian.Uint32(header[4:8])
	var body []byte
	if length > 0 {
		if length > 256*1024*1024 { // spec says 256MB is max
			cn.close()
			return 0, nil, fmt.Errorf("frame too large: %d (header=%v)", length, header)
		}
		body = make([]byte, length)
		if _, err := io.ReadFull(cn.c, body); err != nil {
			cn.close() // better assume that the connection is broken
			return 0, nil, err
		}
	}
	if header[1]&flagCompressed != 0 && cn.compression == "snappy" {
		var err error
		body, err = snappy.Decode(nil, body)
		if err != nil {
			cn.close()
			return 0, nil, err
		}
	}
	if opcode == opError {
		code := binary.BigEndian.Uint32(body[0:4])
		msglen := binary.BigEndian.Uint16(body[4:6])
		msg := string(body[6 : 6+msglen])
		return opcode, body, Error{Code: int(code), Msg: msg}
	}
	return opcode, body, nil
}

func (cn *connection) Begin() (driver.Tx, error) {
	if err := cn.recycleErr(); err != nil {
		return nil, err
	}
	return cn, nil
}

func (cn *connection) Commit() error {
	if cn.c == nil {
		return driver.ErrBadConn
	}
	return nil
}

func (cn *connection) Close() error {
	if cn.c == nil {
		return driver.ErrBadConn
	}
	cn.close()
	return nil
}

func (cn *connection) Rollback() error {
	if cn.c == nil {
		return driver.ErrBadConn
	}
	return nil
}

func (cn *connection) recycleErr() error {
	if cn.c == nil {
		return driver.ErrBadConn
	}
	if cn.recycle.IsZero() {
		return nil
	}
	if time.Now().Before(cn.recycle) {
		return nil
	}
	cn.close()
	return driver.ErrBadConn
}

func retryErr(err error) bool {
	e, ok := err.(Error)
	if !ok {
		return false
	}
	switch e.Code {
	case errorWriteTimeout:
	case errorReadTimeout:
	case errorOverloaded:
	default:
		return false
	}
	return true
}

func (cn *connection) retrySendRecv(send func() error) (op byte, body []byte, err error) {
	for try := 0; try <= cn.retries || cn.retries < 0; try++ {
		err = send()
		if err != nil {
			break
		}
		op, body, err = cn.recv()
		if err == nil {
			break
		}
		if !retryErr(err) {
			break
		}
	}
	return
}

func (cn *connection) Prepare(query string) (driver.Stmt, error) {
	if err := cn.recycleErr(); err != nil {
		return nil, err
	}
	body := make([]byte, len(query)+4)
	binary.BigEndian.PutUint32(body[0:4], uint32(len(query)))
	copy(body[4:], []byte(query))
	opcode, body, err := cn.retrySendRecv(func() error {
		return cn.send(opPrepare, body)
	})
	if err != nil {
		return nil, err
	}
	if opcode != opResult || binary.BigEndian.Uint32(body) != 4 {
		return nil, fmt.Errorf("expected prepared result")
	}
	n := int(binary.BigEndian.Uint16(body[4:]))
	prepared := body[6 : 6+n]
	columns, meta, _ := parseMeta(body[6+n:])
	return &statement{cn: cn, query: query,
		prepared: prepared, columns: columns, meta: meta}, nil
}

type statement struct {
	cn       *connection
	query    string
	prepared []byte
	columns  []string
	meta     []uint16
}

func (s *statement) Close() error {
	return nil
}

func (st *statement) ColumnConverter(idx int) driver.ValueConverter {
	return (&columnEncoder{st.meta}).ColumnConverter(idx)
}

func (st *statement) NumInput() int {
	return len(st.columns)
}

func parseMeta(body []byte) ([]string, []uint16, int) {
	flags := binary.BigEndian.Uint32(body)
	globalTableSpec := flags&1 == 1
	columnCount := int(binary.BigEndian.Uint32(body[4:]))
	i := 8
	if globalTableSpec {
		l := int(binary.BigEndian.Uint16(body[i:]))
		keyspace := string(body[i+2 : i+2+l])
		i += 2 + l
		l = int(binary.BigEndian.Uint16(body[i:]))
		tablename := string(body[i+2 : i+2+l])
		i += 2 + l
		_, _ = keyspace, tablename
	}
	columns := make([]string, columnCount)
	meta := make([]uint16, columnCount)
	for c := 0; c < columnCount; c++ {
		l := int(binary.BigEndian.Uint16(body[i:]))
		columns[c] = string(body[i+2 : i+2+l])
		i += 2 + l
		meta[c] = binary.BigEndian.Uint16(body[i:])
		i += 2
	}
	return columns, meta, i
}

func (st *statement) exec(v []driver.Value, consistency byte) error {
	sz := 6 + len(st.prepared)
	for i := range v {
		if b, ok := v[i].([]byte); ok {
			sz += len(b) + 4
		}
	}
	body, p := make([]byte, sz), 4+len(st.prepared)
	binary.BigEndian.PutUint16(body, uint16(len(st.prepared)))
	copy(body[2:], st.prepared)
	binary.BigEndian.PutUint16(body[p-2:], uint16(len(v)))
	for i := range v {
		b, ok := v[i].([]byte)
		if !ok {
			return fmt.Errorf("unsupported type %T at column %d", v[i], i)
		}
		binary.BigEndian.PutUint32(body[p:], uint32(len(b)))
		copy(body[p+4:], b)
		p += 4 + len(b)
	}
	binary.BigEndian.PutUint16(body[p:], uint16(consistency))
	if err := st.cn.send(opExecute, body); err != nil {
		return err
	}
	return nil
}

func (st *statement) Exec(v []driver.Value) (driver.Result, error) {
	if err := st.cn.recycleErr(); err != nil {
		return nil, err
	}
	opcode, body, err := st.cn.retrySendRecv(func() error {
		return st.exec(v, st.cn.writeConsistency)
	})
	if err != nil {
		return nil, err
	}
	_, _ = opcode, body
	return nil, nil
}

func (st *statement) Query(v []driver.Value) (driver.Rows, error) {
	if err := st.cn.recycleErr(); err != nil {
		return nil, err
	}
	opcode, body, err := st.cn.retrySendRecv(func() error {
		return st.exec(v, st.cn.readConsistency)
	})
	if err != nil {
		return nil, err
	}
	kind := binary.BigEndian.Uint32(body[0:4])
	if opcode != opResult || kind != 2 {
		return nil, fmt.Errorf("expected rows as result")
	}
	columns, meta, n := parseMeta(body[4:])
	i := n + 4
	rows := &rows{
		columns: columns,
		meta:    meta,
		numRows: int(binary.BigEndian.Uint32(body[i:])),
	}
	i += 4
	rows.body = body[i:]
	return rows, nil
}

type rows struct {
	columns []string
	meta    []uint16
	body    []byte
	row     int
	numRows int
}

func (r *rows) Close() error {
	return nil
}

func (r *rows) Columns() []string {
	return r.columns
}

func (r *rows) Next(values []driver.Value) error {
	if r.row >= r.numRows {
		return io.EOF
	}
	for column := 0; column < len(r.columns); column++ {
		n := int32(binary.BigEndian.Uint32(r.body))
		r.body = r.body[4:]
		if n >= 0 {
			values[column] = decode(r.body[:n], r.meta[column])
			r.body = r.body[n:]
		} else {
			values[column] = nil
		}
	}
	r.row++
	return nil
}

type Error struct {
	Code int
	Msg  string
}

func (e Error) Error() string {
	return e.Msg
}

func init() {
	sql.Register("gocql", &drv{})
}
