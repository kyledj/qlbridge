package exec

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"

	u "github.com/araddon/gou"

	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/vm"
)

var (
	_ = u.EMPTY

	// Ensure that we implement the Task Runner interface
	_ TaskRunner = (*JoinMerge)(nil)
)

type KeyEvaluator func(msg datasource.Message) driver.Value

// Evaluate messages to create JoinKey based message, where the
//    Join Key (composite of each value in join expr) hashes consistently
//
type JoinKey struct {
	*TaskBase
	conf     *datasource.RuntimeSchema
	from     *expr.SqlSource
	colIndex map[string]int
}

// A JoinKey task that evaluates the compound JoinKey to allow
//  for parallelized join's
//
//   source1   ->  JoinKey  ->  hash-route
//                                         \
//                                          --  join  -->
//                                         /
//   source2   ->  JoinKey  ->  hash-route
//
func NewJoinKey(from *expr.SqlSource, conf *datasource.RuntimeSchema) (*JoinKey, error) {
	m := &JoinKey{
		TaskBase: NewTaskBase("JoinKey"),
		colIndex: make(map[string]int),
		from:     from,
	}
	return m, nil
}

func (m *JoinKey) Copy() *JoinKey { return &JoinKey{} }

func (m *JoinKey) Close() error {
	if err := m.TaskBase.Close(); err != nil {
		return err
	}
	return nil
}

func (m *JoinKey) Run(context *expr.Context) error {
	defer context.Recover()
	defer close(m.msgOutCh)

	outCh := m.MessageOut()
	inCh := m.MessageIn()
	joinNodes := m.from.JoinNodes()

	for {

		select {
		case <-m.SigChan():
			//u.Debugf("got signal quit")
			return nil
		case msg, ok := <-inCh:
			if !ok {
				//u.Debugf("NICE, got msg shutdown")
				return nil
			} else {
				//u.Infof("In joinkey msg %#v", msg)
			msgTypeSwitch:
				switch mt := msg.(type) {
				case *datasource.SqlDriverMessageMap:
					vals := make([]string, len(joinNodes))
					for i, node := range joinNodes {
						joinVal, ok := vm.Eval(mt, node)
						//u.Debugf("evaluating: ok?%v T:%T result=%v node '%v'", ok, joinVal, joinVal.ToString(), node.String())
						if !ok {
							u.Errorf("could not evaluate: %T %#v   %v", joinVal, joinVal, msg)
							break msgTypeSwitch
						}
						vals[i] = joinVal.ToString()
					}
					key := strings.Join(vals, string(byte(0)))
					mt.SetKeyHashed(key)
					outCh <- mt
				default:
					return fmt.Errorf("To use JoinKey must use SqlDriverMessageMap but got %T", msg)
				}
			}
		}
	}
	return nil
}

// Scan a data source for rows, feed into runner for join sources
//
//  1) join  SELECT t1.name, t2.salary
//               FROM employee AS t1
//               INNER JOIN info AS t2
//               ON t1.name = t2.name;
//
type JoinMerge struct {
	*TaskBase
	conf      *datasource.RuntimeSchema
	leftStmt  *expr.SqlSource
	rightStmt *expr.SqlSource
	ltask     TaskRunner
	rtask     TaskRunner
	colIndex  map[string]int
}

// A very stupid naive parallel join merge, uses Key() as value to merge
//   two different input channels
//
//   source1   ->
//                \
//                  --  join  -->
//                /
//   source2   ->
//
func NewJoinNaiveMerge(ltask, rtask TaskRunner, lfrom, rfrom *expr.SqlSource, conf *datasource.RuntimeSchema) (*JoinMerge, error) {

	m := &JoinMerge{
		TaskBase: NewTaskBase("JoinNaiveMerge"),
		colIndex: make(map[string]int),
	}

	m.ltask = ltask
	m.rtask = rtask
	m.leftStmt = lfrom
	m.rightStmt = rfrom

	return m, nil
}

