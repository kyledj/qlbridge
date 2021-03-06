package exec

import (
	u "github.com/araddon/gou"
	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/value"
	"github.com/araddon/qlbridge/vm"
)

type Projection struct {
	*TaskBase
	sql *expr.SqlSelect
}

func NewProjection(sqlSelect *expr.SqlSelect) *Projection {
	s := &Projection{
		TaskBase: NewTaskBase("Projection"),
		sql:      sqlSelect,
	}
	s.Handler = s.projectionEvaluator()
	return s
}

// Create handler function for evaluation (ie, field selection from tuples)
func (m *Projection) projectionEvaluator() MessageHandler {
	out := m.MessageOut()
	columns := m.sql.Columns
	// if len(m.sql.From) > 1 && m.sql.From[0].Source != nil && len(m.sql.From[0].Source.Columns) > 0 {
	// 	// we have re-written this query, lets build new list of columns
	// 	columns = make(expr.Columns, 0)
	// 	for _, from := range m.sql.From {
	// 		for _, col := range from.Source.Columns {
	// 			columns = append(columns, col)
	// 		}
	// 	}
	// }
	return func(ctx *expr.Context, msg datasource.Message) bool {
		// defer func() {
		// 	if r := recover(); r != nil {
		// 		u.Errorf("crap, %v", r)
		// 	}
		// }()

		//u.Infof("got projection message: %T %#v", msg, msg.Body())
		var outMsg datasource.Message
		switch mt := msg.(type) {
		case *datasource.SqlDriverMessageMap:
			// readContext := datasource.NewContextUrlValues(uv)
			// use our custom write context for example purposes
			writeContext := datasource.NewContextSimple()
			outMsg = writeContext
			//u.Debugf("about to project: %#v", mt)
			for _, col := range columns {
				if col.ParentIndex < 0 {
					continue
				}
				//u.Debugf("col: idx:%v pidx:%v key:%v   %s", col.Index, col.ParentIndex, col.Key(), col.Expr)
				if col.Guard != nil {
					ifColValue, ok := vm.Eval(mt, col.Guard)
					if !ok {
						u.Errorf("Could not evaluate if:   %v", col.Guard.String())
						//return fmt.Errorf("Could not evaluate if clause: %v", col.Guard.String())
					}
					//u.Debugf("if eval val:  %T:%v", ifColValue, ifColValue)
					switch ifColVal := ifColValue.(type) {
					case value.BoolValue:
						if ifColVal.Val() == false {
							//u.Debugf("Filtering out col")
							continue
						}
					}
				}
				if col.Star {
					for k, v := range mt.Row() {
						writeContext.Put(&expr.Column{As: k}, nil, value.NewValue(v))
					}
				} else {
					v, ok := vm.Eval(mt, col.Expr)
					if !ok {
						u.Warnf("failed eval key=%v  val=%#v expr:%s   mt:%#v", col.Key(), v, col.Expr, mt)
					} else if v == nil {
						u.Debugf("evaled: key=%v  val=%v", col.Key(), v)
						writeContext.Put(col, mt, v)
					} else {
						//u.Debugf("evaled: key=%v  val=%v", col.Key(), v.Value())
						writeContext.Put(col, mt, v)
					}
				}
			}

		case *datasource.ContextUrlValues:
			// readContext := datasource.NewContextUrlValues(uv)
			// use our custom write context for example purposes
			writeContext := datasource.NewContextSimple()
			outMsg = writeContext
			//u.Infof("about to project: colsct%v %#v", len(sql.Columns), outMsg)
			for _, col := range columns {
				//u.Debugf("col:   %#v", col)
				if col.Guard != nil {
					ifColValue, ok := vm.Eval(mt, col.Guard)
					if !ok {
						u.Errorf("Could not evaluate if:   %v", col.Guard.String())
						//return fmt.Errorf("Could not evaluate if clause: %v", col.Guard.String())
					}
					//u.Debugf("if eval val:  %T:%v", ifColValue, ifColValue)
					switch ifColVal := ifColValue.(type) {
					case value.BoolValue:
						if ifColVal.Val() == false {
							//u.Debugf("Filtering out col")
							continue
						}
					}
				}
				if col.Star {
					for k, v := range mt.Row() {
						writeContext.Put(&expr.Column{As: k}, nil, v)
					}
				} else {
					//u.Debugf("tree.Root: as?%v %#v", col.As, col.Expr)
					v, ok := vm.Eval(mt, col.Expr)
					//u.Debugf("evaled: ok?%v key=%v  val=%v", ok, col.Key(), v)
					if ok {
						writeContext.Put(col, mt, v)
					}
				}

			}
		default:
			u.Errorf("could not project msg:  %T", msg)
		}

		//u.Debugf("completed projection for: %p %#v", out, outMsg)
		select {
		case out <- outMsg:
			return true
		case <-m.SigChan():
			return false
		}
	}
}
