package main

// The D2D "SecureMessage" layer that wraps every frame after the UKEY2
// handshake.
//
// Shape of one encrypted frame on the wire:
//
//	SecureMessage{
//	  header_and_body = HeaderAndBody{
//	      header = Header{ sig=HMAC_SHA256, enc=AES_256_CBC, iv, public_metadata },
//	      body   = AES-256-CBC( DeviceToDeviceMessage{ seq, message } ),
//	  }
//	  signature = HMAC-SHA256(header_and_body)
//	}
//
// and `message` is itself a serialized connections.OfflineFrame. Encrypt-then-MAC:
// the HMAC covers the serialized HeaderAndBody, so it must be verified before
// anything inside is parsed.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"google.golang.org/protobuf/proto"

	"kindshare/pb/securegcm"
	"kindshare/pb/securemessage"
)

// encrypt wraps an OfflineFrame payload into a SecureMessage.
func (k *sessionKeys) encrypt(payload []byte, seq int32) ([]byte, error) {
	inner, err := proto.Marshal(&securegcm.DeviceToDeviceMessage{
		Message:        payload,
		SequenceNumber: proto.Int32(seq),
	})
	if err != nil {
		return nil, err
	}

	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k.encryptKey)
	if err != nil {
		return nil, err
	}
	plain := pkcs7Pad(inner, aes.BlockSize)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, plain)

	meta, err := proto.Marshal(&securegcm.GcmMetadata{
		Type:    securegcm.Type_DEVICE_TO_DEVICE_MESSAGE.Enum(),
		Version: proto.Int32(1),
	})
	if err != nil {
		return nil, err
	}

	hb, err := proto.Marshal(&securemessage.HeaderAndBody{
		Header: &securemessage.Header{
			SignatureScheme:  securemessage.SigScheme_HMAC_SHA256.Enum(),
			EncryptionScheme: securemessage.EncScheme_AES_256_CBC.Enum(),
			Iv:               iv,
			PublicMetadata:   meta,
		},
		Body: ct,
	})
	if err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, k.sendHMAC)
	mac.Write(hb)

	return proto.Marshal(&securemessage.SecureMessage{
		HeaderAndBody: hb,
		Signature:     mac.Sum(nil),
	})
}

// decrypt unwraps a SecureMessage and returns the inner OfflineFrame bytes.
func (k *sessionKeys) decrypt(raw []byte) ([]byte, int32, error) {
	var sm securemessage.SecureMessage
	if err := proto.Unmarshal(raw, &sm); err != nil {
		return nil, 0, fmt.Errorf("parse SecureMessage: %w", err)
	}

	// Verify before parsing anything inside - the MAC is the only thing
	// standing between us and attacker-controlled protobuf.
	if !verifyHMAC(k.receiveHMAC, sm.GetHeaderAndBody(), sm.GetSignature()) {
		return nil, 0, fmt.Errorf("HMAC mismatch")
	}

	var hb securemessage.HeaderAndBody
	if err := proto.Unmarshal(sm.GetHeaderAndBody(), &hb); err != nil {
		return nil, 0, fmt.Errorf("parse HeaderAndBody: %w", err)
	}

	block, err := aes.NewCipher(k.decryptKey)
	if err != nil {
		return nil, 0, err
	}
	iv := hb.GetHeader().GetIv()
	if len(iv) != aes.BlockSize {
		return nil, 0, fmt.Errorf("bad IV length %d", len(iv))
	}
	body := hb.GetBody()
	if len(body)%aes.BlockSize != 0 || len(body) == 0 {
		return nil, 0, fmt.Errorf("body is not a whole number of blocks (%d)", len(body))
	}
	plain := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, body)
	plain, err = pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return nil, 0, err
	}

	var d2d securegcm.DeviceToDeviceMessage
	if err := proto.Unmarshal(plain, &d2d); err != nil {
		return nil, 0, fmt.Errorf("parse DeviceToDeviceMessage: %w", err)
	}
	return d2d.GetMessage(), d2d.GetSequenceNumber(), nil
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, fmt.Errorf("bad padded length %d", len(b))
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) {
		return nil, fmt.Errorf("bad padding byte %d", n)
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, fmt.Errorf("inconsistent padding")
		}
	}
	return b[:len(b)-n], nil
}
