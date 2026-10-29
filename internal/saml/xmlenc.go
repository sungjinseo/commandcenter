package saml

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"errors"
	"fmt"
)

// Decryption of an EncryptedAssertion (XML Encryption 1.1): the content key
// is transported with RSA-OAEP under the service provider's key, the
// assertion encrypted with AES-GCM or AES-CBC. RSA PKCS#1 v1.5 key
// transport is refused (Bleichenbacher), and every failure past the
// algorithm checks is the same error, so the answers are no oracle.

const (
	algAES128CBC = "http://www.w3.org/2001/04/xmlenc#aes128-cbc"
	algAES192CBC = "http://www.w3.org/2001/04/xmlenc#aes192-cbc"
	algAES256CBC = "http://www.w3.org/2001/04/xmlenc#aes256-cbc"
	algAES128GCM = "http://www.w3.org/2009/xmlenc11#aes128-gcm"
	algAES192GCM = "http://www.w3.org/2009/xmlenc11#aes192-gcm"
	algAES256GCM = "http://www.w3.org/2009/xmlenc11#aes256-gcm"
	algRSAOAEP   = "http://www.w3.org/2001/04/xmlenc#rsa-oaep-mgf1p"
	algRSAOAEP11 = "http://www.w3.org/2009/xmlenc11#rsa-oaep"
	typeElement  = "http://www.w3.org/2001/04/xmlenc#Element"
)

var errDecrypt = errors.New("the encrypted assertion could not be decrypted")

var blockAlgs = map[string]struct {
	keyLen int
	gcm    bool
}{
	algAES128CBC: {16, false}, algAES192CBC: {24, false}, algAES256CBC: {32, false},
	algAES128GCM: {16, true}, algAES192GCM: {24, true}, algAES256GCM: {32, true},
}

var digestAlgs = map[string]crypto.Hash{
	algSHA1: crypto.SHA1, algSHA256: crypto.SHA256,
	"http://www.w3.org/2001/04/xmldsig-more#sha384": crypto.SHA384, "http://www.w3.org/2001/04/xmlenc#sha512": crypto.SHA512,
}

var mgfAlgs = map[string]crypto.Hash{
	nsXENC11 + "mgf1sha1": crypto.SHA1, nsXENC11 + "mgf1sha224": crypto.SHA224, nsXENC11 + "mgf1sha256": crypto.SHA256,
	nsXENC11 + "mgf1sha384": crypto.SHA384, nsXENC11 + "mgf1sha512": crypto.SHA512,
}

// decrypt returns the plaintext of EncryptedAssertion ea: the assertion's XML.
func decrypt(ea *node, key *rsa.PrivateKey) ([]byte, error) {
	ed, err := ea.child(nsXENC, "EncryptedData")
	if err != nil {
		return nil, err
	}
	if t, ok := ed.attr("Type"); ok && t != typeElement {
		return nil, fmt.Errorf("unsupported EncryptedData type %q", t)
	}
	em, err := ed.child(nsXENC, "EncryptionMethod")
	if err != nil {
		return nil, err
	}
	alg, _ := em.attr("Algorithm")
	block, ok := blockAlgs[alg]
	if !ok {
		return nil, fmt.Errorf("unsupported assertion encryption %q", alg)
	}
	// The content key: in EncryptedData's KeyInfo, or beside EncryptedData.
	var eks []*node
	for _, ki := range ed.children(nsDS, "KeyInfo") {
		eks = append(eks, ki.children(nsXENC, "EncryptedKey")...)
	}
	if len(eks) == 0 {
		eks = ea.children(nsXENC, "EncryptedKey")
	}
	if len(eks) != 1 {
		return nil, errors.New("the encrypted assertion must carry exactly one EncryptedKey")
	}
	opts, err := oaepOptions(eks[0])
	if err != nil {
		return nil, err
	}
	wrapped, err := cipherValue(eks[0])
	if err != nil {
		return nil, err
	}
	data, err := cipherValue(ed)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, errDecrypt
	}
	cek, err := key.Decrypt(nil, wrapped, opts)
	if err != nil || len(cek) != block.keyLen {
		return nil, errDecrypt
	}
	c, err := aes.NewCipher(cek)
	if err != nil {
		return nil, errDecrypt
	}
	if block.gcm {
		g, err := cipher.NewGCM(c)
		if err != nil || len(data) < g.NonceSize()+g.Overhead() {
			return nil, errDecrypt
		}
		out, err := g.Open(nil, data[:g.NonceSize()], data[g.NonceSize():], nil)
		if err != nil {
			return nil, errDecrypt
		}
		return out, nil
	}
	// CBC: the IV, then whole blocks; XML Encryption padding says only its
	// length in the last byte.
	if len(data) < 2*aes.BlockSize || len(data)%aes.BlockSize != 0 {
		return nil, errDecrypt
	}
	out := make([]byte, len(data)-aes.BlockSize)
	cipher.NewCBCDecrypter(c, data[:aes.BlockSize]).CryptBlocks(out, data[aes.BlockSize:])
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize {
		return nil, errDecrypt
	}
	return out[:len(out)-pad], nil
}

// oaepOptions reads an EncryptedKey's RSA-OAEP parameters.
func oaepOptions(ek *node) (*rsa.OAEPOptions, error) {
	em, err := ek.child(nsXENC, "EncryptionMethod")
	if err != nil {
		return nil, err
	}
	opts := &rsa.OAEPOptions{Hash: crypto.SHA1, MGFHash: crypto.SHA1}
	alg, _ := em.attr("Algorithm")
	if alg != algRSAOAEP && alg != algRSAOAEP11 {
		return nil, fmt.Errorf("unsupported key transport %q (only RSA-OAEP is)", alg)
	}
	kids, err := em.elements()
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		a, _ := k.attr("Algorithm")
		switch {
		case k.is(nsDS, "DigestMethod"):
			h, ok := digestAlgs[a]
			if !ok {
				return nil, fmt.Errorf("unsupported RSA-OAEP digest %q", a)
			}
			opts.Hash = h
		case k.is(nsXENC11, "MGF") && alg == algRSAOAEP11:
			h, ok := mgfAlgs[a]
			if !ok {
				return nil, fmt.Errorf("unsupported RSA-OAEP mask generation %q", a)
			}
			opts.MGFHash = h
		default:
			return nil, fmt.Errorf("unsupported <%s> in the key transport method", k.local)
		}
	}
	return opts, nil
}

func cipherValue(n *node) ([]byte, error) {
	cd, err := n.child(nsXENC, "CipherData")
	if err != nil {
		return nil, err
	}
	cv, err := cd.child(nsXENC, "CipherValue")
	if err != nil {
		return nil, errors.New("only an inline CipherValue is supported")
	}
	b, err := decode64(cv.content())
	if err != nil {
		return nil, errors.New("a CipherValue is not base64")
	}
	return b, nil
}
