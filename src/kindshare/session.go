package main

// The Nearby Sharing frame state machine, which runs on top of the encrypted
// D2D channel.
//
// Everything meaningful travels as a "payload": the OfflineFrame layer carries
// PAYLOAD_TRANSFER frames, each a chunk of a larger payload identified by id.
// BYTES payloads reassemble into a sharing.Frame (the actual protocol); FILE
// payloads are the file contents and stream straight to disk.
//
// Expected order once encryption is up:
//
//	<- PairedKeyEncryption      -> PairedKeyEncryption
//	<- PairedKeyResult          -> PairedKeyResult(UNABLE)
//	<- Introduction (file list) -> ConnectionResponse(ACCEPT)
//	<- PAYLOAD_TRANSFER(FILE) chunks ... written to disk

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"kindshare/pb/connections"
	"kindshare/pb/sharing"
)

type incomingFile struct {
	name    string
	size    int64
	f       *os.File
	written int64
	path    string
}

type session struct {
	conn net.Conn
	keys *sessionKeys
	dest string

	mu  sync.Mutex
	seq int32

	// BYTES payloads accumulate here until their last chunk arrives.
	bytesBuf map[int64][]byte

	// FILE payloads, keyed by payload id, described by the Introduction.
	files map[int64]*incomingFile
}

func newSession(c net.Conn, k *sessionKeys, dest string) *session {
	return &session{
		conn:     c,
		keys:     k,
		dest:     dest,
		bytesBuf: map[int64][]byte{},
		files:    map[int64]*incomingFile{},
	}
}

// sendOffline encrypts and writes one OfflineFrame.
func (s *session) sendOffline(f *connections.OfflineFrame) error {
	b, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	enc, err := s.keys.encrypt(b, seq)
	if err != nil {
		return err
	}
	return writeFrame(s.conn, enc)
}

// sendSharingFrame packages a sharing.Frame as a BYTES payload: one chunk with
// the data, then a zero-length chunk flagged LAST_CHUNK. The peer will not act
// on a payload until it sees that terminator.
func (s *session) sendSharingFrame(f *sharing.Frame) error {
	body, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	id := randInt64()

	data := &connections.OfflineFrame{
		Version: connections.OfflineFrame_V1.Enum(),
		V1: &connections.V1Frame{
			Type: connections.V1Frame_PAYLOAD_TRANSFER.Enum(),
			PayloadTransfer: &connections.PayloadTransferFrame{
				PacketType: connections.PayloadTransferFrame_DATA.Enum(),
				PayloadHeader: &connections.PayloadTransferFrame_PayloadHeader{
					Id:          proto.Int64(id),
					Type:        connections.PayloadTransferFrame_PayloadHeader_BYTES.Enum(),
					TotalSize:   proto.Int64(int64(len(body))),
					IsSensitive: proto.Bool(false),
				},
				PayloadChunk: &connections.PayloadTransferFrame_PayloadChunk{
					Offset: proto.Int64(0),
					Flags:  proto.Int32(0),
					Body:   body,
				},
			},
		},
	}
	if err := s.sendOffline(data); err != nil {
		return err
	}

	last := proto.Clone(data).(*connections.OfflineFrame)
	last.V1.PayloadTransfer.PayloadChunk = &connections.PayloadTransferFrame_PayloadChunk{
		Offset: proto.Int64(int64(len(body))),
		Flags:  proto.Int32(1), // LAST_CHUNK
	}
	return s.sendOffline(last)
}

func (s *session) sendKeepAliveAck() error {
	return s.sendOffline(&connections.OfflineFrame{
		Version: connections.OfflineFrame_V1.Enum(),
		V1: &connections.V1Frame{
			Type:      connections.V1Frame_KEEP_ALIVE.Enum(),
			KeepAlive: &connections.KeepAliveFrame{Ack: proto.Bool(true)},
		},
	})
}

