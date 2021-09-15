package network

import (
	"bytes"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/sijms/go-ora/v2/converters"
)

type Data interface {
	Write(session *Session) error
	Read(session *Session) error
}
type sessionState struct {
	summary   *SummaryObject
	sendPcks  []PacketInterface
	inBuffer  []byte
	outBuffer []byte
	index     int
}

type Session struct {
	conn              net.Conn
	sslConn           *tls.Conn
	connOption        ConnectionOption
	Context           *SessionContext
	sendPcks          []PacketInterface
	inBuffer          []byte
	outBuffer         bytes.Buffer
	index             int
	key               []byte
	salt              []byte
	verifierType      int
	TimeZone          []byte
	TTCVersion        uint8
	HasEOSCapability  bool
	HasFSAPCapability bool
	Summary           *SummaryObject
	states            []sessionState
	StrConv           converters.IStringConverter
	UseBigClrChunks   bool
	UseBigScn         bool
	ClrChunkSize      int
	SSL               struct {
		CertificateRequest []*x509.CertificateRequest
		PrivateKeys        []*rsa.PrivateKey
		Certificates       []*x509.Certificate
		roots              *x509.CertPool
		tlsCertificates    []tls.Certificate
	}
	//certificates      []*x509.Certificate
}

func NewSession(connOption *ConnectionOption) *Session {
	return &Session{
		conn:            nil,
		inBuffer:        nil,
		index:           0,
		connOption:      *connOption,
		Context:         NewSessionContext(connOption),
		Summary:         nil,
		UseBigClrChunks: false,
		ClrChunkSize:    0x40,
	}
}

func (session *Session) SaveState() {
	session.states = append(session.states, sessionState{
		summary:   session.Summary,
		sendPcks:  session.sendPcks,
		inBuffer:  session.inBuffer,
		outBuffer: session.outBuffer.Bytes(),
		index:     session.index,
	})
}

func (session *Session) LoadState() {
	index := len(session.states) - 1
	if index >= 0 {
		currentState := session.states[index]
		session.Summary = currentState.summary
		session.sendPcks = currentState.sendPcks
		session.inBuffer = currentState.inBuffer
		session.outBuffer.Reset()
		session.outBuffer.Write(currentState.outBuffer) //  = currentState.outBuffer
		session.index = currentState.index
		if index == 0 {
			session.states = nil
		} else {
			session.states = session.states[:index]
		}
	}
}

func (session *Session) LoadSSLData(certs, keys, certRequests [][]byte) error {
	for _, temp := range certs {
		cert, err := x509.ParseCertificate(temp)
		if err != nil {
			return err
		}
		session.SSL.Certificates = append(session.SSL.Certificates, cert)
		for _, temp2 := range keys {
			key, err := x509.ParsePKCS1PrivateKey(temp2)
			if err != nil {
				return err
			}
			if key.PublicKey.Equal(cert.PublicKey) {
				certPem := pem.EncodeToMemory(&pem.Block{
					Type:  "CERTIFICATE",
					Bytes: temp,
				})
				keyPem := pem.EncodeToMemory(&pem.Block{
					Type:  "RSA PRIVATE KEY",
					Bytes: x509.MarshalPKCS1PrivateKey(key),
				})
				tlsCert, err := tls.X509KeyPair(certPem, keyPem)
				if err != nil {
					return err
				}
				session.SSL.tlsCertificates = append(session.SSL.tlsCertificates, tlsCert)
			}
		}
	}
	for _, temp := range certRequests {
		cert, err := x509.ParseCertificateRequest(temp)
		if err != nil {
			return err
		}
		session.SSL.CertificateRequest = append(session.SSL.CertificateRequest, cert)
	}
	return nil
}
func (session *Session) negotiate() {
	if session.SSL.roots == nil {
		session.SSL.roots = x509.NewCertPool()
		for _, cert := range session.SSL.Certificates {
			session.SSL.roots.AddCert(cert)
		}
	}
	config := &tls.Config{
		Certificates: session.SSL.tlsCertificates,
		RootCAs:      session.SSL.roots,
		ServerName:   session.connOption.Host,
	}
	if !session.connOption.SSLVerify {
		config.InsecureSkipVerify = true
	}
	session.sslConn = tls.Client(session.conn, config)
	//session.connOption.Tracer.Print("SSL/TLS HandShake complete")
}

