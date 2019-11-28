// Copyright 2015 Joel Wu
// Copyright 2012 Gary Burd
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package redis

import (
	"fmt"
	"net"
	"sync"
	"time"
	"bufio"
	"errors"
	"strconv"
	"container/list"
	"io"
)

type redisConn struct {
	c net.Conn
	t time.Time

	br *bufio.Reader
	bw *bufio.Writer

	readTimeout  time.Duration
	writeTimeout time.Duration

	// Pending replies to be read in redis pipeling.
	pending int

	// Scratch space for formatting argument length.
	lenScratch [32]byte

	// Scratch space for formatting integer and float.
	numScratch [40]byte
}

type redisNode struct {
	address string

	conns     list.List
	keepAlive int
	aliveTime time.Duration

	connTimeout  time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration

	mutex sync.Mutex

	updateTime time.Time

	closed bool

	password string
}

func (node *redisNode) getConn() (*redisConn, error) {
	node.mutex.Lock()

	if node.closed {
		node.mutex.Unlock()
		return nil, fmt.Errorf("getConn: connection has been closed")
	}

	// remove stale connections
	if node.connTimeout > 0 {
		for {
			elem := node.conns.Back()
			if elem == nil {
				break
			}
			conn := elem.Value.(*redisConn)
			if conn.t.Add(node.aliveTime).After(time.Now()) {
				break
			}
			node.conns.Remove(elem)
		}
	}

	if node.conns.Len() <= 0 {
		node.mutex.Unlock()

		c, err := net.DialTimeout("tcp", node.address, node.connTimeout)
		if err != nil {
			return nil, err
		}

		conn := &redisConn{
			c:            c,
			br:           bufio.NewReader(c),
			bw:           bufio.NewWriter(c),
			readTimeout:  node.readTimeout,
			writeTimeout: node.writeTimeout,
		}

		if node.password != "" {
			err = conn.auth(node.password)
			if err != nil {
				return nil, err
			}
		}

		return conn, nil
	}

	elem := node.conns.Back()
	node.conns.Remove(elem)
	node.mutex.Unlock()

	return elem.Value.(*redisConn), nil
}

func (node *redisNode) releaseConn(conn *redisConn) {
	node.mutex.Lock()
	defer node.mutex.Unlock()

	// Connection still has pending replies, just close it.
	if conn.pending > 0 || node.closed {
		conn.shutdown()
		return
	}

	if node.conns.Len() >= node.keepAlive || node.aliveTime <= 0 {
		conn.shutdown()
		return
	}

	conn.t = time.Now()
	node.conns.PushFront(conn)
}

func (conn *redisConn) auth(password string) (err error) {
	if err = conn.send("AUTH", password); err != nil {
		conn.shutdown()
		return
	}

	if err = conn.flush(); err != nil {
		conn.shutdown()
		return
	}

	_, err = conn.receive()
	if err != nil {
		conn.shutdown()
		return
	}

	return nil
}

func (conn *redisConn) shutdown() {
	conn.c.Close()
}

func (node *redisNode) shutdown() {
	node.mutex.Lock()
	defer node.mutex.Unlock()

	for {
		elem := node.conns.Back()
		if elem == nil {
			break
		}

		conn := elem.Value.(*redisConn)
		conn.c.Close()
		node.conns.Remove(elem)
	}

	node.closed = true
}

func (conn *redisConn) send(cmd string, args ...interface{}) error {
	conn.pending += 1

	if conn.writeTimeout > 0 {
		conn.c.SetWriteDeadline(time.Now().Add(conn.writeTimeout))
	}

	if err := conn.writeCommand(cmd, args); err != nil {
		return err
	}

	return nil
}

func (conn *redisConn) flush() error {
	if conn.writeTimeout > 0 {
		conn.c.SetWriteDeadline(time.Now().Add(conn.writeTimeout))
	}

	if err := conn.bw.Flush(); err != nil {
		return err
	}

	return nil
}

