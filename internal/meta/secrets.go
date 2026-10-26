package meta

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"commandcenter/internal/config"
)

// The stored secrets are the SMTP password, the LDAP bind password and a
// proxy's remoteAuth password and bearer token. They must be sent as they
// are, so they cannot be hashed. With a key (SetSecretKey) they are sealed
// with AES-256-GCM on write and opened on read, so a copy of meta.db or of a
// backup does not give them away (QA N-29). A sealed value is sealedPrefix
// followed by base64 of nonce and ciphertext; any other value is plain text,
// as stored without a key or by older builds.
const sealedPrefix = "enc:v1:"

var (
	errNoSecretKey = errors.New("it is encrypted, and no secret key is set: set COMMANDCENTER_SECRET_KEY or " +
		"secretKeyFile to the key it was encrypted with")
	errWrongSecretKey = errors.New("it does not decrypt with the secret key: it was encrypted with another key")
)

// sealer seals and opens secrets; the zero value has no key and stores
// secrets as given.
type sealer struct{ aead cipher.AEAD }

// SetSecretKey makes d seal secrets with key (32 bytes) from now on and open
// the sealed ones. Call it before d is used.
func (d *DB) SetSecretKey(key []byte) error {
	if len(key) != 32 {
		return errors.New("the secret key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	d.box = sealer{aead}
	return nil
}

func (s sealer) seal(v string) (string, error) {
	switch {
	case v == "":
		return v, nil
	case s.aead == nil:
		if strings.HasPrefix(v, sealedPrefix) { // it would read back as sealed
			return "", fmt.Errorf("a secret must not start with %q", sealedPrefix)
		}
		return v, nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return sealedPrefix + base64.RawStdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(v), nil)), nil
}

func (s sealer) open(v string) (string, error) {
	enc, ok := strings.CutPrefix(v, sealedPrefix)
	if !ok {
		return v, nil
	}
	if s.aead == nil {
		return "", errNoSecretKey
	}
	b, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil || len(b) < s.aead.NonceSize() {
		return "", errWrongSecretKey
	}
	n := s.aead.NonceSize()
	p, err := s.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return "", errWrongSecretKey
	}
	return string(p), nil
}

// sealValue returns a settings value with its secret sealed; values without
// one are returned as they are.
func (s sealer) sealValue(v any) (any, error) {
	var err error
	switch c := v.(type) {
	case *SMTP:
		if c != nil {
			return s.sealValue(*c)
		}
	case *LDAP:
		if c != nil {
			return s.sealValue(*c)
		}
	case SMTP:
		c.Password, err = s.seal(c.Password)
		return c, err
	case LDAP:
		c.BindPassword, err = s.seal(c.BindPassword)
		return c, err
	}
	return v, nil
}

// openValue opens the secret of a decoded settings value in place.
func (s sealer) openValue(key string, v any) (err error) {
	switch c := v.(type) {
	case *SMTP:
		c.Password, err = s.open(c.Password)
	case *LDAP:
		c.BindPassword, err = s.open(c.BindPassword)
	}
	if err != nil {
		return fmt.Errorf("setting %q: %w", key, err)
	}
	return nil
}

func (s sealer) openSettings(set *Settings) error {
	if err := s.openValue(SettingSMTP, &set.SMTP); err != nil {
		return err
	}
	return s.openValue(SettingLDAP, &set.LDAP)
}

// sealRepo returns def with its remoteAuth secrets sealed. def's own
// RemoteAuth is left as it is: the caller keeps the plain values.
func (s sealer) sealRepo(def config.Repository) (config.Repository, error) {
	if def.RemoteAuth == nil {
		return def, nil
	}
	ra := *def.RemoteAuth
	var err error
	if ra.Password, err = s.seal(ra.Password); err != nil {
		return def, fmt.Errorf("repository %q: remoteAuth password: %w", def.Name, err)
	}
	if ra.BearerToken, err = s.seal(ra.BearerToken); err != nil {
		return def, fmt.Errorf("repository %q: remoteAuth bearerToken: %w", def.Name, err)
	}
	def.RemoteAuth = &ra
	return def, nil
}

func (s sealer) openRepo(def *config.Repository) error {
	if def.RemoteAuth == nil {
		return nil
	}
	var err error
	if def.RemoteAuth.Password, err = s.open(def.RemoteAuth.Password); err != nil {
		return fmt.Errorf("repository %q: remoteAuth password: %w", def.Name, err)
	}
	if def.RemoteAuth.BearerToken, err = s.open(def.RemoteAuth.BearerToken); err != nil {
		return fmt.Errorf("repository %q: remoteAuth bearerToken: %w", def.Name, err)
	}
	return nil
}

// SealSecrets runs on every start with a key. It checks that every sealed
// secret opens with the key, then seals the ones still in plain text, in one
// transaction: nothing is written when a secret was sealed with another key.
// It returns how many settings and repository rows it rewrote.
func (d *DB) SealSecrets(ctx context.Context) (int, error) {
	if d.box.aead == nil {
		return 0, errors.New("no secret key is set")
	}
	// seal seals *v unless it is empty or sealed; a sealed one must open.
	seal := func(v *string, changed *bool) error {
		if *v == "" {
			return nil
		}
		if strings.HasPrefix(*v, sealedPrefix) {
			_, err := d.box.open(*v)
			return err
		}
		*changed = true
		var err error
		*v, err = d.box.seal(*v)
		return err
	}
	n := 0
	err := d.Tx(ctx, func(tx *Tx) error {
		n = 0
		// Rewritten as stored, without today's validation: only the secret changes.
		var smtp SMTP
		ldap := DefaultLDAP()
		for _, st := range []struct {
			key    string
			v      any
			secret *string
		}{{SettingSMTP, &smtp, &smtp.Password}, {SettingLDAP, &ldap, &ldap.BindPassword}} {
			found, err := getSettingJSON(ctx, tx.q, st.key, st.v)
			if err != nil {
				return err
			}
			changed := false
			if !found {
				continue
			}
			if err := seal(st.secret, &changed); err != nil {
				return fmt.Errorf("setting %q: %w", st.key, err)
			}
			if !changed {
				continue
			}
			b, err := json.Marshal(st.v)
			if err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, string(b), st.key); err != nil {
				return err
			}
			n++
		}
		rows, err := tx.q.QueryContext(ctx, `SELECT name, config FROM repositories WHERE json_extract(config, '$.remoteAuth') IS NOT NULL`)
		if err != nil {
			return err
		}
		var defs []config.Repository
		for rows.Next() {
			var name, js string
			if err := rows.Scan(&name, &js); err != nil {
				rows.Close()
				return err
			}
			var def config.Repository
			if err := json.Unmarshal([]byte(js), &def); err != nil {
				rows.Close()
				return fmt.Errorf("repository %q: decode stored definition: %w", name, err)
			}
			if def.RemoteAuth != nil {
				def.Name = name
				defs = append(defs, def)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, def := range defs {
			name, changed := def.Name, false
			if err := seal(&def.RemoteAuth.Password, &changed); err != nil {
				return fmt.Errorf("repository %q: remoteAuth password: %w", name, err)
			}
			if err := seal(&def.RemoteAuth.BearerToken, &changed); err != nil {
				return fmt.Errorf("repository %q: remoteAuth bearerToken: %w", name, err)
			}
			if !changed {
				continue
			}
			js, err := json.Marshal(def)
			if err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `UPDATE repositories SET config = ? WHERE name = ?`, string(js), name); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
