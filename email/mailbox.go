// Copyright (c) 2015 Monetas.
// Copyright 2016 Daniel Krawisz.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package email

import (
	"bytes"
	"container/list"
	"errors"
	"math"
	"sync"
	"time"
	"sort"

	"github.com/jordwest/imap-server/mailstore"
	"github.com/jordwest/imap-server/types"
	"github.com/mailhog/data"
	"github.com/DanielKrawisz/bmagent/store"
)

// MessageSequence represents a sequence of uids contained in this mailbox.
// It implements sort.Interface.
type MessageSequence []uint64

// GetSequenceNumber gets the lowest sequence number containing a value 
// higher than or equal to the given uid.
func (uids MessageSequence) GetSequenceNumber(uid uint64) uint32 {
	// If the slice is empty.
	if len(uids) == 0 {
		return 1
	}

	// If the given uid is outside the range of uids in the slice.
	if uid < uids[0] {
		return 1
	}
	if uid > uids[len(uids)-1] {
		return uint32(len(uids) + 1)
	}

	// If there is only one message in the box.
	if len(uids) == 1 {
		// uid must be equal to the one message in the list because
		// it's not higher or lower than it.
		return 1
	}

	var minIndex uint32
	maxIndex := uint32(len(uids) - 1)

	minUID := uids[minIndex]
	maxUID := uids[maxIndex]

	for {
		ratio := (float64(uid - minUID)) / (float64(maxUID - minUID))
		// Ratio should be between zero and one inclusive.
		checkIndex := uint32(math.Floor(float64(minIndex) + ratio*float64(maxIndex-minIndex)))

		newUID := uids[checkIndex]

		if newUID == uid {
			return checkIndex + 1
		}

		if newUID > uid {
			maxUID = newUID
			maxIndex = checkIndex
		} else {
			// Add 1 because we use Floor function earlier.
			minIndex = checkIndex + 1
			minUID = uids[minIndex]
			if minUID > uid {
				return minIndex + 1
			}
		}
	}
}

func (uids MessageSequence) Len() int {
	return len(uids)
}

func (uids MessageSequence) Less(i, j int) bool {
	return uids[i] < uids[j]
}

func (uids MessageSequence) Swap(i, j int) {
	id := uids[i]
	uids[i] = uids[j]
	uids[j] = id
}

type Mailbox interface {
	mailstore.Mailbox
	
	// Save saves an IMAP email in the Mailbox. 
	Save(email *IMAPEmail) error
	
	// AddNew adds a new Bitmessage to the Mailbox.
	AddNew(bmsg *Bitmessage, flags types.Flags) error
	
	// DeleteBitmessageByUID deletes a bitmessage by uid. 
	DeleteBitmessageByUID(id uint64) error
}

// Mailbox implements a mailbox that is compatible with IMAP. It implements the
// mailstore.Mailbox interface. Only public functions take care of
// locking/unlocking the embedded RWMutex.
type mailbox struct {
	mbox         store.Folder
	
	// Used to define a subfolder, in which only those messages
	// which return true belong to the mailbox. Can be nil. 
	sub func (*Bitmessage) bool
	
	// The set of addresses associated with this folder and their names.
	addresses    map[string]*string
	drafts       bool // Whether this is a drafts folder. 
	
	sync.RWMutex // Protect the following fields.
	uids         MessageSequence
	numRecent    uint32
	numUnseen    uint32
	nextUID      uint32
}

func (box *mailbox) decodeBitmessageForImap(uid uint64, seqno uint32, msg []byte) *Bitmessage {
	b, err := DecodeBitmessage(msg)
	if err != nil {
		imapLog.Errorf("DecodeBitmessage for #%d failed: %v", uid, err)
		return nil
	}
	b.ImapData.UID = uid
	b.ImapData.SequenceNumber = seqno
	b.ImapData.Mailbox = box
	return b
}

// Name returns the name of the mailbox.
// This is part of the mailstore.Mailbox interface.
func (box *mailbox) Name() string {
	return box.mbox.Name()
}

