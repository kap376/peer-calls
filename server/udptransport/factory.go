package udptransport

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/juju/errors"
	"github.com/peer-calls/peer-calls/server/logger"
	"github.com/peer-calls/peer-calls/server/pionlogger"
	"github.com/peer-calls/peer-calls/server/servertransport"
	"github.com/peer-calls/peer-calls/server/stringmux"
	"github.com/pion/sctp"
)

// Factory is in charge of creating Transports by an incoming request or a
// local request.
type Factory struct {
	log               logger.Logger
	stringMux         *stringmux.StringMux
	transportsChan    chan *Transport
	transports        map[string]*Transport
	pendingTransports map[string]*Request
	mu                sync.Mutex
	wg                *sync.WaitGroup
}

func NewFactory(
	log logger.Logger,
	wg *sync.WaitGroup,
	stringMux *stringmux.StringMux,
) *Factory {
	return &Factory{
		log:               log.WithNamespaceAppended("transport_factory"),
		stringMux:         stringMux,
		transportsChan:    make(chan *Transport),
		transports:        map[string]*Transport{},
		pendingTransports: map[string]*Request{},
		wg:                wg,
	}
}

func (t *Factory) addPendingTransport(req *Request) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	streamID := req.StreamID()

	if _, ok := t.transports[streamID]; ok {
		return errors.Errorf("transport already exist: %s", streamID)
	}

	if _, ok := t.pendingTransports[streamID]; ok {
		return errors.Errorf("transport promise already exists: %s", streamID)
	}

	t.pendingTransports[streamID] = req

	t.removePendingRequestWhenDone(req)

	return nil
}

func (t *Factory) removePendingRequestWhenDone(req *Request) {
	t.wg.Add(1)

	go func() {
		defer t.wg.Done()

		<-req.Done()

		t.mu.Lock()
		defer t.mu.Unlock()

		delete(t.pendingTransports, req.StreamID())
	}()
}

// AcceptTransport returns a Request. This promise can be either
// canceled by using the Cancel method, or it can be Waited for by using the
// Wait method. The Wait() method must be called and the error must be checked
// and handled.
func (t *Factory) AcceptTransport() *Request {
	conn, err := t.stringMux.AcceptConn()
	if err != nil {
		req := NewRequest(context.Background(), "")
		req.set(nil, errors.Annotate(err, "accept transport"))

		return req
	}

	streamID := conn.StreamID()

	req := NewRequest(context.Background(), streamID)

	if err := t.addPendingTransport(req); err != nil {
		req.set(nil, errors.Annotatef(err, "accept: promise or transport already exists: %s", streamID))

		return req
	}

	t.createTransportAsync(req, conn, true)

	return req
}

func (t *Factory) createTransportAsync(req *Request, conn stringmux.Conn, server bool) {
	raddr := conn.RemoteAddr()
	streamID := conn.StreamID()

	readChanSize := 100

	// This can be optimized in the future since a StringMux has a minimal
	// overhead of 3 bytes, and only a single bit is needed.
	localMux := stringmux.New(stringmux.Params{
		Conn:           conn,
		Log:            t.log,
		MTU:            uint32(servertransport.ReceiveMTU),
		ReadChanSize:   readChanSize,
		ReadBufferSize: 0,
	})

	// transportCreated will be closed as soon as the goroutine from which
	// createTransport is called is done.
	transportCreated := make(chan struct{})

	t.wg.Add(1)

	// The following gouroutine waits for the request context to be done
	// (canceled) and closes the local mux so that the goroutine from which
	// createTransport is called does not block forever.
	go func() {
		defer t.wg.Done()

		select {
		case <-req.Context().Done():
			// Ensure we don't get stuck at sctp.Client() or sctp.Server() forever.
			_ = localMux.Close()
		case <-transportCreated:
		}
	}()

	// TODO maybe we'll need to handle localMux Accept as well

	result, err := t.getOrAcceptStringMux(localMux, map[string]struct{}{
		"s": {},
		"m": {},
	})
	if err != nil {
		localMux.Close()
		req.set(nil, errors.Annotatef(err, "creating 's' and 'r' conns for raddr: %s %s", raddr, streamID))

		return
	}

	sctpConn := result["s"]
	mediaConn := result["m"]

	t.wg.Add(1)

	go func() {
		defer t.wg.Done()
		defer close(transportCreated)

		transport, err := t.createTransport(conn.RemoteAddr(), conn.StreamID(), localMux, mediaConn, sctpConn, server)
		if err != nil {
			mediaConn.Close()
			sctpConn.Close()
			localMux.Close()
		}

		if ok := req.set(transport, errors.Trace(err)); !ok && err == nil {
			// Request has already been canceled so close this transport.
			transport.Close()
		}
	}()
}

