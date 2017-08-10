package core

import (
	"crypto/rsa"
	"encoding/asn1"
	protocol "github.com/s-rah/go-ricochet"
	"log"
	"time"
)

type ProtocolConnection struct {
	Core *Ricochet

	Conn    *protocol.OpenConnection
	Contact *Contact

	// Client-side authentication
	MyHostname string
	PrivateKey rsa.PrivateKey

	// Service-side authentication
	GetContactByHostname func(hostname string) *Contact
}

func (pc *ProtocolConnection) OnReady(oc *protocol.OpenConnection) {
	if pc.Conn != nil && pc.Conn != oc {
		log.Panicf("ProtocolConnection is already assigned connection %v, but OnReady called for connection %v", pc.Conn, oc)
	}

	pc.Conn = oc

	if pc.Conn.Client {
		log.Printf("Connected to %s", pc.Conn.OtherHostname)
		pc.Conn.MyHostname = pc.MyHostname
		pc.Conn.IsAuthed = true // Outbound connections are authenticated
		pc.Conn.Authenticate(1)
	}
}

func (pc *ProtocolConnection) OnDisconnect() {
	log.Printf("protocol: OnDisconnect: %v", pc)
	if pc.Contact != nil {
		pc.Contact.OnConnectionClosed(pc.Conn)
	}
}

// Authentication Management
func (pc *ProtocolConnection) OnAuthenticationRequest(channelID int32, clientCookie [16]byte) {
	log.Printf("protocol: OnAuthenticationRequest")
	pc.Conn.ConfirmAuthChannel(channelID, clientCookie)
}

func (pc *ProtocolConnection) OnAuthenticationChallenge(channelID int32, serverCookie [16]byte) {
	log.Printf("protocol: OnAuthenticationChallenge")
	publicKeyBytes, _ := asn1.Marshal(pc.PrivateKey.PublicKey)
	pc.Conn.SendProof(1, serverCookie, publicKeyBytes, &pc.PrivateKey)
}

func (pc *ProtocolConnection) OnAuthenticationProof(channelID int32, publicKey []byte, signature []byte) {
	result := pc.Conn.ValidateProof(channelID, publicKey, signature)

	if result {
		if len(pc.Conn.OtherHostname) != 16 {
			log.Printf("protocol: Invalid format for hostname '%s' in authentication proof", pc.Conn.OtherHostname)
			result = false
		} else {
			pc.Contact = pc.GetContactByHostname(pc.Conn.OtherHostname)
		}
	}
	isKnownContact := (pc.Contact != nil)

	pc.Conn.SendAuthenticationResult(channelID, result, isKnownContact)
	pc.Conn.IsAuthed = result
	pc.Conn.CloseChannel(channelID)

	log.Printf("protocol: OnAuthenticationProof, result: %v, contact: %v", result, pc.Contact)
	if result && pc.Contact != nil {
		pc.Contact.OnConnectionAuthenticated(pc.Conn, true)
	}
}

func (pc *ProtocolConnection) OnAuthenticationResult(channelID int32, result bool, isKnownContact bool) {
	pc.Conn.IsAuthed = result
	pc.Conn.CloseChannel(channelID)

	if !result {
		log.Printf("protocol: Outbound connection authentication to %s failed", pc.Conn.OtherHostname)
		pc.Conn.Close()
		return
	}

	log.Printf("protocol: Outbound connection to %s authenticated", pc.Conn.OtherHostname)
	if pc.Contact != nil {
		pc.Contact.OnConnectionAuthenticated(pc.Conn, isKnownContact)
	}
}