// handleOffline processes one decrypted OfflineFrame.
func (s *session) handleOffline(f *connections.OfflineFrame) error {
	v1 := f.GetV1()
	switch v1.GetType() {
	case connections.V1Frame_KEEP_ALIVE:
		return s.sendKeepAliveAck()

	case connections.V1Frame_DISCONNECTION:
		return fmt.Errorf("peer disconnected")

	case connections.V1Frame_PAYLOAD_TRANSFER:
		return s.handlePayload(v1.GetPayloadTransfer())
	}
	return nil
}

func (s *session) handlePayload(pt *connections.PayloadTransferFrame) error {
	h := pt.GetPayloadHeader()
	c := pt.GetPayloadChunk()
	id := h.GetId()
	last := c.GetFlags()&1 != 0

	switch h.GetType() {
	case connections.PayloadTransferFrame_PayloadHeader_BYTES:
		s.bytesBuf[id] = append(s.bytesBuf[id], c.GetBody()...)
		if !last {
			return nil
		}
		buf := s.bytesBuf[id]
		delete(s.bytesBuf, id)
		var sf sharing.Frame
		if err := proto.Unmarshal(buf, &sf); err != nil {
			log.Printf("     (bytes payload %d is not a sharing.Frame: %v)", id, err)
			return nil
		}
		return s.handleSharing(&sf)

	case connections.PayloadTransferFrame_PayloadHeader_FILE:
		fi := s.files[id]
		if fi == nil {
			// A file we were never introduced to; ignore rather than guess.
			return nil
		}
		if fi.f == nil {
			if err := os.MkdirAll(s.dest, 0o755); err != nil {
				return err
			}
			out, err := os.Create(fi.path)
			if err != nil {
				return err
			}
			fi.f = out
			log.Printf("     receiving %q -> %s", fi.name, fi.path)
		}
		if body := c.GetBody(); len(body) > 0 {
			n, err := fi.f.Write(body)
			if err != nil {
				return err
			}
			fi.written += int64(n)
		}
		if last {
			fi.f.Close()
			log.Printf("     RECEIVED %s (%d/%d bytes)", fi.path, fi.written, fi.size)
			delete(s.files, id)
			filesReceived.Add(1)
			lastFileName.Store(fi.name)

			// Acknowledge the payload. Without this the sender's UI never
			// leaves "sending" - it keeps the connection alive waiting for
			// confirmation that the bytes landed.
			if err := s.sendPayloadAck(h); err != nil {
				return err
			}

			// Everything offered has arrived: say so and hang up, which is what
			// moves the sender to "Sent" rather than leaving it spinning.
			if len(s.files) == 0 {
				log.Printf("     all files received - disconnecting")
				if err := s.sendDisconnection(); err != nil {
					log.Printf("     (disconnection frame failed: %v)", err)
				}
				return errTransferComplete
			}
		}
	}
	return nil
}

// errTransferComplete ends the read loop cleanly after a successful transfer.
var errTransferComplete = fmt.Errorf("transfer complete")

func (s *session) sendPayloadAck(h *connections.PayloadTransferFrame_PayloadHeader) error {
	return s.sendOffline(&connections.OfflineFrame{
		Version: connections.OfflineFrame_V1.Enum(),
		V1: &connections.V1Frame{
			Type: connections.V1Frame_PAYLOAD_TRANSFER.Enum(),
			PayloadTransfer: &connections.PayloadTransferFrame{
				PacketType: connections.PayloadTransferFrame_PAYLOAD_ACK.Enum(),
				PayloadHeader: &connections.PayloadTransferFrame_PayloadHeader{
					Id:        proto.Int64(h.GetId()),
					Type:      h.GetType().Enum(),
					TotalSize: proto.Int64(h.GetTotalSize()),
				},
				// A PAYLOAD_ACK needs only the header - ControlMessage belongs
				// with PacketType CONTROL - but NearDrop checks every frame it
				// receives for a chunk with an offset and flags before it looks
				// at the packet type, and throws a protocol error when one is
				// missing. It usually hangs up before reading our ack, so this
				// only bites sometimes, and when it does the file has already
				// landed and the Mac still reports a failure. Sending a chunk
				// that describes what we have costs nothing; senders that read
				// the packet type ignore it.
				PayloadChunk: &connections.PayloadTransferFrame_PayloadChunk{
					Offset: proto.Int64(h.GetTotalSize()),
					Flags:  proto.Int32(0),
				},
			},
		},
	})
}