// updateMailboxStats updates the mailbox data like number of recent/unseen
// messages based on the provided Bitmessage.
func (box *mailbox) updateMailboxStats(entry *Bitmessage, id uint64) {
	if entry.ImapData.Flags.HasFlags(types.FlagRecent) {
		box.numRecent++
	}
	if !entry.ImapData.Flags.HasFlags(types.FlagSeen) {
		box.numUnseen++
	}
}

// refresh updates cached statistics like number of messages in inbox,
// next UID, last UID, number of recent/unread messages etc. It is meant to
// be called after the mailbox has been modified by an agent other than the
// IMAP server. This could be the SMTP server, or new message from bmd.
func (box *mailbox) refresh() error {

	// Set NextUID
	box.nextUID = uint32(box.mbox.NextID())

	box.numRecent = 0
	box.numUnseen = 0
	list := list.New()

	// Run through every message to get the uids, count the recent and
	// unseen messages, and to update pkrequests and powqueue.
	err := box.mbox.ForEachMessage(0, 0, 2, func(id, suffix uint64, msg []byte) error {
		entry, err := DecodeBitmessage(msg)
		if err != nil {
			return imapLog.Errorf("Failed to decode message #%d: %v", id, err)
		}
		
		// Only include messages that belong in this mailbox. 
		if box.sub != nil && !box.sub(entry) {
			return nil
		}

		box.updateMailboxStats(entry, id)

		list.PushBack(id)
		return nil
	})
	if err != nil {
		return err
	}

	box.uids = make([]uint64, 0, list.Len())

	for e := list.Front(); e != nil; e = e.Next() {
		box.uids = append(box.uids, e.Value.(uint64))
	}
	
	sort.Sort(box.uids)

	return nil
}

// NextUID returns the unique identifier that will LIKELY be assigned
// to the next mail that is added to this mailbox.
// This is part of the mailstore.Mailbox interface.
func (box *mailbox) NextUID() uint32 {
	box.RLock()
	defer box.RUnlock()

	return box.nextUID
}

// LastUID assigns the UID of the very last message in the mailbox.
// If the mailbox is empty, this should return the next expected UID.
// This is part of the mailstore.Mailbox interface.
func (box *mailbox) LastUID() uint32 {
	box.RLock()
	defer box.RUnlock()

	return uint32(box.uids[len(box.uids)-1])
}

// Recent returns the number of recent messages in the mailbox.
// This is part of the mailstore.Mailbox interface.
func (box *mailbox) Recent() uint32 {
	box.RLock()
	defer box.RUnlock()

	return box.numRecent
}

// Messages returns the number of messages in the mailbox.
// This is part of the mailstore.Mailbox interface.
func (box *mailbox) Messages() uint32 {
	box.RLock()
	defer box.RUnlock()

	return box.messages()
}

// messages returns the number of messages in the mailbox. It doesn't use the
// RWLock.
func (box *mailbox) messages() uint32 {
	return uint32(len(box.uids))
}

// Unseen returns the number of messages that do not have the Unseen flag set yet
// This is part of the mailstore.Mailbox interface.
func (box *mailbox) Unseen() uint32 {
	box.RLock()
	defer box.RUnlock()

	return box.numUnseen
}

// bitmessageBySequenceNumber gets a message by its sequence number
func (box *mailbox) bitmessageBySequenceNumber(seqno uint32) *Bitmessage {

	if seqno < 1 || seqno > box.messages() {
		return nil
	}
	uid := box.uids[seqno - 1]
	return box.bmsgByUID(uid)
}

// MessageBySequenceNumber gets a message by its sequence number
// It is a part of the mail.SMTPFolder interface.
func (box *mailbox) MessageBySequenceNumber(seqno uint32) mailstore.Message {
	box.RLock()
	defer box.RUnlock()

	bm := box.bitmessageBySequenceNumber(seqno)
	if bm == nil {
		return nil
	}
	email, err := bm.ToEmail()
	if err != nil {
		imapLog.Errorf("MessageBySequenceNumber(%d) gave error %v", seqno, err)
		return nil
	}

	return email
}

