// This package is a networking library. You have Hosts which can
// issue connections to others hosts, and Conn which are the connections itself.
// Hosts and Conns are interfaces and can be of type Tcp, or Chans, or Udp or
// whatever protocols you think might implement this interface.
// In this library we also provide a way to encode / decode any kind of packet /
// structs. When you want to send a struct to a conn, you first register
// (one-time operation) this packet to the library, and then directly pass the
// struct itself to the conn that will recognize its type. When decoding,
// it will automatically detect the underlying type of struct given, and decode
// it accordingly. You can provide your own decode / encode methods if for
// example, you have a variable length packet structure. For this, just
// implements MarshalBinary or UnmarshalBinary.

package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/dedis/cothority/lib/cliutils"
	"github.com/dedis/cothority/lib/dbg"
	"github.com/dedis/crypto/abstract"
	"github.com/dedis/protobuf"
)

/// Encoding part ///

type Type uint8

var TypeRegistry = make(map[Type]reflect.Type)
var InvTypeRegistry = make(map[reflect.Type]Type)

var globalOrder = binary.LittleEndian

// DefaultType is reserved by the network library. When you receive a message of
// DefaultType, it is generally because an error happenned, then you can call
// Error() on it.
var DefaultType Type = 0

// This is the default empty message that is returned in case something went
// wrong.
var EmptyApplicationMessage = ApplicationMessage{MsgType: DefaultType}

func DefaultConstructors(suite abstract.Suite) protobuf.Constructors {
	constructors := make(protobuf.Constructors)
	var point abstract.Point
	var secret abstract.Secret
	constructors[reflect.TypeOf(&point).Elem()] = func() interface{} { return suite.Point() }
	constructors[reflect.TypeOf(&secret).Elem()] = func() interface{} { return suite.Secret() }
	return constructors
}

// RegisterProtocolType register a custom "struct" / "packet" and get
// the allocated Type
// Pass simply your non-initialized struct
func RegisterProtocolType(msgType Type, msg ProtocolMessage) error {
	if _, typeRegistered := TypeRegistry[msgType]; typeRegistered {
		return errors.New("Type was already registered")
	}
	val := reflect.ValueOf(msg)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}
	t := val.Type()
	TypeRegistry[msgType] = t
	InvTypeRegistry[t] = msgType

	return nil
}

// String returns the underlying type in human format
func (t Type) String() string {
	ty, ok := TypeRegistry[t]
	if !ok {
		return "unknown"
	}
	return ty.Name()
}

// ProtocolMessage is a type for any message that the user wants to send
type ProtocolMessage interface{}

// ApplicationMessage is the container for any ProtocolMessage
type ApplicationMessage struct {
	// From field can be set by the receivinf connection itself, no need to
	// acutally transmit the value
	//From string
	// What kind of msg do we have
	MsgType Type
	// The underlying message
	Msg ProtocolMessage

	// which constructors are used
	constructors protobuf.Constructors
	// possible error during unmarshaling so that upper layer can know it
	err error
	// Same for the origin of the message
	From string
}

// Error returns the error that has been encountered during the unmarshaling of
// this message.
func (am *ApplicationMessage) Error() error {
	return am.err
}

// workaround so we can set the error after creation of the application
// message...
func (am *ApplicationMessage) SetError(err error) {
	am.err = err
}

// MarshalBinary the application message => to bytes
// Implements BinaryMarshaler interface so it will be used when sending with protobuf
func (am *ApplicationMessage) MarshalBinary() ([]byte, error) {
	b := new(bytes.Buffer)
	if err := binary.Write(b, globalOrder, am.MsgType); err != nil {
		return nil, err
	}
	var buf []byte
	var err error
	if buf, err = protobuf.Encode(am.Msg); err != nil {
		dbg.Print("Error for protobuf encoding")
		return nil, err
	}
	_, err = b.Write(buf)
	return b.Bytes(), err
}

