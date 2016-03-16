// Copyright (c) 2015 Monetas.
// Copyright 2016 Daniel Krawisz.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package keymgr

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/golangcrypto/nacl/secretbox"
	"github.com/DanielKrawisz/bmutil"
	"github.com/DanielKrawisz/bmutil/identity"
	"golang.org/x/crypto/pbkdf2"
)

const (
	// nonceSize is the size of the nonce (in bytes) used by secretbox.
	nonceSize = 24

	// saltLength is the desired length of salt used by PBKDF2.
	saltLength = 32

	// keySize is the size of the symmetric key for use with secretbox.
	keySize = 32

	// numIters is the number of iterations to be done by PBKDF2.
	numIters = 1 << 15

	// latestFileVersion is the most recent version of keyfile. This is how
	// the key manager can know whether to update the keyfile or not.
	latestFileVersion = 1
)

var (
	// ErrDecryptionFailed is returned when decryption of the key file fails.
	// This could be due to invalid passphrase or corrupt/tampered data.
	ErrDecryptionFailed = errors.New("invalid passphrase")

	// ErrDuplicateIdentity is returned by Import when the identity to be
	// imported already exists in the key manager.
	ErrDuplicateIdentity = errors.New("identity already in key manager")

	// ErrNonexistentIdentity is returned when the identity doesn't exist in the
	// key manager.
	ErrNonexistentIdentity = errors.New("identity doesn't exist")
)

// Manager is the key manager used for managing imported as well as
// hierarchically deterministic keys. It is safe for access from multiple
// goroutines.
type Manager struct {
	mutex sync.RWMutex
	db    *db
}

// New creates a new key manager and generates a master key from the provided
// seed. The seed must be between 128 and 512 bits and should be generated by a
// cryptographically secure random generation source. Refer to docs for
// hdkeychain.NewMaster and hdkeychain.GenerateSeed for more info.
func New(seed []byte) (*Manager, error) {
	mgr := &Manager{db: &db{}}

	// Generate master key.
	mKey, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}

	mgr.db.init()
	mgr.db.MasterKey = (*MasterKey)(mKey)
	mgr.db.Version = latestFileVersion

	return mgr, nil
}

// deriveKey is used to derive a 32 byte key for encryption/decryption
// operations with secretbox. It runs a large number of rounds of PBKDF2 on the
// password using the specified salt to arrive at the key.
func deriveKey(pass, salt []byte) *[keySize]byte {
	out := pbkdf2.Key(pass, salt, numIters, keySize, sha256.New)
	var key [keySize]byte
	copy(key[:], out)
	return &key
}

// FromEncrypted creates a new Manager object from the specified encrypted data
// and passphrase. The actual key used for decryption is derived from the salt,
// which is a part of enc, and the passphrase using PBKDF2.
func FromEncrypted(enc, pass []byte) (*Manager, error) {
	if len(enc) < secretbox.Overhead+nonceSize+saltLength {
		return nil, errors.New("encrypted data too small")
	}

	var nonce [nonceSize]byte
	copy(nonce[:], enc[:nonceSize])
	n := nonceSize

	salt := enc[n : n+saltLength]
	n += saltLength

	contents, success := secretbox.Open(nil, enc[n:], &nonce,
		deriveKey(pass, salt))

	if !success {
		return nil, ErrDecryptionFailed
	}

	mgr := &Manager{db: &db{}}
	mgr.db.init()
	err := json.Unmarshal(contents, mgr.db)
	if err != nil {
		return nil, err
	}

	// Upgrade previous version database to new version.
	err = mgr.db.checkAndUpgrade()
	if err != nil {
		return nil, err
	}

	return mgr, nil
}

// SaveEncrypted encrypts the current state of the key manager with the
// specified password and returns it. The salt used as input to PBKDF2 as well
// as nonce for input to secretbox are randomly generated. The actual key used
// for encryption is derived from the salt and the passphrase using PBKDF2.
func (mgr *Manager) SaveEncrypted(pass []byte) ([]byte, error) {
	plain, err := mgr.ExportUnencrypted()
	if err != nil {
		return nil, err
	}

	var nonce [nonceSize]byte
	_, err = rand.Read(nonce[:])
	if err != nil {
		return nil, err
	}

	salt := make([]byte, saltLength)
	_, err = rand.Read(salt)
	if err != nil {
		return nil, err
	}

	ret := make([]byte, nonceSize+saltLength)
	copy(ret[:nonceSize], nonce[:])
	copy(ret[nonceSize:], salt)
	ret = secretbox.Seal(ret, plain, &nonce, deriveKey(pass, salt))

	return ret, nil
}

