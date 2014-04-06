// Copyright 2014 Garrett D'Amore
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License.
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sp

import (
	"sync"
)

// inproc implements the Pipe interface on top of channels.
type inproc struct {
	rq     chan *Message
	wq     chan *Message
	closeq chan struct{}
	readyq chan struct{}
	proto  uint16
	addr   string
	peer   *inproc
}

type inprocTran struct{}

var inprocServers struct {
	// Who is listening, on which "address"?
	rendezvous map[string]*inprocRendezvous
	sync.Mutex
}

func init() {
	inprocServers.rendezvous = make(map[string]*inprocRendezvous)
}

func (p *inproc) Recv() (*Message, error) {

	if p.peer == nil {
		return nil, ErrClosed
	}
	select {
	case msg, ok := <-p.rq:
		if msg == nil || !ok {
			return nil, ErrClosed
		}
		// Upper protocols expect to have to pick header and
		// body part.  So mush them back together.
		//msg.Body = append(msg.Header, msg.Body...)
		//msg.Header = make([]byte, 0, 32)
		return msg, nil
	case <-p.closeq:
		return nil, ErrClosed
	case <-p.peer.closeq:
		return nil, ErrClosed
	}
}

func (p *inproc) Send(msg *Message) error {

	if p.peer == nil {
		return ErrClosed
	}

	// Upper protocols expect to have to pick header and body part.
	// Also we need to have a fresh copy of the message for receiver, to
	// break ownership.
	nmsg := NewMessage(len(msg.Header) + len(msg.Body))
	nmsg.Body = append(nmsg.Body, msg.Header...)
	nmsg.Body = append(nmsg.Body, msg.Body...)
	select {
	case p.wq <- nmsg:
		return nil
	case <-p.closeq:
		nmsg.Free()
		return ErrClosed
	case <-p.peer.closeq:
		nmsg.Free()
		return ErrClosed
	}
}

func (p *inproc) LocalProtocol() uint16 {
	return p.proto
}

func (p *inproc) RemoteProtocol() uint16 {
	if p.peer != nil {
		return p.peer.proto
	}
	return 0
}

func (p *inproc) Close() error {
	close(p.closeq)
	return nil
}

func (p *inproc) IsOpen() bool {
	select {
	case <-p.closeq:
		return false
	default:
		return true
	}
}

type inprocRendezvous struct {
	sync.Mutex
	addr       string
	proto      uint16
	servers    chan *inproc
	clients    chan *inproc
	closeq     chan interface{}
	server     *inproc // pending inproc
	client     *inproc // pending inproc
	processing bool    // true if a listener is listening (exclusion)
}

type inprocDialer struct {
	addr  string
	proto uint16
}

func inprocGetRendezvous(addr string, proto uint16, server bool) *inprocRendezvous {
	var r *inprocRendezvous
	var ok bool
	inprocServers.Lock()
	defer inprocServers.Unlock()
	if r, ok = inprocServers.rendezvous[addr]; r == nil || !ok {
		r = &inprocRendezvous{addr: addr, proto: proto}
		r.servers = make(chan *inproc, 1)
		r.clients = make(chan *inproc, 1)
		inprocServers.rendezvous[addr] = r
	}
	if server {
		if r.processing {
			// Server is already "processing" (Listen called)
			return nil
		}
		r.closeq = make(chan interface{})
		r.processing = true
		go r.rendezvous()
	} else if !r.processing {
		// Translates to Connection Refused
		return nil
	}
	return r
}

func (d *inprocDialer) Dial() (Pipe, error) {
	var r *inprocRendezvous
	if r = inprocGetRendezvous(d.addr, d.proto, false); r == nil {
		return nil, ErrConnRefused
	}

	client := &inproc{proto: r.proto, addr: r.addr}
	client.rq = make(chan *Message)
	client.wq = make(chan *Message)
	client.readyq = make(chan struct{})
	client.closeq = make(chan struct{})

	// submit this client to the rendezvous
	select {
	case r.clients <- client:
	}
	// wait for rendezvous to tell us we're ready
	select {
	case <-client.readyq:
	}
	// No timeouts (YET)
	return client, nil
}

func (r *inprocRendezvous) Accept() (Pipe, error) {
	server := &inproc{proto: r.proto, addr: r.addr}
	server.readyq = make(chan struct{})
	server.closeq = make(chan struct{})
	// inprocRendezvous will fill in rq and wq from client
	select {
	case r.servers <- server:
	}
	// wait for rendezvous to tell us we're ready
	select {
	case <-server.readyq:
	}
	return server, nil
}

// rendezvous() runs in a goroutine to continuously rendezvous
// on the same location
func (r *inprocRendezvous) rendezvous() {
	for {
		if r.server == nil {
			select {
			case r.server = <-r.servers:
			case <-r.closeq:
				return
			}
		}
		if r.client == nil {
			select {
			case r.client = <-r.clients:
			case <-r.closeq:
				return
			}
		}
		server := r.server
		client := r.client
		r.server = nil
		r.client = nil
		client.peer = server
		server.peer = client
		server.wq = client.rq
		server.rq = client.wq

		close(server.readyq) // wake server
		close(client.readyq) // wake client
	}
}

func (r *inprocRendezvous) Close() error {
	inprocServers.Lock()
	defer inprocServers.Unlock()

	r.processing = false
	select {
	case <-r.closeq:
	default:
		close(r.closeq)
	}
	if inprocServers.rendezvous[r.addr] == r {
		delete(inprocServers.rendezvous, r.addr)
	}
	return nil
}

func (t *inprocTran) Scheme() string {
	return "inproc"
}

func (t *inprocTran) NewDialer(addr string, proto uint16) (PipeDialer, error) {
	return &inprocDialer{addr: addr, proto: proto}, nil
}

func (t *inprocTran) NewAccepter(addr string, proto uint16) (PipeAccepter, error) {
	var r *inprocRendezvous
	if r = inprocGetRendezvous(addr, proto, true); r == nil {
		return nil, ErrAddrInUse
	}
	return r, nil
}

// SetOption implements the Transport SetOption method.  No options are
// supported at this time.
func (*inprocTran) SetOption(string, interface{}) error {
	// Likely we should support some options here...
	return ErrBadOption
}

// GetOption implements the Transport GetOption method.  No options are
// supported at this time.
func (*inprocTran) GetOption(string) (interface{}, error) {
	return nil, ErrBadOption
}

type inprocFactory int

func (inprocFactory) NewTransport() Transport {
	return new(inprocTran)
}

// InprocFactory is used by the core to create inproc Transport instances.
var InprocFactory inprocFactory
