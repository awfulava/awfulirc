package awfulirc

import (
	"context"
	"fmt"
	"io"
	"iter"
	"log"
	"maps"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// serverConnection is an internal representation of an IRC server
// connection. This is responsible for managing communication between
// client and server.
type serverConnection struct {
	server *Server
	conn   net.Conn
	queue  chan io.Reader
	ctx    context.Context
	cancel context.CancelFunc

	lastMessageFromClient atomic.Int64

	nick     string
	user     string
	host     string
	realname string

	threads map[string]*threadRepresentation
}

// run is the internal entrypoint for a client.
func (s *serverConnection) run() {
	go s.sendToClientLoop()
	go s.receiveFromClientLoop()
	s.enqueueMotd()
	go s.sendPingLoop()
}

// enqueueMotd sends the message of the day.
func (s *serverConnection) enqueueMotd() {
	s.enqueueLines(
		fmt.Sprintf("375 :- %s Message of the day - ", s.server.name),
		"372 :- awfulirc bridge",
		"376 :End of /MOTD command",
	)
}

// sendToClientLoop sends messages to the client.
func (s *serverConnection) sendToClientLoop() {
	defer s.cancel()
	for {
		select {
		case <-s.ctx.Done():
			return
		case r, ok := <-s.queue:
			if !ok {
				return
			}
			_, err := io.Copy(s.conn, r)
			if err != nil {
				return
			}
		}
	}
}

// sendPingLoop sends a PING to the client periodically to enforce keepalive.
func (s *serverConnection) sendPingLoop() {
	pingMessage := fmt.Sprintf("PING :%s", s.server.name)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(90 * time.Second):
			s.enqueueLines(pingMessage)
		}
	}
}

// enqueue sends a raw message to the client. The message is not
// assumed to be valid or delimited.
func (s *serverConnection) enqueue(r io.Reader) {
	select {
	case <-s.ctx.Done():
	case s.queue <- r:
	}
}

// enqueueLines sends each string as a delimited IRC message. No
// validity check is performed.
func (s *serverConnection) enqueueLines(msgs ...string) {
	if len(msgs) == 0 {
		return
	}
	rdrs := make([]io.Reader, 2*len(msgs))
	for i, m := range msgs {
		rdrs[2*i] = strings.NewReader(m)
		rdrs[2*i+1] = strings.NewReader("\r\n")
	}
	s.enqueue(io.MultiReader(rdrs...))
}

func (s *serverConnection) enqueueNeedMoreParams(cmd string) {
	s.enqueueLines(fmt.Sprintf("461 %s :Not enough parameters", cmd))
}

func (s *serverConnection) enqueueNoNicknameGiven() {
	s.enqueueLines("431 :No nickname given")
}

func (s *serverConnection) onNick(msg *ClientMessage) {
	switch len(msg.Parameters) {
	case 0:
		s.enqueueNoNicknameGiven()
	default:
		s.nick = msg.Parameters[0]
	}
}

func (s *serverConnection) enqueueAlreadyRegistered() {
	s.enqueueLines("462 :You may not reregister")
}

func (s *serverConnection) onUser(msg *ClientMessage) {
	switch len(msg.Parameters) {
	case 0, 1, 2, 3:
		s.enqueueNeedMoreParams("USER")
	default:
		if s.user != "" {
			s.enqueueAlreadyRegistered()
			return
		}
		s.user = msg.Parameters[0]
		s.host = msg.Parameters[1]
		// Ignore server intentionally
		s.realname = msg.Parameters[3]
	}
}

func (s *serverConnection) onCap(msg *ClientMessage) {
	if len(msg.Parameters) == 0 {
		s.enqueueNeedMoreParams("CAP")
		return
	}

	switch sc := msg.Parameters[0]; sc {
	case "LS":
		s.enqueueCapLsReply()
	case "LIST":
		log.Print("unhandled CAP LIST")
	case "REQ":
		log.Print("unhandled CAP REQ")
	case "END":
		s.enqueueCapEndReply()
	default:
		s.enqueueInvalidCap(sc)
	}
}

func (s *serverConnection) enqueueCapLsReply() {
	nick := "*"
	if s.nick != "" {
		nick = s.nick
	}
	s.enqueueLines(fmt.Sprintf("CAP %s LS *", nick))
}

func (s *serverConnection) enqueueCapEndReply() {
	nick := "*"
	if s.nick != "" {
		nick = s.nick
	}
	s.enqueueLines(fmt.Sprintf("001 %s :Connected to %s", nick, s.server.name))
}

func (s *serverConnection) enqueueInvalidCap(sc string) {
	nick := "*"
	if s.nick != "" {
		nick = s.nick
	}
	s.enqueueLines(fmt.Sprintf("410 %s %s :Invalid CAP command", nick, sc))
}

func (s *serverConnection) enqueueNoSuchChannel(ch string) {
	s.enqueueLines(fmt.Sprintf("403 %s :No such channel", ch))
}

