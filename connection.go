// Copyright (c) 2012, Sean Treadway, SoundCloud Ltd.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Source code and contact info at http://github.com/streadway/amqp

package amqp

import (
	"bufio"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Used in Open to specify the desired tuning parameters used during a
// connection open handshake.  The negotiated tuning will be stored in the
// resultant connection.
type Config struct {
	// The SASL mechanisms to try in the client request, and the successful
	// mechanism used on the Connection object
	SASL []Authentication

	Vhost string

	Channels  int           // 0 max channels means unlimited
	FrameSize int           // 0 max bytes means unlimited
	Heartbeat time.Duration // less than 1s interval means no heartbeats
}

// Manages the serialization and deserialization of frames from IO and
// dispatches the frames to the appropriate channel.
type Connection struct {
	destructor sync.Once
	m          sync.Mutex // writer and notify mutex

	conn io.ReadWriteCloser

	rpc    chan message
	writer *writer
	sends  chan time.Time // timestamps of each frame sent

	sequence uint32
	channels map[uint16]*Channel

	closes []chan *Error

	errors chan *Error

	Config Config // The negotiated Config after connection.open

	Major int // Server's major version
	Minor int // Server's minor version

	Properties Table // Server properties
}

type readDeadliner interface {
	SetReadDeadline(time.Time) error
}

// Dial accepts a string in the AMQP URI format, and returns a new Connection
// over TCP using PlainAuth.  Defaults to a server heartbeat interval of 10
// seconds and sets the initial read deadline to 30 seconds.
func Dial(amqp string) (*Connection, error) {
	uri, err := ParseURI(amqp)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(uri.Host, strconv.FormatInt(int64(uri.Port), 10))

	conn, err := net.DialTimeout("tcp", addr, 30*time.Second)
	if err != nil {
		return nil, err
	}

	// Heartbeating hasn't started yet, don't stall forever on a dead server.
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, err
	}

	return Open(conn, Config{
		SASL:      []Authentication{uri.PlainAuth()},
		Vhost:     uri.Vhost,
		Heartbeat: 10 * time.Second,
	})
}

func Open(conn io.ReadWriteCloser, config Config) (*Connection, error) {
	me := &Connection{
		conn:     conn,
		writer:   &writer{bufio.NewWriter(conn)},
		rpc:      make(chan message),
		channels: make(map[uint16]*Channel),
		sends:    make(chan time.Time),
		errors:   make(chan *Error, 1),
	}
	go me.reader()
	return me, me.open(config)
}

func (me *Connection) nextChannelId() uint16 {
	return uint16(atomic.AddUint32(&me.sequence, 1))
}

// Listens for close events either initiated by an error accompaning a
// connection.close method or by a normal shutdown.
//
// On normal shutdowns, the chan will be closed.
func (me *Connection) NotifyClose(c chan *Error) chan *Error {
	me.m.Lock()
	defer me.m.Unlock()
	me.closes = append(me.closes, c)
	return c
}

/*
Requests, and waits for the response to close the AMQP connection.

It's advisable to use this message when publishing so to make sure that all
kernel buffers have been flushed before exiting.

An error indicates that server may not have received this request to close but
the connection should be treated as closed regardless.

After returning from this call, all resources associated with this connection,
including the underlying io, Channels, Notify listeners and Channel consumers
will also be closed.
*/
func (me *Connection) Close() error {
	defer me.shutdown(nil)
	return me.call(
		&connectionClose{
			ReplyCode: replySuccess,
			ReplyText: "kthxbai",
		},
		&connectionCloseOk{},
	)
}

func (me *Connection) closeWith(err *Error) error {
	defer me.shutdown(err)
	return me.call(
		&connectionClose{
			ReplyCode: uint16(err.Code),
			ReplyText: err.Reason,
		},
		&connectionCloseOk{},
	)
}

func (me *Connection) send(f frame) error {
	me.m.Lock()
	err := me.writer.WriteFrame(f)
	me.m.Unlock()

	if err != nil {
		// Assuming the connection is dead, and closeWith would be re-entrant so
		// shutdown all the things
		me.shutdown(&Error{
			Code:   FrameError,
			Reason: err.Error(),
		})
	} else {
		// Broadcast we sent a frame, reducing heartbeats, only
		// if there is something that can receive - like a non-reentrant
		// call or if the heartbeater isn't running
		select {
		case me.sends <- time.Now():
		default:
		}
	}

	return err
}

