package ui

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

// The secrets block of an exported configuration: one JSON object
// {section: {key: {field: value}}} sealed with AES-256-GCM under a key
// derived from the passphrase with scrypt (N=32768, r=8, p=1: 32 MiB and
// about 0.1 s per derivation; a 16-byte random salt, a 12-byte random nonce;
// the additional data is confSecretsAAD). The parameters are stored with
// the block; an apply accepts N up to 32768, r = 8 and p up to 4, so a
// document cannot make a low-memory server spend more, and one derivation
// runs at a time.

const (
	confKDF       = "scrypt"
	confCipher    = "AES-256-GCM"
	confScryptN   = 1 << 15
	confScryptR   = 8
	confScryptP   = 1
	confMaxN      = 1 << 15
	confMinPhrase = 12
)

var (
	confSecretsAAD = []byte("commandcenter configuration secrets v1")
	confKDFSlot    sync.Mutex // one 32 MiB derivation at a time
)

type confSecretsBox struct {
	Cipher string `json:"cipher"`
	KDF    string `json:"kdf"`
	N      int    `json:"n"`
	R      int    `json:"r"`
	P      int    `json:"p"`
	Salt   string `json:"salt"`
	Nonce  string `json:"nonce"`
	Data   string `json:"data"`
}

func checkPassphrase(p string) error {
	if utf8.RuneCountInString(p) < confMinPhrase {
		return fmt.Errorf("the secrets passphrase (%s) must be at least %d characters", ConfigPassphraseHeader, confMinPhrase)
	}
	return nil
}

func confAEAD(pass string, salt []byte, n, r, p int) (cipher.AEAD, error) {
	confKDFSlot.Lock()
	key, err := scrypt.Key([]byte(pass), salt, n, r, p, 32)
	confKDFSlot.Unlock()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealConfigSecrets(payload map[string]map[string]map[string]string, pass string) (confSecretsBox, error) {
	plain, err := json.Marshal(payload)
	if err != nil {
		return confSecretsBox{}, err
	}
	salt, nonce := make([]byte, 16), make([]byte, 12)
	rand.Read(salt)
	rand.Read(nonce)
	aead, err := confAEAD(pass, salt, confScryptN, confScryptR, confScryptP)
	if err != nil {
		return confSecretsBox{}, err
	}
	enc := base64.StdEncoding.EncodeToString
	return confSecretsBox{Cipher: confCipher, KDF: confKDF, N: confScryptN, R: confScryptR, P: confScryptP,
		Salt: enc(salt), Nonce: enc(nonce), Data: enc(aead.Seal(nil, nonce, plain, confSecretsAAD))}, nil
}

func openConfigSecrets(v any, pass string) (map[string]map[string]map[string]string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var box confSecretsBox
	if err := json.Unmarshal(b, &box); err != nil {
		return nil, errors.New("not a secrets block")
	}
	switch {
	case box.Cipher != confCipher || box.KDF != confKDF:
		return nil, fmt.Errorf("cipher must be %s and kdf %s", confCipher, confKDF)
	case box.N < 2 || box.N > confMaxN || box.N&(box.N-1) != 0 || box.R != confScryptR || box.P < 1 || box.P > 4:
		return nil, fmt.Errorf("scrypt parameters out of range (n a power of two up to %d, r %d, p 1 to 4)", confMaxN, confScryptR)
	}
	dec := base64.StdEncoding.DecodeString
	salt, err1 := dec(box.Salt)
	nonce, err2 := dec(box.Nonce)
	data, err3 := dec(box.Data)
	if err := errors.Join(err1, err2, err3); err != nil || len(nonce) != 12 || len(salt) < 8 {
		return nil, errors.New("salt, nonce or data is not valid base64 of the right length")
	}
	aead, err := confAEAD(pass, salt, box.N, box.R, box.P)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, data, confSecretsAAD)
	if err != nil {
		return nil, errors.New("they do not decrypt: wrong passphrase, or the block was changed")
	}
	var out map[string]map[string]map[string]string
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, errors.New("the decrypted block is not the secrets object")
	}
	return out, nil
}
