package awfulirc

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Server is the IRC bridge. The server scans bookmarks and
// periodically polls threads for new messages, sending those to
// connected clients. All connected clients are treated as if they are
// logged in as the server's credentials. Client messages in a thread
// channel will post to that thread.
type Server struct {
	name string

	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	client   *AwfulClient

	lock                *sync.RWMutex
	threads             map[int64]*threadRepresentation
	shortNames          map[string]*threadRepresentation
	privateMessages     map[string][]string
	seenPrivateMessages map[int64]struct{}
	connections         map[*serverConnection]struct{}
}

// Listen starts an IRC bridge with the given somethingawful.com
// credentials, IRC server name, and listen address. The address is
// usually 127.0.0.1:<port>. Use non-local IP masks at your peril.
func Listen(ctx context.Context, client *AwfulClient, name, addr string) (*Server, error) {
	ctx, cancel := context.WithCancel(ctx)

	var config net.ListenConfig
	l, err := config.Listen(ctx, "tcp", addr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("Listen: %w", err)
	}

	s := &Server{
		name: name,

		client: client,

		listener: l,
		ctx:      ctx,
		cancel:   cancel,

		lock:       &sync.RWMutex{},
		threads:    make(map[int64]*threadRepresentation),
		shortNames: make(map[string]*threadRepresentation),

		privateMessages:     make(map[string][]string),
		seenPrivateMessages: make(map[int64]struct{}),

		connections: make(map[*serverConnection]struct{}),
	}

	func() {
		tr := &threadRepresentation{
			meta: ThreadMetadata{
				ID:    -1,
				Title: "Leper's Colony",
			},
			shortName:   "lc",
			subscribers: make(map[*serverConnection]struct{}),
		}
		s.shortNames["lc"] = tr
		go s.repeatUpdatingLC(tr)
	}()

	go s.repeatUpdatingThreads()
	go s.repeatUpdatingPrivateMessages()
	go s.loop()

	return s, nil
}

// startThreadListener listens to the given thread if a listener has
// not already been started. The goroutine periodically polls the
// thread and dispatches updates to connected clients. Instead of
// listening to all bookmarks, we lazily listen to bookmarks when a
// client first connects.
//
// TODO: stop listening when all clients disconnect.
func (s *Server) startThreadListener(thread *threadRepresentation) {
	s.startThreadListenerWithPostGetter(thread, func(ctx context.Context, thread *threadRepresentation) (Posts, error) {
		return s.client.ParseUnreadPosts(ctx, thread.meta)
	})
}

func (s *Server) repeatUpdatingLC(thread *threadRepresentation) {
	s.startThreadListenerWithPostGetter(thread, func(ctx context.Context, thread *threadRepresentation) (Posts, error) {
		posts, err := s.client.ParseLepersColony(ctx)
		if err != nil {
			return Posts{}, err
		}
		return *posts, nil
	})
}

func (s *Server) startThreadListenerWithPostGetter(thread *threadRepresentation, pg func(context.Context, *threadRepresentation) (Posts, error)) {
	thread.lock.Lock()
	defer thread.lock.Unlock()
	if thread.listening {
		return
	}
	thread.seen = make(map[int64]struct{})
	thread.authors = make(map[string]struct{})
	ctx, cancel := context.WithCancel(s.ctx)
	go func() {
		defer cancel()

		for {
			p, err := pg(ctx, thread)
			if err != nil {
				log.Print(err)
				// TODO: Remove all subscribers in error cases. Right
				// now assume nothing bad happens with local
				// connections.
				return
			}

			thread.lock.Lock()
			origLen := len(thread.posts)
			joined := make(map[string]struct{})
			for _, post := range p.Posts {
				if _, ok := thread.seen[post.ID]; ok {
					continue
				}
				thread.seen[post.ID] = struct{}{}
				auth := AuthorToIRC(post.Author)
				if _, ok := thread.authors[auth]; !ok {
					joined[auth] = struct{}{}
				}
				thread.authors[auth] = struct{}{}
				thread.posts = append(thread.posts, post)
			}

			// Consider the new author to have joined if we see a new post from them.
			for auth := range joined {
				joinMessage := fmt.Sprintf(":%s!%s@somethingawful.com JOIN #%s", auth, auth, thread.shortName)
				for sub := range thread.subscribers {
					sub.enqueueLines(joinMessage)
				}
			}

			// Mirror all posts now that no authors are missing.
			for i := origLen; i < len(thread.posts); i++ {
				author := AuthorToIRC(thread.posts[i].Author)
				ircPost := MessageToIRC(author, "#"+thread.shortName, thread.posts[i].Body)
				for sub := range thread.subscribers {
					sub.enqueueLines(ircPost...)
				}
			}
			thread.lock.Unlock()

			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}()
	thread.listening = true
}

// lockedMakeShortName implements some heuristics to convert a thread
// title into a channel name. In general, we'll try to take the first
// nontrivial word, with some handling for duplicates.
func (s *Server) lockedMakeShortName(title string) string {
	// Assumes already locked.

	title = strings.ToLower(title)

	// Special case some known conventions.
	title = strings.ReplaceAll(title, "l@@k", "look")

	// Remove everything except letters and numbers.
	var cleaned strings.Builder

	prevSpace := true
	for _, r := range title {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cleaned.WriteRune(r)
			prevSpace = false
		} else if (unicode.IsSpace(r) || r == '_' || r == '-') && !prevSpace {
			cleaned.WriteByte(' ')
			prevSpace = true
		}
	}

	pieces := strings.Split(cleaned.String(), " ")

	for {
		// Backup: empty, so create a placeholder short name.
		// This is guaranteed to eventually work.
		if len(pieces) == 0 {
			var digit int
			for {
				short := fmt.Sprintf("empty-%d", digit)
				if s.shortNames[short] == nil {
					return short
				}
				digit++
			}
		}

		switch pieces[0] {
		case "a", "an", "the":
			pieces = pieces[1:]
		default:
			// Try hyphen joined.
			for i := 1; i <= len(pieces); i++ {
				short := strings.Join(pieces[0:i], "-")
				if s.shortNames[short] == nil {
					return short
				}
			}

			// Go back to the first word and add variants.
			// This is guaranteed to eventually work.
			var digit int
			for {
				short := fmt.Sprintf("%s-%d", pieces[0], digit)
				if s.shortNames[short] == nil {
					return short
				}
				digit++
			}
		}
	}
}