func (t *Factory) getOrAcceptStringMux(
	localMux *stringmux.StringMux,
	reqStreamIDs map[string]struct{},
) (map[string]stringmux.Conn, error) {
	var localMu sync.Mutex

	localWaitCh := make(chan struct{})
	localWaitChOnceClose := sync.Once{}

	conns := make(map[string]stringmux.Conn, len(reqStreamIDs))

	handleConn := func(conn stringmux.Conn) {
		localMu.Lock()
		defer localMu.Unlock()

		if _, ok := reqStreamIDs[conn.StreamID()]; ok {
			conns[conn.StreamID()] = conn
		} else {
			t.log.Warn("Unexpected conn", logger.Ctx{
				"stream_id":   conn.StreamID(),
				"remote_addr": conn.RemoteAddr(),
			})

			// // drain data from blocking the event loop

			// t.wg.Add(1)
			// go func() {
			// 	defer t.wg.Done()

			// 	buf := make([]byte, 1500)
			// 	for {
			// 		_, err := conn.Read(buf)
			// 		if err != nil {
			// 			return
			// 		}
			// 	}
			// }()
		}

		if len(reqStreamIDs) == len(conns) {
			localWaitChOnceClose.Do(func() {
				close(localWaitCh)
			})
		}
	}

	var errConn error

	t.wg.Add(1)

	go func() {
		defer t.wg.Done()

		for {
			conn, err := localMux.AcceptConn()
			if err != nil {
				localWaitChOnceClose.Do(func() {
					// existing connections should be closed here so no need to close.
					errConn = errors.Trace(err)
					close(localWaitCh)
				})

				return
			}

			handleConn(conn)
		}
	}()

	for reqStreamID := range reqStreamIDs {
		if conn, err := localMux.GetConn(reqStreamID); err == nil {
			handleConn(conn)
		}
	}

	if len(reqStreamIDs) > 0 {
		<-localWaitCh
	}

	return conns, errors.Trace(errConn)
}

func (t *Factory) createTransport(
	raddr net.Addr,
	streamID string,
	localMux *stringmux.StringMux,
	mediaConn io.ReadWriteCloser,
	sctpConn net.Conn,
	server bool,
) (*Transport, error) {
	sctpConfig := sctp.Config{
		NetConn:              sctpConn,
		LoggerFactory:        pionlogger.NewFactory(t.log),
		MaxMessageSize:       0,
		MaxReceiveBufferSize: 0,
	}

	var (
		association *sctp.Association
		err         error
	)

	if server {
		association, err = sctp.Server(sctpConfig)
	} else {
		association, err = sctp.Client(sctpConfig)
	}

	if err != nil {
		return nil, errors.Annotatef(err, "creating sctp association for raddr: %s %s", raddr, streamID)
	}

	// TODO check if handling association.Accept is necessary since OpenStream
	// can return an error. Perhaps we need to wait for Accept as well, check the
	// StreamIdentifier and log stream IDs we are not expecting.

	metadataStream, err := association.OpenStream(0, sctp.PayloadTypeWebRTCBinary)
	if err != nil {
		association.Close()

		return nil, errors.Annotatef(err, "creating metadata sctp stream for raddr: %s %s", raddr, streamID)
	}

	dataStream, err := association.OpenStream(1, sctp.PayloadTypeWebRTCBinary)
	if err != nil {
		metadataStream.Close()
		association.Close()

		return nil, errors.Annotatef(err, "creating data sctp stream for raddr: %s %s", raddr, streamID)
	}

	transport := servertransport.NewTransport(t.log, mediaConn, dataStream, metadataStream)

	streamTransport := &Transport{
		Transport:   transport,
		StreamID:    streamID,
		association: association,
		stringMux:   localMux,
	}

	t.mu.Lock()
	t.transports[streamID] = streamTransport
	t.mu.Unlock()

	t.wg.Add(1)

	go func() {
		defer t.wg.Done()
		<-transport.Done()

		t.mu.Lock()
		defer t.mu.Unlock()

		delete(t.transports, streamID)
	}()

	return streamTransport, nil
}

func (t *Factory) CloseTransport(streamID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if req, ok := t.pendingTransports[streamID]; ok {
		// Cancel the pending request.
		req.Cancel()

		// Wait for pending request to settle.
		<-req.Done()
	}

	if transport, ok := t.transports[streamID]; ok {
		if err := transport.Close(); err != nil {
			t.log.Error("Close transport", errors.Trace(err), logger.Ctx{
				"stream_id": streamID,
			})
		}
	}
}

// NewTransport returns a Request. This promise can be either canceled
// by using the Cancel method, or it can be Waited for by using the Wait
// method. The Wait() method must be called and the error must be checked and
// handled.
func (t *Factory) NewTransport(streamID string) *Request {
	req := NewRequest(context.Background(), streamID)

	if err := t.addPendingTransport(req); err != nil {
		req.set(nil, errors.Annotatef(err, "new: promise or transport already exists: %s", streamID))

		return req
	}

	conn, err := t.stringMux.GetConn(streamID)
	if err != nil {
		req.set(nil, errors.Annotatef(err, "retrieving transport conn: %s", streamID))

		return req
	}

	t.createTransportAsync(req, conn, false)

	return req
}

func (t *Factory) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	for streamID, transport := range t.transports {
		transport.Close()
		delete(t.transports, streamID)
	}

	return nil
}