func (session *Session) Connect() error {
	session.Disconnect()
	session.connOption.Tracer.Print("Connect")
	var err error
	addr := fmt.Sprintf("%s:%d", session.connOption.Host, session.connOption.Port)
	session.conn, err = net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	if session.connOption.SSL {
		session.connOption.Tracer.Print("Using SSL/TLS")
		session.negotiate()
	}

	connectPacket := newConnectPacket(*session.Context)
	err = session.writePacket(connectPacket)
	if err != nil {
		return err
	}
	if uint16(connectPacket.packet.length) == connectPacket.packet.dataOffset {
		session.PutBytes(connectPacket.buffer...)
		err = session.Write()
		if err != nil {
			return err
		}
	}
	pck, err := session.readPacket()
	if err != nil {
		return err
	}

	if acceptPacket, ok := pck.(*AcceptPacket); ok {
		*session.Context = acceptPacket.sessionCtx
		session.Context.handshakeComplete = true
		session.connOption.Tracer.Print("Handshake Complete")
		return nil
	}
	if redirectPacket, ok := pck.(*RedirectPacket); ok {
		session.connOption.Tracer.Print("Redirect")
		session.connOption.connData = redirectPacket.reconnectData
		if len(redirectPacket.protocol()) != 0 {
			session.connOption.Protocol = redirectPacket.protocol()
		}
		if len(redirectPacket.host()) != 0 {
			session.connOption.Host = redirectPacket.host()
		}
		if len(redirectPacket.port()) != 0 {
			session.connOption.Port, err = strconv.Atoi(redirectPacket.port())
			if err != nil {
				return errors.New("redirect packet with wrong port")
			}
		}
		return session.Connect()
	}
	if refusePacket, ok := pck.(*RefusePacket); ok {
		errorMessage := fmt.Sprintf(
			"connection refused by the server. user reason: %d; system reason: %d; error message: %s",
			refusePacket.UserReason, refusePacket.SystemReason, refusePacket.message)
		return errors.New(errorMessage)
	}
	return errors.New("connection refused by the server due to unknown reason")

	//for {
	//	err = session.writePacket(newConnectPacket(*session.Context))
	//
	//	rPck, err := session.readPacket()
	//	if err != nil {
	//		return err
	//	}
	//	if rPck == nil {
	//		return errors.New("packet is null due to unknown packet type")
	//	}
	//
	//	tmpPck, ok := rPck.(*Packet)
	//	if ok && tmpPck.packetType == RESEND {
	//		continue
	//	}
	//}
}

func (session *Session) Disconnect() {
	session.ResetBuffer()
	if session.sslConn != nil {
		_ = session.sslConn.Close()
		session.sslConn = nil
	}
	if session.conn != nil {
		_ = session.conn.Close()
		session.conn = nil
	}
}

func (session *Session) ResetBuffer() {
	session.Summary = nil
	session.sendPcks = nil
	session.inBuffer = nil
	//session.outBuffer = nil
	session.outBuffer.Reset()
	session.index = 0
}

func (session *Session) Debug() {
	//if session.index > 350 && session.index < 370 {
	fmt.Println("index: ", session.index)
	fmt.Printf("data buffer: %#v\n", session.inBuffer[session.index:session.index+30])
	oldIndex := session.index
	fmt.Println(session.GetClr())
	session.index = oldIndex
	//}
}
func (session *Session) DumpIn() {
	log.Printf("%#v\n", session.inBuffer)
}

func (session *Session) DumpOut() {
	log.Printf("%#v\n", session.outBuffer)
}

func (session *Session) Write() error {
	outputBytes := session.outBuffer.Bytes()
	size := session.outBuffer.Len()
	if size == 0 {
		// send empty data packet
		pck, err := newDataPacket(nil, session.Context)
		if err != nil {
			return err
		}
		return session.writePacket(pck)
		//return errors.New("the output buffer is empty")
	}

	segmentLen := int(session.Context.SessionDataUnit - 20)
	offset := 0

	for size > segmentLen {
		pck, err := newDataPacket(outputBytes[offset:offset+segmentLen], session.Context)
		if err != nil {
			return err
		}
		err = session.writePacket(pck)
		if err != nil {
			session.outBuffer.Reset()
			return err
		}
		size -= segmentLen
		offset += segmentLen
	}
	if size != 0 {
		pck, err := newDataPacket(outputBytes[offset:], session.Context)
		if err != nil {
			return err
		}
		err = session.writePacket(pck)
		if err != nil {
			session.outBuffer.Reset()
			return err
		}
	}
	return nil
}