// updateThreads makes sure that the server is tracking all of the
// given threads. This doesn't mean the server will start polling
// those threads for posts.
//
// Threads are persistent for each server invocation by thread ID, so
// title changes won't result in new channel names.
func (s *Server) updateThreads(threads []ThreadMetadata) {
	for _, th := range threads {
		s.lock.Lock()
		got, ok := s.threads[th.ID]
		if ok {
			// Broadcast the title change as a channel topic update.
			// Don't change the underlying posts or any other internal
			// state, since we equate threads by ID.
			got.lock.Lock()
			changed := got.meta.Title != th.Title
			got.meta = th
			if changed {
				for sub := range got.subscribers {
					sub.enqueueLines("TOPIC #%s :%s", got.shortName, th.Title)
				}
			}
			got.lock.Unlock()
		} else {
			var shortName string
			if th.Hint != "" {
				shortName = s.lockedMakeShortName(th.Hint)
			} else {
				shortName = s.lockedMakeShortName(th.Title)
			}

			repr := &threadRepresentation{
				meta:        th,
				shortName:   shortName,
				subscribers: make(map[*serverConnection]struct{}),
			}
			s.threads[th.ID] = repr
			s.shortNames[repr.shortName] = repr
		}
		s.lock.Unlock()
	}
}

func (s *Server) repeatUpdatingPrivateMessages() {
	for {
		pms, err := s.client.ParsePrivateMessages(s.ctx)
		if err != nil {
			log.Print("ParsePrivateMessages: ", err)
		}

		// PMs are listed in descending order. Form the list in
		// chronological order.
		for _, pm := range slices.Backward(pms) {
			// Private messages are one per thread, so if we've seen
			// it, no need to re-query the thread.
			_, ok := s.seenPrivateMessages[pm.ID]
			if ok {
				continue
			}

			msg, err := s.client.parsePrivateMessageID(s.ctx, pm.ID)
			if err != nil {
				log.Print(err)
				continue
			}

			s.seenPrivateMessages[pm.ID] = struct{}{}
			s.lock.Lock()
			author := AuthorToIRC(pm.Author)
			s.privateMessages[author] = append(s.privateMessages[author], msg)
			for sub := range s.connections {
				lines := MessageToIRC(author, sub.nick, msg)
				sub.enqueueLines(lines...)
			}
			s.lock.Unlock()
		}

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}

	}
}

// repeatUpdatingThreads periodically parses bookmarks to create the
// server channel list.
func (s *Server) repeatUpdatingThreads() {
	s.updateThreads(WellKnownThreads)

	// Try every minute until the initial parse succeeds.
	//
	// TODO: Not really tested since I don't have many bookmarks. Try
	// later once there are multiple pages.
	for {
		allThreads, err := s.client.ParseAllBookmarks(s.ctx)
		s.updateThreads(allThreads)
		if err != nil {
			log.Print("ParseAllBookmarks:", err)
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		} else {
			break
		}
	}

	// Refresh the bookmarks to make sure all channels can be
	// joined. We don't expect the list of bookmarked threads to
	// change too often.
	//
	// TODO: technically there is a race condition between getting all threads,
	// and so many threads updating at once that we miss updated threads beyond
	// the first page of bookmarks. Doesn't really matter in practice.
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}

		updated, err := s.client.ParseRecentBookmarks(s.ctx)
		if err != nil {
			log.Print("ParseRecentBookmarks:", err)
		}
		s.updateThreads(updated)
	}
}

// loop is the main blocking server loop that accepts new clients.
func (s *Server) loop() {
	for {
		if err := s.accept(); err != nil {
			log.Print(err)
		}
	}
}

// accept starts a client connection handler for a given IRC
// connection.
func (s *Server) accept() error {
	conn, err := s.listener.Accept()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(s.ctx)
	sc := &serverConnection{
		server: s,
		conn:   conn,
		queue:  make(chan io.Reader, 64),
		ctx:    ctx,
		cancel: cancel,

		host: "cloaked",

		threads:             make(map[string]*threadRepresentation),
		seenPrivateMessages: make(map[int64]struct{}),
	}

	go sc.run()

	s.lock.Lock()
	s.connections[sc] = struct{}{}
	s.lock.Unlock()

	return nil
}
