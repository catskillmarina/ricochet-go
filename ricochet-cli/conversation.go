package main

import (
	"errors"
	"fmt"
	"github.com/ricochet-im/ricochet-go/rpc"
	"golang.org/x/net/context"
	"log"
	"time"
)

const (
	maxMessageTextLength = 2000
	// Number of messages before the first unread message shown when
	// opening a conversation
	backlogContextNum = 3
	// Maximum number of messages to keep in the backlog. Unread messages
	// will never be discarded to keep this limit, and at least
	// backlogContextNum messages are kept before the first unread message.
	backlogSoftLimit = 100
	// Hard limit for the maximum numer of messages to keep in the backlog
	backlogHardLimit = 200
)

type Conversation struct {
	Client  *Client
	Contact *Contact

	messages  []*ricochet.Message
	numUnread int
	active    bool
}

// Send an outbound message to the contact and add that message into the
// conversation backlog. Blocking API call.
func (c *Conversation) SendMessage(text string) error {
	msg, err := c.Client.Backend.SendMessage(context.Background(), &ricochet.Message{
		Sender:    &ricochet.Entity{IsSelf: true},
		Recipient: &ricochet.Entity{Address: c.Contact.Data.Address},
		Text:      text,
	})
	if err != nil {
		fmt.Fprintf(Ui.Stdout, "send message error: %v\n", err)
		return err
	}

	if err := c.validateMessage(msg); err != nil {
		log.Printf("Conversation sent message does not validate: %v", err)
	}
	return nil
}

// Add a message to the conversation. The message can be inbound or
// outbound. This is called for message events from the backend, and should
// not be used when sending messages. If 'populating' is true, this message
// is part of the initial sync of the history from the backend.
func (c *Conversation) AddMessage(msg *ricochet.Message, populating bool) {
	if err := c.validateMessage(msg); err != nil {
		log.Printf("Rejected conversation message: %v", err)
		return
	}

	c.messages = append(c.messages, msg)
	if msg.Status == ricochet.Message_UNREAD {
		c.numUnread++
	}
	c.trimBacklog()
	if !populating {
		c.printMessage(msg)
		if c.active {
			c.MarkAsReadBefore(msg)
		}
	}
}

func (c *Conversation) UpdateMessage(updatedMsg *ricochet.Message) {
	if err := c.validateMessage(updatedMsg); err != nil {
		log.Printf("Rejected conversation message update: %v", err)
		return
	}

	for i := len(c.messages) - 1; i >= 0; i-- {
		msg := c.messages[i]
		if msg.Sender.IsSelf != updatedMsg.Sender.IsSelf ||
			msg.Identifier != updatedMsg.Identifier {
			continue
		}

		if msg.Status == ricochet.Message_UNREAD &&
			updatedMsg.Status != ricochet.Message_UNREAD {
			c.numUnread--
		}

		c.messages[i] = updatedMsg
		return
	}

	log.Printf("Ignoring message update for unknown message: %v", updatedMsg)
}

// XXX
func (c *Conversation) AddStatusMessage(text string, backlog bool) {
}

// Mark all unread messages in this conversation as read on the backend.
func (c *Conversation) MarkAsRead() error {
	// Get the identifier of the last received message
	var lastRecvMsg *ricochet.Message

findMessageId:
	for i := len(c.messages) - 1; i >= 0; i-- {
		switch c.messages[i].Status {
		case ricochet.Message_UNREAD:
			lastRecvMsg = c.messages[i]
			break findMessageId
		case ricochet.Message_READ:
			break findMessageId
		}
	}

	if lastRecvMsg != nil {
		return c.MarkAsReadBefore(lastRecvMsg)
	}
	return nil
}

func (c *Conversation) MarkAsReadBefore(message *ricochet.Message) error {
	if err := c.validateMessage(message); err != nil {
		return err
	} else if message.Sender.IsSelf {
		return errors.New("Outbound messages cannot be marked as read")
	}

	// XXX This probably means it's impossible to mark messages as read
	// if the sender uses 0 identifiers. We really should not use actual
	// protocol identifiers in RPC API.
	_, err := c.Client.Backend.MarkConversationRead(context.Background(),
		&ricochet.MarkConversationReadRequest{
			Entity:             message.Sender,
			LastRecvIdentifier: message.Identifier,
		})
	if err != nil {
		log.Printf("Mark conversation read failed: %v", err)
	}
	return err
}