// bmsgByUID returns a Bitmessage by its uid. This function not protected with locks.
func (box *mailbox) bmsgByUID(uid uint64) *Bitmessage {
	seqno := box.uids.GetSequenceNumber(uint64(uid))
	if seqno > uint32(len(box.uids)) || box.uids[seqno - 1] != uid {
		// This message does not exist in this mailbox. 
		return nil
	}
	
	suffix, msg, err := box.mbox.GetMessage(uid)
	if err != nil {
		imapLog.Errorf("Mailbox(%s).GetMessage gave error: %v", box.Name(), err)
		return nil
	}
	if suffix != 2 {
		imapLog.Errorf("For message #%d expected suffix %d got %d", uid, 2, suffix)
		return nil
	}

	return box.decodeBitmessageForImap(uid, seqno, msg)
}

// BitmessageByUID returns a Bitmessage by its uid.
func (box *mailbox) BitmessageByUID(uid uint64) *Bitmessage {
	box.RLock()
	defer box.RUnlock()

	return box.bmsgByUID(uid)
}

// MessageByUID gets a message by its uid number
// It is a part of the mail.SMTPFolder interface.
func (box *mailbox) MessageByUID(uid uint32) mailstore.Message {
	box.RLock()
	defer box.RUnlock()

	letter := box.bmsgByUID(uint64(uid))
	if letter == nil {
		return nil
	}
	email, err := letter.ToEmail()
	if err != nil {
		imapLog.Errorf("Failed to convert message #%d to e-mail: %v", uid, err)
	}
	return email
}

// lastBitmessage returns the last Bitmessage in the mailbox.
func (box *mailbox) lastBitmessage() *Bitmessage {
	if box.messages() == 0 {
		return nil
	}

	uid := box.uids[len(box.uids)-1]
	return box.bmsgByUID(uid)
}

// getRange returns a sequence of bitmessages from the mailbox in a range from
// startUID to endUID. It does not check whether the given sequence numbers make
// sense.
func (box *mailbox) getRange(startUID, endUID uint64, startSequence, endSequence uint32) []*Bitmessage {
	bitmessages := make([]*Bitmessage, 0, endSequence-startSequence+1)

	i := uint32(0)
	err := box.mbox.ForEachMessage(startUID, endUID, 2, func(id, suffix uint64, msg []byte) error {
		
		bm := box.decodeBitmessageForImap(id, startSequence+i, msg)
		if bm == nil {
			return nil // Skip this message, error has already been logged.
		}
		
		if box.sub != nil && !box.sub(bm) {
			return nil
		}
		
		bitmessages = append(bitmessages, bm)
		
		i++
		return nil
	})
	if err != nil {
		return nil
	}
	return bitmessages
}

// getSince returns a sequence of bitmessages from the mailbox which includes
// all greater than or equal to a given uid number. It does not check whether
// the given sequence number makes sense.
func (box *mailbox) getSince(startUID uint64, startSequence uint32) []*Bitmessage {
	return box.getRange(startUID, box.uids[len(box.uids) - 1], startSequence, box.messages())
}

// bitmessagesByUIDRange returns Bitmessage with UIDs between start and end.
func (box *mailbox) bitmessagesByUIDRange(start, end uint64) []*Bitmessage {
	startSequence := box.uids.GetSequenceNumber(start)
	endSequence := box.uids.GetSequenceNumber(end)

	if startSequence > endSequence {
		return []*Bitmessage{}
	}
	return box.getRange(start, end, startSequence, endSequence)
}

// bitmessagesSinceUID returns messages with UIDs greater than start.
func (box *mailbox) bitmessagesSinceUID(start uint64) []*Bitmessage {
	startSequence := box.uids.GetSequenceNumber(start)
	return box.getSince(start, startSequence)
}

// bitmessagesBySequenceRange returns a set of Bitmessages in a range between
// two sequence numbers inclusive.
func (box *mailbox) bitmessagesBySequenceRange(start, end uint32) []*Bitmessage {
	if start < 1 || start > box.messages() ||
		end < 1 || end > box.messages() || end < start {
		return nil
	}
	startUID := box.uids[start - 1]
	endUID := box.uids[end - 1]
	return box.getRange(startUID, endUID, start, end)
}

