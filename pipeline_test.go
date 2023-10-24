package redisc

import (
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/ljfuyuan/redisc/redistest"
	"github.com/stretchr/testify/assert"
)

func TestPipeline(t *testing.T) {
	fn, ports := redistest.StartCluster(t, nil)
	defer fn()

	for i, p := range ports {
		ports[i] = ":" + p
	}

	t.Run("Send", func(t *testing.T) { testPipelineSend(t, ports) })
	t.Run("buildBatches", func(t *testing.T) { testPipelineBuildBatches(t, ports) })
	t.Run("batchRun", func(t *testing.T) { testPipelineBatchRun(t, ports) })
	t.Run("empty", func(t *testing.T) { testPipelineEmpty(t, ports) })
	t.Run("Do", func(t *testing.T) { testPipelineDo(t, ports) })
	t.Run("Move", func(t *testing.T) { testPipelineMove(t, ports) })
	t.Run("MGET", func(t *testing.T) { testPipelineMget(t, ports) })
}

func testPipelineSend(t *testing.T, ports []string) {
	c := &Cluster{
		StartupNodes: []string{ports[0]},
	}
	defer c.Close()

	err := c.Refresh()
	if assert.NoError(t, err, "Refresh") {
		pipe := c.GetPipeline()
		assert.Error(t, pipe.Send("MULTI"))
		assert.NoError(t, pipe.Send("SET", "abc", 1))
		assert.NoError(t, pipe.Send("SET", "abc", 2))
		assert.Len(t, pipe.(*PipeConn).cmds, 2)
	}
}

func testPipelineBuildBatches(t *testing.T, ports []string) {
	c := &Cluster{
		StartupNodes: []string{ports[0]},
	}
	defer c.Close()

	err := c.Refresh()
	if assert.NoError(t, err, "Refresh") {
		pipe := c.GetPipeline()
		assert.NoError(t, pipe.Send("SET", "{a}bc", 0))
		assert.NoError(t, pipe.Send("SET", "a{b}c", 1))
		assert.NoError(t, pipe.Send("SET", "ab{c}", 2))
		assert.NoError(t, pipe.Send("SET", "ac{c}", 3))
		conn, _ := pipe.(*PipeConn)
		assert.Len(t, conn.cmds, 4)
		assert.NoError(t, conn.buildBatches())
		assert.Len(t, conn.batches, 3)

		pipe = c.GetPipeline(EnableTransaction())
		// a slot 15495
		// b slot 3300
		c.mapping[15495] = []string{"server-1-master", "server-1-replica"}
		c.mapping[3300] = []string{"server-1-master", "server-1-replica"}
		assert.NoError(t, pipe.Send("SET", "a", 0))
		assert.NoError(t, pipe.Send("SET", "b", 1))
		conn, _ = pipe.(*PipeConn)
		assert.Len(t, conn.cmds, 2)
		assert.Error(t, conn.buildBatches())
	}
}

func testPipelineBatchRun(t *testing.T, ports []string) {
	c := &Cluster{
		StartupNodes: []string{ports[0]},
	}
	defer c.Close()

	err := c.Refresh()
	if assert.NoError(t, err, "Refresh") {
		pipe := c.GetPipeline()
		assert.NoError(t, pipe.Send("SET", "{a}bc", 0))
		assert.NoError(t, pipe.Send("SET", "a{b}c", 1))
		assert.NoError(t, pipe.Send("SET", "ab{c}", 2))
		assert.NoError(t, pipe.Send("SET", "ac{c}", 3))
		conn, _ := pipe.(*PipeConn)
		assert.Len(t, conn.cmds, 4)
		assert.NoError(t, conn.Flush())

		for _, cmd := range conn.cmds {
			assert.Equal(t, "OK", cmd.reply)
		}

		for range conn.cmds {
			reply, err := pipe.Receive()
			assert.Equal(t, "OK", reply)
			assert.Equal(t, nil, err)
		}

		assert.NoError(t, pipe.Send("SET", "ac{c}", 3))
		assert.NoError(t, pipe.Send("LPOP", "ac{c}"))
		assert.NoError(t, pipe.Flush())

		for k := range conn.cmds {
			reply, err := pipe.Receive()
			if k == 0 {
				assert.Equal(t, "OK", reply)
				assert.NoError(t, err)
			} else {
				assert.Nil(t, reply)
				assert.Error(t, err)
			}
		}

		pipe = c.GetPipeline(EnableTransaction())
		assert.NoError(t, pipe.Send("SET", "a", 0))
		assert.NoError(t, pipe.Send("SET", "a", 1))
		assert.NoError(t, pipe.Send("GET", "a"))
		conn, _ = pipe.(*PipeConn)
		assert.Len(t, conn.cmds, 3)
		assert.NoError(t, conn.Flush())
		for k, cmd := range conn.cmds {
			if k == 2 {
				reply, _ := redis.Int(cmd.reply, nil)
				assert.Equal(t, 1, reply)
			} else {
				assert.Equal(t, "OK", cmd.reply)
			}
		}

		assert.NoError(t, pipe.Send("SET", "ac{c}", 3))
		assert.NoError(t, pipe.Send("LPOP", "ac{c}"))
		assert.NoError(t, pipe.Flush())
		for k := range conn.cmds {
			reply, err := pipe.Receive()
			if k < 3 {
				if k == 2 {
					reply, _ := redis.Int(reply, nil)
					assert.Equal(t, 1, reply)
				} else {
					assert.Equal(t, "OK", reply)
				}
			} else {
				if k == 3 {
					assert.Equal(t, "OK", reply)
					assert.NoError(t, err)
				} else {
					assert.Nil(t, reply)
					assert.Error(t, err)
				}
			}
		}
		assert.NoError(t, pipe.Send("SET", "ac{c}", 3))
		assert.NoError(t, pipe.Send("LPOPXX", "ac{c}"))
		assert.NoError(t, pipe.Flush())
		for _, cmd := range conn.cmds {
			assert.Error(t, cmd.err)
		}
	}
}

