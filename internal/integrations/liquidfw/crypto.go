package liquidfw

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

func loadOrGenKey(path string) (*ecdh.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err == nil && len(data) == 32 {
		return ecdh.X25519().NewPrivateKey(data)
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, priv.Bytes(), 0600); err != nil {
		return nil, fmt.Errorf("save key: %w", err)
	}
	return priv, nil
}

// hkdf32 implements RFC 5869 HKDF-SHA256 with no salt (uses 32 zero bytes per spec).
// info must match what the device firmware uses: "liquidfw_v1_<08hex(deviceID)>"
func hkdf32(ikm []byte, info string) [32]byte {
	salt := make([]byte, 32) // HashLen zeros
	// Extract
	h := hmac.New(sha256.New, salt)
	h.Write(ikm)
	prk := h.Sum(nil)
	// Expand T(1)
	h = hmac.New(sha256.New, prk)
	h.Write([]byte(info))
	h.Write([]byte{0x01})
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// deriveSessionKey computes X25519(hfPriv, ephemeralPubBytes) then HKDF-SHA256.
// ephemeralPubBytes is the per-rotation ephemeral key from the ANNOUNCE v2 packet,
// NOT the long-term identity key. Info string v2 matches firmware CryptoContext::_rotateEphemeral().
func deriveSessionKey(hfPriv *ecdh.PrivateKey, ephemeralPubBytes [32]byte, deviceID uint32) ([32]byte, error) {
	var zero [32]byte
	peerPub, err := ecdh.X25519().NewPublicKey(ephemeralPubBytes[:])
	if err != nil {
		return zero, fmt.Errorf("bad ephemeral pub: %w", err)
	}
	shared, err := hfPriv.ECDH(peerPub)
	if err != nil {
		return zero, fmt.Errorf("ecdh: %w", err)
	}
	info := fmt.Sprintf("liquidfw_v2_%08x", deviceID)
	return hkdf32(shared, info), nil
}

// decryptState decrypts a PKT_STATE packet.
// Wire format: [5B aad][12B nonce][NB ciphertext][16B tag]
func decryptState(key [32]byte, pkt []byte) ([]byte, error) {
	if len(pkt) < 5+12+16 {
		return nil, fmt.Errorf("too short: %d", len(pkt))
	}
	aad        := pkt[:5]
	nonce      := pkt[5:17]
	rest       := pkt[17:]
	tag        := rest[len(rest)-16:]
	ciphertext := rest[:len(rest)-16]

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, append(ciphertext, tag...), aad)
}

// hmacSHA256 computes HMAC-SHA256(key, msg).
func hmacSHA256(key [32]byte, msg []byte) []byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write(msg)
	return mac.Sum(nil)
}

// buildCmdAuth builds X-Nonce and X-Sig for a signed /cmd request.
func buildCmdAuth(key [32]byte, nonce uint64, body []byte) (nonceHex, sigHex string) {
	nonceHex = fmt.Sprintf("%016x", nonce)
	var nonceBE [8]byte
	binary.BigEndian.PutUint64(nonceBE[:], nonce)
	sig := hmacSHA256(key, append(nonceBE[:], body...))
	sigHex = fmt.Sprintf("%x", sig)
	return
}

// pubKeyBytes returns the 32-byte raw X25519 public key.
func pubKeyBytes(priv *ecdh.PrivateKey) []byte {
	return priv.PublicKey().Bytes()
}

// readNASEntropy fills buf with random bytes using Go's crypto/rand.
func readNASEntropy(buf []byte) {
	_, _ = io.ReadFull(rand.Reader, buf)
}
