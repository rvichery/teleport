/*
Copyright 2017 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/gravitational/trace"
)

// NewCloserConn returns new connection wrapper that
// when closed will also close passed closers
func NewCloserConn(conn net.Conn, closers ...io.Closer) *CloserConn {
	return &CloserConn{
		Conn:    conn,
		closers: closers,
	}
}

// CloserConn wraps connection and attaches additional closers to it
type CloserConn struct {
	net.Conn
	closers []io.Closer
}

// AddCloser adds any closer in ctx that will be called
// whenever server closes session channel
func (c *CloserConn) AddCloser(closer io.Closer) {
	c.closers = append(c.closers, closer)
}

func (c *CloserConn) Close() error {
	var errors []error
	for _, closer := range c.closers {
		errors = append(errors, closer.Close())
	}
	errors = append(errors, c.Conn.Close())
	return trace.NewAggregate(errors...)
}

// Roundtrip is a single connection simplistic HTTP client
// that allows us to bypass a connection pool to test load balancing
// used in tests, as it only supports GET request on /
func Roundtrip(addr string) (string, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return RoundtripWithConn(conn)
}

// RoundtripWithConn uses HTTP GET on the existing connection,
// used in tests as it only performs GET request on /
func RoundtripWithConn(conn net.Conn) (string, error) {
	_, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	if err != nil {
		return "", err
	}

	re, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return "", err
	}
	defer re.Body.Close()
	out, err := ioutil.ReadAll(re.Body)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// StatConn is a net.Conn that keeps track of how much data was transmitted
// (TX) and received (RX) over the net.Conn. A maximum of about 18446
// petabytes can be kept track of for TX and RX before it rolls over.
// See https://golang.org/ref/spec#Numeric_types for more details.
type StatConn struct {
	conn net.Conn

	txBytes uint64
	rxBytes uint64
}

func NewStatConn(conn net.Conn) *StatConn {
	return &StatConn{
		conn: conn,
	}
}

// Stat returns the transmitted (TX) and received (RX) bytes over the net.Conn.
func (s *StatConn) Stat() (uint64, uint64) {
	return s.txBytes, s.rxBytes
}

func (s *StatConn) Read(b []byte) (n int, err error) {
	s.rxBytes = s.rxBytes + uint64(len(b))
	return s.conn.Read(b)
}

func (s *StatConn) Write(b []byte) (n int, err error) {
	s.txBytes = s.txBytes + uint64(len(b))
	return s.conn.Write(b)
}

func (s *StatConn) Close() error {
	return s.conn.Close()
}

func (s *StatConn) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *StatConn) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

func (s *StatConn) SetDeadline(t time.Time) error {
	return s.conn.SetDeadline(t)
}

func (s *StatConn) SetReadDeadline(t time.Time) error {
	return s.conn.SetReadDeadline(t)
}

func (s *StatConn) SetWriteDeadline(t time.Time) error {
	return s.conn.SetWriteDeadline(t)
}
