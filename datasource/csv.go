package datasource

import (
	"database/sql/driver"
	"encoding/csv"
	"io"
	"os"

	u "github.com/araddon/gou"

	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/schema"
	"github.com/araddon/qlbridge/value"
)

var (
	_ schema.DataSource = (*CsvDataSource)(nil)
	_ schema.SourceConn = (*CsvDataSource)(nil)
	_ schema.Scanner    = (*CsvDataSource)(nil)
)

// Csv DataSource, implements qlbridge DataSource to scan through data
//   - very, very naive scanner, forward only single pass
//   - can open a file with .Open()
//   - assumes comma delimited
//   - not thread-safe
type CsvDataSource struct {
	table     string
	tblschema *schema.Table
	exit      <-chan bool
	csvr      *csv.Reader
	rowct     uint64
	headers   []string
	colindex  map[string]int
	indexCol  int
	rc        io.ReadCloser
	filter    expr.Node
}

// Csv reader assumes we are getting first row as headers
//
func NewCsvSource(table string, indexCol int, ior io.Reader, exit <-chan bool) (*CsvDataSource, error) {
	m := CsvDataSource{table: table, indexCol: indexCol}
	if rc, ok := ior.(io.ReadCloser); ok {
		m.rc = rc
	}
	m.csvr = csv.NewReader(ior)
	m.csvr.TrailingComma = true // allow empty fields
	// if flagCsvDelimiter == "|" {
	// 	m.csvr.Comma = '|'
	// } else if flagCsvDelimiter == "\t" || flagCsvDelimiter == "t" {
	// 	m.csvr.Comma = '\t'
	// }
	headers, err := m.csvr.Read()
	if err != nil {
		u.Warnf("err csv %v", err)
		return nil, err
	}
	//u.Debugf("headers: %v", headers)
	m.headers = headers
	m.colindex = make(map[string]int, len(headers))
	for i, key := range headers {
		m.colindex[key] = i
	}
	return &m, nil
}

func (m *CsvDataSource) Tables() []string                                { return []string{m.table} }
func (m *CsvDataSource) Columns() []string                               { return m.headers }
func (m *CsvDataSource) CreateIterator(filter expr.Node) schema.Iterator { return m }
func (m *CsvDataSource) Table(tableName string) (*schema.Table, error) {
	if m.tblschema != nil {
		return m.tblschema, nil
	}
	m.tblschema = schema.NewTable(tableName, nil)
	for _, col := range m.Columns() {
		m.tblschema.AddField(schema.NewFieldBase(col, value.StringType, 64, "string"))
	}
	m.tblschema.SetColumns(m.Columns())
	return m.tblschema, nil
}

func (m *CsvDataSource) Open(connInfo string) (schema.SourceConn, error) {
	if connInfo == "stdio" || connInfo == "stdin" {
		connInfo = "/dev/stdin"
	}
	f, err := os.Open(connInfo)
	if err != nil {
		return nil, err
	}
	exit := make(<-chan bool, 1)
	return NewCsvSource(connInfo, 0, f, exit)
}

func (m *CsvDataSource) Close() error {
	defer func() {
		if r := recover(); r != nil {
			u.Errorf("close error: %v", r)
		}
	}()
	if m.rc != nil {
		m.rc.Close()
	}
	return nil
}

func (m *CsvDataSource) MesgChan(filter expr.Node) <-chan schema.Message {
	iter := m.CreateIterator(filter)
	return SourceIterChannel(iter, filter, m.exit)
}

func (m *CsvDataSource) Next() schema.Message {
	select {
	case <-m.exit:
		return nil
	default:
		for {
			row, err := m.csvr.Read()
			//u.Debugf("headers: %#v \n\trows:  %#v", m.headers, row)
			if err != nil {
				if err == io.EOF {
					return nil
				}
				u.Warnf("could not read row? %v", err)
				continue
			}
			m.rowct++
			if len(row) != len(m.headers) {
				u.Warnf("headers/cols dont match, dropping expected:%d got:%d   vals=", len(m.headers), len(row), row)
				continue
			}
			vals := make([]driver.Value, len(row))
			for i, val := range row {
				vals[i] = val
			}
			return NewSqlDriverMessageMap(m.rowct, vals, m.colindex)
		}
	}
}