func (m *JoinMerge) Copy() *JoinMerge { return &JoinMerge{} }

func (m *JoinMerge) Close() error {
	if err := m.TaskBase.Close(); err != nil {
		return err
	}
	return nil
}

func (m *JoinMerge) Run(context *expr.Context) error {
	defer context.Recover()
	defer close(m.msgOutCh)

	outCh := m.MessageOut()

	leftIn := m.ltask.MessageOut()
	rightIn := m.rtask.MessageOut()

	//u.Infof("left? %s", m.leftStmt)
	// lhNodes := m.leftStmt.JoinNodes()
	// rhNodes := m.rightStmt.JoinNodes()

	// Build an index of source to destination column indexing
	for _, col := range m.leftStmt.Source.Columns {
		//u.Debugf("left col:  idx=%d  key=%q as=%q col=%v parentidx=%v", len(m.colIndex), col.Key(), col.As, col.String(), col.ParentIndex)
		m.colIndex[m.leftStmt.Alias+"."+col.Key()] = col.ParentIndex
		//u.Debugf("colIndex:  %15q : %d", m.leftStmt.Alias+"."+col.Key(), col.SourceIndex)
	}
	for _, col := range m.rightStmt.Source.Columns {
		//u.Debugf("right col:  idx=%d  key=%q as=%q col=%v", len(m.colIndex), col.Key(), col.As, col.String())
		m.colIndex[m.rightStmt.Alias+"."+col.Key()] = col.ParentIndex
		//u.Debugf("colIndex:  %15q : %d", m.rightStmt.Alias+"."+col.Key(), col.SourceIndex)
	}

	// lcols := m.leftStmt.Source.AliasedColumns()
	// rcols := m.rightStmt.Source.AliasedColumns()

	//u.Infof("lcols:  %#v for sql %s", lcols, m.leftStmt.Source.String())
	//u.Infof("rcols:  %#v for sql %v", rcols, m.rightStmt.Source.String())
	lh := make(map[string][]*datasource.SqlDriverMessageMap)
	rh := make(map[string][]*datasource.SqlDriverMessageMap)

	wg := new(sync.WaitGroup)
	wg.Add(1)
	var fatalErr error
	go func() {
		for {
			//u.Infof("In source Scanner msg %#v", msg)
			select {
			case <-m.SigChan():
				u.Warnf("got signal quit")
				return
			case msg, ok := <-leftIn:
				if !ok {
					//u.Warnf("NICE, got left shutdown")
					wg.Done()
					return
				} else {
					switch mt := msg.(type) {
					case *datasource.SqlDriverMessageMap:
						key := mt.Key()
						if key == "" {
							fatalErr = fmt.Errorf(`To use Join msgs must have keys but got "" for %+v`, mt.Row())
							close(m.TaskBase.sigCh)
							return
						}
						lh[key] = append(lh[key], mt)
					default:
						fatalErr = fmt.Errorf("To use Join must use SqlDriverMessageMap but got %T", msg)
						close(m.TaskBase.sigCh)
						return
					}
				}
			}

		}
	}()
	wg.Add(1)
	go func() {
		for {

			//u.Infof("In source Scanner iter %#v", item)
			select {
			case <-m.SigChan():
				u.Warnf("got quit signal join source 1")
				return
			case msg, ok := <-rightIn:
				if !ok {
					//u.Warnf("NICE, got right shutdown")
					wg.Done()
					return
				} else {
					switch mt := msg.(type) {
					case *datasource.SqlDriverMessageMap:
						key := mt.Key()
						if key == "" {
							fatalErr = fmt.Errorf(`To use Join msgs must have keys but got "" for %+v`, mt.Row())
							close(m.TaskBase.sigCh)
							return
						}
						rh[key] = append(rh[key], mt)
					default:
						fatalErr = fmt.Errorf("To use Join must use SqlDriverMessageMap but got %T", msg)
						close(m.TaskBase.sigCh)
						return
					}
				}
			}

		}
	}()
	wg.Wait()
	//u.Info("leaving source scanner")
	i := uint64(0)
	for keyLeft, valLeft := range lh {
		//u.Debugf("compare:  key:%v  left:%#v  right:%#v  rh: %#v", keyLeft, valLeft, rh[keyLeft], rh)
		if valRight, ok := rh[keyLeft]; ok {
			//u.Debugf("found match?\n\t%d left=%#v\n\t%d right=%#v", len(valLeft), valLeft, len(valRight), valRight)
			msgs := m.mergeValueMessages(valLeft, valRight)
			//u.Debugf("msgsct: %v   msgs:%#v", len(msgs), msgs)
			for _, msg := range msgs {
				//outCh <- datasource.NewUrlValuesMsg(i, msg)
				//u.Debugf("i:%d   msg:%#v", i, msg.Row())
				msg.IdVal = i
				i++
				outCh <- msg
			}
		}
	}
	return nil
}