func (session *Session) read(numBytes int) ([]byte, error) {
	if session.index+numBytes > len(session.inBuffer) {
		pck, err := session.readPacket()
		if err != nil {
			return nil, err
		}
		if dataPck, ok := pck.(*DataPacket); ok {
			session.inBuffer = append(session.inBuffer, dataPck.buffer...)
		} else {
			return nil, errors.New("the packet received is not data packet")
		}
	}
	ret := session.inBuffer[session.index : session.index+numBytes]
	session.index += numBytes
	return ret, nil
}

//func (session *Session) writePackets() error {
//
//	return  nil
//}
func (session *Session) writePacket(pck PacketInterface) error {
	session.sendPcks = append(session.sendPcks, pck)
	tmp := pck.bytes()
	session.connOption.Tracer.LogPacket("Write packet:", tmp)
	var err error
	if session.sslConn != nil {
		_, err = session.sslConn.Write(tmp)
	} else {
		_, err = session.conn.Write(tmp)
	}
	return err
}

func (session *Session) HasError() bool {
	return session.Summary != nil && session.Summary.RetCode != 0
}

func (session *Session) GetError() *OracleError {
	err := &OracleError{}
	if session.Summary != nil && session.Summary.RetCode != 0 {
		err.ErrCode = session.Summary.RetCode
		if session.StrConv != nil {
			err.ErrMsg = session.StrConv.Decode(session.Summary.ErrorMessage)
		} else {
			err.ErrMsg = string(session.Summary.ErrorMessage)
		}
	}
	return err
}

func (session *Session) readPacket() (PacketInterface, error) {

	readPacketData := func() ([]byte, error) {
		trials := 0
		for {
			if trials > 3 {
				return nil, errors.New("abnormal response")
			}
			trials++
			head := make([]byte, 8)
			var err error
			if session.sslConn != nil {
				_, err = session.sslConn.Read(head)
			} else {
				_, err = session.conn.Read(head)
			}
			//_, err := conn.Read(head)
			if err != nil {
				return nil, err
			}
			pckType := PacketType(head[4])
			var length uint32
			if session.Context.handshakeComplete && session.Context.Version >= 315 {
				length = binary.BigEndian.Uint32(head)
			} else {
				length = uint32(binary.BigEndian.Uint16(head))
			}
			length -= 8
			body := make([]byte, length)
			index := uint32(0)
			for index < length {
				var temp int
				if session.sslConn != nil {
					temp, err = session.sslConn.Read(body[index:])
				} else {
					temp, err = session.conn.Read(body[index:])
				}
				//temp, err := conn.Read(body[index:])
				if err != nil {
					if e, ok := err.(net.Error); ok && e.Timeout() && temp != 0 {
						index += uint32(temp)
						continue
					}
					return nil, err
				}
				index += uint32(temp)
			}

			if pckType == RESEND {
				for _, pck := range session.sendPcks {
					//log.Printf("Request: %#v\n\n", pck.bytes())
					var err error
					if session.connOption.SSL {
						session.negotiate()
						_, err = session.sslConn.Write(pck.bytes())
					} else {
						_, err = session.conn.Write(pck.bytes())
					}
					if err != nil {
						return nil, err
					}
				}
				continue
			}
			ret := append(head, body...)
			session.connOption.Tracer.LogPacket("Read packet:", ret)
			return ret, nil
		}

	}
	var packetData []byte
	var err error
	packetData, err = readPacketData()

	if err != nil {
		return nil, err
	}
	pckType := PacketType(packetData[4])
	//log.Printf("Response: %#v\n\n", packetData)
	switch pckType {
	case ACCEPT:
		return newAcceptPacketFromData(packetData, session.Context.ConnOption), nil
	case REFUSE:
		return newRefusePacketFromData(packetData), nil
	case REDIRECT:
		pck := newRedirectPacketFromData(packetData)
		dataLen := binary.BigEndian.Uint16(packetData[8:])
		var data string
		if uint16(pck.packet.length) <= pck.packet.dataOffset {
			packetData, err = readPacketData()
			dataPck, err := newDataPacketFromData(packetData, session.Context)
			if err != nil {
				return nil, err
			}
			data = string(dataPck.buffer)
		} else {
			data = string(packetData[10 : 10+dataLen])
		}
		//fmt.Println("data returned: ", data)
		length := strings.Index(data, "\x00")
		if pck.packet.flag&2 != 0 && length > 0 {
			pck.redirectAddr = data[:length]
			pck.reconnectData = data[length:]
		} else {
			pck.redirectAddr = data
		}
		//fmt.Println("redirect address: ", pck.redirectAddr)
		//fmt.Println("reconnect data: ", pck.reconnectData)
		//session.Disconnect()

		// if the length > dataoffset use data in the packet
		// else get data from the server
		// disconnect the current session
		// connect through redirectConnectData
		return pck, nil
	case DATA:
		return newDataPacketFromData(packetData, session.Context)
	case MARKER:
		pck := newMarkerPacketFromData(packetData, session.Context)
		breakConnection := false
		resetConnection := false
		switch pck.markerType {
		case 0:
			breakConnection = true
		case 1:
			if pck.markerData == 2 {
				resetConnection = true
			} else {
				breakConnection = true
			}
		default:
			return nil, errors.New("unknown marker type")
		}
		trials := 1
		for breakConnection && !resetConnection {
			if trials > 3 {
				return nil, errors.New("connection break")
			}
			packetData, err = readPacketData()
			if err != nil {
				return nil, err
			}
			pck = newMarkerPacketFromData(packetData, session.Context)
			if pck == nil {
				return nil, errors.New("connection break")
			}
			switch pck.markerType {
			case 0:
				breakConnection = true
			case 1:
				if pck.markerData == 2 {
					resetConnection = true
				} else {
					breakConnection = true
				}
			default:
				return nil, errors.New("unknown marker type")
			}
			trials++
		}
		session.ResetBuffer()
		err = session.writePacket(newMarkerPacket(2, session.Context))
		if err != nil {
			return nil, err
		}
		if resetConnection && session.Context.AdvancedService.HashAlgo != nil {
			err = session.Context.AdvancedService.HashAlgo.Init()
			if err != nil {
				return nil, err
			}
		}
		packetData, err = readPacketData()
		if err != nil {
			return nil, err
		}
		dataPck, err := newDataPacketFromData(packetData, session.Context)
		if err != nil {
			return nil, err
		}
		if dataPck == nil {
			return nil, errors.New("connection break")
		}
		session.inBuffer = dataPck.buffer
		session.index = 0
		msg, err := session.GetByte()
		if err != nil {
			return nil, err
		}
		if msg == 4 {
			session.Summary, err = NewSummary(session)
			if err != nil {
				return nil, err
			}
			if session.HasError() {
				return nil, session.GetError()
			}
		}
		fallthrough
	default:
		return nil, nil
	}
}

