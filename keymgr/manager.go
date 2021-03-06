// Copyright (c) 2015 Monetas.
// Copyright 2016 Daniel Krawisz.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package keymgr

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"
	"io"
	"strings"
	"strconv"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/golangcrypto/nacl/secretbox"
	"github.com/DanielKrawisz/bmutil/identity"
	"github.com/DanielKrawisz/bmutil/pow"
	"golang.org/x/crypto/pbkdf2"
	ini "github.com/vaughan0/go-ini"
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

	// ImportedIDs contains all IDs that weren't derived from the master key.
	// This could include channels or addresses imported from PyBitmessage.
	importedIDs []string 

	// DerivedIDs contains all IDs derived from the master key.
	derivedIDs []string 
}

// New creates a new key manager and generates a master key from the provided
// seed. The seed must be between 128 and 512 bits and should be generated by a
// cryptographically secure random generation source. Refer to docs for
// hdkeychain.NewMaster and hdkeychain.GenerateSeed for more info.
func New(seed []byte) (*Manager, error) {
	// Generate master key.
	mKey, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}
	
	mgr := &Manager{
		db: newDb((*MasterKey)(mKey), latestFileVersion),
		importedIDs : make([]string, 0, dbInitSize),
		derivedIDs : make([]string, 0, dbInitSize),
	}

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
	
	return FromPlaintext(bytes.NewReader(contents))
}

// Imports a key manager from plaintext. 
func FromPlaintext(r io.Reader) (*Manager, error) {	
	db, err := openDb(r)
	if err != nil {
		return nil, err
	}
	mgr := &Manager{
		db: db,
		importedIDs : make([]string, 0, len(db.IDs)),
		derivedIDs : make([]string, 0, len(db.IDs)),
	}
	
	for addr, id := range db.IDs {
		if (id.Imported) {
			mgr.importedIDs = append(mgr.importedIDs, addr)
		} else {
			mgr.derivedIDs = append(mgr.derivedIDs, addr)
		}
	}

	// Upgrade previous version database to new version.
	err = mgr.db.checkAndUpgrade()
	if err != nil {
		return nil, err
	}

	return mgr, nil
}

// ExportEncrypted encrypts the current state of the key manager with the
// specified password and returns it. The salt used as input to PBKDF2 as well
// as nonce for input to secretbox are randomly generated. The actual key used
// for encryption is derived from the salt and the passphrase using PBKDF2.
func (mgr *Manager) ExportEncrypted(pass []byte) ([]byte, error) {
	plain, err := mgr.ExportPlaintext()
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

// ExportPlaintext exports the state of the key manager in an unencrypted
// state. It's useful for debugging or manual inspection of keys. 
func (mgr *Manager) ExportPlaintext() ([]byte, error) {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	return mgr.db.Serialize()
}

// ImportIdentity imports an existing identity into the key manager. It's useful
// for users who have existing identities or want to subscribe to channels.
func (mgr *Manager) ImportIdentity(privID PrivateID) {
	
	// Encode as string. 
	str := privID.Address() 
	
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()

	// Check if the identity already exists.
	if _, ok := mgr.db.IDs[str]; ok {
		return
	}
	
	// Insert a copy into database.
	privID.Imported = true // Make sure it is marked as imported. 
	mgr.db.IDs[str] = &privID
	mgr.importedIDs = append(mgr.importedIDs, str)

	return
}

// RemoveImported removes an imported identity from the key manager.
/*func (mgr *Manager) RemoveImported(str string) {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()
	
	if _, ok := mgr.db.IDs[str]; !ok {
		return
	}
	
	delete(mgr.db.IDs, str)

	for i, addr := range mgr.importedIDs {
		if str == addr {
			// Delete element from slice.
			a := mgr.importedIDs

			// From https://github.com/golang/go/wiki/SliceTricks
			copy(a[i:], a[i+1:])
			a[len(a)-1] = ""
			mgr.importedIDs = a[:len(a)-1]
		}
	}
}*/

// NewHDIdentity generates a new HD identity and numbers it based on previously
// derived identities. If 2^32 identities have already been generated, new
// identities would be duplicates because of overflow problems.
func (mgr *Manager) NewHDIdentity(stream uint32, name string) *PrivateID {
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
		Name: name,
	}
	
	// Encode address as string. 
	str, err := privID.Address.Encode()
	if err != nil {
		return nil
	}

	// Add to derived ids. 
	mgr.derivedIDs = append(mgr.derivedIDs, str)

	// Insert in addresses.
	mgr.db.IDs[str] = id

	return id
}

func (mgr *Manager) NewHDUnnamedIdentity(stream uint32) *PrivateID {
	return mgr.NewHDIdentity(stream, "");
}

func (mgr *Manager) forEach(f func(*PrivateID) error) error {
	// Go through HD identities first.
	for _, id := range mgr.db.IDs {
		err := f(id)
		if err != nil {
			return err
		}
	}
	return nil
}

