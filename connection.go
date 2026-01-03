package awfulirc

import (
	"context"
	"fmt"
	"io"
	"iter"
	"log"
	"maps"
	"net"
	"slices"
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
	forums  map[string]*forumRepresentation

	registered bool

	seenPrivateMessages map[int64]struct{}
}

// run is the internal entrypoint for a client.
func (s *serverConnection) run() {
	go s.sendToClientLoop()
	go s.receiveFromClientLoop()
	go s.sendPingLoop()
	s.updatePrivateMessages()
}

// nickArgument returns the IRC argument for this connection. It is * before
// connecting and the nick after connecting.
func (s *serverConnection) nickArgument() string {
	if s.nick == "" {
		return "*"
	}
	return s.nick
}

// updatePrivateMessages does an initial dump of private messages on
// connection.
func (s *serverConnection) updatePrivateMessages() {
	s.server.lock.Lock()
	defer s.server.lock.Unlock()
	for author, messages := range s.server.privateMessages {
		for _, m := range messages {
			lines := MessageToIRC(author, s.nickArgument(), m)
			s.enqueueLines(lines...)
		}
	}
}

// enqueueMotd sends the message of the day.
func (s *serverConnection) enqueueMotd() {
	s.enqueueLines(
		fmt.Sprintf("375 %s :- %s Message of the day - ", s.nickArgument(), s.server.name),
		fmt.Sprintf("372 %s :- awfulirc bridge", s.nickArgument()),
		fmt.Sprintf("376 %s :End of /MOTD command", s.nickArgument()),
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
		if s.registered {
			s.enqueueLines(fmt.Sprintf(":%s NICK %s", s.nick, msg.Parameters[0]))
		}
		s.nick = msg.Parameters[0]
		s.maybeRegister()
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
		s.maybeRegister()
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
		s.maybeRegister()
	default:
		s.enqueueInvalidCap(sc)
	}
}

func (s *serverConnection) enqueueCapLsReply() {
	s.enqueueLines(fmt.Sprintf("CAP %s LS :awfulirc", s.nickArgument()))
}

func (s *serverConnection) maybeRegister() {
	if s.registered {
		return
	}
	if s.nick == "" || s.user == "" {
		return
	}
	s.registered = true
	s.enqueueLines(fmt.Sprintf("001 %s :Connected to %s", s.nickArgument(), s.server.name))
}