func (session *Session) PutString(data string) {
	session.PutClr([]byte(data))
}

func (session *Session) GetString(length int) (string, error) {
	ret, err := session.GetClr()
	return string(ret[:length]), err
}

func (session *Session) PutBytes(data ...byte) {
	session.outBuffer.Write(data)
	//session.outBuffer = append(session.outBuffer, )
}

//func (session *Session) PutByte(num byte) {
//		session.outBuffer = append(session.outBuffer, num)
//}

func (session *Session) PutUint(number interface{}, size uint8, bigEndian bool, compress bool) {
	var num uint64
	switch number := number.(type) {
	case int64:
		num = uint64(number)
	case int32:
		num = uint64(number)
	case int16:
		num = uint64(number)
	case int8:
		num = uint64(number)
	case uint64:
		num = number
	case uint32:
		num = uint64(number)
	case uint16:
		num = uint64(number)
	case uint8:
		num = uint64(number)
	case uint:
		num = uint64(number)
	case int:
		num = uint64(number)
	default:
		panic("you need to pass an integer to this function")
	}
	if size == 1 {
		session.outBuffer.WriteByte(uint8(num))
		//session.outBuffer = append(session.outBuffer, uint8(num))
		return
	}
	if compress {
		// if the size is one byte no compression occur only one byte written
		temp := make([]byte, 8)
		binary.BigEndian.PutUint64(temp, num)
		temp = bytes.TrimLeft(temp, "\x00")
		if size > uint8(len(temp)) {
			size = uint8(len(temp))
		}
		if size == 0 {
			session.outBuffer.WriteByte(0)
			//session.outBuffer = append(session.outBuffer, 0)
		} else {
			session.outBuffer.WriteByte(size)
			session.outBuffer.Write(temp)
			//session.outBuffer = append(session.outBuffer, size)
			//session.outBuffer = append(session.outBuffer, temp...)
		}
	} else {
		temp := make([]byte, size)
		if bigEndian {
			switch size {
			case 2:
				binary.BigEndian.PutUint16(temp, uint16(num))
			case 4:
				binary.BigEndian.PutUint32(temp, uint32(num))
			case 8:
				binary.BigEndian.PutUint64(temp, num)
			}
		} else {
			switch size {
			case 2:
				binary.LittleEndian.PutUint16(temp, uint16(num))
			case 4:
				binary.LittleEndian.PutUint32(temp, uint32(num))
			case 8:
				binary.LittleEndian.PutUint64(temp, num)
			}
		}
		session.outBuffer.Write(temp)
		//session.outBuffer = append(session.outBuffer, temp...)
	}
}