func (me *Connection) shutdown(err *Error) {
	me.destructor.Do(func() {
		if err != nil {
			for _, c := range me.closes {
				c <- err
			}
		}

		for id, c := range me.channels {
			delete(me.channels, id)
			c.shutdown(err)
		}

		if err != nil {
			me.errors <- err
		}

		close(me.sends)
		me.sends = nil

		me.conn.Close()

		for _, c := range me.closes {
			close(c)
		}
	})
}

// All methods sent to the connection channel should be synchronous so we
// can handle them directly without a framing component
func (me *Connection) demux(f frame) {
	if f.channel() == 0 {
		me.dispatch0(f)
	} else {
		me.dispatchN(f)
	}
}

func (me *Connection) dispatch0(f frame) {
	switch mf := f.(type) {
	case *methodFrame:
		switch m := mf.Method.(type) {
		case *connectionClose:
			// Send immediately as shutdown will close our side of the writer.
			me.send(&methodFrame{
				ChannelId: 0,
				Method:    &connectionCloseOk{},
			})

			me.shutdown(newError(m.ReplyCode, m.ReplyText))
		default:
			me.rpc <- m
		}
	case *heartbeatFrame:
		// kthx - all reads reset our deadline.  so we can drop this
	default:
		// lolwat - channel0 only responds to methods and heartbeats
		me.closeWith(ErrUnexpectedFrame)
	}
}

func (me *Connection) dispatchN(f frame) {
	if channel, ok := me.channels[f.channel()]; ok {
		channel.recv(channel, f)
	} else {
		me.dispatchClosed(f)
	}
}

// section 2.3.7: "When a peer decides to close a channel or connection, it
// sends a Close method.  The receiving peer MUST respond to a Close with a
// Close-Ok, and then both parties can close their channel or connection.  Note
// that if peers ignore Close, deadlock can happen when both peers send Close
// at the same time."
//
// When we don't have a channel, so we must respond with close-ok on a close
// method.  This can happen between a channel exception on an asynchronous
// method like basic.publish and a synchronous close with channel.close.
// In that case, we'll get both a channel.close and channel.close-ok in any
// order.
func (me *Connection) dispatchClosed(f frame) {
	// Only consider method frames, drop content/header frames
	if mf, ok := f.(*methodFrame); ok {
		switch mf.Method.(type) {
		case *channelClose:
			me.send(&methodFrame{
				ChannelId: f.channel(),
				Method:    &channelCloseOk{},
			})
		case *channelCloseOk:
			// we are already closed, so do nothing
		default:
			// unexpected method on closed channel
			me.closeWith(ErrClosed)
		}
	}
}

// Reads each frame off the IO and hand off to the connection object that
// will demux the streams and dispatch to one of the opened channels or
// handle on channel 0 (the connection channel).
func (me *Connection) reader() {
	buf := bufio.NewReader(me.conn)
	frames := &reader{buf}

	for {
		frame, err := frames.ReadFrame()

		if err != nil {
			me.shutdown(&Error{Code: FrameError, Reason: err.Error()})
			return
		}

		me.demux(frame)

		// Reset the blocking read deadline on the underlying connection when it
		// implements SetReadDeadline to three times the requested heartbeat interval.
		// On error, resort to blocking reads.
		if me.Config.Heartbeat > 0 {
			if c, ok := me.conn.(readDeadliner); ok {
				c.SetReadDeadline(time.Now().Add(3 * me.Config.Heartbeat))
			}
		}
	}
}

// Ensures that at least one frame is being sent at the tuned interval with a
// jitter tolerance of 1s
func (me *Connection) heartbeater(interval time.Duration) {
	last := time.Now()
	tick := time.Tick(interval)

	for {
		select {
		case at := <-tick:
			if at.Sub(last) > interval-time.Second {
				if err := me.send(&heartbeatFrame{}); err != nil {
					// send heartbeats even after close/closeOk so we
					// tick until the connection starts erroring
					return
				}
			}
		case at, open := <-me.sends:
			if open {
				last = at
			} else {
				return
			}
		}
	}
}

// Convienence method to inspect the Connection.Properties["capabilities"]
// Table for server identified capabilities like "basic.ack" or
// "confirm.select".
func (me *Connection) isCapable(featureName string) bool {
	if me.Properties != nil {
		if v, ok := me.Properties["capabilities"]; ok {
			if capabilities, ok := v.(Table); ok {
				if feature, ok := capabilities[featureName]; ok {
					if has, ok := feature.(bool); ok && has {
						return true
					}
				}
			}
		}
	}
	return false
}