func (conn *redisConn) receive() (interface{}, error) {
	if conn.readTimeout > 0 {
		conn.c.SetWriteDeadline(time.Now().Add(conn.readTimeout))
	}

	if conn.pending <= 0 {
		return nil, errors.New("no more pending reply")
	}

	conn.pending -= 1

	return conn.readReply()
}

func (node *redisNode) do(cmd string, args ...interface{}) (interface{}, error) {
	conn, err := node.getConn()
	if err != nil {
		return fmt.Sprintf("ECONNTIMEOUT: %v", err), nil
	}

	if err = conn.send(cmd, args...); err != nil {
		conn.shutdown()
		return nil, err
	}

	if err = conn.flush(); err != nil {
		conn.shutdown()
		return nil, err
	}

	reply, err := conn.receive()
	if err != nil {
		conn.shutdown()
		return nil, err
	}

	node.releaseConn(conn)

	return reply, err
}

func (conn *redisConn) writeLen(prefix byte, n int) error {
	conn.lenScratch[len(conn.lenScratch)-1] = '\n'
	conn.lenScratch[len(conn.lenScratch)-2] = '\r'
	i := len(conn.lenScratch) - 3

	for {
		conn.lenScratch[i] = byte('0' + n%10)
		i -= 1
		n = n / 10
		if n == 0 {
			break
		}
	}

	conn.lenScratch[i] = prefix
	_, err := conn.bw.Write(conn.lenScratch[i:])

	return err
}

func (conn *redisConn) writeString(s string) error {
	conn.writeLen('$', len(s))
	conn.bw.WriteString(s)
	_, err := conn.bw.WriteString("\r\n")

	return err
}

func (conn *redisConn) writeBytes(p []byte) error {
	conn.writeLen('$', len(p))
	conn.bw.Write(p)
	_, err := conn.bw.WriteString("\r\n")

	return err
}

func (conn *redisConn) writeInt64(n int64) error {
	return conn.writeBytes(strconv.AppendInt(conn.numScratch[:0], n, 10))
}

func (conn *redisConn) writeUInt64(n uint64) error {
	return conn.writeBytes(strconv.AppendUint(conn.numScratch[:0], n, 10))
}

func (conn *redisConn) writeFloat64(n float64) error {
	return conn.writeBytes(strconv.AppendFloat(conn.numScratch[:0], n, 'g', -1, 64))
}

// Args must be int64, float64, string, []byte, other types are not supported for safe reason.
func (conn *redisConn) writeCommand(cmd string, args []interface{}) error {
	conn.writeLen('*', len(args)+1)
	err := conn.writeString(cmd)

	for _, arg := range args {
		if err != nil {
			break
		}
		switch arg := arg.(type) {
		case int32:
			err = conn.writeInt64(int64(arg))
		case int:
			err = conn.writeInt64(int64(arg))
		case int64:
			err = conn.writeInt64(arg)
		case uint32:
			err = conn.writeUInt64(uint64(arg))
		case uint:
			err = conn.writeUInt64(uint64(arg))
		case uint64:
			err = conn.writeUInt64(arg)
		case float64:
			err = conn.writeFloat64(arg)
		case string:
			err = conn.writeString(arg)
		case []byte:
			err = conn.writeBytes(arg)
		default:
			err = fmt.Errorf("unknown type %T", arg)
		}
	}

	return err
}

// readLine read a single line terminated with CRLF.
func (conn *redisConn) readLine() ([]byte, error) {
	var line []byte
	for {
		p, err := conn.br.ReadBytes('\n')
		if err != nil {
			return nil, err
		}

		n := len(p) - 2
		if n < 0 {
			return nil, fmt.Errorf("invalid response: readLine data illegal: %v", p)
		}

		// bulk string may contain '\n', such as CLUSTER NODES
		if p[n] != '\r' {
			if line != nil {
				line = append(line, p[:]...)
			} else {
				line = p
			}
			continue
		}

		if line != nil {
			return append(line, p[:n]...), nil
		} else {
			return p[:n], nil
		}
	}
}