func (session *Session) PutInt(number interface{}, size uint8, bigEndian bool, compress bool) {
	var num int64
	switch number := number.(type) {
	case int64:
		num = number
	case int32:
		num = int64(number)
	case int16:
		num = int64(number)
	case int8:
		num = int64(number)
	case uint64:
		num = int64(number)
	case uint32:
		num = int64(number)
	case uint16:
		num = int64(number)
	case uint8:
		num = int64(number)
	case uint:
		num = int64(number)
	case int:
		num = int64(number)
	default:
		panic("you need to pass an integer to this function")
	}

	if compress {
		temp := make([]byte, 8)
		binary.BigEndian.PutUint64(temp, uint64(num))
		temp = bytes.TrimLeft(temp, "\x00")
		if size > uint8(len(temp)) {
			size = uint8(len(temp))
		}
		if size == 0 {
			session.outBuffer.WriteByte(0)
			//session.outBuffer = append(session.outBuffer, 0)
		} else {
			if num < 0 {
				num = num * -1
				size = size & 0x80
			}
			session.outBuffer.WriteByte(size)
			session.outBuffer.Write(temp)
			//session.outBuffer = append(session.outBuffer, size)
			//session.outBuffer = append(session.outBuffer, temp...)
		}
	} else {
		if size == 1 {
			session.outBuffer.WriteByte(uint8(num))
			//session.outBuffer = append(session.outBuffer, uint8(num))
		} else {
			temp := make([]byte, size)
			if bigEndian {
				switch size {
				case 2:
					binary.BigEndian.PutUint16(temp, uint16(num))
				case 4:
					binary.BigEndian.PutUint32(temp, uint32(num))
				case 8:
					binary.BigEndian.PutUint64(temp, uint64(num))
				}
			} else {
				switch size {
				case 2:
					binary.LittleEndian.PutUint16(temp, uint16(num))
				case 4:
					binary.LittleEndian.PutUint32(temp, uint32(num))
				case 8:
					binary.LittleEndian.PutUint64(temp, uint64(num))
				}
			}
			session.outBuffer.Write(temp)
			//session.outBuffer = append(session.outBuffer, temp...)
		}
	}
}

func (session *Session) PutClr(data []byte) {
	dataLen := len(data)
	if dataLen > 0xFC {
		session.outBuffer.WriteByte(0xFE)
		start := 0
		for start < dataLen {
			end := start + session.ClrChunkSize
			if end > dataLen {
				end = dataLen
			}
			temp := data[start:end]
			if session.UseBigClrChunks {
				session.PutInt(len(temp), 4, true, true)
			} else {
				session.outBuffer.WriteByte(uint8(len(temp)))
			}
			session.outBuffer.Write(temp)
			start += session.ClrChunkSize
		}
		session.outBuffer.WriteByte(0)
	} else if dataLen == 0 {
		session.outBuffer.WriteByte(0)
	} else {
		session.outBuffer.WriteByte(uint8(len(data)))
		session.outBuffer.Write(data)
	}
}

func (session *Session) PutKeyValString(key string, val string, num uint8) {
	session.PutKeyVal([]byte(key), []byte(val), num)
}

func (session *Session) PutKeyVal(key []byte, val []byte, num uint8) {
	if len(key) == 0 {
		session.outBuffer.WriteByte(0)
		//session.outBuffer = append(session.outBuffer, 0)
	} else {
		session.PutUint(len(key), 4, true, true)
		session.PutClr(key)
	}
	if len(val) == 0 {
		session.outBuffer.WriteByte(0)
		//session.outBuffer = append(session.outBuffer, 0)
	} else {
		session.PutUint(len(val), 4, true, true)
		session.PutClr(val)
	}
	session.PutInt(num, 4, true, true)
}