func (m *JoinMerge) mergeValueMessages(lmsgs, rmsgs []*datasource.SqlDriverMessageMap) []*datasource.SqlDriverMessageMap {
	// m.leftStmt.Columns, m.rightStmt.Columns, nil
	//func mergeValuesMsgs(lmsgs, rmsgs []datasource.Message, lcols, rcols []*expr.Column, cols map[string]*expr.Column) []*datasource.SqlDriverMessageMap {
	out := make([]*datasource.SqlDriverMessageMap, 0)
	//u.Infof("merge values: %v:%v", len(lcols), len(rcols))
	for _, lm := range lmsgs {
		//u.Warnf("nice SqlDriverMessageMap: %#v", lmt)
		for _, rm := range rmsgs {
			// for k, val := range rmt.Row() {
			// 	u.Debugf("k=%v v=%v", k, val)
			// }
			vals := make([]driver.Value, len(m.colIndex))
			vals = m.valIndexing(vals, lm.Values(), m.leftStmt.Source.Columns)
			vals = m.valIndexing(vals, rm.Values(), m.rightStmt.Source.Columns)
			newMsg := datasource.NewSqlDriverMessageMap(0, vals, m.colIndex)
			//u.Infof("out: %+v", newMsg)
			out = append(out, newMsg)
		}
	}
	return out
}

func (m *JoinMerge) valIndexing(valOut, valSource []driver.Value, cols []*expr.Column) []driver.Value {
	for _, col := range cols {
		if col.ParentIndex < 0 {
			continue
		}
		if col.ParentIndex >= len(valOut) {
			u.Warnf("not enough values to read col? i=%v len(vals)=%v  %#v", col.ParentIndex, len(valOut), valOut)
			continue
		}
		//u.Infof("found: i=%v pi:%v as=%v	val=%v	source:%v", col.SourceIndex, col.ParentIndex, col.As, valSource[col.SourceIndex], valSource)
		valOut[col.ParentIndex] = valSource[col.SourceIndex]
	}
	return valOut
}