// UnmarshalBinary will decode the incoming bytes
// It checks if the underlying packet is self-decodable
// by using its UnmarshalBinary interface
// otherwise, use abstract.Encoding (suite) to decode
func (am *ApplicationMessage) UnmarshalBinary(buf []byte) error {
	b := bytes.NewBuffer(buf)
	if err := binary.Read(b, globalOrder, &am.MsgType); err != nil {
		return err
	}
	if typ, ok := TypeRegistry[am.MsgType]; !ok {
		return fmt.Errorf("Type %s not registered.", am.MsgType.String())
	} else {
		ptrVal := reflect.New(typ)
		ptr := ptrVal.Interface()
		var err error
		if err = protobuf.DecodeWithConstructors(b.Bytes(), ptr, am.constructors); err != nil {
			return err
		}
		am.Msg = ptrVal.Elem().Interface()
	}
	return nil
}

// ConstructFrom takes a ProtocolMessage and then construct a
// ApplicationMessage from it. Error if the type is unknown
func NewApplicationMessage(obj ProtocolMessage) (*ApplicationMessage, error) {
	val := reflect.ValueOf(obj)
	if val.Kind() != reflect.Ptr {
		return nil, fmt.Errorf("Send takes a POINTER to the message. Given a copy here...")
	}
	val = val.Elem()
	t := val.Type()
	ty, ok := InvTypeRegistry[t]
	if !ok {
		return &ApplicationMessage{}, errors.New(fmt.Sprintf("Packet to send is not known. Please register packet: %s\n", t.String()))
	}
	return &ApplicationMessage{
		MsgType: ty,
		Msg:     obj}, nil
}

// Network part //

// How many times should we try to connect
const maxRetry = 10
const waitRetry = 1 * time.Second
const timeOut = 5 * time.Second

var ErrClosed = errors.New("Connection Closed")
var ErrEOF = errors.New("EOF")
var ErrCanceled = errors.New("Operation Canceled")
var ErrTemp = errors.New("Temporary Error")
var ErrTimeout = errors.New("Timeout Error")
var ErrUnknown = errors.New("Unknown Error")

// Host is the basic interface to represent a Host of any kind
// Host can open new Conn(ections) and Listen for any incoming Conn(...)
type Host interface {
	Name() string
	Open(name string) (Conn, error)
	Listen(addr string, fn func(Conn)) error // the srv processing function
	Close() error
}

// Conn is the basic interface to represent any communication mean
// between two host. It is closely related to the underlying type of Host
// since a TcpHost will generate only TcpConn
type Conn interface {
	// Gives the address of the remote endpoint
	Remote() string
	// Send a message through the connection. Always pass a pointer !
	Send(ctx context.Context, obj ProtocolMessage) error
	// Receive any message through the connection.
	Receive(ctx context.Context) (ApplicationMessage, error)
	Close() error
}

// TcpHost is the underlying implementation of
// Host using Tcp as a communication channel
type TcpHost struct {
	// its name (usually its IP address)
	name string
	// A list of connection maintained by this host
	peers map[string]Conn
	// its listeners
	listener net.Listener
	// the close channel used to indicate to the listener we want to quit
	quit chan bool
	// indicates wether this host is closed already or not
	closed bool
	// a list of constructors for en/decoding
	constructors protobuf.Constructors
}

// TcpConn is the underlying implementation of
// Conn using Tcp
type TcpConn struct {
	// The name of the endpoint we are connected to.
	Endpoint string

	// The connection used
	Conn net.Conn

	// closed indicator
	closed bool
	// A pointer to the associated host (just-in-case)
	host *TcpHost
}

// PeerName returns the name of the peer at the end point of
// the conn
func (c *TcpConn) Remote() string {
	return c.Endpoint
}

// handleError produces the higher layer error depending on the type
// so user of the package can know what is the cause of the problem
func handleError(err error) error {

	if strings.Contains(err.Error(), "use of closed") {
		return ErrClosed
	} else if strings.Contains(err.Error(), "canceled") {
		return ErrCanceled
	} else if strings.Contains(err.Error(), "EOF") {
		return ErrEOF
	}

	netErr, ok := err.(net.Error)
	if !ok {
		return ErrUnknown
	}
	if netErr.Temporary() {
		return ErrTemp
	} else if netErr.Timeout() {
		return ErrTimeout
	}
	return ErrUnknown
}