// Constructs and opens a unique channel for concurrent operations
func (me *Connection) Channel() (*Channel, error) {
	id := me.nextChannelId()
	channel := newChannel(me, id)
	me.channels[id] = channel
	return channel, channel.open()
}

func (me *Connection) call(req message, res ...message) error {
	// Special case for when the protocol header frame is sent insted of a
	// request method
	if req != nil {
		if err := me.send(&methodFrame{ChannelId: 0, Method: req}); err != nil {
			return err
		}
	}

	select {
	case err := <-me.errors:
		return err

	case msg := <-me.rpc:
		// Try to match one of the result types
		for _, try := range res {
			if reflect.TypeOf(msg) == reflect.TypeOf(try) {
				// *res = *msg
				vres := reflect.ValueOf(try).Elem()
				vmsg := reflect.ValueOf(msg).Elem()
				vres.Set(vmsg)
				return nil
			}
		}
		return ErrCommandInvalid
	}

	panic("unreachable")
}

//    Connection          = open-Connection *use-Connection close-Connection
//    open-Connection     = C:protocol-header
//                          S:START C:START-OK
//                          *challenge
//                          S:TUNE C:TUNE-OK
//                          C:OPEN S:OPEN-OK
//    challenge           = S:SECURE C:SECURE-OK
//    use-Connection      = *channel
//    close-Connection    = C:CLOSE S:CLOSE-OK
//                        / S:CLOSE C:CLOSE-OK
func (me *Connection) open(config Config) error {
	if err := me.send(&protocolHeader{}); err != nil {
		return err
	}

	return me.openStart(config)
}

func (me *Connection) openStart(config Config) error {
	start := &connectionStart{}

	if err := me.call(nil, start); err != nil {
		return err
	}

	me.Major = int(start.VersionMajor)
	me.Minor = int(start.VersionMinor)
	me.Properties = Table(start.ServerProperties)

	// eventually support challenge/response here by also responding to
	// connectionSecure.
	auth, ok := pickSASLMechanism(config.SASL, strings.Split(start.Mechanisms, " "))
	if !ok {
		return ErrSASL
	}

	// Save this mechanism off as the one we chose
	me.Config.SASL = []Authentication{auth}

	return me.openTune(config, auth)
}

func (me *Connection) openTune(config Config, auth Authentication) error {
	ok := &connectionStartOk{
		Mechanism: auth.Mechanism(),
		Response:  auth.Response(),
	}
	tune := &connectionTune{}

	if err := me.call(ok, tune); err != nil {
		// per spec, a connection can only be closed when it has been opened
		// so at this point, we know it's an auth error, but the socket
		// was closed instead.  Return a meaningful error.
		return ErrCredentials
	}

	// When this is bounded, share the bound.  We're effectively only bounded
	// by MaxUint16.  If you hit a wrap around bug, throw a small party then
	// make an github issue.
	me.Config.Channels = pick(config.Channels, int(tune.ChannelMax))

	// Frame size includes headers and end byte (len(payload)+8), even if
	// this is less than FrameMinSize, use what the server sends because the
	// alternative is to stop the handshake here.
	me.Config.FrameSize = pick(config.FrameSize, int(tune.FrameMax))

	// Save this off for resetDeadline()
	me.Config.Heartbeat = time.Second * time.Duration(pick(
		int(config.Heartbeat/time.Second),
		int(tune.Heartbeat)))

	// "The client should start sending heartbeats after receiving a
	// Connection.Tune method"
	if me.Config.Heartbeat > 0 {
		go me.heartbeater(me.Config.Heartbeat)
	}

	if err := me.send(&methodFrame{
		ChannelId: 0,
		Method: &connectionTuneOk{
			ChannelMax: uint16(me.Config.Channels),
			FrameMax:   uint32(me.Config.FrameSize),
			Heartbeat:  uint16(me.Config.Heartbeat / time.Second),
		},
	}); err != nil {
		return err
	}

	return me.openVhost(config)
}

func (me *Connection) openVhost(config Config) error {
	req := &connectionOpen{VirtualHost: config.Vhost}
	res := &connectionOpenOk{}

	if err := me.call(req, res); err != nil {
		// Cannot be closed yet, but we know it's a vhost problem
		return ErrVhost
	}

	me.Config.Vhost = config.Vhost

	return nil
}

func pick(client, server int) int {
	if client == 0 || server == 0 {
		// max
		if client > server {
			return client
		} else {
			return server
		}
	} else {
		// min
		if client > server {
			return server
		} else {
			return client
		}
	}
	panic("unreachable")
}
