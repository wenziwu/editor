package lsproto

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/rpc"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
)

//----------

// Implements rpc.ClientCodec
type JsonCodec struct {
	OnNotificationMessage func(*NotificationMessage)
	OnIOReadError         func(error) // callback that allows immediate action on err

	rwc           io.ReadWriteCloser
	responses     chan interface{}
	simulatedResp chan interface{}

	// used by read response header/body
	readData readData

	mu      sync.Mutex
	closing bool
}

func NewJsonCodec(rwc io.ReadWriteCloser) *JsonCodec {
	c := &JsonCodec{rwc: rwc}
	c.responses = make(chan interface{}, 1)
	c.simulatedResp = make(chan interface{}, 1)
	go c.readLoop()
	return c
}

//----------

func (c *JsonCodec) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
	return c.rwc.Close()
}

func (c *JsonCodec) isClosing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closing
}

//----------

func noreplyMethod(method string) (string, bool) {
	prefix := "noreply:"
	if strings.HasPrefix(method, prefix) {
		return method[len(prefix):], true
	}
	return method, false
}

//----------

func (c *JsonCodec) WriteRequest(req *rpc.Request, data interface{}) error {
	method := req.ServiceMethod

	// internal: methods with a noreply prefix don't expect a reply. This was done throught the method name to be able to use net/rpc. This is not part of the lsp protocol.
	noreply := false
	if m, ok := noreplyMethod(method); ok {
		noreply = true
		method = m
	}

	msg := &RequestMessage{
		JsonRpc: "2.0",
		Id:      int(req.Seq),
		Method:  method,
		Params:  data,
	}
	logPrintf("write req -->: %v(%v)", msg.Method, msg.Id)

	b, err := encodeJson(msg)
	if err != nil {
		return err
	}

	// build header+body
	h := fmt.Sprintf("Content-Length: %v\r\n\r\n", len(b))
	buf := make([]byte, len(h)+len(b))
	copy(buf, []byte(h))  // header
	copy(buf[len(h):], b) // body

	_, err = c.rwc.Write(buf)
	if err != nil {
		return err
	}

	// simulate a response (noreply) with the seq if there is no err writing the msg
	if noreply {
		// can't use c.responses or it could be writing to a closed channel
		c.simulatedResp <- req.Seq
	}
	return nil
}

//----------

func (c *JsonCodec) readLoop() {
	defer func() { close(c.responses) }()
	for {
		cl, err := c.readHeaders() // content-length header
		if err != nil {
			if c.isClosing() {
				break
			}
			logPrintf("read resp err <--:\n%v\n", err)
			c.responses <- err
			break
		}
		b, err := c.readContent(cl)
		if err != nil {
			if c.isClosing() {
				break
			}
			logPrintf("read resp err <--:\n%v\n", err)
			c.responses <- err
			break
		}

		logPrintf("read resp <--:\n%s\n", b)
		c.responses <- b
	}
}

//----------

func (c *JsonCodec) ReadResponseHeader(resp *rpc.Response) error {
	c.readData = readData{} // reset

	var v interface{}
	select {
	case u, ok := <-c.responses:
		if !ok {
			return fmt.Errorf("responses chan closed")
		}
		v = u
	case u, _ := <-c.simulatedResp:
		v = u
	}

	switch t := v.(type) {
	case error:
		// read error: there is no corresponding call
		// only way to make this known
		if c.OnIOReadError != nil {
			go c.OnIOReadError(t)
		}
		return t // nothing will handle this return
	case uint64: // request id that expects noreply
		c.readData = readData{noReply: true}
		resp.Seq = t
		return nil
	case []byte:
		// decode json
		lspResp := &Response{}
		rd := bytes.NewReader(t)
		if err := decodeJson(rd, lspResp); err != nil {
			c.readData = readData{noReply: true}
			// returning error breaks the client loop
			return fmt.Errorf("jsoncodec: decode: %v", err)
		}
		c.readData.lspResp = lspResp
		// msg id (needed for the rpc to run the reply to the caller)
		if lspResp.isServerPush() {
			// no seq, zero is first msg, need to use maxuint64
			resp.Seq = math.MaxInt64 // can't print math.MaxUint64 const
		} else {
			resp.Seq = uint64(lspResp.Id)
		}
		return nil
	default:
		panic("!")
	}
}

func (c *JsonCodec) ReadResponseBody(reply interface{}) error {
	// exhaust requests with noreply
	if c.readData.noReply {
		return nil
	}

	// server push callback (no id)
	if c.readData.lspResp.isServerPush() {
		if reply != nil {
			return fmt.Errorf("jsoncodec: server push with reply expecting data: %v", reply)
		}
		// run callback
		nm := c.readData.lspResp.NotificationMessage
		if c.OnNotificationMessage != nil {
			c.OnNotificationMessage(&nm)
		}
		return nil
	}

	// assign data
	if lspResp, ok := reply.(*Response); ok {
		*lspResp = *c.readData.lspResp
		return nil
	}

	// error
	if reply == nil {
		//return fmt.Errorf("jsoncodec: reply has no handler?")

		//err := fmt.Errorf("jsoncodec: reply has no handler?")
		//fmt.Fprintf(os.Stderr, "error: %v\n", err)

		// TODO: gopls: replies to some msgs that area not expecting a reply. Returning an error will stop the connection. Don't stop due to this (return nil).
		return nil
	}
	return fmt.Errorf("jsoncodec: reply data not assigned: %v", reply)
}

//----------

func (c *JsonCodec) readHeaders() (int, error) {
	var length int
	for {
		// read line
		var line string
		for {
			p := make([]byte, 1)
			_, err := c.rwc.Read(p)
			if err != nil {
				return 0, err
			}
			line += string(p)
			if p[0] == '\n' {
				break
			}
			if len(line) > 1024 {
				return 0, errors.New("header line too long")
			}
		}
		line = strings.TrimSpace(line)
		// header finished
		if line == "" {
			break
		}
		// header line
		colon := strings.IndexRune(line, ':')
		if colon < 0 {
			return 0, fmt.Errorf("invalid header line %q", line)
		}
		name := strings.ToLower(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		switch name {
		case "content-length":
			l, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				return 0, fmt.Errorf("failed parsing content-length: %v", value)
			}
			if l <= 0 {
				return 0, fmt.Errorf("invalid content-length: %v", l)
			}
			length = int(l)
		}
	}
	if length == 0 {
		return 0, fmt.Errorf("missing content-length")
	}
	return length, nil
}

func (c *JsonCodec) readContent(length int) ([]byte, error) {
	b := make([]byte, length)
	_, err := io.ReadFull(c.rwc, b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

//----------

type readData struct {
	noReply bool
	lspResp *Response
}

//----------