// Contact Management
func (pc *ProtocolConnection) OnContactRequest(channelID int32, nick string, message string) {
	if pc.Conn.Client || !pc.Conn.IsAuthed || pc.Contact != nil {
		pc.Conn.CloseChannel(channelID)
		return
	}

	address, ok := AddressFromPlainHost(pc.Conn.OtherHostname)
	if !ok {
		pc.Conn.CloseChannel(channelID)
		return
	}
	if len(nick) > 0 && !IsNicknameAcceptable(nick) {
		log.Printf("protocol: Stripping unacceptable nickname from inbound request; encoded: %x", []byte(nick))
		nick = ""
	}
	if len(message) > 0 && !IsMessageAcceptable(message) {
		log.Printf("protocol: Stripping unacceptable message from inbound request; len: %d, encoded: %x", len(message), []byte(message))
		message = ""
	}

	contactList := pc.Core.Identity.ContactList()
	request, contact := contactList.AddOrUpdateInboundContactRequest(address, nick, message)

	if contact != nil {
		// Accepted immediately
		pc.Conn.AckContactRequestOnResponse(channelID, "Accepted")
		pc.Conn.CloseChannel(channelID)
		contact.OnConnectionAuthenticated(pc.Conn, true)
	} else if request != nil && !request.IsRejected() {
		// Pending
		pc.Conn.AckContactRequestOnResponse(channelID, "Pending")
		request.SetConnection(pc.Conn, channelID)
	} else {
		// Rejected
		pc.Conn.AckContactRequestOnResponse(channelID, "Rejected")
		pc.Conn.CloseChannel(channelID)
		pc.Conn.Close()
		if request != nil {
			contactList.RemoveInboundContactRequest(request)
		}
	}
}

func (pc *ProtocolConnection) OnContactRequestAck(channelID int32, status string) {
	if !pc.Conn.Client || pc.Contact == nil {
		pc.Conn.CloseChannel(channelID)
		return
	}

	if !pc.Contact.UpdateContactRequest(status) {
		pc.Conn.CloseChannel(channelID)
		return
	}
}

func (pc *ProtocolConnection) IsKnownContact(hostname string) bool {
	// All uses of this are for authenticated contacts, so it's sufficient to check pc.Contact
	if pc.Contact != nil {
		contactHostname, _ := PlainHostFromOnion(pc.Contact.Hostname())
		if hostname != contactHostname {
			log.Panicf("IsKnownContact called for unexpected hostname '%s'", hostname)
		}
		return true
	}
	return false
}

// Managing Channels
func (pc *ProtocolConnection) OnOpenChannelRequest(channelID int32, channelType string) {
	log.Printf("open channel request: %v %v", channelID, channelType)
	pc.Conn.AckOpenChannel(channelID, channelType)
}

func (pc *ProtocolConnection) OnOpenChannelRequestSuccess(channelID int32) {
	log.Printf("open channel request success: %v", channelID)
}

func (pc *ProtocolConnection) OnChannelClosed(channelID int32) {
	log.Printf("channel closed: %v", channelID)
}

// Chat Messages
// XXX messageID should be (at least) uint32
func (pc *ProtocolConnection) OnChatMessage(channelID int32, messageID int32, message string) {
	// XXX no time delta?
	// XXX sanity checks, message contents, etc
	log.Printf("chat message: %d %d %s", channelID, messageID, message)

	// XXX error case
	if pc.Contact == nil {
		pc.Conn.Close()
	}

	// XXX cache?
	conversation := pc.Contact.Conversation()
	conversation.Receive(uint64(messageID), time.Now().Unix(), message)

	pc.Conn.AckChatMessage(channelID, messageID)
}

func (pc *ProtocolConnection) OnChatMessageAck(channelID int32, messageID int32) {
	// XXX no success
	log.Printf("chat ack: %d %d", channelID, messageID)

	// XXX error case
	if pc.Contact == nil {
		pc.Conn.Close()
	}

	conversation := pc.Contact.Conversation()
	conversation.UpdateSentStatus(uint64(messageID), true)
}

// Handle Errors
func (pc *ProtocolConnection) OnFailedChannelOpen(channelID int32, errorType string) {
	log.Printf("failed channel open: %d %s", channelID, errorType)
	pc.Conn.UnsetChannel(channelID)
}
func (pc *ProtocolConnection) OnGenericError(channelID int32) {
	pc.Conn.RejectOpenChannel(channelID, "GenericError")
}
func (pc *ProtocolConnection) OnUnknownTypeError(channelID int32) {
	pc.Conn.RejectOpenChannel(channelID, "UnknownTypeError")
}
func (pc *ProtocolConnection) OnUnauthorizedError(channelID int32) {
	pc.Conn.RejectOpenChannel(channelID, "UnauthorizedError")
}
func (pc *ProtocolConnection) OnBadUsageError(channelID int32) {
	pc.Conn.RejectOpenChannel(channelID, "BadUsageError")
}
func (pc *ProtocolConnection) OnFailedError(channelID int32) {
	pc.Conn.RejectOpenChannel(channelID, "FailedError")
}