func (c *Conversation) PrintContext() {
	// Print starting from backlogContextNum messages before the first unread
	start := len(c.messages) - backlogContextNum
	for i, message := range c.messages {
		if message.Status == ricochet.Message_UNREAD {
			start = i - backlogContextNum
			break
		}
	}

	if start < 0 {
		start = 0
	}

	for i := start; i < len(c.messages); i++ {
		c.printMessage(c.messages[i])
	}
}

func (c *Conversation) trimBacklog() {
	if len(c.messages) > backlogHardLimit {
		c.messages = c.messages[len(c.messages)-backlogHardLimit:]
		c.recountUnread()
	}
	if len(c.messages) <= backlogSoftLimit {
		return
	}

	// Find the index of the oldest unread message
	var keepIndex int
	for i, message := range c.messages {
		if message.Status == ricochet.Message_UNREAD {
			// Keep backlogContextNum messages before the first unread one
			keepIndex = i - backlogContextNum
			if keepIndex < 0 {
				keepIndex = 0
			}
			break
		} else if len(c.messages)-i <= backlogSoftLimit {
			// Remove all messages before this one to reduce to the limit
			keepIndex = i
			break
		}
	}

	c.messages = c.messages[keepIndex:]
}

// Validate that a message object is well-formed, sane, and belongs
// to this conversation.
func (c *Conversation) validateMessage(msg *ricochet.Message) error {
	if msg.Sender == nil || msg.Recipient == nil {
		return fmt.Errorf("Message entities are incomplete: %v %v", msg.Sender, msg.Recipient)
	}

	var localEntity *ricochet.Entity
	var remoteEntity *ricochet.Entity
	if msg.Sender.IsSelf {
		localEntity = msg.Sender
		remoteEntity = msg.Recipient
	} else {
		localEntity = msg.Recipient
		remoteEntity = msg.Sender
	}

	if !localEntity.IsSelf ||
		(len(localEntity.Address) > 0 && localEntity.Address != c.Client.Identity.Address) {
		return fmt.Errorf("Invalid self entity on message: %v", localEntity)
	}

	if remoteEntity.IsSelf || remoteEntity.Address != c.Contact.Data.Address {
		return fmt.Errorf("Invalid remote entity on message: %v", remoteEntity)
	}

	// XXX timestamp
	// XXX identifier

	if msg.Status == ricochet.Message_NULL {
		return fmt.Errorf("Message has null status: %v", msg)
	}

	// XXX more sanity checks on message text?
	if len(msg.Text) == 0 || len(msg.Text) > maxMessageTextLength {
		return errors.New("Message text is unacceptable")
	}

	return nil
}

func (c *Conversation) printMessage(msg *ricochet.Message) {
	if !c.active {
		if msg.Sender.IsSelf {
			return
		}
		messages := fmt.Sprintf("%d new message", c.numUnread)
		if c.numUnread > 1 {
			messages += "s"
		}
		fmt.Fprintf(Ui.Stdout, "\r\x1b[31m[[ \x1b[1;34m%s\x1b[0m from \x1b[1m%s\x1b[0m (\x1b[1m%s\x1b[0m) \x1b[31m]]\x1b[39m\n", messages, c.Contact.Data.Nickname, Ui.PrefixForAddress(c.Contact.Data.Address))
		return
	}

	// XXX actual timestamp
	ts := "\x1b[90m" + time.Now().Format("15:04") + "\x1b[39m"

	var direction string
	if msg.Sender.IsSelf {
		direction = "\x1b[34m<<\x1b[39m"
	} else {
		direction = "\x1b[31m>>\x1b[39m"
	}

	// XXX shell escaping
	fmt.Fprintf(Ui.Stdout, "%s | %s %s %s\n",
		ts,
		c.Contact.Data.Nickname,
		direction,
		msg.Text)
}

func (c *Conversation) UnreadCount() int {
	return c.numUnread
}

func (c *Conversation) recountUnread() {
	c.numUnread = 0
	for _, msg := range c.messages {
		if msg.Status == ricochet.Message_UNREAD {
			c.numUnread++
		}
	}
}

func (c *Conversation) SetActive(active bool) {
	if active == c.active {
		return
	}

	c.active = active
	if active {
		c.PrintContext()
		c.MarkAsRead()
	}
}