// Receive waits for any input on the connection and returns
// the ApplicationMessage **decoded** and an error if something
// wrong occured
func (c *TcpConn) Receive(ctx context.Context) (ApplicationMessage, error) {
	var am ApplicationMessage
	am.constructors = c.host.constructors
	bufferSize := 256
	b := make([]byte, bufferSize)
	var buffer bytes.Buffer
	var err error
	//c.Conn.SetReadDeadline(time.Now().Add(timeOut))
	for {
		n, err := c.Conn.Read(b)
		b = b[:n]
		buffer.Write(b)
		if err != nil {
			return EmptyApplicationMessage, handleError(err)
		}
		if n < bufferSize {
			// read all data
			break
		}
	}

	err = am.UnmarshalBinary(buffer.Bytes())
	if err != nil {
		return am, fmt.Errorf("Error unmarshaling message: %s", err.Error())
	}
	am.From = c.Remote()
	return am, nil
}

// Send will convert the Protocolmessage into an ApplicationMessage
// Then send the message through the Gob encoder
// Returns an error if anything was wrong
func (c *TcpConn) Send(ctx context.Context, obj ProtocolMessage) error {
	am, err := NewApplicationMessage(obj)
	if err != nil {
		return fmt.Errorf("Error converting packet: %v\n", err)
	}
	var b []byte
	b, err = am.MarshalBinary()
	if err != nil {
		return fmt.Errorf("Error marshaling  message: %s", err.Error())
	}

	c.Conn.SetWriteDeadline(time.Now().Add(timeOut))
	_, err = c.Conn.Write(b)
	if err != nil {
		return handleError(err)
	}
	return nil
}

// Close ... closes the connection
func (c *TcpConn) Close() error {
	if c.closed == true {
		return nil
	}
	err := c.Conn.Close()
	c.closed = true
	if err != nil {
		return handleError(err)
	}
	return nil
}

// NewTcpHost returns a Fresh TCP Host
func NewTcpHost(name string, constructors protobuf.Constructors) *TcpHost {
	return &TcpHost{
		name:         name,
		peers:        make(map[string]Conn),
		quit:         make(chan bool),
		constructors: constructors,
	}
}

// Name is the name ofthis host
func (t *TcpHost) Name() string {
	return t.name
}

// Open will create a new connection between this host
// and the remote host named "name". This is a TcpConn.
// If anything went wrong, Conn will be nil.
func (t *TcpHost) Open(name string) (Conn, error) {
	var conn net.Conn
	var err error
	for i := 0; i < maxRetry; i++ {
		conn, err = net.Dial("tcp", name)
		if err != nil {
			dbg.Lvl3(t.Name(), "(", i, "/", maxRetry, ") Error opening connection to", name)
			time.Sleep(waitRetry)
		} else {
			break
		}
		time.Sleep(waitRetry)
	}
	if conn == nil {
		return nil, fmt.Errorf("%s could not connect to %s: ABORT", t.Name(), name)
	}
	c := TcpConn{
		Endpoint: name,
		Conn:     conn,
		host:     t,
	}
	t.peers[name] = &c
	return &c, nil
}

// Listen for any host trying to contact him.
// Will launch in a goroutine the srv function once a connection is established
func (t *TcpHost) Listen(addr string, fn func(Conn)) error {
	global, _ := cliutils.GlobalBind(addr)
	ln, err := net.Listen("tcp", global)
	if err != nil {
		return fmt.Errorf("%s Error opening listener on address %s", t.Name(), addr)
	}
	t.listener = ln
	dbg.Lvl3(t.Name(), "Waiting for connections on addr", addr, "..\n")
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.quit:
				dbg.Lvl3(t.Name(), "Stop listening on", addr)
				return nil
			default:
				dbg.Lvl2(t.Name(), "error accepting connection:", err)
			}
			continue
		}
		c := TcpConn{
			Endpoint: conn.RemoteAddr().String(),
			Conn:     conn,
			host:     t,
		}
		t.peers[conn.RemoteAddr().String()] = &c
		go fn(&c)
	}
}

// Close will close every connection this host has opened
func (t *TcpHost) Close() error {
	if t.closed == true {
		return nil
	}
	t.closed = true
	for _, c := range t.peers {
		if err := c.Close(); err != nil {
			return handleError(err)
		}
	}
	close(t.quit)
	if t.listener != nil {
		return t.listener.Close()
	}
	return nil
}
