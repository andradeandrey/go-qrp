package qrp

// TOOD: Make query decoding more efficient by setting Query.ProcedureArguments as a bencode.RawMessage

import (
	"bytes"
	"github.com/zeebo/bencode" // BEncode
	"net"
	"sync"
	"reflect"
	"time"
	"log"
	"errors"
	"fmt"
)

// A procedure that can be invoked by other machines
type procedure struct {
	Method    reflect.Method
	ArgType   reflect.Type
	ReplyType reflect.Type
	Receiver reflect.Value
}

// Our local node
type Node struct {
	connection net.PacketConn
	connectionMTU uint32
	procedures map[string] *procedure // Registered procedures on the node
	pending map[call] responseChannel // A map of calls to queries pending responses
	messageID uint32
	
	pendingMutex sync.Mutex // to protect pending, messageID
	sendingMutex sync.Mutex
	proceduresMutex sync.Mutex
	
	serving bool
}

type responseChannel chan bencode.RawMessage

type call struct {
	MessageID uint32
	Addr net.Addr
}

// Creates a node using UDP (IPv6), returning an error if failure
func CreateNodeUDP(addr string, mtu uint32) (error, *Node) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err, nil
	}
	return CreateNode(conn, mtu)
}

// Creates a node that performs IO on connection
// Returns an error if failure
func CreateNode(connection net.PacketConn, mtu uint32) (error, *Node) {
	// Allocation
	procedures := make(map[string]*procedure)
	pending := make(map[call] responseChannel)
	sendingMutex, pendingMutex, proceduresMutex := new(sync.Mutex), new(sync.Mutex), new(sync.Mutex)
	
	// Initialize messageID to random value
	messageID := uint32(time.Now().Nanosecond())
	node := Node {
		connection: connection,
		connectionMTU: mtu,
		procedures: procedures, 
		pending: pending, 
		messageID: messageID,
		sendingMutex: *sendingMutex, 
		pendingMutex: *pendingMutex,
		proceduresMutex: *proceduresMutex,
		}
	return nil, &node
}

// Listens and Serves, returning an error on failure
func (node *Node) ListenAndServe() (err error) {
	if node.serving {
		return errors.New("Already serving")
	}
	node.serving = true
	defer node.connection.Close()
	defer func(){ node.serving = false }()
	
	for {
		// Buffer size is 512 because it's the largest size without possible fragmentation
		//
		// IPv4 and IPv6 define minimum reassembly buffer size, the minimum datagram size that we are guaranteed
		// any implementation must support. For IPv4, this is 576 bytes. IPv6 raises this to 1,500 bytes 
		// ~ UNIX Network Programming, Volume 2, Second Edition: Interprocess Communication
		
		buffer := make([]byte, node.connectionMTU, node.connectionMTU)

		// Read a packet into the buffer
		bytesRead, fromAddr, err := node.connection.ReadFrom(buffer)
		if err != nil {
			println("Error reading from connection")
			return err
		}
		
		// If we read a packet
		if bytesRead > 0 {
			// Process packet
			go func() {
				err = node.processPacket(&buffer, bytesRead, fromAddr)
				if err != nil {
					fmt.Printf("Error processing message: %s\n", err.Error())
				}
			}()
		}
	}
	
	return nil
}

// Processes received packets
func (node *Node) processPacket(data *[]byte, read int, addr net.Addr) (error) {	
	data_bigEndian, err := decodeIntoBigEndian(bytes.NewBuffer(*data))
	if err != nil {
		fmt.Println("Couldn't read packet into BigEndian:", err)
		return err
	}
	
	// Unmarshal BigEndian BEncode into struct
	bencodeD := bencode.NewDecoder(bytes.NewBuffer(data_bigEndian))
	var message Message
	if err := bencodeD.Decode(&message); err != nil {
		//return err
	}
	
	// Further processing
	return node.processMessage(&message, addr)
}

// Processes raw messages
func (node *Node) processMessage(message *Message, addr net.Addr) (error) {
	if query := message.Query; query != nil {
		// Query
		return node.processQuery(query, addr)
	} else if reply := message.Reply; reply != nil {
		// Reply
		return node.processReply(reply, addr)
	} else {
		return new(InvalidMessageError)
	}
	return nil
}

