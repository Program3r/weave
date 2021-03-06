package router

import (
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

type Connection interface {
	Local() *Peer
	Remote() *Peer
	BreakTie(Connection) ConnectionTieBreak
	RemoteTCPAddr() string
	Established() bool
	Shutdown(error)
}

type ConnectionTieBreak int

const (
	TieBreakWon ConnectionTieBreak = iota
	TieBreakLost
	TieBreakTied
)

type RemoteConnection struct {
	local         *Peer
	remote        *Peer
	remoteTCPAddr string
	established   bool
}

type LocalConnection struct {
	sync.RWMutex
	RemoteConnection
	TCPConn           *net.TCPConn
	tcpSender         TCPSender
	remoteUDPAddr     *net.UDPAddr
	receivedHeartbeat bool
	stackFrag         bool
	effectivePMTU     int
	SessionKey        *[32]byte
	heartbeatTimeout  *time.Timer
	heartbeatFrame    *ForwardedFrame
	heartbeat         *time.Ticker
	fragTest          *time.Ticker
	forwardChan       chan<- *ForwardedFrame
	forwardChanDF     chan<- *ForwardedFrame
	stopForward       chan<- interface{}
	stopForwardDF     chan<- interface{}
	verifyPMTU        chan<- int
	Decryptor         Decryptor
	Router            *Router
	uid               uint64
	queryChan         chan<- *ConnectionInteraction
	finished          <-chan struct{} // closed to signal that queryLoop has finished
}

type ConnectionInteraction struct {
	Interaction
	payload interface{}
}

func NewRemoteConnection(from, to *Peer, tcpAddr string, established bool) *RemoteConnection {
	return &RemoteConnection{
		local:         from,
		remote:        to,
		remoteTCPAddr: tcpAddr,
		established:   established}
}

func (conn *RemoteConnection) Local() *Peer {
	return conn.local
}

func (conn *RemoteConnection) Remote() *Peer {
	return conn.remote
}

func (conn *RemoteConnection) BreakTie(Connection) ConnectionTieBreak {
	return TieBreakTied
}

func (conn *RemoteConnection) RemoteTCPAddr() string {
	return conn.remoteTCPAddr
}

func (conn *RemoteConnection) Established() bool {
	return conn.established
}

func (conn *RemoteConnection) Shutdown(error) {
}

func (conn *RemoteConnection) String() string {
	from := "<nil>"
	if conn.local != nil {
		from = conn.local.Name.String()
	}
	to := "<nil>"
	if conn.remote != nil {
		to = conn.remote.Name.String()
	}
	return fmt.Sprint("Connection ", from, "->", to)
}

func NewLocalConnection(connRemote *RemoteConnection, tcpConn *net.TCPConn, udpAddr *net.UDPAddr, router *Router) *LocalConnection {
	if connRemote.local != router.Ourself.Peer {
		log.Fatal("Attempt to create local connection from a peer which is not ourself")
	}
	// NB, we're taking a copy of connRemote here.
	return &LocalConnection{
		RemoteConnection: *connRemote,
		Router:           router,
		TCPConn:          tcpConn,
		remoteUDPAddr:    udpAddr,
		effectivePMTU:    DefaultPMTU}
}

// Async. Does not return anything. If the connection is successful,
// it will end up in the local peer's connections map.
func (conn *LocalConnection) Start(acceptNewPeer bool) {
	queryChan := make(chan *ConnectionInteraction, ChannelSize)
	conn.queryChan = queryChan
	finished := make(chan struct{})
	conn.finished = finished
	go conn.run(queryChan, finished, acceptNewPeer)
}

func (conn *LocalConnection) BreakTie(dupConn Connection) ConnectionTieBreak {
	dupConnLocal := dupConn.(*LocalConnection)
	// conn.uid is used as the tie breaker here, in the knowledge that
	// both sides will make the same decision.
	if conn.uid < dupConnLocal.uid {
		return TieBreakWon
	} else if dupConnLocal.uid < conn.uid {
		return TieBreakLost
	} else {
		return TieBreakTied
	}
}

// Read by the forwarder processes when in the UDP senders
func (conn *LocalConnection) RemoteUDPAddr() *net.UDPAddr {
	conn.RLock()
	defer conn.RUnlock()
	return conn.remoteUDPAddr
}

func (conn *LocalConnection) Established() bool {
	conn.RLock()
	defer conn.RUnlock()
	return conn.established
}

// Called by forwarder processes, read in Forward (by sniffer and udp
// listener process in router).
func (conn *LocalConnection) setEffectivePMTU(pmtu int) {
	conn.Lock()
	defer conn.Unlock()
	if conn.effectivePMTU != pmtu {
		conn.effectivePMTU = pmtu
		conn.log("Effective PMTU set to", pmtu)
	}
}

// Called by the connection's actor process, and by the connection's
// TCP received process. StackFrag is read in conn.Forward (called by
// router udp listener and sniffer processes)
func (conn *LocalConnection) setStackFrag(frag bool) {
	conn.Lock()
	defer conn.Unlock()
	conn.stackFrag = frag
}

func (conn *LocalConnection) log(args ...interface{}) {
	log.Println(append(append([]interface{}{}, fmt.Sprintf("->[%s]:", conn.remote.Name)), args...)...)
}

// Send directly, not via the Actor.  If it goes via the Actor we can
// get a deadlock where LocalConnection is blocked talking to
// LocalPeer and LocalPeer is blocked trying send a ProtocolMsg via
// LocalConnection, and the channels are full in both directions so
// nothing can proceed.
func (conn *LocalConnection) SendProtocolMsg(m ProtocolMsg) {
	if err := conn.sendProtocolMsg(m); err != nil {
		conn.Shutdown(err)
	}
}

// ACTOR client API

const (
	CSetEstablished = iota
	CReceivedHeartbeat
	CShutdown
)

// Send an actor request to the queryLoop, but don't block if queryLoop has exited
// - see http://blog.golang.org/pipelines for pattern
func (conn *LocalConnection) sendQuery(code int, payload interface{}) {
	select {
	case conn.queryChan <- &ConnectionInteraction{
		Interaction: Interaction{code: code},
		payload:     payload}:
	case <-conn.finished:
	}
}

// Async
func (conn *LocalConnection) Shutdown(err error) {
	// Run on its own goroutine in case the channel is backed up
	go conn.sendQuery(CShutdown, err)
}

// Async
//
// Heartbeating serves two purposes: a) keeping NAT paths alive, and
// b) updating a remote peer's knowledge of our address, in the event
// it changes (e.g. because NAT paths expired).
func (conn *LocalConnection) ReceivedHeartbeat(remoteUDPAddr *net.UDPAddr, connUID uint64) {
	if remoteUDPAddr == nil || connUID != conn.uid {
		return
	}
	conn.sendQuery(CReceivedHeartbeat, remoteUDPAddr)
}

// Async
func (conn *LocalConnection) SetEstablished() {
	conn.sendQuery(CSetEstablished, nil)
}

// ACTOR server

func (conn *LocalConnection) run(queryChan <-chan *ConnectionInteraction, finished chan<- struct{}, acceptNewPeer bool) {
	defer conn.handleShutdown()
	defer close(finished)

	tcpConn := conn.TCPConn
	tcpConn.SetLinger(0)
	enc := gob.NewEncoder(tcpConn)
	dec := gob.NewDecoder(tcpConn)

	if err := conn.handshake(enc, dec, acceptNewPeer); err != nil {
		log.Printf("->[%s] connection shutting down due to error during handshake: %v\n", conn.remoteTCPAddr, err)
		return
	}
	log.Printf("->[%s] completed handshake with %s\n", conn.remoteTCPAddr, conn.remote.Name)

	// We invoke AddConnection in the same goroutine that subsequently
	// becomes the tcp receive loop, rather than outside, because a)
	// the ordering relative to the receive loop is the only one that
	// matters [1], b) it prevents unnecessary delays in entering the
	// main connection loop, and c) it guards against potential
	// deadlocks.
	go func() {
		conn.Router.Ourself.AddConnection(conn)
		conn.receiveTCP(dec)
	}()

	heartbeatFrameBytes := make([]byte, EthernetOverhead+8)
	binary.BigEndian.PutUint64(heartbeatFrameBytes[EthernetOverhead:], conn.uid)
	conn.heartbeatFrame = &ForwardedFrame{
		srcPeer: conn.local,
		dstPeer: conn.remote,
		frame:   heartbeatFrameBytes}

	if conn.remoteUDPAddr != nil {
		if err := conn.sendFastHeartbeats(); err != nil {
			conn.log("connection shutting down due to error:", err)
			return
		}
	}

	conn.heartbeatTimeout = time.NewTimer(HeartbeatTimeout)

	if err := conn.queryLoop(queryChan); err != nil {
		conn.log("connection shutting down due to error:", err)
	} else {
		conn.log("connection shutting down")
	}
}

// [1] In the absence of any indirect connectivity to the remote peer,
// the first we hear about it (and any peers reachable from it) is
// through topology gossip it sends us on the connection. We must
// ensure that the connection has been added to Ourself prior to
// processing any such gossip, otherwise we risk immediately gc'ing
// part of that newly received portion of the topology (though not the
// remote peer itself, since that will have a positive ref count),
// leaving behind dangling references to peers. Therefore we invoke
// AddConnection, which is *synchronous*, before entering the tcp
// receive loop.

func (conn *LocalConnection) queryLoop(queryChan <-chan *ConnectionInteraction) (err error) {
	terminate := false
	for !terminate && err == nil {
		select {
		case query, ok := <-queryChan:
			if !ok {
				break
			}
			switch query.code {
			case CShutdown:
				err = query.payload.(error)
				terminate = true
			case CReceivedHeartbeat:
				err = conn.handleReceivedHeartbeat(query.payload.(*net.UDPAddr))
			case CSetEstablished:
				err = conn.handleSetEstablished()
			}
		case <-conn.heartbeatTimeout.C:
			err = fmt.Errorf("timed out waiting for UDP heartbeat")
		case <-tickerChan(conn.heartbeat):
			conn.Forward(true, conn.heartbeatFrame, nil)
		case <-tickerChan(conn.fragTest):
			conn.setStackFrag(false)
			err = conn.sendSimpleProtocolMsg(ProtocolStartFragmentationTest)
		}
	}
	return
}

// Handlers
//
// NB: The conn.* fields are only written by the connection actor
// process, which is the caller of the handlers. Hence we do not need
// locks for reading, and only need write locks for fields read by
// other processes.

func (conn *LocalConnection) handleReceivedHeartbeat(remoteUDPAddr *net.UDPAddr) error {
	oldRemoteUDPAddr := conn.remoteUDPAddr
	old := conn.receivedHeartbeat
	conn.Lock()
	conn.remoteUDPAddr = remoteUDPAddr
	conn.receivedHeartbeat = true
	conn.Unlock()
	conn.heartbeatTimeout.Reset(HeartbeatTimeout)
	if !old {
		if err := conn.sendSimpleProtocolMsg(ProtocolConnectionEstablished); err != nil {
			return err
		}
	}
	if oldRemoteUDPAddr == nil {
		return conn.sendFastHeartbeats()
	} else if oldRemoteUDPAddr.String() != remoteUDPAddr.String() {
		log.Println("Peer", conn.remote.Name, "moved from", old, "to", remoteUDPAddr)
	}
	return nil
}

func (conn *LocalConnection) handleSetEstablished() error {
	stopTicker(conn.heartbeat)
	old := conn.established
	conn.Lock()
	conn.established = true
	conn.Unlock()
	if old {
		return nil
	}
	conn.Router.Ourself.ConnectionEstablished(conn)
	if err := conn.ensureForwarders(); err != nil {
		return err
	}
	// Send a large frame down the DF channel in order to prompt
	// PMTU discovery to start.
	conn.Forward(true, &ForwardedFrame{
		srcPeer: conn.local,
		dstPeer: conn.remote,
		frame:   PMTUDiscovery},
		nil)
	conn.heartbeat = time.NewTicker(SlowHeartbeat)
	conn.fragTest = time.NewTicker(FragTestInterval)
	// avoid initial waits for timers to fire
	conn.Forward(true, conn.heartbeatFrame, nil)
	conn.setStackFrag(false)
	if err := conn.sendSimpleProtocolMsg(ProtocolStartFragmentationTest); err != nil {
		return err
	}
	return nil
}

func (conn *LocalConnection) handleShutdown() {
	if conn.TCPConn != nil {
		checkWarn(conn.TCPConn.Close())
	}

	if conn.remote != nil {
		conn.remote.DecrementLocalRefCount()
		conn.Router.Ourself.DeleteConnection(conn)
	}

	if conn.heartbeatTimeout != nil {
		conn.heartbeatTimeout.Stop()
	}

	stopTicker(conn.heartbeat)
	stopTicker(conn.fragTest)

	// blank out the forwardChan so that the router processes don't
	// try to send any more
	conn.stopForwarders()

	conn.Router.ConnectionMaker.ConnectionTerminated(conn.remoteTCPAddr)
}

// Helpers

func (conn *LocalConnection) sendSimpleProtocolMsg(tag ProtocolTag) error {
	return conn.sendProtocolMsg(ProtocolMsg{tag: tag})
}

func (conn *LocalConnection) sendProtocolMsg(m ProtocolMsg) error {
	return conn.tcpSender.Send(Concat([]byte{byte(m.tag)}, m.msg))
}

func (conn *LocalConnection) receiveTCP(decoder *gob.Decoder) {
	defer conn.Decryptor.Shutdown()
	usingPassword := conn.SessionKey != nil
	var receiver TCPReceiver
	if usingPassword {
		receiver = NewEncryptedTCPReceiver(conn)
	} else {
		receiver = NewSimpleTCPReceiver()
	}
	var err error
	for {
		var msg []byte
		conn.extendReadDeadline()
		if err = decoder.Decode(&msg); err != nil {
			break
		}
		msg, err = receiver.Decode(msg)
		if err != nil {
			break
		}
		if len(msg) < 1 {
			conn.log("ignoring blank msg")
			continue
		}
		if err = conn.handleProtocolMsg(ProtocolTag(msg[0]), msg[1:]); err != nil {
			break
		}
	}
	conn.Shutdown(err)
}

func (conn *LocalConnection) handleProtocolMsg(tag ProtocolTag, payload []byte) error {
	switch tag {
	case ProtocolConnectionEstablished:
		// We sent fast heartbeats to the remote peer, which has now
		// received at least one of them and told us via this message.
		// We can now consider the connection as established from our
		// end.
		conn.SetEstablished()
	case ProtocolStartFragmentationTest:
		conn.Forward(false, &ForwardedFrame{
			srcPeer: conn.local,
			dstPeer: conn.remote,
			frame:   FragTest},
			nil)
	case ProtocolFragmentationReceived:
		conn.setStackFrag(true)
	case ProtocolNonce:
		if conn.SessionKey == nil {
			return fmt.Errorf("unexpected nonce on unencrypted connection")
		}
		conn.Decryptor.ReceiveNonce(payload)
	case ProtocolPMTUVerified:
		conn.verifyPMTU <- int(binary.BigEndian.Uint16(payload))
	case ProtocolGossipUnicast:
		return conn.Router.handleGossip(payload, deliverGossipUnicast)
	case ProtocolGossipBroadcast:
		return conn.Router.handleGossip(payload, deliverGossipBroadcast)
	case ProtocolGossip:
		return conn.Router.handleGossip(payload, deliverGossip)
	default:
		conn.log("ignoring unknown protocol tag:", tag)
	}
	return nil
}

func (conn *LocalConnection) extendReadDeadline() {
	conn.TCPConn.SetReadDeadline(time.Now().Add(ReadTimeout))
}

func (conn *LocalConnection) sendFastHeartbeats() error {
	err := conn.ensureForwarders()
	if err == nil {
		conn.heartbeat = time.NewTicker(FastHeartbeat)
		conn.Forward(true, conn.heartbeatFrame, nil) // avoid initial wait
	}
	return err
}

func tickerChan(ticker *time.Ticker) <-chan time.Time {
	if ticker != nil {
		return ticker.C
	}
	return nil
}

func stopTicker(ticker *time.Ticker) {
	if ticker != nil {
		ticker.Stop()
	}
}
