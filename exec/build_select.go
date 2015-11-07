package exec

import (
	"fmt"
	"strings"

	u "github.com/araddon/gou"

	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/datasource/membtree"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/value"
)

const (
	MaxAllowedPacket = 1024 * 1024
)

func (m *JobBuilder) VisitSelect(stmt *expr.SqlSelect) (expr.Task, expr.VisitStatus, error) {

	u.Debugf("VisitSelect %+v", stmt)

	tasks := make(Tasks, 0)

	if len(stmt.From) == 0 {
		if stmt.SystemQry() {
			return m.VisitSelectSystemInfo(stmt)
		}
		u.Warnf("no from? %v", stmt.String())
		return nil, expr.VisitError, fmt.Errorf("No From for %v", stmt.String())

	} else if len(stmt.From) == 1 {

		stmt.From[0].Source = stmt
		task, status, err := m.VisitSubSelect(stmt.From[0])
		if err != nil {
			return nil, status, err
		}
		if status == expr.VisitFinal {
			u.Warnf("subselect visit final returning job.proj: %p", m.Projection)
			return task, status, nil
		}
		tasks.Add(task.(TaskRunner))

		// Add a Final Projection to choose the columns for results
		projection := NewProjectionFinal(stmt)
		//u.Infof("adding projection: %#v", projection)
		tasks.Add(projection)

		return NewSequential("select", tasks), expr.VisitContinue, nil

	} else {

		var prevTask TaskRunner
		var prevFrom *expr.SqlSource

		for i, from := range stmt.From {

			// Need to rewrite the From statement
			from.Rewrite(stmt)
			sourceTask, status, err := m.VisitSubSelect(from)
			if err != nil {
				u.Errorf("Could not visitsubselect %v  %s", err, from)
				return nil, status, err
			}

			// now fold into previous task
			curTask := sourceTask.(TaskRunner)
			if i != 0 {
				from.Seekable = true
				twoTasks := []TaskRunner{prevTask, curTask}
				curMergeTask := NewTaskParallel("select-sources", nil, twoTasks)
				tasks.Add(curMergeTask)

				// fold this source into previous
				in, err := NewJoinNaiveMerge(prevTask, curTask, prevFrom, from, m.Conf)
				if err != nil {
					return nil, expr.VisitError, err
				}
				tasks.Add(in)
			}
			prevTask = curTask
			prevFrom = from
			//u.Debugf("got task: %T", prevTask)
		}
	}

	if stmt.Where != nil {
		switch {
		case stmt.Where.Source != nil:
			u.Warnf("Found un-supported subquery: %#v", stmt.Where)
			return nil, expr.VisitError, fmt.Errorf("Unsupported Where Type")
		case stmt.Where.Expr != nil:
			//u.Debugf("adding where: %q", stmt.Where.Expr)
			where := NewWhereFinal(stmt.Where.Expr, stmt)
			tasks.Add(where)
		default:
			u.Warnf("Found un-supported where type: %#v", stmt.Where)
			return nil, expr.VisitError, fmt.Errorf("Unsupported Where Type")
		}

	}

	// Add a Final Projection to choose the columns for results
	projection := NewProjectionFinal(stmt)
	m.Projection = nil
	u.Infof("%p projection added  %s", projection, stmt.String())
	tasks.Add(projection)

	return NewSequential("select", tasks), expr.VisitContinue, nil
}

// Build Column Name to Position index for given *source* (from) used to interpret
// positional []driver.Value args, mutate the *from* itself to hold this map
func buildColIndex(sourceConn datasource.SourceConn, from *expr.SqlSource) error {
	if from.Source == nil {
		return nil
	}
	colSchema, ok := sourceConn.(datasource.SchemaColumns)
	if !ok {
		u.Errorf("Could not create column Schema for %v  %T %#v", from.Name, sourceConn, sourceConn)
		return fmt.Errorf("Must Implement SchemaColumns for BuildColIndex")
	}
	from.BuildColIndex(colSchema.Columns())
	return nil
}