func (s *serverConnection) enqueueInvalidCap(sc string) {
	s.enqueueLines(fmt.Sprintf("410 %s %s :Invalid CAP command", s.nickArgument(), sc))
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
		if strings.HasPrefix(ch, "#") && !strings.HasPrefix(ch, "##") && ch != "#" {
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
			s.enqueueLines(fmt.Sprintf(":%s!%s@%s JOIN %s", s.nick, s.user, s.host, ch))
			s.enqueueChannelTopic(ch, thread.meta.Title)
			s.enqueueChannelMembers(ch, thread.authors)

			// Broadcast all initial messages to the user.
			for _, p := range thread.posts {
				author := AuthorToIRC(p.Author)
				s.enqueueLines(MessageToIRC(author, ch, p.Body)...)
			}
			thread.lock.Unlock()
		} else if strings.HasPrefix(ch, "##") && ch != "##" {
			unprefixedChannel := string(ch[2:])
			if got := s.forums[unprefixedChannel]; got != nil {
				continue
			}

			// Since this is not general-purpose IRC, just fail if it's
			// not a channel we're tracking. The server doesn't handle IRC
			// user communication.
			s.server.lock.Lock()
			forum := s.server.forumNames[unprefixedChannel]
			s.server.lock.Unlock()
			if forum == nil {
				s.enqueueNoSuchChannel(ch)
				continue
			}

			s.server.startForumListener(forum)

			// Thread exists. Add the bidirectional mapping between
			// connected client and subscribed thread.
			s.forums[unprefixedChannel] = forum
			forum.lock.Lock()
			forum.subscribers[s] = struct{}{}

			// Broadcast the topic and members to the user.
			s.enqueueLines(fmt.Sprintf(":%s!%s@%s JOIN %s", s.nick, s.user, s.host, ch))
			s.enqueueChannelTopic(ch, fmt.Sprintf("%s (https://forums.somethingawful.com/forumdisplay.php?forumid=%d)", forum.name, forum.forumid))
			s.enqueueChannelMembers(ch, forum.authors)

			// Broadcast all initial messages to the user.
			for _, p := range forum.threads {
				var channelShortName string
				s.server.lock.Lock()
				if known := s.server.threads[p.ID]; known != nil {
					channelShortName = known.shortName
				}
				s.server.lock.Unlock()
				ircPost := formatThreadPostUpdate(p, ch, channelShortName)
				s.enqueueLines(ircPost...)
			}
			forum.lock.Unlock()
		} else {
			s.enqueueNoSuchChannel(ch)
		}
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
	var (
		it  iter.Seq2[string, *threadRepresentation]
		fit iter.Seq2[string, *forumRepresentation]
	)
	if len(msg.Parameters) > 0 {
		chans := strings.Split(msg.Parameters[0], ",")
		it = func(yield func(string, *threadRepresentation) bool) {
			for _, ch := range chans {
				if strings.HasPrefix(ch, "#") && !strings.HasPrefix(ch, "##") && len(ch) > 1 {
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
		fit = func(yield func(string, *forumRepresentation) bool) {
			for _, ch := range chans {
				if strings.HasPrefix(ch, "##") && len(ch) > 2 {
					name := string(ch[2:])
					if got, ok := s.server.forumNames[name]; ok {
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
		fit = maps.All(s.server.forumNames)
	}

	s.enqueueLines(fmt.Sprintf("321 %s Channel :Users  Name", s.nickArgument()))
	s.server.lock.RLock()
	for name, rep := range it {
		m := fmt.Sprintf("322 %s #%s %d :%s", s.nickArgument(), name, rep.meta.Replies, rep.meta.Title)
		s.enqueueLines(m)
	}
	for name, rep := range fit {
		m := fmt.Sprintf("322 %s ##%s %d :%s", s.nickArgument(), name, len(rep.threads), rep.name)
		s.enqueueLines(m)
	}
	s.server.lock.RUnlock()

	s.enqueueLines(fmt.Sprintf("323 %s :End of /LIST", s.nickArgument()))
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

			// As a special case, support <nick>: syntax to reply to
			// known users. For example Foo: whatever will quote the
			// previous post by Foo and then put your message.
			message := msg.Parameters[1]
			if replyTo, body, ok := strings.Cut(msg.Parameters[1], ": "); ok {
				got.lock.Lock()
				if _, ok := got.authors[replyTo]; ok {
					// Hunt for the most recent post by that author.
					// TODO: this should be indexed but who has time for that.
					toFind := IRCToAuthor(replyTo)
					for _, post := range slices.Backward(got.posts) {
						if post.Author == toFind {
							var b strings.Builder
							b.WriteString(fmt.Sprintf("[quote=%q post=\"%d\"]\n[/quote]\n\n", post.Author, post.ID))
							b.WriteString(body)
							message = b.String()
							break
						}
					}
				}
				got.lock.Unlock()
			}

			if err := s.server.client.ReplyToThread(s.ctx, got.meta, message); err != nil {
				s.enqueueLines(fmt.Sprintf("404 %s %s :%s", s.nick, target, err.Error()))
				continue
			}
		} else if !strings.HasPrefix(target, "#") && len(target) > 0 {
			// Assume this is a nick.
			author := IRCToAuthor(target)
			if err := s.server.client.SendPrivateMessage(s.ctx, author, "chat", msg.Parameters[1]); err != nil {
				log.Print(err)
			}

		} else {
			s.enqueueLines(fmt.Sprintf("401 %s %s :No such nick/channel", s.nick, target))
		}
	}
}

func (s *serverConnection) enqueueChannelTopic(ch string, topic string) {
	s.enqueueLines(fmt.Sprintf("332 %s %s :%s", s.nick, ch, topic))
}

func (s *serverConnection) onWho(msg *ClientMessage) {
	if len(msg.Parameters) == 0 {
		s.enqueueLines(fmt.Sprintf("315 %s :End of /WHO list", s.nickArgument()))
		return
	}

	if !strings.HasPrefix(msg.Parameters[0], "#") || msg.Parameters[0] == "#" {
		s.enqueueLines(fmt.Sprintf("315 %s :End of /WHO list", msg.Parameters[0]))
		return
	}

	ch := string(msg.Parameters[0][1:])
	s.server.lock.Lock()
	thread := s.server.shortNames[ch]
	s.server.lock.Unlock()

	if thread == nil {
		s.enqueueLines(fmt.Sprintf("315 %s :End of /WHO list", ch))
		return
	}

	thread.lock.Lock()
	defer thread.lock.Unlock()
	for author := range thread.authors {
		s.enqueueLines(fmt.Sprintf("352 %s #%s %s somethingawful.com * %s H :0 %s", s.nickArgument(), msg.Parameters[0], author, author, author))
	}
	s.enqueueLines(fmt.Sprintf("315 %s %s :End of /WHO list", s.nickArgument(), ch))

}

func (s *serverConnection) onIson(msg *ClientMessage) {
	if len(msg.Parameters) == 0 {
		s.enqueueNeedMoreParams("ISON")
		return
	}
	s.enqueueLines(fmt.Sprintf("303 :%s", strings.Join(msg.Parameters, " ")))
}

func (s *serverConnection) onWhois(msg *ClientMessage) {
	if len(msg.Parameters) == 0 {
		s.enqueueNeedMoreParams("WHOIS")
		return
	}
	s.enqueueLines(fmt.Sprintf("401 %s :No such nick/channel", msg.Parameters[0]))
}

func (s *serverConnection) onPing(msg *ClientMessage) {
	if len(msg.Parameters) == 0 {
		s.enqueueNeedMoreParams("PING")
		return
	}
	s.enqueueLines(fmt.Sprintf("PONG %s :%s", s.server.name, msg.Parameters[0]))
}

func (s *serverConnection) onPart(msg *ClientMessage) {
	if len(msg.Parameters) == 0 {
		s.enqueueNeedMoreParams("PART")
		return
	}
	for ch := range strings.SplitSeq(msg.Parameters[0], ",") {
		if strings.HasPrefix(ch, "#") && !strings.HasPrefix(ch, "##") && len(ch) > 1 {
			shortName := string(ch[1:])
			if _, ok := s.threads[shortName]; !ok {
				s.enqueueLines(fmt.Sprintf("442 %s %s :You're not on that channel", s.nick, ch))
				continue
			}

			got := s.server.shortNames[shortName]
			got.lock.Lock()
			delete(got.subscribers, s)
			got.lock.Unlock()
			delete(s.threads, shortName)

			s.enqueueLines(fmt.Sprintf(":%s PART %s", s.nick, ch))
		} else if strings.HasPrefix(ch, "##") && len(ch) > 2 {
			shortName := string(ch[2:])
			if _, ok := s.forums[shortName]; !ok {
				s.enqueueLines(fmt.Sprintf("442 %s %s :You're not on that channel", s.nick, ch))
				continue
			}

			got := s.server.forumNames[shortName]
			got.lock.Lock()
			delete(got.subscribers, s)
			got.lock.Unlock()
			delete(s.forums, shortName)

			s.enqueueLines(fmt.Sprintf(":%s PART %s", s.nick, ch))
		} else {
			s.enqueueLines(fmt.Sprintf("442 %s %s :No such channel", s.nick, ch))
			continue
		}
	}
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
			case Command_Ping:
				s.onPing(msg)
			case Command_Pong:
				// Do nothing; we update the time above already.
			case Command_Join:
				s.onJoin(msg)
			case Command_List:
				s.onList(msg)
			case Command_Privmsg:
				s.onPrivmsg(msg)
			case Command_Who:
				s.onWho(msg)
			case Command_Ison:
				s.onIson(msg)
			case Command_Whois:
				s.onWhois(msg)
			case Command_Mode:
				// Do nothing but suppress errors on clients.
			case Command_Part:
				s.onPart(msg)
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