func testPipelineEmpty(t *testing.T, ports []string) {
	c := &Cluster{
		StartupNodes: []string{ports[0]},
	}
	defer c.Close()

	err := c.Refresh()
	if assert.NoError(t, err, "Refresh") {
		pipe := c.GetPipeline()
		assert.NoError(t, pipe.Flush())
		reply, err := pipe.Receive()
		assert.Nil(t, reply)
		assert.NotNil(t, err)
	}
}

func testPipelineDo(t *testing.T, ports []string) {
	c := &Cluster{
		StartupNodes: []string{ports[0]},
	}
	defer c.Close()

	err := c.Refresh()
	if assert.NoError(t, err, "Refresh") {
		pipe := c.GetPipeline()
		assert.Nil(t, pipe.Send("INCR", "foo"))
		assert.Nil(t, pipe.Send("INCR", "bar"))
		reply, err := redis.Values(pipe.Do(""))
		assert.Nil(t, err)
		assert.Len(t, reply, 2)
		var value1, value2 int
		_, err = redis.Scan(reply, &value1, &value2)
		assert.Nil(t, err)
		assert.Equal(t, value1, 1)
		assert.Equal(t, value2, 1)

		pipe = c.GetPipeline()
		assert.Nil(t, pipe.Send("INCR", "foo"))
		assert.Nil(t, pipe.Send("INCR", "bar"))
		assert.NoError(t, pipe.Flush())
		// do not receive, Do instead
		reply, err = redis.Values(pipe.Do(""))
		assert.Nil(t, err)
		assert.Len(t, reply, 2)
		_, err = redis.Scan(reply, &value1, &value2)
		assert.Nil(t, err)
		assert.Equal(t, value1, 2)
		assert.Equal(t, value2, 2)
		assert.Nil(t, pipe.Send("INCR", "foo"))
		assert.Nil(t, pipe.Send("INCR", "bar"))
		replyInt, err := redis.Int(pipe.Do("INCR", "foo"))
		assert.Nil(t, err)
		assert.Equal(t, replyInt, 4)

		pipe = c.GetPipeline(EnableTransaction())
		assert.Nil(t, pipe.Send("INCR", "fo{o}"))
		assert.Nil(t, pipe.Send("INCR", "ba{o}"))

		reply, err = redis.Values(pipe.Do(""))
		assert.Nil(t, err)
		assert.Len(t, reply, 2)
		_, err = redis.Scan(reply, &value1, &value2)
		assert.Nil(t, err)
		assert.Equal(t, value1, 1)
		assert.Equal(t, value2, 1)

		replyInt, err = redis.Int(pipe.Do("INCR", "fo{o}"))
		assert.Nil(t, err)
		assert.Equal(t, replyInt, 2)
	}
}

func testPipelineMove(t *testing.T, ports []string) {
	c := &Cluster{
		StartupNodes: []string{ports[0]},
	}
	defer c.Close()

	err := c.Refresh()
	if assert.NoError(t, err, "Refresh") {
		pipe := c.GetPipeline()
		assert.Nil(t, pipe.Send("INCR", "a"))
		assert.Nil(t, pipe.Send("INCR", "b"))
		conn, _ := pipe.(*PipeConn)
		assert.Nil(t, conn.buildBatches())
		assert.Len(t, conn.batches, 2)
		// a slot 15945
		index := 0
		if conn.batches[0].cmds[0] == 0 {
			index = 1
		}
		conn.batches[index].conn, _, err = conn.cluster.getConn(15495, false, false)

		assert.Nil(t, err)
		conn.runBatches()
		assert.NotNil(t, conn.cmds[1].re)
		reply, err := redis.Int(c.Get().Do("GET", "b"))
		assert.Equal(t, err, redis.ErrNil)
		assert.Equal(t, reply, 0)
		conn.doRedirect()
		reply, err = redis.Int(c.Get().Do("GET", "b"))
		assert.Nil(t, err)
		assert.Equal(t, reply, 1)
	}
}

func testPipelineMget(t *testing.T, ports []string) {
	c := &Cluster{
		StartupNodes: []string{ports[0]},
	}
	defer c.Close()

	err := c.Refresh()
	if assert.NoError(t, err, "Refresh") {
		pipe := c.GetPipeline()
		assert.Nil(t, pipe.Send("MGET", "a", "b"))
		assert.Nil(t, pipe.Flush())
		reply, err := pipe.Receive()
		assert.NotNil(t, err)
		assert.True(t, IsCrossSlot(err))
		assert.Nil(t, reply)
	}
}