func (m *JobBuilder) VisitSubSelect(from *expr.SqlSource) (expr.Task, expr.VisitStatus, error) {

	if from.Source != nil {
		u.Debugf("VisitSubselect from.source = %q", from.Source)
	} else {
		u.Debugf("VisitSubselect from=%q", from)
	}

	tasks := make(Tasks, 0)
	needsJoinKey := false

	sourceFeatures := m.Conf.Sources.Get(from.SourceName())
	if sourceFeatures == nil {
		return nil, expr.VisitError, fmt.Errorf("Could not find source for %v", from.SourceName())
	}
	source, err := sourceFeatures.DataSource.Open(from.SourceName())
	if err != nil {
		return nil, expr.VisitError, err
	}

	if from.Source != nil && len(from.JoinNodes()) > 0 {
		needsJoinKey = true
	}

	sourcePlan, implementsSourceBuilder := source.(datasource.SourceSelectPlanner)
	//u.Debugf("source: tbl:%q  Builder?%v   %T  %#v", from.SourceName(), implementsSourceBuilder, source, source)
	// Must provider either Scanner, SourcePlanner, Seeker interfaces
	if implementsSourceBuilder {
		//  This is flawed, visitor pattern would have you pass in a object which implements interface
		//    but is one of many different objects that implement that interface so that the
		//    Accept() method calls the apppropriate method
		//u.Debugf("yes, a SourcePlanner????  %T", sourcePlan)
		// builder := NewJobBuilder(conf, connInfo)
		// task, err := stmt.Accept(builder)
		builder, err := sourcePlan.SubSelectVisitor()
		//u.Warnf("builder? %T", builder)
		if err != nil {
			u.Errorf("error on builder: %v", err)
			return nil, expr.VisitError, err
		} else if builder == nil {
			return nil, expr.VisitError, fmt.Errorf("No builder for %T", sourcePlan)
		}
		task, status, err := builder.VisitSubSelect(from)
		if err != nil {
			// PolyFill instead?
			return nil, status, err
		}
		if status == expr.VisitFinal {

			m.Projection, _ = sourcePlan.Projection()
			tasks.Add(task.(TaskRunner))

			if needsJoinKey {
				if _, ok := builder.(datasource.SchemaColumns); ok {
					if err := buildColIndex(source, from); err != nil {
						return nil, expr.VisitError, err
					}
				} else {
					u.Errorf("Didn't implement schema %T", builder)
				}

				joinKeyTask, err := NewJoinKey(from, m.Conf)
				if err != nil {
					return nil, expr.VisitError, err
				}
				tasks.Add(joinKeyTask)
			}
			return NewSequential("sub-select", tasks), status, nil
			//return task, status, nil
		}
		if task != nil {
			u.Infof("found task?")
			//tasks.Add(task)
			//return NewSequential("source-planner", tasks), nil
			return task, expr.VisitContinue, nil
		}
		u.Errorf("Could not source plan for %v  %T %#v", from.SourceName(), source, source)
	}
	scanner, hasScanner := source.(datasource.Scanner)
	if !hasScanner {
		u.Warnf("source %T does not implement datasource.Scanner", source)
		return nil, expr.VisitError, fmt.Errorf("%T Must Implement Scanner for %q", source, from.String())
	}

	switch {

	case needsJoinKey: //from.Source != nil && len(from.JoinNodes()) > 0:
		// This is a source that is part of a join expression
		if err := buildColIndex(scanner, from); err != nil {
			return nil, expr.VisitError, err
		}
		sourceTask := NewSourceJoin(from, scanner)
		tasks.Add(sourceTask)

	default:
		// If we have table name and no Source(sub-query/join-query) then just read source
		if err := buildColIndex(scanner, from); err != nil {
			return nil, expr.VisitError, err
		}
		sourceTask := NewSource(from, scanner)
		tasks.Add(sourceTask)

	}

	if from.Source != nil && from.Source.Where != nil {
		switch {
		case from.Source.Where.Expr != nil:
			//u.Debugf("adding where: %q", from.Source.Where.Expr)
			where := NewWhereFilter(from.Source.Where.Expr, from.Source)
			tasks.Add(where)
		default:
			u.Warnf("Found un-supported where type: %#v", from.Source)
			return nil, expr.VisitError, fmt.Errorf("Unsupported Where clause:  %q", from)
		}
	}

	if needsJoinKey {
		joinKeyTask, err := NewJoinKey(from, m.Conf)
		if err != nil {
			return nil, expr.VisitError, err
		}
		tasks.Add(joinKeyTask)
	}

	// TODO: projection, groupby, having
	// Plan?   Parallel?  hash?
	return NewSequential("sub-select", tasks), expr.VisitContinue, nil
}