// Processes received queries
func (node *Node) processQuery(query *Query, addr net.Addr) (error) {
	procedureName := query.ProcedureName
	if procedure := node.procedures[procedureName]; procedure != nil {
		method := procedure.Method
		function := method.Func
		
		// Initialize value
		argValue, replyValue := reflect.New(procedure.ArgType.Elem()), reflect.New(procedure.ReplyType.Elem())		
		
		// Set value of arg
		argsReader := bytes.NewReader(query.ProcedureData)
		argsDecoder := bencode.NewDecoder(argsReader)
		err := argsDecoder.Decode(argValue.Interface())
		if err != nil {
			fmt.Printf("Error decoding procedure data into value: %s\n", err)
		}
		
		// Invoke the function
		function.Call([]reflect.Value{procedure.Receiver, argValue, replyValue})
		
		// Create reply
		reply := Reply { MessageID: query.MessageID }
		argsBuf := new(bytes.Buffer) 
		argsEncoder := bencode.NewEncoder(argsBuf)
		argsEncoder.Encode(replyValue.Interface())
		reply.ReturnData, err = encodeIntoBigEndian(argsBuf)
		
		if err != nil {
			fmt.Printf("Error encoding reply return data: %s\n", err)
			return err
		}
		
		// Create message
		message := Message { Reply: &reply }
		
		// Encode message
		messageBuf := new(bytes.Buffer)
		messageEncoder := bencode.NewEncoder(messageBuf)
		err = messageEncoder.Encode(message)
		if err != nil {
			fmt.Printf("Error encoding reply message into BEncode: %s\n", err)
			return err
		}
		
		message_bigEndian, err := encodeIntoBigEndian(messageBuf)
		if err != nil {
			fmt.Printf("Error encoding reply message into BigEndian: %s\n", err)
			return err
		}
		
		// Send to host
		node.sendingMutex.Lock()
		node.connection.WriteTo(message_bigEndian, addr)
		node.sendingMutex.Unlock()
		
		return nil
	} else {
		return &BadProcedureError{ procedureName }
	}
	return nil
}

// Processes received replies
func (node *Node) processReply(reply *Reply, addr net.Addr) (error) {
	// Construct call
	chanCall := call { MessageID: reply.MessageID, Addr: addr }
	
	// Get associated channel
	responseChan := node.pending[chanCall] // get response channel
	
	fmt.Printf("CLIENT:%s\n", node.pending)
	fmt.Printf("CLIENT:%s\n", chanCall)
	if responseChan == nil {
		fmt.Println("CLIENT: Chan is nil")
	}
	// The problem is we aren't getting the channel
	if cap(responseChan) == 0 {
		return &InvalidMessageMappingError { reply.MessageID }
	}
	
	// Send return data
	node.pendingMutex.Lock()
	responseChan <- reply.ReturnData
	node.pendingMutex.Unlock()
	
	return nil
}

// Returns the next available call slot for an IP
// The ID space is unique to 2 communicating nodes
// This is managed by maintaining a map of IDs to Calls. A call contains an IP+ID combination for the call. Only IDs that come from the same IP can be mapped to calls 
func (node *Node) nextCall(addr net.Addr) (nextCall call) {
	node.pendingMutex.Lock()
	
	// True when we have found an ID which is free
	// In ~99.999999999% of circumstances, this will run once
	callCreateDone := false
	
	for !callCreateDone {
		// Go doesn't panic after integer overflow, so this is OKAY!
		node.messageID++
		nextCall = call { MessageID: node.messageID, Addr: addr }
		
		// If there isn't already a pending call with the same IP+ID combination
		if node.pending[nextCall] == nil {
			callCreateDone = true
		}
	}
	
	node.pendingMutex.Unlock()
	
	return nextCall
}

func (node *Node) CallUDP(procedure string, addrString string, args interface{}, reply interface{}, timeout int) (err error) {
	addr, err := net.ResolveUDPAddr("ip", addrString)
	if err != nil {
		return err
	}
	
	return node.Call(procedure, addr, args, reply, timeout)
}

