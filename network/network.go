package network

import (
	"bytes"
	"errors"
	"net"
	"reflect"
	"strconv"
	"time"

	"github.com/NebulousLabs/Andromeda/encoding"
)

const (
	timeout   = time.Second * 5
	maxMsgLen = 1 << 16
)

// A NetAddress contains the information needed to contact a peer over TCP.
type NetAddress struct {
	Host string
	Port uint16
}

// String returns the NetAddress as a string, concatentating the hostname and
// port number.
func (na *NetAddress) String() string {
	return net.JoinHostPort(na.Host, strconv.Itoa(int(na.Port)))
}

// Call establishes a TCP connection to the NetAddress, calls the provided
// function on it, and closes the connection.
func (na *NetAddress) Call(fn func(net.Conn) error) error {
	conn, err := net.DialTimeout("tcp", na.String(), timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(conn)
}

func ReadPrefix(conn net.Conn) ([]byte, error) {
	prefix := make([]byte, 4)
	if n, err := conn.Read(prefix); err != nil || n != len(prefix) {
		return nil, errors.New("could not read length prefix")
	}
	msgLen := int(encoding.DecUint64(prefix))
	if msgLen > maxMsgLen {
		return nil, errors.New("message too long")
	}
	// read msgLen bytes
	data := new(bytes.Buffer)
	for total := 0; total < msgLen; {
		n, err := data.ReadFrom(conn)
		if err != nil {
			return nil, err
		}
		total += int(n)
	}
	if data.Len() != msgLen {
		return nil, errors.New("message length mismatch")
	}
	return data.Bytes(), nil
}

func WritePrefix(conn net.Conn, data []byte) (int, error) {
	encLen := encoding.EncUint64(uint64(len(data)))
	return conn.Write(append(encLen[:4], data...))
}

// SendVal returns a closure that can be used in conjuction with Call to send
// a value to a NetAddress. It prefixes the encoded data with a header,
// containing the message's type and length
func SendVal(t byte, val interface{}) func(net.Conn) error {
	encVal := encoding.Marshal(val)
	encLen := encoding.EncUint64(uint64(len(encVal)))
	msg := append([]byte{t},
		append(encLen[:4], encVal...)...)

	return func(conn net.Conn) error {
		_, err := conn.Write(msg)
		return err
	}
}

// TBD
var BootstrapPeers = []NetAddress{}

// A TCPServer sends and receives messages. It also maintains an address book
// of peers to broadcast to and make requests of.
type TCPServer struct {
	net.Listener
	myAddr      NetAddress
	addressbook map[NetAddress]struct{}
	handlerMap  map[byte]func(net.Conn, []byte) error
}

// RandomPeer selects and returns a random peer from the address book.
// TODO: probably not smart to depend on map iteration...
func (tcps *TCPServer) RandomPeer() (rand NetAddress) {
	for addr := range tcps.addressbook {
		rand = addr
		break
	}
	return
}

// Broadcast calls the specified function on each peer in the address book.
func (tcps *TCPServer) Broadcast(fn func(net.Conn) error) {
	for addr := range tcps.addressbook {
		addr.Call(fn)
	}
}

// RegisterHandler registers a message type with a message handler. The
// existing handler for that type will be overwritten.
func (tcps *TCPServer) RegisterHandler(t byte, fn func(net.Conn, []byte) error) {
	tcps.handlerMap[t] = fn
}

// RegisterRPC is for simple handlers. A simple handler decodes the message
// data and passes it to fn. fn must have the type signature:
//   func(Type) error
// i.e. a 1-adic function that returns an error.
func (tcps *TCPServer) RegisterRPC(t byte, fn interface{}) error {
	// if fn not correct type, panic
	val, typ := reflect.ValueOf(fn), reflect.TypeOf(fn)
	if typ.Kind() != reflect.Func || typ.NumIn() != 1 ||
		typ.NumOut() != 1 || typ.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		return errors.New("registered function has wrong type signature")
	}

	// create function:
	sfn := func(_ net.Conn, b []byte) error {
		v := reflect.New(typ.In(0))
		if err := encoding.Unmarshal(b, v.Interface()); err != nil {
			return err
		}
		if err := val.Call([]reflect.Value{v.Elem()})[0].Interface(); err != nil {
			return err.(error)
		}
		return nil
	}

	tcps.RegisterHandler(t, sfn)
	return nil
}

// NewTCPServer creates a TCPServer that listens on the specified port.
func NewTCPServer(port uint16) (tcps *TCPServer, err error) {
	tcpServ, err := net.Listen("tcp", ":"+strconv.Itoa(int(port)))
	if err != nil {
		return
	}
	tcps = &TCPServer{
		Listener:    tcpServ,
		myAddr:      NetAddress{"", port},
		addressbook: make(map[NetAddress]struct{}),
	}
	// default handlers
	tcps.handlerMap = map[byte]func(net.Conn, []byte) error{
		'H': sendHostname,
		'P': tcps.sharePeers,
		'A': tcps.addPeer,
	}

	// spawn listener
	go tcps.listen()
	return
}

