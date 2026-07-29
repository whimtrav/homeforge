package esphome

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/flynn/noise"
)

// msgConn abstracts plaintext and noise-encrypted ESPHome API transports.
type msgConn interface {
	readMsg() (msgType int, data []byte, err error)
	writeMsg(msgType int, data []byte) error
}

// ---- Plaintext transport ----

type plaintextConn struct {
	r   *bufio.Reader
	w   net.Conn
	wmu sync.Mutex
}

func newPlaintextConn(conn net.Conn) *plaintextConn {
	return &plaintextConn{r: bufio.NewReaderSize(conn, 8192), w: conn}
}

func (c *plaintextConn) readMsg() (int, []byte, error) {
	preamble, err := c.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if preamble != 0x00 {
		return 0, nil, fmt.Errorf("bad plaintext preamble 0x%02x", preamble)
	}
	dataLen, err := binary.ReadUvarint(c.r)
	if err != nil {
		return 0, nil, err
	}
	msgType, err := binary.ReadUvarint(c.r)
	if err != nil {
		return 0, nil, err
	}
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(c.r, data); err != nil {
		return 0, nil, err
	}
	return int(msgType), data, nil
}

func (c *plaintextConn) writeMsg(msgType int, data []byte) error {
	buf := make([]byte, 0, 1+10+10+len(data))
	buf = append(buf, 0x00)
	buf = append(buf, encodeVarint(uint64(len(data)))...)
	buf = append(buf, encodeVarint(uint64(msgType))...)
	buf = append(buf, data...)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.w.Write(buf)
	return err
}

// ---- Noise transport (Noise_NNpsk0_25519_ChaChaPoly_SHA256) ----

type noiseConn struct {
	r    *bufio.Reader
	w    net.Conn
	send *noise.CipherState
	recv *noise.CipherState
	wmu  sync.Mutex
}

// establishNoise performs the ESPHome noise handshake and returns a ready connection.
func establishNoise(conn net.Conn, pskB64 string) (*noiseConn, error) {
	psk, err := base64.StdEncoding.DecodeString(pskB64)
	if err != nil {
		return nil, fmt.Errorf("invalid noise_psk: %w", err)
	}
	if len(psk) != 32 {
		return nil, fmt.Errorf("noise_psk must be 32 bytes, got %d", len(psk))
	}

	r := bufio.NewReaderSize(conn, 8192)

	// Client hello: indicator byte + zero length (2 bytes big-endian).
	if _, err := conn.Write([]byte{0x01, 0x00, 0x00}); err != nil {
		return nil, fmt.Errorf("noise hello: %w", err)
	}

	// Server hello (content ignored).
	if _, err := readNoiseFrame(r); err != nil {
		return nil, fmt.Errorf("server hello: %w", err)
	}

	cs := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           cs,
		Pattern:               noise.HandshakeNN,
		Initiator:             true,
		PresharedKey:          psk,
		PresharedKeyPlacement: 0,
		Prologue:              []byte("NoiseAPIInit\x00\x00"),
		Random:                rand.Reader,
	})
	if err != nil {
		return nil, fmt.Errorf("noise state: %w", err)
	}

	// Handshake message 1: -> psk, e
	// ESPHome frames handshake payloads with a 0x00 sub-type prefix.
	msg1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("noise msg1: %w", err)
	}
	if err := writeNoiseFrame(conn, append([]byte{0x00}, msg1...)); err != nil {
		return nil, err
	}

	// Handshake message 2: <- e, ee
	// Strip the 0x00 sub-type prefix before passing to the noise library.
	msg2raw, err := readNoiseFrame(r)
	if err != nil {
		return nil, fmt.Errorf("noise msg2: %w", err)
	}
	if len(msg2raw) == 0 {
		return nil, fmt.Errorf("noise msg2: empty frame")
	}
	if msg2raw[0] != 0x00 {
		return nil, fmt.Errorf("noise handshake failed: server error 0x%02x", msg2raw[0])
	}
	_, cs1, cs2, err := hs.ReadMessage(nil, msg2raw[1:])
	if err != nil {
		return nil, fmt.Errorf("noise handshake failed: %w", err)
	}

	return &noiseConn{r: r, w: conn, send: cs1, recv: cs2}, nil
}

func readNoiseFrame(r *bufio.Reader) ([]byte, error) {
	indicator, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if indicator != 0x01 {
		return nil, fmt.Errorf("bad noise indicator 0x%02x", indicator)
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint16(lenBuf[:])
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return data, nil
}

func writeNoiseFrame(w io.Writer, data []byte) error {
	buf := make([]byte, 3+len(data))
	buf[0] = 0x01
	binary.BigEndian.PutUint16(buf[1:3], uint16(len(data)))
	copy(buf[3:], data)
	_, err := w.Write(buf)
	return err
}

// Noise transport messages use uint16-BE type and length headers inside encryption.
func (nc *noiseConn) readMsg() (int, []byte, error) {
	frame, err := readNoiseFrame(nc.r)
	if err != nil {
		return 0, nil, err
	}
	plain, err := nc.recv.Decrypt(nil, nil, frame)
	if err != nil {
		return 0, nil, fmt.Errorf("decrypt: %w", err)
	}
	if len(plain) < 4 {
		return 0, nil, fmt.Errorf("noise message too short (%d bytes)", len(plain))
	}
	msgType := int(binary.BigEndian.Uint16(plain[0:2]))
	msgLen := int(binary.BigEndian.Uint16(plain[2:4]))
	if len(plain) < 4+msgLen {
		return 0, nil, fmt.Errorf("noise message truncated")
	}
	return msgType, plain[4 : 4+msgLen], nil
}

func (nc *noiseConn) writeMsg(msgType int, data []byte) error {
	plain := make([]byte, 4+len(data))
	binary.BigEndian.PutUint16(plain[0:2], uint16(msgType))
	binary.BigEndian.PutUint16(plain[2:4], uint16(len(data)))
	copy(plain[4:], data)
	nc.wmu.Lock()
	encrypted, err := nc.send.Encrypt(nil, nil, plain)
	nc.wmu.Unlock()
	if err != nil {
		return err
	}
	return writeNoiseFrame(nc.w, encrypted)
}