// Tries to call 'procedure' on remote node, with supplied 'args' and allocated return values 'reply'. 
// 'timeout' can be used to specify a maximum time to wait for a reply (in seconds). If timeout is 0, we wait forever. 
// The reliability of this completing successfully is dependent on the network protocol (UDP is unreliable)
// Returns an error if there is a timeout
func (node *Node) Call(procedure string, addr net.Addr, args interface{}, reply interface{}, timeout int) (err error) {
	// Get our call, which contains the message ID
	call := node.nextCall(addr)
	
	// Create Query
	query := Query { ProcedureName: procedure, MessageID: call.MessageID }
	query.constructArgs(args)
	
	// Create Message
	message := Message { Query: &query }
	
	// Encode it into BEncode
	buf := new(bytes.Buffer)
	bencodeE := bencode.NewEncoder(buf)
	if err := bencodeE.Encode(message); err != nil {
		return err
	}
	
	buf_bigEndian, err := encodeIntoBigEndian(buf)
	if err != nil {
		return err
	}
	
	// Create channel for receiving response
	responseChan := make(responseChannel, 1)
	
	// Allocate channel
	node.pending[call] = responseChan
	
	// Delete channel after exit
	defer func() {
		delete(node.pending, call)
	}()
	
	// Send to host
	node.sendingMutex.Lock()
	node.connection.WriteTo(buf_bigEndian, addr)
	node.sendingMutex.Unlock()
	
	// If timeout isn't 0, initate the timeout function concurrently
	timeoutChan := make(chan bool, 1)
	if timeout > 0 {
		go func(){
			// Timeout function
			time.Sleep(time.Duration(timeout) * time.Second)
			timeoutChan <- true
		}()
	}
	
	// Wait for response on channel
	select {
    case replydata := <-responseChan:
		// We received a reply
		// Decode args
		argsReader := bytes.NewReader(replydata)
		argsDecoder := bencode.NewDecoder(argsReader)
		err := argsDecoder.Decode(reply)
		if err != nil {
			fmt.Printf("Error decoding reply return data into value: %s\n", err)
			return err
		}
    case <-timeoutChan:
    	// We timed out
		return new(TimeoutError)
    }
    
	return nil
}

// Registers method as a procedure, which must satisfy the following conditions:
//	- exported
//  - has a receiver
//	- two arguments, both pointers to exported structs
//	- one return value, of type error
// It returns an error if the method does not satisfy these conditions
func (node *Node) Register(receiver interface{}) error {
	return node.register(receiver)
}

// Registers a method that
func (node *Node) register(receiver interface{}) error {
	// Lock mutex, prevents state corruption
	node.proceduresMutex.Lock()
	
	// Create service map if not made already
	if node.procedures == nil {
		node.procedures = make(map[string]*procedure)
	}
	
	// Declarations
	argIndex, replyIndex := 1, 2
	// Method needs two/three ins: receiver, *args, *reply.
	maxIns := 3
	receiverType := reflect.TypeOf(receiver)
	
	// Install the methods
	for m := 0; m < receiverType.NumMethod(); m++ {
		method := receiverType.Method(m)
		procedure := new(procedure)
		methodType := method.Type
		methodName := method.Name
		
		var errorBuf bytes.Buffer
		throwError := func() error {
			log.Println(errorBuf.String())
			return errors.New(errorBuf.String())
		}
		
		if methodType.NumIn() != maxIns {
			fmt.Fprintln(&errorBuf, "method", methodName, "has wrong number of ins:", methodType.NumIn())
			throwError()
		}
		
		// First arg need not be a pointer.
		argType := methodType.In(argIndex)
		if !isExportedOrBuiltinType(argType) {
			fmt.Fprintln(&errorBuf, methodName, "argument type not exported:", argType)
			throwError()
		}
		
		// Second arg must be a pointer.
		replyType := methodType.In(replyIndex)
		if replyType.Kind() != reflect.Ptr {
			fmt.Fprintln(&errorBuf, "method", methodName, "reply type not a pointer:", replyType)
			throwError()
		}
		
		// Reply type must be exported.
		if !isExportedOrBuiltinType(replyType) {
			fmt.Fprintln(&errorBuf, "method", methodName, "reply type not exported:", replyType)
			throwError()
		}
		
		// Method needs one out.
		/*if methodType.NumOut() != 1 {
			fmt.Fprintln(&errorBuf, "method", methodName, "has wrong number of outs:", methodType.NumOut())
			throwError()
		}
		
		// The return type of the method must be error.
		if returnType := methodType.Out(0); returnType != typeOfError {
			fmt.Fprintln(&errorBuf, "method", methodName, "returns", returnType.String(), "not error")
			throwError()
		}*/
		// Register method
		procedure.Method = method
		procedure.ArgType = argType
		procedure.ReplyType = replyType
		procedure.Receiver = reflect.ValueOf(receiver)
		node.procedures[methodName] = procedure
	}
	node.proceduresMutex.Unlock()
	return nil
}