// parseLen parses bulk string and array length.
func parseLen(p []byte) (int, error) {
	if len(p) == 0 {
		return -1, errors.New("invalid response: parseLen == 0")
	}

	// null element.
	if p[0] == '-' && len(p) == 2 && p[1] == '1' {
		return -1, nil
	}

	var n int
	for _, b := range p {
		n *= 10
		if b < '0' || b > '9' {
			return -1, fmt.Errorf("invalid response: parseLen: parse character[%c] failed, data: %v", b, p)
		}
		n += int(b - '0')
	}

	return n, nil
}

// parseInt parses an integer reply.
func parseInt(p []byte) (int64, error) {
	if len(p) == 0 {
		return 0, fmt.Errorf("invalid response: parse int failed[length == 0]")
	}

	var negate bool
	if p[0] == '-' {
		negate = true
		p = p[1:]
		if len(p) == 0 {
			return 0, fmt.Errorf("invalid response: parse int failed[p: %v]", p)
		}
	}

	var n int64
	for _, b := range p {
		n *= 10
		if b < '0' || b > '9' {
			return 0, errors.New("invalid response: parseInt: character[%c] failed, data: %v")
		}
		n += int64(b - '0')
	}

	if negate {
		n = -n
	}

	return n, nil
}

var (
	okReply   interface{} = "OK"
	pongReply interface{} = "PONG"
)

type redisError string

func (err redisError) Error() string { return string(err) }

func (conn *redisConn) readReply() (interface{}, error) {
	line, err := conn.readLine()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, errors.New("invalid response: return size is 0")
	}

	switch line[0] {
	case '+':
		switch {
		case len(line) == 3 && line[1] == 'O' && line[2] == 'K':
			// Avoid allocation for frequent "+OK" response.
			return okReply, nil
		case len(line) == 5 && line[1] == 'P' && line[2] == 'O' && line[3] == 'N' && line[4] == 'G':
			// Avoid allocation in PING command benchmarks :)
			return pongReply, nil
		default:
			return string(line[1:]), nil
		}
	case '-':
		return nil, redisError(string(line[1:]))
	case ':':
		return parseInt(line[1:])
	case '$':
		n, err := parseLen(line[1:])
		if n == -1 {
			// -1 is legal
			return []byte{}, nil
		} else if n < -1 || err != nil {
            return nil, fmt.Errorf("parse length failed: %v, line[0]: [%v], length: [%v]", err, rune(line[0]), n)
		}

		/*
		 * Bugfix: see https://github.com/alibaba/RedisFullCheck/issues/73.
		 * This may include bug when '\r\n' occurs in the data.
		 * line, err = conn.readLine()
		 *
		 * if err != nil {
		 * 	   return nil, fmt.Errorf("read length failed: %v, line[0]: %v", err, line[0])
		 * }
		 * if len(line) != n {
		 * 	   return nil, fmt.Errorf("invalid response: line length[%v] != n[%v]", len(line), n)
		 * }
		 *
		 * return line, nil
		 */

		buf := make([]byte, n + 2)
		x, err := io.ReadFull(conn.br, buf)
		if err != nil {
			return nil, err
		}

		if x < n || buf[n] != '\r' || buf[n + 1] != '\n' {
			return nil, fmt.Errorf("invalid response: length[%v] != n[%v] or suffix != \r\n, line: %v",
				len(buf), n, buf)
		}

		return buf[:n], nil
	case '*':
		n, err := parseLen(line[1:])
		if n < 0 || err != nil {
			return nil, fmt.Errorf("parse length failed: %v, line[0]: %v", err, line[0])
		}

		r := make([]interface{}, n)
		for i := range r {
			r[i], err = conn.readReply()
			if err != nil {
				return nil, fmt.Errorf("read reply failed: %v, line[0]: %v", err, line[0])
			}
		}

		return r, nil
	}

	return nil, fmt.Errorf("invalid response: line[0]: %v", line[0])
}