// bitmessagesSinceSequenceNumber returns the set of Bitmessages since and
// including a given uid value.
func (box *mailbox) bitmessagesSinceSequenceNumber(start uint32) []*Bitmessage {
	if start < 1 || start > box.Messages() {
		return nil
	}
	startUID := box.uids[start - 1]
	return box.getSince(startUID, start)
}

// bitmessageSetByUID gets messages belonging to a set of ranges of UIDs
func (box *mailbox) bitmessageSetByUID(set types.SequenceSet) []*Bitmessage {
	var msgs []*Bitmessage

	// If the mailbox is empty, return empty slice
	if box.messages() == 0 {
		return msgs
	}

	for _, msgRange := range set {
		// If Min is "*", meaning the last UID in the mailbox, Max should
		// always be Nil
		if msgRange.Min.Last() {
			// Return the last message in the mailbox
			msgs = append(msgs, box.lastBitmessage())
			continue
		}

		start, err := msgRange.Min.Value()
		if err != nil {
			return msgs
		}

		// If no Max is specified, then return only the min value.
		if msgRange.Max.Nil() {
			// Fetch specific message by sequence number
			msgs = append(msgs, box.bmsgByUID(uint64(start)))
			if err != nil {
				return msgs
			}
			continue
		}

		var end uint32
		if msgRange.Max.Last() {
			since := box.bitmessagesSinceUID(uint64(start))
			if since == nil {
				continue // Some error occurred
			}
			msgs = append(msgs, since...)
		} else {
			end, err = msgRange.Max.Value()
			if err != nil {
				return msgs
			}
			msgs = append(msgs, box.bitmessagesByUIDRange(uint64(start), uint64(end))...)
		}
	}
	return msgs
}

// bitmessageSetBySequenceNumber gets messages belonging to a set of ranges of
// sequence numbers.
func (box *mailbox) bitmessageSetBySequenceNumber(set types.SequenceSet) []*Bitmessage {
	var msgs []*Bitmessage

	// If the mailbox is empty, return empty array
	if box.Messages() == 0 {
		return msgs
	}

	for _, msgRange := range set {
		// If Min is "*", meaning the last UID in the mailbox, Max should
		// always be Nil
		if msgRange.Min.Last() {
			// Return the last message in the mailbox
			msgs = append(msgs, box.lastBitmessage())
			continue
		}

		startIndex, err := msgRange.Min.Value()
		if err != nil {
			return msgs
		}
		if startIndex < 1 || startIndex > box.Messages() {
			return msgs
		}
		start := uint32(box.uids[startIndex-1])

		// If no Max is specified, then return only the min value.
		if msgRange.Max.Nil() {
			// Fetch specific message by sequence number
			msgs = append(msgs, box.bitmessageBySequenceNumber(start))
			if err != nil {
				return msgs
			}
			continue
		}

		var end uint32
		if msgRange.Max.Last() {
			msgs = append(msgs, box.bitmessagesSinceSequenceNumber(start)...)
		} else {
			end, err = msgRange.Max.Value()
			if err != nil {
				return msgs
			}
			msgs = append(msgs, box.bitmessagesBySequenceRange(start, end)...)
		}
	}

	return msgs
}

// AddNew adds a new Bitmessage to the Mailbox.
func (box *mailbox) AddNew(bmsg *Bitmessage, flags types.Flags) error {
	box.Lock()
	defer box.Unlock()

	smtpLog.Trace("AddNew: Bitmessage received in folder ", box.Name())

	if bmsg.state == nil {
		bmsg.state = &MessageState{}
	}

	bmsg.ImapData = &ImapData{
		SequenceNumber: box.messages() + 1,
		Flags:          flags,
		TimeReceived:   time.Now(),
		Mailbox:        box,
	}

	box.saveBitmessage(bmsg)

	return nil
}

// MessageSetByUID returns the slice of messages belonging to a set of ranges of
// UIDs.
// It is a part of the mail.SMTPFolder interface.
func (box *mailbox) MessageSetByUID(set types.SequenceSet) []mailstore.Message {
	box.RLock()
	defer box.RUnlock()
	var err error

	msgs := box.bitmessageSetByUID(set)
	email := make([]mailstore.Message, len(msgs))
	for i, msg := range msgs {
		email[i], err = msg.ToEmail()
		if err != nil {
			imapLog.Errorf("Failed to convert message #%d to e-mail: %v",
				msg.ImapData.UID, err)
			return nil
		}
	}
	return email
}