/*

func joinValue(nodes []expr.Node, msg datasource.Message) (string, bool) {

	if msg == nil {
		u.Warnf("got nil message?")
	}
	//u.Infof("joinValue msg T:%T Body %#v", msg, msg.Body())
	//switch mt := msg.(type) {
	// case *datasource.SqlDriverMessage:
	// 	msgReader := datasource.NewValueContextWrapper(mt, cols)
	// 	vals := make([]string, len(nodes))
	// 	for i, node := range nodes {
	// 		joinVal, ok := vm.Eval(msgReader, node)
	// 		//u.Debugf("msg: %#v", msgReader)
	// 		//u.Debugf("evaluating: ok?%v T:%T result=%v node '%v'", ok, joinVal, joinVal.ToString(), node.String())
	// 		if !ok {
	// 			u.Errorf("could not evaluate: %T %#v   %v", joinVal, joinVal, msg)
	// 			return "", false
	// 		}
	// 		vals[i] = joinVal.ToString()
	// 	}
	// 	return strings.Join(vals, string(byte(0))), true
	//default:
	if msgReader, ok := msg.Body().(expr.ContextReader); ok {
		vals := make([]string, len(nodes))
		for i, node := range nodes {
			joinVal, ok := vm.Eval(msgReader, node)
			//u.Debugf("msg: %#v", msgReader)
			//u.Debugf("evaluating: ok?%v T:%T result=%v node '%v'", ok, joinVal, joinVal.ToString(), node.String())
			if !ok {
				u.Errorf("could not evaluate: %T %#v   %v", joinVal, joinVal, msg)
				return "", false
			}
			vals[i] = joinVal.ToString()
		}
		return strings.Join(vals, string(byte(0))), true
	} else {
		u.Errorf("could not convert to message reader: %T", msg.Body())
	}
	//}

	return "", false
}


func reAlias2(msg *datasource.SqlDriverMessageMap, vals []driver.Value, cols []*expr.Column) *datasource.SqlDriverMessageMap {

	// for _, col := range cols {
	// 	if col.Index >= len(vals) {
	// 		u.Warnf("not enough values to read col? i=%v len(vals)=%v  %#v", col.Index, len(vals), vals)
	// 		continue
	// 	}
	// 	//u.Infof("found: i=%v as=%v   val=%v", col.Index, col.As, vals[col.Index])
	// 	m.Vals[col.As] = vals[col.Index]
	// }
	msg.SetRow(vals)
	return msg
}

func mergeUv(m1, m2 *datasource.ContextUrlValues) *datasource.ContextUrlValues {
	out := datasource.NewContextUrlValues(m1.Data)
	for k, val := range m2.Data {
		//u.Debugf("k=%v v=%v", k, val)
		out.Data[k] = val
	}
	return out
}

func mergeUvMsgs(lmsgs, rmsgs []datasource.Message, lcols, rcols map[string]*expr.Column) []*datasource.ContextUrlValues {
	out := make([]*datasource.ContextUrlValues, 0)
	for _, lm := range lmsgs {
		switch lmt := lm.Body().(type) {
		case *datasource.ContextUrlValues:
			for _, rm := range rmsgs {
				switch rmt := rm.Body().(type) {
				case *datasource.ContextUrlValues:
					// for k, val := range rmt.Data {
					// 	u.Debugf("k=%v v=%v", k, val)
					// }
					newMsg := datasource.NewContextUrlValues(url.Values{})
					newMsg = reAlias(newMsg, lmt.Data, lcols)
					newMsg = reAlias(newMsg, rmt.Data, rcols)
					//u.Debugf("pre:  %#v", lmt.Data)
					//u.Debugf("post:  %#v", newMsg.Data)
					out = append(out, newMsg)
				default:
					u.Warnf("uknown type: %T", rm)
				}
			}
		default:
			u.Warnf("uknown type: %T   %T", lmt, lm)
		}
	}
	return out
}

func reAlias(m *datasource.ContextUrlValues, vals url.Values, cols map[string]*expr.Column) *datasource.ContextUrlValues {
	for k, val := range vals {
		if col, ok := cols[k]; !ok {
			u.Warnf("Should not happen? missing %v  ", k)
		} else {
			//u.Infof("found: k=%v as=%v   val=%v", k, col.As, val)
			m.Data[col.As] = val
		}
	}
	return m
}

func reAliasMap(m *datasource.SqlDriverMessageMap, vals map[string]driver.Value, cols []*expr.Column) *datasource.SqlDriverMessageMap {
	row := make([]driver.Value, len(cols))
	for _, col := range cols {
		//u.Infof("found: i=%v as=%v   val=%v", col.Index, col.As, vals[col.Index])
		//m.Vals[col.As] = vals[col.Key()]
		row[col.Index] = vals[col.Key()]
	}
	m.SetRow(row)
	return m
}
*/
