package redisc

import (
	"errors"
	"sync"

	"github.com/gomodule/redigo/redis"
)

// Support pipeline command and redirect handling for a redis cluster.
// You can use it to run a pipeline in a redis cluster(without proxy), just like using a redis.Conn.
// PipeConn splits a pipeline into multiple batches according to the keys commands, then runs every batch in goroutines concurrently.
// Every batch is a real pipeline request to a redis node. Every response will be stored in the cmd.reply until all requests got their request.
// If MOVED response occurs, the PipeConn will invoke Cluster.Refresh to refresh the cluster slots mapping, and then handle them in the new
// batches according the addresses for new nodes.

type cmd struct {
	command string
	args    []interface{}
	reply   interface{}
	err     error
	re      *RedirError
}

// batch includes the commands corresponding a same redis node. A real redis pipeline will be run when a batch runs
type batch struct {
	conn redis.Conn
	cmds []int // indexes
}

// PipeConn is a struct that implements redis.Conn interface. It is used to handle pipeline command in a redis cluster
type PipeConn struct {
	cluster     *Cluster // immutable
	forceDial   bool     // immutable
	cmds        []cmd
	batches     []batch
	send        int
	recv        int
	transaction bool
	readOnly    bool
	mu          sync.Mutex
	err         error
}

// run a batch that do the real redis pipeline request
func (bt *batch) run(p *PipeConn) (err error) {
	if len(bt.cmds) == 0 {
		return nil
	}

	defer func() {
		if err != nil {
			for _, idx := range bt.cmds {
				p.cmds[idx].err = err
			}
		}
	}()

	if bt.conn == nil {
		cmd := p.cmds[bt.cmds[0]]
		key, _ := cmdKey(cmd.command, cmd.args)
		slot := Slot(key)
		if bt.conn, _, err = p.cluster.getConn(slot, p.forceDial, p.readOnly); err != nil {
			return
		}
	}

	if p.transaction {
		if err = bt.conn.Send("MULTI"); err != nil {
			return err
		}
	}

	for _, idx := range bt.cmds {
		cmd := p.cmds[idx]
		if err = bt.conn.Send(cmd.command, cmd.args...); err != nil {
			return err
		}
	}

	var replies []interface{}
	if p.transaction {
		if replies, err = redis.Values(bt.conn.Do("EXEC")); err != nil {
			return
		}
	} else {
		if err = bt.conn.Flush(); err != nil {
			return
		}
		for range bt.cmds {
			if reply, err := bt.conn.Receive(); err != nil {
				replies = append(replies, err)
			} else {
				replies = append(replies, reply)
			}
		}
	}

	if len(replies) != len(bt.cmds) {
		err = errors.New("unexpected reply")
		return
	}

	for i, idx := range bt.cmds {
		if err, ok := replies[i].(error); ok {
			p.cmds[idx].err = err
			if err != nil {
				if re := ParseRedir(err); re != nil {
					p.cmds[idx].re = re
					if re.Type == "MOVED" {
						p.cluster.needsRefresh(re)
					}
				}
			}
		} else {
			p.cmds[idx].reply = replies[i]
		}
	}
	return
}

// buildBatches split the commands into the batches with node
func (p *PipeConn) buildBatches() error {
	cmds := p.cmds[p.send:]
	if len(cmds) == 0 {
		return nil
	}

	keys := make([]string, 0, len(cmds))
	for _, v := range cmds {
		key, err := cmdKey(v.command, v.args)
		if err != nil {
			return err
		}
		keys = append(keys, key)
	}

	byNode := SplitByNode(p.cluster, keys...)
	if p.batches == nil {
		p.batches = make([]batch, 0, len(byNode))
	}

	for _, node := range byNode {
		var bt batch
		for _, key := range node {
			for i, k := range keys {
				if key == k {
					bt.cmds = append(bt.cmds, i+p.send)
					keys[i] = ""
					break
				}
			}
		}
		p.batches = append(p.batches, bt)
	}

	if p.transaction {
		switch len(p.batches) {
		case 1:
			if len(SplitBySlot(byNode[0]...)) == 1 {
				break
			}
			fallthrough
		default:
			return errors.New("keys must be one slot in transaction mode")
		}
	}

	return nil
}

// buildRedirectBatches build the redirect batches to handle MOVED error
func (p *PipeConn) buildRedirectBatches() {
	mapping := make(map[string][]int)
	for i, cmd := range p.cmds {
		if cmd.re != nil {
			mapping[cmd.re.Addr] = append(mapping[cmd.re.Addr], i)
		}
	}

	for _, indexes := range mapping {
		p.batches = append(p.batches, batch{
			cmds: indexes,
		})
	}
}

func (p *PipeConn) doRedirect() {
	p.buildRedirectBatches()
	p.runBatches()
}

// runBatches run all the batches in goroutines, and wait them return
func (p *PipeConn) runBatches() {
	l := len(p.batches)
	switch l {
	case 0:
		return
	case 1:
		p.batches[0].run(p)
	default:
		var wg sync.WaitGroup
		wg.Add(l)
		for _, bt := range p.batches {
			go func(bt batch) {
				defer wg.Done()
				bt.run(p)
			}(bt)
		}
		wg.Wait()
	}

	for _, bt := range p.batches {
		bt.cmds = bt.cmds[:0]
		if bt.conn != nil {
			bt.conn.Close()
		}
	}

	p.batches = p.batches[:0]
}

/* redis.Conn interface impl*/

// Err returns a non-nil value if the connection is broken. Applications
// should close broken connections.
func (p *PipeConn) Err() error {
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()

	return err
}

func (p *PipeConn) Do(command string, args ...interface{}) (interface{}, error) {
	if command != "" {
		if err := p.Send(command, args...); err != nil {
			return nil, err
		}
	}

	if err := p.Flush(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if command == "" {
		reply := make([]interface{}, len(p.cmds))
		for i := range reply {
			r, e := p.receive()
			if e != nil {
				return nil, e
			}
			reply[i] = r
		}
		return reply, nil
	}

	var err error
	var reply interface{}
	for i := 0; i <= len(p.cmds); i++ {
		var e error
		if reply, e = p.receive(); e != nil {
			return nil, err
		}
		if e, ok := reply.(redis.Error); ok && err == nil {
			err = e
		}
	}
	return reply, err
}

// Send check command and append the command into the cmd list
func (p *PipeConn) Send(command string, args ...interface{}) error {

	if _, err := cmdKey(command, args); err != nil {
		return errors.New("key should not be empty")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.cmds = append(p.cmds, cmd{
		command: command,
		args:    args,
	})

	return nil
}

func (p *PipeConn) Close() error {
	return nil
}

// Flush build all the batches, and run them concurrently in different goroutines.
// all replies will be stored in every cmd struct once all requests respond.
// the redirection will be handled if there is any MOVED error returned.
func (p *PipeConn) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.buildBatches(); err != nil {
		return err
	}

	p.runBatches()
	p.doRedirect()
	p.send += len(p.cmds[p.send:])

	return nil
}

// Receive a reply from pipeline
func (p *PipeConn) Receive() (reply interface{}, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.receive()
}

func (p *PipeConn) receive() (reply interface{}, err error) {

	if p.send <= p.recv {
		return nil, errors.New("no more reply")
	}

	reply = p.cmds[p.recv].reply
	err = p.cmds[p.recv].err

	p.recv++
	if p.recv == p.send {
		p.recv = 0
		p.send = 0
		p.cmds = p.cmds[:0]
	}

	return
}
