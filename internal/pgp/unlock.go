package pgp

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	_ "crypto/md5" // S2K hashes, registered for crypto.Hash
	"crypto/sha1"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"errors"
	"fmt"
)

// String-to-key specifier types (§3.7.1) and gpg's extension.
const (
	s2kSimple   = 0
	s2kSalted   = 1
	s2kIterated = 3
	s2kGNU      = 101 // gpg: no secret part in the packet (a stub)
)

// s2kHashes are the S2K hash algorithms (§9.4) a key export uses.
var s2kHashes = map[byte]crypto.Hash{1: crypto.MD5, 2: crypto.SHA1, 8: crypto.SHA256, 9: crypto.SHA384, 10: crypto.SHA512, 11: crypto.SHA224}

// s2kCiphers are the symmetric algorithms (§9.2) a secret key may be
// encrypted with, by their key size; gpg 2.1 and later export with AES-128.
var s2kCiphers = map[byte]struct {
	keyLen int
	block  func([]byte) (cipher.Block, error)
}{
	2: {24, des.NewTripleDESCipher},
	7: {16, aes.NewCipher}, 8: {24, aes.NewCipher}, 9: {32, aes.NewCipher},
}

// Unlock returns the ASCII-armored private key block armored with the
// secret part of every key decrypted with passphrase and stored in the
// clear, the form ParsePrivateKey reads (gpg --export-secret-keys of a key
// with a passphrase). Keys already in the clear and gpg's stubs are kept
// as they are. A wrong passphrase is an error.
func Unlock(armored, passphrase string) (string, error) {
	raw, err := unarmor(armored, "PGP PRIVATE KEY BLOCK")
	if err != nil {
		return "", err
	}
	pkts, err := parsePackets(raw)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	for _, p := range pkts {
		if p.tag == tagSecretKey || p.tag == tagSecretSubkey {
			if p.body, err = unlockPacket(p.body, []byte(passphrase)); err != nil {
				return "", err
			}
		}
		writePacket(&out, p.tag, p.body)
	}
	return armor("PGP PRIVATE KEY BLOCK", out.Bytes()), nil
}

// unlockPacket decrypts a secret key packet body (§5.5.3) protected with
// usage 254 (SHA-1 check) or 255 (checksum): cipher, S2K specifier, IV,
// then the CFB-encrypted secret MPIs and their check. It returns the body
// with usage 0, the MPIs and their checksum.
func unlockPacket(b, pass []byte) ([]byte, error) {
	n, err := publicKeyLen(b)
	if err != nil {
		return nil, err
	}
	rest := b[n:]
	if len(rest) < 3 || rest[0] == 0 || rest[2] == s2kGNU && (rest[0] == 254 || rest[0] == 255) {
		return b, nil // in the clear already, or a stub
	}
	usage := rest[0]
	if usage != 254 && usage != 255 {
		return nil, fmt.Errorf("secret key protection %d is not supported (gpg exports use 254)", usage)
	}
	c, ok := s2kCiphers[rest[1]]
	if !ok {
		return nil, fmt.Errorf("secret key cipher %d is not supported (AES and 3DES are)", rest[1])
	}
	key, rest, err := s2k(rest[2:], pass, c.keyLen)
	if err != nil {
		return nil, err
	}
	block, err := c.block(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if len(rest) < bs {
		return nil, errors.New("truncated secret key packet")
	}
	plain := make([]byte, len(rest)-bs)
	cipher.NewCFBDecrypter(block, rest[:bs]).XORKeyStream(plain, rest[bs:])
	var mpis []byte
	switch {
	case usage == 254 && len(plain) >= sha1.Size:
		mpis = plain[:len(plain)-sha1.Size]
		if h := sha1.Sum(mpis); !bytes.Equal(h[:], plain[len(mpis):]) {
			return nil, errors.New("wrong passphrase for the private key")
		}
	case usage == 255 && len(plain) >= 2:
		mpis = plain[:len(plain)-2]
		if s := sum16(mpis); byte(s>>8) != plain[len(mpis)] || byte(s) != plain[len(mpis)+1] {
			return nil, errors.New("wrong passphrase for the private key")
		}
	default:
		return nil, errors.New("truncated secret key packet")
	}
	s := sum16(mpis)
	out := append(append(append([]byte{}, b[:n]...), 0), mpis...)
	return append(out, byte(s>>8), byte(s)), nil
}

// s2k derives a key of keyLen octets from pass with the specifier at the
// start of b (§3.7.1) and returns what follows the specifier.
func s2k(b, pass []byte, keyLen int) (key, rest []byte, err error) {
	if len(b) < 2 {
		return nil, nil, errors.New("truncated string-to-key specifier")
	}
	h, ok := s2kHashes[b[1]]
	if !ok {
		return nil, nil, fmt.Errorf("string-to-key hash %d is not supported", b[1])
	}
	var salt []byte
	count := 0
	switch b[0] {
	case s2kSimple:
		rest = b[2:]
	case s2kSalted, s2kIterated:
		if len(b) < 10 || b[0] == s2kIterated && len(b) < 11 {
			return nil, nil, errors.New("truncated string-to-key specifier")
		}
		salt, rest = b[2:10], b[10:]
		if b[0] == s2kIterated {
			c := int(b[10])
			count, rest = (16+c&15)<<(c>>4+6), b[11:]
		}
	default:
		return nil, nil, fmt.Errorf("string-to-key type %d is not supported", b[0])
	}
	data := append(append([]byte{}, salt...), pass...)
	count = max(count, len(data))
	// The salt and passphrase repeat until count octets are hashed (gpg
	// exports use 65011712): written in chunks of whole repeats.
	chunk := data
	if len(data) > 0 {
		chunk = bytes.Repeat(data, max(1, 1<<16/len(data)))
	}
	// Each context starts with one more zero octet than the one before,
	// until the hashes give keyLen octets.
	for i := 0; len(key) < keyLen; i++ {
		d := h.New()
		d.Write(make([]byte, i))
		for left := count; left > 0; left -= len(chunk) {
			d.Write(chunk[:min(left, len(chunk))])
		}
		key = d.Sum(key)
	}
	return key[:keyLen], rest, nil
}