func (session *Session) PutData(data Data) error {
	return data.Write(session)
}
func (session *Session) GetData(data Data) error {
	return data.Read(session)
}
func (session *Session) GetByte() (uint8, error) {
	rb, err := session.read(1)
	if err != nil {
		return 0, err
	}
	return rb[0], nil
}

func (session *Session) GetInt64(size int, compress bool, bigEndian bool) (int64, error) {
	var ret int64
	negFlag := false
	if compress {
		rb, err := session.read(1)
		if err != nil {
			return 0, err
		}
		size = int(rb[0])
		if size&0x80 > 0 {
			negFlag = true
			size = size & 0x7F
		}
		bigEndian = true
	}
	if size == 0 {
		return 0, nil
	}
	rb, err := session.read(size)
	if err != nil {
		return 0, err
	}
	temp := make([]byte, 8)
	if bigEndian {
		copy(temp[8-size:], rb)
		ret = int64(binary.BigEndian.Uint64(temp))
	} else {
		copy(temp[:size], rb)
		//temp = append(pck.buffer[pck.index: pck.index + size], temp...)
		ret = int64(binary.LittleEndian.Uint64(temp))
	}
	if negFlag {
		ret = ret * -1
	}
	return ret, nil
}
func (session *Session) GetInt(size int, compress bool, bigEndian bool) (int, error) {
	temp, err := session.GetInt64(size, compress, bigEndian)
	if err != nil {
		return 0, err
	}
	return int(temp), nil
}
func (session *Session) GetNullTermString(maxSize int) (result string, err error) {
	oldIndex := session.index
	temp, err := session.read(maxSize)
	if err != nil {
		return
	}
	find := bytes.Index(temp, []byte{0})
	if find > 0 {
		result = string(temp[:find])
		session.index = oldIndex + find + 1
	} else {
		result = string(temp)
	}
	return
}

func (session *Session) GetClr() (output []byte, err error) {
	var size uint8
	var rb []byte
	size, err = session.GetByte()
	if err != nil {
		return
	}
	//if size == 253 {
	//	err = errors.New("TTC error")
	//	return
	//}
	if size == 0 || size == 0xFF {
		output = nil
		err = nil
		return
	}
	if size != 0xFE {
		output, err = session.read(int(size))
		return
	}

	if !session.UseBigClrChunks {
		buff := &bytes.Buffer{}
		for {
			h, err := session.GetByte()
			if err != nil {
				return nil, err
			}
			if h == 0x00 {
				break
			} else if h == 0xff {
				maxTry := 1024 * 4
				for {
					b, err := session.GetByte()
					if err != nil {
						return nil, err
					}
					if b == 0x00 {
						break
					}
					buff.WriteByte(b)
					if buff.Len() >= maxTry {
						break
					}
				}
				break
			} else if h >= 64 {
				return nil, fmt.Errorf("invalid chunk size: %v", h)
			} else {
				dat, err := session.read(int(h))
				if err != nil {
					return nil, err
				}
				buff.Write(dat)
			}
		}
		return buff.Bytes(), nil
	}

	//output = make([]byte, 0, 1000)
	var tempBuffer bytes.Buffer
	for {
		var size1 int
		if session.UseBigClrChunks {
			size1, err = session.GetInt(4, true, true)
		} else {
			size1, err = session.GetInt(1, true, true)
		}
		if err != nil || size1 == 0 {
			break
		}
		rb, err = session.read(size1)
		if err != nil {
			return
		}
		tempBuffer.Write(rb)
	}
	output = tempBuffer.Bytes()
	return
}

func (session *Session) GetDlc() (output []byte, err error) {
	var length int
	length, err = session.GetInt(4, true, true)
	if err != nil {
		return
	}
	if length > 0 {
		output, err = session.GetClr()
		if len(output) > length {
			output = output[:length]
		}
	}
	return
}

func (session *Session) GetBytes(length int) ([]byte, error) {
	return session.read(length)
}

func (session *Session) GetKeyVal() (key []byte, val []byte, num int, err error) {
	key, err = session.GetDlc()
	if err != nil {
		return
	}
	val, err = session.GetDlc()
	if err != nil {
		return
	}
	num, err = session.GetInt(4, true, true)
	return
}