// MessageSetBySequenceNumber returns the slice of messages belonging to a set
// of ranges of sequence numbers.
// It is a part of the mail.SMTPFolder interface.
func (box *mailbox) MessageSetBySequenceNumber(set types.SequenceSet) []mailstore.Message {
	box.RLock()
	defer box.RUnlock()
	var err error

	msgs := box.bitmessageSetBySequenceNumber(set)
	email := make([]mailstore.Message, len(msgs))
	for i, msg := range msgs {
		email[i], err = msg.ToEmail()
		if err != nil {
			imapLog.Errorf("Failed to convert message #%d to e-mail: %v",
				msg.ImapData.UID, err)
			return nil
		}
	}
	return email
}

// DeleteBitmessageByUID deletes a Bitmessage by its UID.
func (box *mailbox) DeleteBitmessageByUID(id uint64) error {
	box.Lock()
	defer box.Unlock()
	
	bmsg := box.bmsgByUID(id)
	if bmsg == nil {
		return nil
	}

	err := box.mbox.DeleteMessage(id)
	if err != nil {
		return err
	}

	// Update the box's state based on the information in the message deleted.
	if bmsg.ImapData != nil {

		if bmsg.ImapData.Flags&types.FlagRecent == types.FlagRecent {
			box.numRecent--
		}

		if bmsg.ImapData.Flags&types.FlagSeen != types.FlagSeen {
			box.numUnseen--
		}
	}

	for i, uid := range box.uids {
		if uid == id {
			box.uids = append(box.uids[0:i], box.uids[i+1:]...)
			break
		}
	}
	return nil
}

// saveBitmessage saves the given Bitmessage in the folder.
func (box *mailbox) saveBitmessage(msg *Bitmessage) error {
	// Generate the new version of the message.
	encode, err := msg.Serialize()
	if err != nil {
		return err
	}

	// Insert the new version of the message.
	if (msg.ImapData.UID == 0) {
		msg.ImapData.UID, err = box.mbox.InsertNewMessage(encode, msg.Message.Encoding())
	} else {
		// Delete the old message from the database.
		err := box.mbox.DeleteMessage(uint64(msg.ImapData.UID))
		if err != nil {
			imapLog.Errorf("Mailbox(%s).DeleteMessage(%d) gave error %v",
				box.Name(), msg.ImapData.UID, err)
			return err
		}
		
		_, _, err = box.mbox.GetMessage(msg.ImapData.UID) 
		if err == nil {
			// There is still a message there despite our attempts to delete it. 
			// That indicates that an entry exists in the folder which does not 
			// belong to this mailbox. 
			return errors.New("Unable to save.")
		}
		
		err = box.mbox.InsertMessage(msg.ImapData.UID, encode, msg.Message.Encoding())
	}
	
	if err != nil {
		imapLog.Errorf("Mailbox(%s).InsertMessage(id=%d, suffix=%d) gave error %v",
			box.Name(), msg.ImapData.UID, msg.Message.Encoding(), err)
		return err
	}

	// TODO: don't refresh the whole thing every time we save. Jeez that's 
	// a lot of extra work! 
	err = box.refresh()
	if err != nil {
		imapLog.Errorf("Mailbox(%s).Refresh gave error %v", box.Name(), err)
		return err
	}

	return nil
}

