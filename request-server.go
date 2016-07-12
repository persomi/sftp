package sftp

import (
	"encoding"
	"io"
	"io/ioutil"
	"sync"
	"syscall"
)

// Server takes the dataHandler and openHandler as arguments
// starts up packet handlers
// packet handlers convert packets to datas
// call dataHandler with data
// is done with packet/data
//
// dataHandler should call Handler() on data to process data and
// reply to client
//
// tricky bit about reading/writing spinning up workers to handle all packets

// datas using Id for switch
// + only 1 type + const
// - duplicates sftp prot Id

// datas using data-type for switch
// + types as types
// + type.Handle could enforce type of arg
// - requires dummy interface only for typing

var maxTxPacket uint32 = 1 << 15

type handleHandler func(string) string

type Handlers struct {
	FileGet  FileReader
	FilePut  FileWriter
	FileCmd  FileCmder
	FileInfo FileInfoer
}

// Server that abstracts the sftp protocol for a http request-like protocol
type RequestServer struct {
	serverConn
	Handlers        Handlers
	debugStream     io.Writer
	pktChan         chan packet
	openRequests    map[string]*Request
	openRequestLock sync.RWMutex
}

// simple factory function
// one server per user-session
func NewRequestServer(rwc io.ReadWriteCloser) (*RequestServer, error) {
	s := &RequestServer{
		serverConn: serverConn{
			conn: conn{
				Reader:      rwc,
				WriteCloser: rwc,
			},
		},
		debugStream:  ioutil.Discard,
		pktChan:      make(chan packet, sftpServerWorkerCount),
		openRequests: make(map[string]*Request),
	}

	return s, nil
}

func (rs *RequestServer) nextRequest(r *Request) string {
	rs.openRequestLock.Lock()
	defer rs.openRequestLock.Unlock()
	rs.openRequests[r.Filepath] = r
	return r.Filepath
}

func (rs *RequestServer) getRequest(handle string) (*Request, bool) {
	rs.openRequestLock.Lock()
	defer rs.openRequestLock.Unlock()
	r, ok := rs.openRequests[handle]
	return r, ok
}

func (rs *RequestServer) closeRequest(handle string) {
	rs.openRequestLock.Lock()
	defer rs.openRequestLock.Unlock()
	if _, ok := rs.openRequests[handle]; ok {
		delete(rs.openRequests, handle)
	}
}

// start serving requests from user session
func (rs *RequestServer) Serve() error {
	var wg sync.WaitGroup
	wg.Add(sftpServerWorkerCount)
	for i := 0; i < sftpServerWorkerCount; i++ {
		go func() {
			defer wg.Done()
			if err := rs.packetWorker(); err != nil {
				rs.conn.Close() // shuts down recvPacket
			}
		}()
	}

	var err error
	var pktType uint8
	var pktBytes []byte
	for {
		pktType, pktBytes, err = rs.recvPacket()
		if err != nil { break }
		pkt, err := makePacket(rxPacket{fxp(pktType), pktBytes})
		if err != nil { break }
		rs.pktChan <- pkt
	}

	close(rs.pktChan) // shuts down sftpServerWorkers
	wg.Wait()         // wait for all workers to exit
	return err
}

func (rs *RequestServer) packetWorker() error {
	for pkt := range rs.pktChan {
		// handle packet specific pre-processing
		var handle string
		var rpkt encoding.BinaryMarshaler
		var err error
		switch pkt := pkt.(type) {
		case *sshFxInitPacket:
			err := rs.sendPacket(sshFxVersionPacket{sftpProtocolVersion, nil})
			if err != nil { return err }
			continue
		case *sshFxpOpenPacket:
			handle = rs.nextRequest(newRequest(pkt.getPath()))
			err := rs.sendPacket(sshFxpHandlePacket{pkt.id(), handle})
			if err != nil { return err }
			continue
		case *sshFxpOpendirPacket:
			handle = rs.nextRequest(newRequest(pkt.getPath()))
			err := rs.sendPacket(sshFxpHandlePacket{pkt.id(), handle})
			if err != nil { return err }
			continue
		case *sshFxpClosePacket:
			handle = pkt.getHandle()
			rs.closeRequest(handle)
			err := rs.sendError(pkt, nil)
			if err != nil { return err }
			continue
		case hasHandle:
			handle = pkt.getHandle()
		case hasPath:
			handle = rs.nextRequest(newRequest(pkt.getPath()))
		}

		request, ok := rs.getRequest(handle)
		if !ok { rpkt = statusFromError(pkt, syscall.EBADF) }
		request.populate(pkt)
		rpkt, err = request.handleRequest(rs.Handlers)
		if err != nil { rpkt = statusFromError(pkt, err) }

		err = rs.sendPacket(rpkt)
		if err != nil { return err }
	}
	return nil
}