func (s *session) sendDisconnection() error {
	return s.sendOffline(&connections.OfflineFrame{
		Version: connections.OfflineFrame_V1.Enum(),
		V1: &connections.V1Frame{
			Type:          connections.V1Frame_DISCONNECTION.Enum(),
			Disconnection: &connections.DisconnectionFrame{},
		},
	})
}

func (s *session) handleSharing(f *sharing.Frame) error {
	v1 := f.GetV1()
	log.Printf("     sharing frame: %v", v1.GetType())

	switch v1.GetType() {
	case sharing.V1Frame_PAIRED_KEY_ENCRYPTION:
		// We have no paired certificates, so this is deliberately random: the
		// peer cannot verify us, which downgrades to the "unknown device"
		// experience rather than failing outright.
		return s.sendSharingFrame(&sharing.Frame{
			Version: sharing.Frame_V1.Enum(),
			V1: &sharing.V1Frame{
				Type: sharing.V1Frame_PAIRED_KEY_ENCRYPTION.Enum(),
				PairedKeyEncryption: &sharing.PairedKeyEncryptionFrame{
					SignedData:   randBytes(72),
					SecretIdHash: randBytes(6),
				},
			},
		})

	case sharing.V1Frame_PAIRED_KEY_RESULT:
		return s.sendSharingFrame(&sharing.Frame{
			Version: sharing.Frame_V1.Enum(),
			V1: &sharing.V1Frame{
				Type: sharing.V1Frame_PAIRED_KEY_RESULT.Enum(),
				PairedKeyResult: &sharing.PairedKeyResultFrame{
					Status: sharing.PairedKeyResultFrame_UNABLE.Enum(),
				},
			},
		})

	case sharing.V1Frame_INTRODUCTION:
		intro := v1.GetIntroduction()
		if len(intro.GetFileMetadata()) == 0 {
			log.Printf("     introduction with no files; rejecting")
			return s.sendSharingFrame(rejectFrame())
		}
		for _, m := range intro.GetFileMetadata() {
			name := safeBase(m.GetName())
			s.files[m.GetPayloadId()] = &incomingFile{
				name: name,
				size: m.GetSize(),
				path: filepath.Join(s.dest, name),
			}
			log.Printf("     offered: %q (%d bytes, %v)", name, m.GetSize(), m.GetType())
		}
		// Auto-accept: this device exists to receive its owner's books, and
		// there is no practical way to prompt mid-transfer on e-ink.
		return s.sendSharingFrame(&sharing.Frame{
			Version: sharing.Frame_V1.Enum(),
			V1: &sharing.V1Frame{
				Type: sharing.V1Frame_RESPONSE.Enum(),
				ConnectionResponse: &sharing.ConnectionResponseFrame{
					Status: sharing.ConnectionResponseFrame_ACCEPT.Enum(),
				},
			},
		})
	}
	return nil
}

func rejectFrame() *sharing.Frame {
	return &sharing.Frame{
		Version: sharing.Frame_V1.Enum(),
		V1: &sharing.V1Frame{
			Type: sharing.V1Frame_RESPONSE.Enum(),
			ConnectionResponse: &sharing.ConnectionResponseFrame{
				Status: sharing.ConnectionResponseFrame_REJECT.Enum(),
			},
		},
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func randInt64() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return int64(binary.BigEndian.Uint64(b[:]) >> 1)
}

// safeBase strips directory components so a sender cannot write outside dest.
func safeBase(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, `\`, "/"))
	if n == "" || n == "." || n == ".." || n == "/" {
		return "received-" + fmt.Sprint(randInt64()%100000)
	}
	return n
}