// ForEach runs the specified function for all the identities stored in the key
// manager. It does not return until the function has been invoked for all keys
// and breaks early on error. 
func (mgr *Manager) ForEach(f func(*PrivateID) error) error {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	return mgr.forEach(f)
}

// LookupByAddress looks up a private identity in the key manager by its
// address. If no matching identity can be found, ErrNonexistentIdentity is
// returned.
func (mgr *Manager) LookupByAddress(address string) *PrivateID {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()
	
	p := mgr.db.IDs[address]
	
	return p
}

// NumImported returns the number of imported identities that the key manager
// has in the database.
func (mgr *Manager) NumImported() int {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	return len(mgr.importedIDs)
}

// NumDeterministic returns the number of identities that have been created
// deterministically (according to BIP-BM01).
func (mgr *Manager) NumDeterministic() int {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	return len(mgr.derivedIDs)
}

func (mgr *Manager) Size() int {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()
	
	return len(mgr.db.IDs)
}

// GetAddresses returns the set of addresses in the key manager. 
func (mgr *Manager) Addresses() []string {
	addresses := make([]string, len(mgr.db.IDs))
	
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()
	
	var i int= 0;
	for address, _ := range mgr.db.IDs {
		addresses[i] = address
		i ++
	}
	return addresses
}

// NameAddress names an address.
func (mgr *Manager) NameAddress(address, name string) error {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()
	
	// Does the address exist in the database? 
	if id, ok := mgr.db.IDs[address]; !ok {
		return ErrNonexistentIdentity
	} else {
		id.Name = name
		mgr.db.IDs[address] = id
	}
	
	return nil
} 

// UnnameAddress removes a name from the address.
func (mgr *Manager) UnnameAddress(address string) error {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()
	
	// Does the address exist in the database? 
	if id, ok := mgr.db.IDs[address]; !ok {
		return ErrNonexistentIdentity
	} else {
		id.Name = ""
		mgr.db.IDs[address] = id
	}
	return nil
}

// Get the map of addresses to names. 
func (mgr *Manager) Names() map[string]string {
	
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()
	
	names := make(map[string]string)
	
	for address, id := range mgr.db.IDs {
		names[address] = id.Name
	}
	
	return names
}

// Import keys from Pybitmessage or another bmagent identity. 
// Returns a map containing the imported addresses and names. 
func (mgr *Manager) ImportKeys(data []byte) map[string]string {
	
	// First try to import as a pybitmessage format. 
	f, err := ini.Load(bytes.NewReader(data))
	if err == nil {
		return mgr.ImportKeysFromPyBitmessage(f)
	}
	
	// Next try to import from standard format. 
	m, err := FromPlaintext(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	
	addresses := make(map[string]string)

	m.ForEach(func(p *PrivateID) error {
		addresses[p.Address()] = p.Name
		mgr.ImportIdentity(*p)
		return nil
	})

	return addresses
}

// Given an ini file like that created by PyBitmessage, 
// import into the key manager.
func (mgr *Manager) ImportKeysFromPyBitmessage(f ini.File) map[string]string {
	
	addresses := make(map[string]string)
	
	for k, v := range f {
		if k == "bitmessagesettings" {
			continue
		}

		signingKey, ok := v["privsigningkey"]
		if !ok {
			continue
		}
		encKey, ok := v["privencryptionkey"]
		if !ok {
			continue
		}

		address := k
		nonceTrials := readIniUint64(v, "noncetrialsperbyte", pow.DefaultNonceTrialsPerByte)
		extraBytes := readIniUint64(v, "payloadlengthextrabytes", pow.DefaultExtraBytes)
		enabled := readIniBool(v, "enabled", true)
		isChan := readIniBool(v, "chan", false)
		
		name := v["label"]

		// Now that we have read everything related to the identity, create it.
		id, err := identity.ImportWIF(address, signingKey, encKey, nonceTrials, extraBytes)
		if err != nil {
			continue
		}
		
		p := PrivateID{
			Private:  *id,
			IsChan:   isChan,
			Disabled: !enabled,
			Name: name, 
		}
		
		addresses[p.Address()] = p.Name
 
		mgr.ImportIdentity(p)
	}
	
	return addresses
}

func readIniBool(m map[string]string, key string, defaultValue bool) bool {
	str, ok := m[key]
	ret := defaultValue
	if ok {
		if strings.ToLower(str) == "false" {
			ret = false
		} else if strings.ToLower(str) == "true" {
			ret = true
		}
	}
	return ret
}

func readIniUint64(m map[string]string, key string, defaultValue uint64) uint64 {
	str, ok := m[key]
	ret := defaultValue
	if ok {
		n, err := strconv.Atoi(str)
		if err == nil {
			ret = uint64(n)
		}
	}
	return ret
}
