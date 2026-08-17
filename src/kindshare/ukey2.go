package main

// UKEY2 handshake, server (receiver) side.
//
// Sequence on the wire, each message preceded by a 4-byte big-endian length:
//
//	client -> ConnectionRequest   (OfflineFrame, plaintext)
//	client -> Ukey2ClientInit
//	server -> Ukey2ServerInit
//	client -> Ukey2ClientFinish
//	both   -> ConnectionResponse  (OfflineFrame, plaintext)
//	...     everything after this is encrypted
//
// Key derivation follows google/ukey2 plus the D2D layer documented in
// NearDrop's PROTOCOL.md. Getting a single salt or info string wrong fails
// opaquely much later as an HMAC mismatch, so each constant is spelled out with
// its source rather than folded into a helper.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"

	"golang.org/x/crypto/hkdf"
	"google.golang.org/protobuf/proto"

	"kindshare/pb/securegcm"
	"kindshare/pb/securemessage"
)

// The cipher UKEY2 negotiates for Nearby Share. Only one is ever offered.
const nextProtocol = "AES_256_CBC-HMAC_SHA256"

// Salts from NearDrop's PROTOCOL.md. They are fixed constants of the D2D layer,
// not per-session values.
var (
	// D2D key derivation.
	saltD2D = mustHex("82AA55A0D397F88346CA1CEE8D3909B95F13FA7DEB1D4AB38376B8256DA85510")
	// Encryption/MAC key derivation from the D2D keys.
	saltEncMac = mustHex("BF9D2A53C63616D75DB0A7165B91C1EF73E537F2427405FA23610A4BE657642E")
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// sessionKeys holds the four directional keys derived after the handshake.
type sessionKeys struct {
	decryptKey  []byte // client -> us
	receiveHMAC []byte
	encryptKey  []byte // us -> client
	sendHMAC    []byte

	authString []byte // used for the visual/PIN verification
}

// ---------------------------------------------------------------- framing

// readFrame reads one length-prefixed message.
func readFrame(r io.Reader) ([]byte, error) {
	var l uint32
	if err := binary.Read(r, binary.BigEndian, &l); err != nil {
		return nil, err
	}
	if l > 8<<20 {
		return nil, fmt.Errorf("implausible frame length %d", l)
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func writeFrame(w io.Writer, b []byte) error {
	if err := binary.Write(w, binary.BigEndian, uint32(len(b))); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

// ---------------------------------------------------------------- handshake

// doHandshake runs the UKEY2 exchange and returns the derived session keys.
// conn is positioned just after the ConnectionRequest frame has been read.
func doHandshake(conn net.Conn, clientInitRaw []byte) (*sessionKeys, error) {
	// --- parse ClientInit -------------------------------------------------
	var msg securegcm.Ukey2Message
	if err := proto.Unmarshal(clientInitRaw, &msg); err != nil {
		return nil, fmt.Errorf("parse Ukey2Message: %w", err)
	}
	if msg.GetMessageType() != securegcm.Ukey2Message_CLIENT_INIT {
		return nil, fmt.Errorf("expected CLIENT_INIT, got %v", msg.GetMessageType())
	}
	var clientInit securegcm.Ukey2ClientInit
	if err := proto.Unmarshal(msg.GetMessageData(), &clientInit); err != nil {
		return nil, fmt.Errorf("parse ClientInit: %w", err)
	}

	// Find the commitment for the cipher we support. The client commits to the
	// SHA512 of its ClientFinish; we verify it later.
	var commitment []byte
	for _, c := range clientInit.GetCipherCommitments() {
		if c.GetHandshakeCipher() == securegcm.Ukey2HandshakeCipher_P256_SHA512 {
			commitment = c.GetCommitment()
		}
	}
	if commitment == nil {
		return nil, fmt.Errorf("client offered no P256_SHA512 commitment")
	}

	// --- build ServerInit -------------------------------------------------
	curve := ecdh.P256()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate P-256 key: %w", err)
	}
	// Uncompressed point: 0x04 || X(32) || Y(32)
	pubBytes := priv.PublicKey().Bytes()
	x := new(big.Int).SetBytes(pubBytes[1:33])
	y := new(big.Int).SetBytes(pubBytes[33:65])

	serverPub, err := proto.Marshal(&securemessage.GenericPublicKey{
		Type: securemessage.PublicKeyType_EC_P256.Enum(),
		EcP256PublicKey: &securemessage.EcP256PublicKey{
			// Java BigInteger encoding: signed big-endian, so a high bit set
			// means a leading 0x00 byte. Mirror that or the peer's parser
			// rejects the point.
			X: javaBigInt(x),
			Y: javaBigInt(y),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}

	serverRandom := make([]byte, 32)
	if _, err := rand.Read(serverRandom); err != nil {
		return nil, err
	}

	serverInitBody, err := proto.Marshal(&securegcm.Ukey2ServerInit{
		Version:         proto.Int32(1),
		Random:          serverRandom,
		HandshakeCipher: securegcm.Ukey2HandshakeCipher_P256_SHA512.Enum(),
		PublicKey:       serverPub,
	})
	if err != nil {
		return nil, err
	}
	serverInitRaw, err := proto.Marshal(&securegcm.Ukey2Message{
		MessageType: securegcm.Ukey2Message_SERVER_INIT.Enum(),
		MessageData: serverInitBody,
	})
	if err != nil {
		return nil, err
	}
	if err := writeFrame(conn, serverInitRaw); err != nil {
		return nil, fmt.Errorf("send ServerInit: %w", err)
	}

	// --- read ClientFinish ------------------------------------------------
	clientFinishRaw, err := readFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("read ClientFinish: %w", err)
	}
	// The commitment covers the ENTIRE Ukey2Message, as received.
	if sum := sha512.Sum512(clientFinishRaw); !bytes.Equal(sum[:], commitment) {
		return nil, fmt.Errorf("ClientFinish does not match the committed hash")
	}

	var finishMsg securegcm.Ukey2Message
	if err := proto.Unmarshal(clientFinishRaw, &finishMsg); err != nil {
		return nil, fmt.Errorf("parse ClientFinish message: %w", err)
	}
	if finishMsg.GetMessageType() != securegcm.Ukey2Message_CLIENT_FINISH {
		return nil, fmt.Errorf("expected CLIENT_FINISH, got %v", finishMsg.GetMessageType())
	}
	var clientFinish securegcm.Ukey2ClientFinished
	if err := proto.Unmarshal(finishMsg.GetMessageData(), &clientFinish); err != nil {
		return nil, fmt.Errorf("parse ClientFinished: %w", err)
	}

	var clientPubKey securemessage.GenericPublicKey
	if err := proto.Unmarshal(clientFinish.GetPublicKey(), &clientPubKey); err != nil {
		return nil, fmt.Errorf("parse client public key: %w", err)
	}
	ec := clientPubKey.GetEcP256PublicKey()
	if ec == nil {
		return nil, fmt.Errorf("client key is not EC_P256")
	}
	peerPoint := append([]byte{0x04}, append(pad32(ec.GetX()), pad32(ec.GetY())...)...)
	peer, err := curve.NewPublicKey(peerPoint)
	if err != nil {
		return nil, fmt.Errorf("invalid client point: %w", err)
	}

	dhs, err := priv.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}

	// --- derive keys ------------------------------------------------------
	// UKEY2: IKM is SHA256 of the raw shared secret; info is M1||M2, the two
	// handshake messages exactly as they appeared on the wire.
	ikm := sha256.Sum256(dhs)
	info := append(append([]byte{}, clientInitRaw...), serverInitRaw...)

	authString := hkdfBytes(ikm[:], []byte("UKEY2 v1 auth"), info, 32)
	nextSecret := hkdfBytes(ikm[:], []byte("UKEY2 v1 next"), info, 32)

	// D2D layer: one key per direction, then encrypt/MAC keys from each.
	d2dClient := hkdfBytes(nextSecret, saltD2D, []byte("client"), 32)
	d2dServer := hkdfBytes(nextSecret, saltD2D, []byte("server"), 32)

	return &sessionKeys{
		// The client encrypts with its key, so we decrypt with it.
		decryptKey:  hkdfBytes(d2dClient, saltEncMac, []byte("ENC:2"), 32),
		receiveHMAC: hkdfBytes(d2dClient, saltEncMac, []byte("SIG:1"), 32),
		encryptKey:  hkdfBytes(d2dServer, saltEncMac, []byte("ENC:2"), 32),
		sendHMAC:    hkdfBytes(d2dServer, saltEncMac, []byte("SIG:1"), 32),
		authString:  authString,
	}, nil
}

// hkdfBytes is HKDF-SHA256 with explicit salt/info, returning n bytes.
func hkdfBytes(ikm, salt, info []byte, n int) []byte {
	r := hkdf.New(sha256.New, ikm, salt, info)
	out := make([]byte, n)
	if _, err := io.ReadFull(r, out); err != nil {
		panic(err)
	}
	return out
}

// javaBigInt renders a positive integer the way Java's BigInteger.toByteArray
// does: big-endian two's complement, so a leading 0x00 is prepended when the
// top bit would otherwise read as a sign bit.
func javaBigInt(v *big.Int) []byte {
	b := v.Bytes()
	if len(b) > 0 && b[0]&0x80 != 0 {
		return append([]byte{0x00}, b...)
	}
	return b
}

// pad32 strips any Java sign byte and left-pads to a fixed 32-byte coordinate.
func pad32(b []byte) []byte {
	for len(b) > 32 && b[0] == 0x00 {
		b = b[1:]
	}
	if len(b) == 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// verifyHMAC is a constant-time compare helper used by the secure-message layer.
func verifyHMAC(key, data, tag []byte) bool {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return hmac.Equal(m.Sum(nil), tag)
}
