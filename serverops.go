// Copyright (c) 2015 Monetas.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"time"

	"github.com/monetas/bmclient/keymgr"
	"github.com/monetas/bmclient/store"
	"github.com/monetas/bmutil/identity"
	"github.com/monetas/bmutil/wire"
)

// serverOps implements the email.ServerOps interface.
type serverOps struct {
	pubIDs map[string]*identity.Public // a cache
	server *server
}

// GetOrRequestPublic attempts to retreive a public identity for the given
// address. If the function returns nil with no error, that means that a pubkey
// request was successfully queued for proof-of-work.
func (s *serverOps) GetOrRequestPublicID(addr string) (*identity.Public, error) {
	// Check the map of cached identities.
	identity, ok := s.pubIDs[addr]
	if ok {
		return identity, nil
	}

	// Check the private identities, just in case.
	private, err := s.GetPrivateID(addr)
	if err == nil {
		return private.ToPublic(), nil
	}
	if err != keymgr.ErrNonexistentIdentity {
		return nil, err
	}

	pubID, err := s.server.getOrRequestPublicIdentity(addr)
	if err != nil { // Some error occured.
		return nil, err
	}
	if pubID == nil && err == nil { // Getpubkey request sent.
		return nil, nil
	}

	s.pubIDs[addr] = pubID
	return pubID, nil
}

// GetPrivateID queries the key manager for the right private key for the given
// address.
func (s *serverOps) GetPrivateID(addr string) (*keymgr.PrivateID, error) {
	identity, err := s.server.keymgr.LookupByAddress(addr)
	if err != nil {
		return nil, err
	}

	return identity, nil
}

// GetObjectExpiry returns the time duration after which an object of the
// given type will expire on the network. It's used for POW calculations.
func (s *serverOps) GetObjectExpiry(objType wire.ObjectType) time.Duration {
	switch objType {
	case wire.ObjectTypeMsg:
		return cfg.MsgExpiry
	case wire.ObjectTypeBroadcast:
		return cfg.BroadcastExpiry
	case wire.ObjectTypeGetPubKey:
		return defaultGetpubkeyExpiry
	case wire.ObjectTypePubKey:
		return defaultPubkeyExpiry
	default:
		return defaultUnknownObjExpiry
	}
}

// PowQueue returns the store.PowQueue associated with the server.
func (s *serverOps) RunPow(target uint64, obj []byte) (uint64, error) {
	return s.server.powManager.RunPow(target, obj)
}

// Store returns the store.Store associated with the server.
func (s *serverOps) Mailboxes() []*store.Mailbox {
	return s.server.store.Mailboxes()
}
