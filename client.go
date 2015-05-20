package apns

import (
	"container/list"
	"crypto/tls"
	"io"
	"log"
	"sync"
	"time"
)

type buffer struct {
	size int
	*list.List
}

func newBuffer(size int) *buffer {
	return &buffer{size, list.New()}
}

func (b *buffer) Add(v interface{}) *list.Element {
	e := b.PushBack(v)

	if b.Len() > b.size {
		b.Remove(b.Front())
	}

	return e
}

type Client struct {
	Conn         *Conn
	FailedNotifs chan NotificationResult

	sent   *buffer
	cursor *list.Element
	errs   chan error
	id     uint32

	// concurrency
	reconnecting bool
	cond         *sync.Cond
}

func newClientWithConn(gw string, conn Conn) Client {
	sent := newBuffer(50)
	errs := make(chan error)
	c := Client{
		Conn:         &conn,
		FailedNotifs: make(chan NotificationResult),
		sent:         sent,
		cursor:       sent.Front(),
		errs:         errs,
		id:           uint32(1),
		cond:         sync.NewCond(&sync.Mutex{}),
	}

	c.restart()

	return c
}

func NewClientWithCert(gw string, cert tls.Certificate) Client {
	conn := NewConnWithCert(gw, cert)

	return newClientWithConn(gw, conn)
}

func NewClient(gw string, cert string, key string) (Client, error) {
	conn, err := NewConn(gw, cert, key)
	if err != nil {
		return Client{}, err
	}

	return newClientWithConn(gw, conn), nil
}

func NewClientWithFiles(gw string, certFile string, keyFile string) (Client, error) {
	conn, err := NewConnWithFiles(gw, certFile, keyFile)
	if err != nil {
		return Client{}, err
	}

	return newClientWithConn(gw, conn), nil
}

func (c *Client) restart() {
	c.reconnect()
	go c.readErrs()
	c.requeue(c.cursor)
}

func (c *Client) Send(n Notification) (err error) {
	defer func() {
		if err != nil {
			c.restart()
		}
	}()

	c.cond.L.Lock()
	for c.reconnecting {
		c.cond.Wait()
	}
	c.cond.L.Unlock()

	// Check for errors. There is a chance we'll send notifications
	// if we already have an error since `select` will "pseudorandomly" choose a
	// ready channels. It turns out to be fine because the connection will already
	// be closed and it'll requeue. We could check before we get to this select
	// block, but it doesn't seem worth the extra code and complexity.
	select {
	case err = <-c.errs:
	default:
	}

	// If there is an error we understand, find the notification that failed,
	// move the cursor right after it.
	if nErr, ok := err.(*Error); ok {
		c.cursor = c.handleError(nErr)
		return
	}

	if err != nil {
		return
	}

	// Add to list
	c.cursor = c.sent.Add(n)

	// Set identifier if not specified
	if n.Identifier == 0 {
		n.Identifier = c.id
		c.id++
	} else if c.id < n.Identifier {
		c.id = n.Identifier + 1
	}

	var b []byte
	b, err = n.ToBinary()
	if err != nil {
		log.Println("Could not convert APNS notification to binary:", err)
		return nil
	}

	_, err = c.Conn.Write(b)

	if err == io.EOF {
		log.Println("EOF trying to write notification")
		return
	}

	if err != nil {
		log.Println("err writing to apns", err.Error())
		return
	}

	c.cursor = c.cursor.Next()
	return
}

func (c *Client) reportFailedPush(v interface{}, err *Error) {
	failedNotif, ok := v.(Notification)
	if !ok || v == nil {
		return
	}

	select {
	case c.FailedNotifs <- NotificationResult{Notif: failedNotif, Err: *err}:
	default:
	}
}

func (c *Client) requeue(cursor *list.Element) {
	// If `cursor` is not nil, this means there are notifications that
	// need to be delivered (or redelivered)
	for ; cursor != nil; cursor = cursor.Next() {
		if n, ok := cursor.Value.(Notification); ok {
			go c.Send(n)
		}
	}
}

func (c *Client) handleError(err *Error) *list.Element {
	cursor := c.sent.Back()

	for cursor != nil {
		// Get notification
		n, _ := cursor.Value.(Notification)

		// If the notification, move cursor after the trouble notification
		if n.Identifier == err.Identifier {
			go c.reportFailedPush(cursor.Value, err)

			next := cursor.Next()

			c.sent.Remove(cursor)
			return next
		}

		cursor = cursor.Prev()
	}

	return cursor
}

func (c *Client) readErrs() {
	p := make([]byte, 6, 6)
	_, err := c.Conn.Read(p)
	if err != nil {
		c.errs <- err
		return
	}

	e := NewError(p)
	c.errs <- &e
}

func (c *Client) reconnect() {
	var cont bool
	c.cond.L.Lock()
	if c.reconnecting {
		cont = false
	} else {
		c.reconnecting = true
		cont = true
	}
	c.cond.L.Unlock()

	if !cont {
		return
	}

	for {
		if err := c.Conn.Connect(); err != nil {
			// TODO Probably want to exponentially backoff...
			log.Println("Error connecting to APNS, will retry:", err)
			time.Sleep(time.Second)
			continue
		}
		break
	}

	c.cond.L.Lock()
	c.reconnecting = false
	c.cond.L.Unlock()
	c.cond.Broadcast()
}