// listen runs in the background, accepting incoming connections and serving
// them. listen will return after TCPServer.Close() is called, because the
// Accept() call will fail.
func (tcps *TCPServer) listen() {
	for {
		conn, err := tcps.Accept()
		if err != nil {
			return
		}
		// it is the handler's responsibility to close the connection
		go tcps.handleConn(conn)
	}
}

// handleConn reads header data from a connection, unmarshals the data
// structures it contains, and routes the data to other functions for
// processing.
// TODO: set deadlines?
func (tcps *TCPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	msgType := make([]byte, 1)
	if n, err := conn.Read(msgType); err != nil || n != 1 {
		// TODO: log error
		return
	}
	msgData, err := ReadPrefix(conn)
	if err != nil {
		// TODO: log error
		return
	}
	// call registered handler for this message type
	if fn, ok := tcps.handlerMap[msgType[0]]; ok {
		fn(conn, msgData)
		// TODO: log error
		// no wait, send the error?
	}
	return
}

// sendHostname replies to the send with the sender's external IP.
func sendHostname(conn net.Conn, _ []byte) error {
	_, err := WritePrefix(conn, []byte(conn.RemoteAddr().String()))
	return err
}

// sharePeers transmits at most 'num' peers over the connection.
// TODO: choose random peers?
func (tcps *TCPServer) sharePeers(conn net.Conn, msgData []byte) error {
	if len(msgData) != 1 {
		return errors.New("invalid number of peers")
	}
	num := msgData[0]
	var addrs []NetAddress
	for addr := range tcps.addressbook {
		if num == 0 {
			break
		}
		addrs = append(addrs, addr)
		num--
	}
	_, err := WritePrefix(conn, encoding.Marshal(addrs))
	return err
}

// addPeer adds the connecting peer to its address book
func (tcps *TCPServer) addPeer(_ net.Conn, data []byte) (err error) {
	var addr NetAddress
	if err = encoding.Unmarshal(data, &addr); err != nil {
		return
	}
	tcps.addressbook[addr] = struct{}{}
	return
}

// Ping returns whether a NetAddress is reachable. It accomplishes this by
// initiating a TCP connection and immediately closes it. This is pretty
// unsophisticated. I'll add a Pong later.
func (tcps *TCPServer) Ping(addr NetAddress) bool {
	conn, err := net.DialTimeout("tcp", addr.String(), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// learnHostname learns the external IP of the TCPServer.
func (tcps *TCPServer) learnHostname(conn net.Conn) (err error) {
	// send hostname request
	if _, err = conn.Write([]byte{'H', 0, 0, 0, 0}); err != nil {
		return
	}
	// read response
	data, err := ReadPrefix(conn)
	if err != nil {
		return
	}
	// TODO: try to ping ourselves?
	host, _, err := net.SplitHostPort(string(data))
	if err != nil {
		return
	}
	tcps.myAddr.Host = host
	return
}

// requestPeers queries a peer for additional peers, and adds any new peers to
// the address book.
func (tcps *TCPServer) requestPeers(conn net.Conn) (err error) {
	// request 10 peers
	if _, err = conn.Write([]byte{'P', 1, 0, 0, 0, 10}); err != nil {
		return
	}
	// read response
	data, err := ReadPrefix(conn)
	if err != nil {
		return
	}
	var addrs []NetAddress
	if err = encoding.Unmarshal(data, &addrs); err != nil {
		return
	}
	// add peers
	for _, addr := range addrs {
		if addr != tcps.myAddr && tcps.Ping(addr) {
			tcps.addressbook[addr] = struct{}{}
		}
	}
	return
}

// Bootstrap discovers the external IP of the TCPServer, requests peers from
// the initial peer list, and announces itself to those peers.
func (tcps *TCPServer) Bootstrap() (err error) {
	// populate initial peer list
	for _, addr := range BootstrapPeers {
		if tcps.Ping(addr) {
			tcps.addressbook[addr] = struct{}{}
		}
	}

	// learn hostname
	for addr := range tcps.addressbook {
		if addr.Call(tcps.learnHostname) == nil {
			break
		}
	}

	// request peers
	// TODO: maybe iterate until we have enough new peers?
	tcps.Broadcast(tcps.requestPeers)

	// announce ourselves to new peers
	tcps.Broadcast(SendVal('A', tcps.myAddr))

	return
}

func (tcps *TCPServer) AddressBook() (book []NetAddress) {
	for address := range tcps.addressbook {
		book = append(book, address)
	}
	return
}
