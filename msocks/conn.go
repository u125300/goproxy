package msocks

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"
)

type DelayDo struct {
	lock  sync.Mutex
	delay time.Duration
	timer *time.Timer
	cnt   int
	do    func(int) error
}

func NewDelayDo(delay time.Duration, do func(int) error) (d *DelayDo) {
	d = &DelayDo{
		delay: delay,
		do:    do,
	}
	return
}

func (d *DelayDo) Add() {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.cnt += 1
	if d.cnt >= WIN_SIZE {
		d.do(d.cnt)
		if d.timer != nil {
			d.timer.Stop()
			d.timer = nil
		}
		d.cnt = 0
	}

	if d.cnt != 0 && d.timer == nil {
		d.timer = time.AfterFunc(d.delay, func() {
			d.lock.Lock()
			defer d.lock.Unlock()
			if d.cnt > 0 {
				d.do(d.cnt)
			}
			d.timer = nil
			d.cnt = 0
		})
	}
	return
}

type Pipe struct {
	Closed bool
	pr     *io.PipeReader
	pw     *io.PipeWriter
}

func NewPipe() (p *Pipe) {
	pr, pw := io.Pipe()
	p = &Pipe{pr: pr, pw: pw}
	return
}

func (p *Pipe) Read(data []byte) (n int, err error) {
	n, err = p.pr.Read(data)
	if err == io.ErrClosedPipe {
		err = io.EOF
	}
	return
}

func (p *Pipe) Write(data []byte) (n int, err error) {
	n, err = p.pw.Write(data)
	if err == io.ErrClosedPipe {
		err = io.EOF
	}
	return
}

func (p *Pipe) Close() (err error) {
	p.Closed = true
	p.pr.Close()
	p.pw.Close()
	return
}

type ChanFrameSender chan Frame

func NewChanFrameSender(i int) ChanFrameSender {
	return make(chan Frame, i)
}

func (c ChanFrameSender) RecvWithTimeout(t time.Duration) (f Frame) {
	ch_timeout := time.After(t)
	select {
	case f := <-c:
		return f
	case <-ch_timeout: // timeout
		return nil
	}
}

func (c ChanFrameSender) SendFrame(f Frame) (b bool) {
	defer func() { recover() }()
	select {
	case c <- f:
		return true
	default:
	}
	return
}

func (c ChanFrameSender) CloseSend() {
	defer func() { recover() }()
	close(c)
}

type Conn struct {
	Pipe
	ChanFrameSender
	sess       *Session
	streamid   uint16
	removefunc sync.Once
	dd         *DelayDo
	sw         *SeqWriter
}

func NewConn(streamid uint16, sess *Session) (c *Conn) {
	c = &Conn{
		Pipe:            *NewPipe(),
		ChanFrameSender: NewChanFrameSender(CHANLEN),
		streamid:        streamid,
		sess:            sess,
		dd:              NewDelayDo(ACKDELAY, nil),
		sw:              NewSeqWriter(sess),
	}
	c.dd.do = c.send_ack
	go c.Run()
	return
}

func (c *Conn) Run() {
	var err error
	for {
		f, ok := <-c.ChanFrameSender
		if !ok {
			c.CloseAll()
			return
		}

		switch ft := f.(type) {
		default:
			logger.Err("unexpected package")
			c.CloseAll()
			return
		case *FrameData:
			f.Debug()
			c.dd.Add()
			logger.Infof("%p(%d) recved %d bytes from remote.",
				c.sess, ft.Streamid, len(ft.Data))
			_, err = c.Pipe.Write(ft.Data)
			switch err {
			case io.EOF:
				logger.Errf("%p(%d) buf is closed.",
					c.sess, c.streamid)
				c.CloseAll()
				return
			case nil:
			default:
				logger.Errf("%p(%d) buf is full.",
					c.sess, c.streamid)
				c.CloseAll()
				return
			}
		case *FrameAck:
			f.Debug()
			n := c.sw.Release(ft.Window)
			logger.Debugf("remote readed %d, window size maybe: %d.",
				ft.Window, n)
		case *FrameFin:
			f.Debug()
			c.Pipe.Close()
			logger.Infof("connection %p(%d) closed from remote.",
				c.sess, c.streamid)
			if c.sw.Closed() {
				c.remove_port()
			}
			return
		}
	}
}

func (c *Conn) send_ack(n int) (err error) {
	logger.Debugf("%p(%d) send ack %d.", c.sess, c.streamid, n)
	// send readed bytes back

	err = c.sw.Ack(c.streamid, int32(n))
	if err != nil {
		logger.Err(err)
		c.Close()
	}
	return
}

func (c *Conn) Write(data []byte) (n int, err error) {
	for len(data) > 0 {
		size := uint32(len(data))
		// random size
		switch {
		case size > 8*1024:
			size = uint32(3*1024 + rand.Intn(1024))
		case 4*1024 < size && size <= 8*1024:
			size /= 2
		}

		err = c.sw.Data(c.streamid, data[:size])
		// write closed, so we don't care window too much.
		if err != nil {
			return
		}
		logger.Debugf("%p(%d) send chunk size %d at %d.",
			c.sess, c.streamid, size, n)

		data = data[size:]
		n += int(size)
	}
	logger.Infof("%p(%d) send size %d.", c.sess, c.streamid, n)
	return
}

func (c *Conn) remove_port() {
	c.removefunc.Do(func() {
		err := c.sess.RemovePorts(c.streamid)
		if err != nil {
			logger.Err(err)
		}
		c.ChanFrameSender.CloseSend()
	})
}

func (c *Conn) Close() (err error) {
	// make sure just one will enter this func
	err = c.sw.Close(c.streamid)
	if err == io.EOF {
		// ok for already closed
		err = nil
	}
	if err != nil {
		return err
	}

	logger.Infof("connection %p(%d) closing from local.", c.sess, c.streamid)

	if c.Pipe.Closed {
		c.remove_port()
	}
	return
}

func (c *Conn) CloseAll() {
	c.sw.Close(c.streamid)
	c.Pipe.Close()
	c.remove_port()
	logger.Infof("connection %p(%d) close all.", c.sess, c.streamid)
}

func (c *Conn) LocalAddr() net.Addr {
	return &Addr{
		c.sess.LocalAddr(),
		c.streamid,
	}
}

func (c *Conn) RemoteAddr() net.Addr {
	return &Addr{
		c.sess.RemoteAddr(),
		c.streamid,
	}
}

func (c *Conn) SetDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	return nil
}

type Addr struct {
	net.Addr
	streamid uint16
}

func (a *Addr) String() (s string) {
	return fmt.Sprintf("%s(%d)", a.Addr.String(), a.streamid)
}