// ExportUnencrypted exports the state of the key manager in an unencrypted
// state. It's useful for debugging or manual inspection of keys. Note that
// the key manager CANNOT import the unencrypted JSON format.
func (mgr *Manager) ExportUnencrypted() ([]byte, error) {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	return json.Marshal(mgr.db)
}

// ImportIdentity imports an existing identity into the key manager. It's useful
// for users who have existing identities or want to subscribe to channels.
func (mgr *Manager) ImportIdentity(privID *PrivateID) error {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()

	// Check if the identity already exists.
	err := mgr.forEach(func(id *PrivateID) error {
		if bytes.Equal(id.Tag(), privID.Tag()) {
			return ErrDuplicateIdentity
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Insert a copy into database.
	copyID := *privID
	mgr.db.ImportedIDs = append(mgr.db.ImportedIDs, &copyID)

	return nil
}

// RemoveImported removes an imported identity from the key manager.
func (mgr *Manager) RemoveImported(privID *PrivateID) error {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()

	tag := privID.Tag()

	for i, id := range mgr.db.ImportedIDs {
		if bytes.Equal(tag, id.Tag()) {
			// Delete element from slice.
			a := mgr.db.ImportedIDs

			// From https://github.com/golang/go/wiki/SliceTricks
			copy(a[i:], a[i+1:])
			a[len(a)-1] = nil // or the zero value of T
			mgr.db.ImportedIDs = a[:len(a)-1]

			return nil
		}
	}

	return ErrNonexistentIdentity
}

// NewHDIdentity generates a new HD identity and numbers it based on previously
// derived identities. If 2^32 identities have already been generated, new
// identities would be duplicates because of overflow problems.
func (mgr *Manager) NewHDIdentity(stream uint32) *PrivateID {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()

	var privID *identity.Private
	var err error

	// We use a loop because identity generation might fail, although the odds
	// may be extremely small.
	for i := uint32(0); true; i++ {
		privID, err = identity.NewHD((*hdkeychain.ExtendedKey)(mgr.db.MasterKey),
			mgr.db.NewIDIndex+i, stream)
		if err == nil {
			mgr.db.NewIDIndex += i + 1
			break
		}
	}

	id := &PrivateID{
		Private: *privID,
		IsChan:  false,
	}

	mgr.db.DerivedIDs = append(mgr.db.DerivedIDs, id)

	ret := *id // copy
	return &ret
}

func (mgr *Manager) forEach(f func(*PrivateID) error) error {
	// Go through HD identities first.
	for _, id := range mgr.db.DerivedIDs {
		err := f(id)
		if err != nil {
			return err
		}
	}

	// Go through imported identities.
	for _, id := range mgr.db.ImportedIDs {
		err := f(id)
		if err != nil {
			return err
		}
	}

	return nil
}

// ForEach runs the specified function for all the identities stored in the key
// manager. It does not return until the function has been invoked for all keys
// and breaks early on error. The function must not modify the private keys
// in any way.
func (mgr *Manager) ForEach(f func(*PrivateID) error) error {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	return mgr.forEach(f)
}

// LookupByAddress looks up a private identity in the key manager by its
// address. If no matching identity can be found, ErrNonexistentIdentity is
// returned.
func (mgr *Manager) LookupByAddress(address string) (*PrivateID, error) {
	addr, err := bmutil.DecodeAddress(address)
	if err != nil {
		return nil, err
	}

	var res PrivateID
	err = mgr.ForEach(func(id *PrivateID) error {
		if bytes.Equal(addr.Ripe[:], id.Address.Ripe[:]) &&
			addr.Stream == id.Address.Stream &&
			addr.Version == id.Address.Version {
			res = *id
			return errors.New("Found a match, so break.")
		}
		return nil
	})
	if err == nil { // No match.
		return nil, ErrNonexistentIdentity
	}
	return &res, nil

}

// NumImported returns the number of imported identities that the key manager
// has in the database.
func (mgr *Manager) NumImported() int {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	return len(mgr.db.ImportedIDs)
}

// NumDeterministic returns the number of identities that have been created
// deterministically (according to BIP-BM01).
func (mgr *Manager) NumDeterministic() int {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	return len(mgr.db.DerivedIDs)
}