// queries for internal schema/variables such as:
//
//    select @@max_allowed_packets
//    select current_user()
//    select connection_id()
//    select timediff(curtime(), utc_time())
//
func (m *JobBuilder) VisitSelectSystemInfo(stmt *expr.SqlSelect) (expr.Task, expr.VisitStatus, error) {

	u.Debugf("VisitSelectSchemaInfo %+v", stmt)

	if sysVar := stmt.SysVariable(); len(sysVar) > 0 {
		return m.VisitSysVariable(stmt)
	} else if len(stmt.From) == 0 && len(stmt.Columns) == 1 && strings.ToLower(stmt.Columns[0].As) == "database" {
		return m.VisitSelectDatabase(stmt)
	}

	tasks := make(Tasks, 0)

	task, status, err := m.VisitSubSelect(stmt.From[0])
	if err != nil {
		return nil, expr.VisitError, err
	}
	if status == expr.VisitFinal {
		return task, status, nil
	}
	tasks.Add(task.(TaskRunner))

	if stmt.Where != nil {
		switch {
		case stmt.Where.Source != nil:
			u.Warnf("Found un-supported subquery: %#v", stmt.Where)
			return nil, expr.VisitError, fmt.Errorf("Unsupported Where Type")
		case stmt.Where.Expr != nil:
			//u.Debugf("adding where: %q", stmt.Where.Expr)
			where := NewWhereFinal(stmt.Where.Expr, stmt)
			tasks.Add(where)
		default:
			u.Warnf("Found un-supported where type: %#v", stmt.Where)
			return nil, expr.VisitError, fmt.Errorf("Unsupported Where Type")
		}

	}

	// Add a Projection to choose the columns for results
	projection := NewProjectionInProcess(stmt)
	//u.Infof("adding projection: %#v", projection)
	tasks.Add(projection)

	return NewSequential("select-schemainfo", tasks), expr.VisitContinue, nil
}

func (m *JobBuilder) VisitSelectDatabase(stmt *expr.SqlSelect) (expr.Task, expr.VisitStatus, error) {
	u.Debugf("VisitSelectDatabase %+v", stmt)

	tasks := make(Tasks, 0)
	val := m.connInfo
	static := membtree.NewStaticDataValue(val, "database")
	sourceTask := NewSource(nil, static)
	tasks.Add(sourceTask)
	m.Projection = StaticProjection("database", value.StringType)
	return NewSequential("database", tasks), expr.VisitContinue, nil
}

func (m *JobBuilder) VisitSysVariable(stmt *expr.SqlSelect) (expr.Task, expr.VisitStatus, error) {
	u.Debugf("VisitSysVariable %+v", stmt)

	switch sysVar := strings.ToLower(stmt.SysVariable()); sysVar {
	case "@@max_allowed_packet":
		u.Infof("max allowed")
		m.Projection = StaticProjection("@@max_allowed_packet", value.IntType)
		return m.sysVarTasks(sysVar, MaxAllowedPacket)
	case "current_user()", "current_user":
		return m.sysVarTasks(sysVar, "user")
	case "connection_id()":
		return m.sysVarTasks(sysVar, 1)
	case "timediff(curtime(), utc_time())":
		return m.sysVarTasks("timediff", "00:00:00.000000")
		//
	default:
		u.Errorf("unknown var: %v", sysVar)
		return nil, expr.VisitError, fmt.Errorf("Unrecognized System Variable: %v", sysVar)
	}
}

// A very simple tasks/builder for system variables
//
func (m *JobBuilder) sysVarTasks(name string, val interface{}) (expr.Task, expr.VisitStatus, error) {
	tasks := make(Tasks, 0)
	static := membtree.NewStaticDataValue(name, val)
	sourceTask := NewSource(nil, static)
	tasks.Add(sourceTask)
	switch val.(type) {
	case int, int64:
		m.Projection = StaticProjection(name, value.IntType)
	case string:
		m.Projection = StaticProjection(name, value.StringType)
	case float32, float64:
		m.Projection = StaticProjection(name, value.NumberType)
	case bool:
		m.Projection = StaticProjection(name, value.BoolType)
	default:
		u.Errorf("unknown var: %v", val)
		return nil, expr.VisitError, fmt.Errorf("Unrecognized Data Type: %v", val)
	}
	return NewSequential("sys-var", tasks), expr.VisitContinue, nil
}

// A very simple projection of name=value, for single row/column
//   select @@max_bytes
//
func StaticProjection(name string, vt value.ValueType) *expr.Projection {
	p := expr.NewProjection()
	p.AddColumnShort(name, vt)
	return p
}