// Save saves an IMAP email in the Mailbox. 
func (box *mailbox) Save(email *IMAPEmail) error {	
	var msg *Bitmessage
	var err error
	if box.drafts {
		msg, err = NewBitmessageDraftFromSMTP(email.Content)
	} else {
		msg, err = NewBitmessageFromSMTP(email.Content)
	}
	if err != nil {
		imapLog.Errorf("Error saving message #%d: %v", email.ImapUID, err)
		return err
	}

	msg.ImapData = &ImapData{
		UID:            email.ImapUID,
		SequenceNumber: email.ImapSequenceNumber,
		Flags:          email.ImapFlags,
		TimeReceived:   email.Date,
		Mailbox:        box,
	}

	if msg.ImapData.UID != 0 { // The message already exists and needs to be replaced.
		// Check that the uid, date, and sequence number are consistent with one another.
		previous := box.BitmessageByUID(msg.ImapData.UID)
		if previous == nil {
			return errors.New("Invalid sequence number")
		}
		if previous.ImapData.UID != msg.ImapData.UID {
			return errors.New("Invalid uid")
		}
		if previous.ImapData.TimeReceived != msg.ImapData.TimeReceived {
			return errors.New("Cannot change date received")
		}

		msg.state = previous.state
	}

	box.Lock()
	defer box.Unlock()
	return box.saveBitmessage(msg)
}

// DeleteFlaggedMessages deletes messages that were flagged for deletion.
func (box *mailbox) DeleteFlaggedMessages() ([]mailstore.Message, error) {
	box.RLock()
	var delBMsgs []*Bitmessage

	// Gather UIDs of all messages to be deleted.
	for _, uid := range box.uids {
		b := box.bmsgByUID(uid)
		if b == nil {
			continue
		}
		if b.ImapData.Flags.HasFlags(types.FlagDeleted) {
			delBMsgs = append(delBMsgs, b)
		}
	}
	box.RUnlock()

	// Delete them.
	msgs := make([]mailstore.Message, 0, len(delBMsgs))
	for _, b := range delBMsgs {
		msg, err := b.ToEmail()
		if err != nil {
			imapLog.Errorf("Failed to convert #%d to e-mail: %v", b.ImapData.UID,
				err)
			// Don't return because we want this message to be deleted anyway.
		} else {
			msgs = append(msgs, msg)
		}

		err = box.DeleteBitmessageByUID(b.ImapData.UID)
		if err != nil {
			return nil, err
		}
	}

	return msgs, nil
}

// This error is used to cause mailbox.ForEachMessage to stop looping through
// every message once an ack is found, but is not really an error.
var errAckFound = errors.New("Ack Found")

// ReceiveAck takes an object payload and tests it against messages in the
// folder to see if it matches the ack of any sent message in the folder.
// The first such message found is returned.
func (box *mailbox) ReceiveAck(ack []byte) *Bitmessage {
	var ackMatch *Bitmessage

	box.mbox.ForEachMessage(0, 0, 2, func(id, suffix uint64, msg []byte) error {
		entry, err := DecodeBitmessage(msg)
		if err != nil {
			return err
		}
		
		if box.sub != nil && !box.sub(entry) {
			return nil
		}

		if bytes.Equal(entry.Ack, ack) {
			ackMatch = entry

			// Stop ForEachMessage from searching the rest of the messages.
			return errAckFound
		}
		return nil
	})
	if ackMatch == nil {
		return nil
	}

	ackMatch.state.AckReceived = true

	box.Lock()
	box.saveBitmessage(ackMatch)
	box.Unlock()

	return ackMatch
}

// NewMessage creates a new empty message associated with this folder.
// It is part of the IMAPMailbox interface.
func (box *mailbox) NewMessage() mailstore.Message {
	return &IMAPEmail{
		ImapFlags: types.FlagRecent,
		Mailbox:   box,
		Content:   &data.Content{},
		Date:      time.Now(),
	}
}

// NewMailbox returns a new mailbox.
func NewMailbox(mbox store.Folder, addresses map[string]*string) (*mailbox, error) {
	if mbox == nil {
		return nil, errors.New("Nil mailbox.");
	}
	
	m := &mailbox{
		mbox: mbox,
		addresses: addresses, 
	}

	// Populate various data fields.
	if err := m.refresh(); err != nil {
		return nil, err
	}
	return m, nil
}

// NewDrafts returns a new Drafts folder.
func NewDrafts(mbox store.Folder, addresses map[string]*string) (*mailbox, error) {
	if mbox == nil {
		return nil, errors.New("Nil mailbox.")
	}
	
	m := &mailbox{
		mbox: mbox,
		addresses: addresses, 
		drafts: true,
	}

	// Populate various data fields.
	if err := m.refresh(); err != nil {
		return nil, err
	}
	return m, nil
}