func (s *serverConnection) onJoin(msg *ClientMessage) {
	if len(msg.Parameters) == 0 {
		s.enqueueNeedMoreParams("JOIN")
		return
	}

	channels := strings.Split(msg.Parameters[0], ",")
	for _, ch := range channels {
		if !strings.HasPrefix(ch, "#") || ch == "#" {
			s.enqueueNoSuchChannel(ch)
			continue
		}

		unprefixedChannel := string(ch[1:])
		if got := s.threads[unprefixedChannel]; got != nil {
			continue
		}

		// Since this is not general-purpose IRC, just fail if it's
		// not a channel we're tracking. The server doesn't handle IRC
		// user communication.
		s.server.lock.Lock()
		thread := s.server.shortNames[unprefixedChannel]
		s.server.lock.Unlock()
		if thread == nil {
			s.enqueueNoSuchChannel(ch)
			continue
		}

		s.server.startThreadListener(thread)

		// Thread exists. Add the bidirectional mapping between
		// connected client and subscribed thread.
		s.threads[unprefixedChannel] = thread
		thread.lock.Lock()
		thread.subscribers[s] = struct{}{}

		// Broadcast the topic and members to the user.
		s.enqueueLines(fmt.Sprintf(":%s JOIN %s", s.nick, ch))
		s.enqueueChannelTopic(ch, thread.meta.Title)
		s.enqueueChannelMembers(ch, thread.authors)

		// Broadcast all initial messages to the user.
		for _, p := range thread.posts {
			s.enqueueLines(p.IRCLines(ch)...)
		}
		thread.lock.Unlock()
	}
}

func (s *serverConnection) enqueueChannelMembers(ch string, authors map[string]struct{}) {
	for auth := range authors {
		s.enqueueLines(fmt.Sprintf("353 %s = %s :%s", s.nick, ch, auth))
	}
	s.enqueueLines(fmt.Sprintf("366 %s %s :End of /NAMES list", s.nick, ch))
}

func (s *serverConnection) onList(msg *ClientMessage) {
	// Send only information about requested channels.
	// TODO: Not tested.
	var it iter.Seq2[string, *threadRepresentation]
	if len(msg.Parameters) > 0 {
		chans := strings.Split(msg.Parameters[0], ",")
		it = func(yield func(string, *threadRepresentation) bool) {
			for _, ch := range chans {
				if strings.HasPrefix(ch, "#") && len(ch) > 1 {
					name := string(ch[1:])
					if got, ok := s.server.shortNames[name]; ok {
						next := yield(name, got)
						if !next {
							return
						}
					}
				}
			}
		}
	} else {
		it = maps.All(s.server.shortNames)
	}

	s.enqueueLines("321 Channel :Users  Name")
	s.server.lock.RLock()
	for name, rep := range it {
		m := fmt.Sprintf("322 %s #%s 0 :%s", s.nick, name, rep.meta.Title)
		s.enqueueLines(m)
	}
	s.server.lock.RUnlock()
	s.enqueueLines("323 :End of /LIST")
}

func (s *serverConnection) onPrivmsg(msg *ClientMessage) {
	if len(msg.Parameters) == 0 {
		s.enqueueLines("411 :No recipient given (PRIVMSG)")
		return
	}

	if len(msg.Parameters) == 1 {
		s.enqueueLines("412 :No text to send")
		return
	}

	for target := range strings.SplitSeq(msg.Parameters[0], ",") {
		if strings.HasPrefix(target, "#") && len(target) > 1 {

			s.server.lock.Lock()
			got := s.server.shortNames[string(target[1:])]
			s.server.lock.Unlock()
			if got == nil {
				s.enqueueLines(fmt.Sprintf("401 %s %s :No such nick/channel", s.nick, target))
				continue
			}

			if err := s.server.client.ReplyToThread(s.ctx, got.meta, msg.Parameters[1]); err != nil {
				s.enqueueLines(fmt.Sprintf("404 %s %s :%s", s.nick, target, err.Error()))
				continue

			}
		} else {
			// TODO: support for direct messages.
			s.enqueueLines(fmt.Sprintf("401 %s %s :No such nick/channel", s.nick, target))
		}
	}
}

func (s *serverConnection) enqueueChannelTopic(ch string, topic string) {
	s.enqueueLines(fmt.Sprintf("332 %s %s :%s", s.nick, ch, topic))
}

func (s *serverConnection) receiveFromClientLoop() {
	it := NewClientMessageIterator(s.conn)
	for msg, err := range it {
		// TODO: We don't actually enforce disconnect timeouts right now.
		s.lastMessageFromClient.Swap(time.Now().Unix())
		switch err {
		case nil:
			switch msg.Command {
			case Command_Nick:
				s.onNick(msg)
			case Command_User:
				s.onUser(msg)
			case Command_Cap:
				s.onCap(msg)
			case Command_Pong:
				// Do nothing; we update the time above already.
			case Command_Join:
				s.onJoin(msg)
			case Command_List:
				s.onList(msg)
			case Command_Privmsg:
				s.onPrivmsg(msg)
			default:
				log.Print("Unknown message: ", msg)
				s.enqueueLines(fmt.Sprintf("421 %s :Unknown command", msg.RawCommand))
			}
		case ErrMessageTooLong:
			log.Print("Message too long!")
		case ErrPrefixOnlyMessage:
			log.Print("Prefix only message!")
		case ErrEmptyCommand:
			log.Print("Empty command!")
		default:
			log.Print(err)
		}
	}
}
