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
	forums              map[int64]*forumRepresentation
	forumNames          map[string]*forumRepresentation
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
		forums:     make(map[int64]*forumRepresentation),
		forumNames: make(map[string]*forumRepresentation),

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
	go s.repeatUpdatingForums()
	go s.repeatUpdatingPrivateMessages()
	go s.loop()

	return s, nil
}

func (s *Server) startForumListener(forum *forumRepresentation) {
	forum.lock.Lock()
	defer forum.lock.Unlock()
	if forum.listening {
		return
	}
	forum.seen = make(map[int64]struct{})
	forum.authors = make(map[string]struct{})
	forum.threads = make(map[int64]ThreadMetadata)
	forum.subscribers = make(map[*serverConnection]struct{})
	ctx, cancel := context.WithCancel(s.ctx)
	go func() {
		defer cancel()
		for {
			threads, err := s.client.ParseForumThreads(ctx, forum.forumid)
			if err != nil {
				log.Print(err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
					// Connection was interrupted; try again later.
					continue
				}
			}

			forum.lock.Lock()
			joined := make(map[string]struct{})
			var updates []ThreadMetadata
			for _, th := range threads {
				auth := AuthorToIRC(th.KilledBy)
				if _, ok := forum.authors[auth]; !ok {
					joined[auth] = struct{}{}
				}
				forum.authors[auth] = struct{}{}

				// If the thread is new or the update time has
				// changed, queue an update.
				existing, ok := forum.threads[th.ID]
				if !ok || !existing.Updated.Equal(th.Updated) {
					updates = append(updates, th)
				}
				forum.threads[th.ID] = th
			}

			// Consider the new author to have joined if we see a new post from them.
			for auth := range joined {
				joinMessage := fmt.Sprintf(":%s!%s@somethingawful.com JOIN &%s", auth, auth, forum.shortName)
				for sub := range forum.subscribers {
					sub.enqueueLines(joinMessage)
				}
			}

			for _, u := range updates {
				ircPost := formatThreadPostUpdate(u, "##"+forum.shortName)
				for sub := range forum.subscribers {
					sub.enqueueLines(ircPost...)
				}
			}
			forum.lock.Unlock()

			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}()
	forum.listening = true
}

func formatThreadPostUpdate(u ThreadMetadata, ch string) []string {
	author := AuthorToIRC(u.KilledBy)
	var b strings.Builder
	b.WriteString("\x01ACTION posted in: ")
	b.WriteString(u.Title)
	b.WriteString("\x01\n")
	b.WriteString(fmt.Sprintf("https://forums.somethingawful.com/showthread.php?threadid=%d&goto=lastpost", u.ID))
	return MessageToIRC(author, ch, b.String())
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
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
					// Connection was interrupted; try again later.
					continue
				}
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
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(time.Minute):
				continue
			}
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

func (s *Server) repeatUpdatingForums() {
	s.lock.Lock()
	defer s.lock.Unlock()
	// For now, this is one-time manual.
	// TODO: Pull from the home page.
	s.forums[682] = &forumRepresentation{
		name:      "Friday",
		shortName: "friday",
	}

	s.forums[273] = &forumRepresentation{
		name:      "General Bullshit",
		shortName: "gbs",
	}
	s.forums[669] = &forumRepresentation{
		name:      "Fuck You and Dine",
		shortName: "gbs-food",
	}
	s.forums[155] = &forumRepresentation{
		name:      "SA's Front Page Discussion",
		shortName: "gbs-home",
	}
	s.forums[214] = &forumRepresentation{
		name:      "everything/nothing",
		shortName: "en",
	}

	s.forums[26] = &forumRepresentation{
		name:      "FYAD",
		shortName: "fyad",
	}
	s.forums[154] = &forumRepresentation{
		name:      "The Hell Circus",
		shortName: "fyad-gas",
	}

	s.forums[167] = &forumRepresentation{
		name:      "Post Your Favorite",
		shortName: "pyf",
	}
	s.forums[670] = &forumRepresentation{
		name:      "Post My Favorites",
		shortName: "pyf-pmf",
	}
	s.forums[268] = &forumRepresentation{
		name:      "BYOB",
		shortName: "byob",
	}
	s.forums[196] = &forumRepresentation{
		name:      "Cool Crew Chat Central",
		shortName: "cccc",
	}

	s.forums[44] = &forumRepresentation{
		name:      "Video Games",
		shortName: "vgs",
	}
	s.forums[191] = &forumRepresentation{
		name:      "Let's Play!",
		shortName: "vgs-lp",
	}
	s.forums[146] = &forumRepresentation{
		name:      "WoW: Goon Squad",
		shortName: "vgs-wow",
	}
	s.forums[145] = &forumRepresentation{
		name:      "The MMO HMO",
		shortName: "vgs-mmo",
	}
	s.forums[279] = &forumRepresentation{
		name:      "Mobile Gaming",
		shortName: "vgs-mobile",
	}
	s.forums[278] = &forumRepresentation{
		name:      "Retro Games",
		shortName: "vgs-retro",
	}
	s.forums[93] = &forumRepresentation{
		name:      "Private Game Servers",
		shortName: "vgs-private",
	}

	s.forums[234] = &forumRepresentation{
		name:      "Traditional Games",
		shortName: "tg",
	}
	s.forums[103] = &forumRepresentation{
		name:      "The Game Room",
		shortName: "tg-room",
	}
	s.forums[46] = &forumRepresentation{
		name:      "Debate & Discussion",
		shortName: "dnd",
	}
	s.forums[269] = &forumRepresentation{
		name:      "C-SPAM",
		shortName: "cspam",
	}
	s.forums[158] = &forumRepresentation{
		name:      "Ask/Tell",
		shortName: "at",
	}
	s.forums[211] = &forumRepresentation{
		name:      "Tourism & Travel",
		shortName: "at-travel",
	}
	s.forums[162] = &forumRepresentation{
		name:      "Science, Academics & Languages",
		shortName: "at-science",
	}
	s.forums[200] = &forumRepresentation{
		name:      "Business, Finance & Careers",
		shortName: "bfc",
	}
	s.forums[22] = &forumRepresentation{
		name:      "Serious Hardware / Software Crap",
		shortName: "shsc",
	}
	s.forums[170] = &forumRepresentation{
		name:      "Haus of Tech Support",
		shortName: "shsc-tech",
	}
	s.forums[202] = &forumRepresentation{
		name:      "The Cavern of COBOL",
		shortName: "shsc-cobol",
	}
	s.forums[219] = &forumRepresentation{
		name:      "YOSPOS",
		shortName: "yospos",
	}
	s.forums[192] = &forumRepresentation{
		name:      "Inspect Your Gadgets",
		shortName: "iyg",
	}
	s.forums[122] = &forumRepresentation{
		name:      "Sports Argument Stadium",
		shortName: "sas",
	}
	s.forums[181] = &forumRepresentation{
		name:      "The Football Funhouse",
		shortName: "sas-football",
	}
	s.forums[175] = &forumRepresentation{
		name:      "The Ray Parlour",
		shortName: "sas-soccer",
	}
	s.forums[248] = &forumRepresentation{
		name:      "The Armchair Quarterback",
		shortName: "sas-fantasy",
	}
	s.forums[139] = &forumRepresentation{
		name:      "Poker is Totally Rigged",
		shortName: "sas-poker",
	}
	s.forums[177] = &forumRepresentation{
		name:      "Punch Sport Pagoda",
		shortName: "sas-punch",
	}
	s.forums[179] = &forumRepresentation{
		name:      "You Look Like Shit",
		shortName: "ylls",
	}
	s.forums[183] = &forumRepresentation{
		name:      "The Goon Doctor",
		shortName: "ylls-doctor",
	}
	s.forums[244] = &forumRepresentation{
		name:      "The Fitness Log Cabin",
		shortName: "ylls-fitness-logs",
	}
	s.forums[272] = &forumRepresentation{
		name:      "The Great Outdoors",
		shortName: "tgo",
	}
	s.forums[161] = &forumRepresentation{
		name:      "Goons with Spoons",
		shortName: "gws",
	}
	s.forums[91] = &forumRepresentation{
		name:      "Automative Insanity",
		shortName: "ai",
	}
	s.forums[236] = &forumRepresentation{
		name:      "Cycle Asylum",
		shortName: "ai-cycle",
	}
	s.forums[210] = &forumRepresentation{
		name:      "Hobbies, Crafts & Houses",
		shortName: "diy",
	}
	s.forums[124] = &forumRepresentation{
		name:      "Pet Island",
		shortName: "pi",
	}
	s.forums[132] = &forumRepresentation{
		name:      "The Firing Range",
		shortName: "tfr",
	}
	s.forums[277] = &forumRepresentation{
		name:      "The Pellet Palace",
		shortName: "tfr-pellet",
	}
	s.forums[90] = &forumRepresentation{
		name:      "The Crackhead Clubhouse",
		shortName: "tcc",
	}
	s.forums[218] = &forumRepresentation{
		name:      "Goons in Platoons",
		shortName: "gip",
	}
	s.forums[275] = &forumRepresentation{
		name:      "The Minority Rapport",
		shortName: "tmr",
	}

	s.forums[267] = &forumRepresentation{
		name:      "Imp Zone",
		shortName: "iz",
	}
	s.forums[681] = &forumRepresentation{
		name:      "The Enclosed Pool Area",
		shortName: "iz-pool",
	}
	s.forums[274] = &forumRepresentation{
		name:      "Blockbuster Video",
		shortName: "iz-video",
	}
	s.forums[668] = &forumRepresentation{
		name:      "Sci-Fi Wi-Fi",
		shortName: "sfwf",
	}
	s.forums[151] = &forumRepresentation{
		name:      "Cinema Discussion",
		shortName: "cd",
	}
	s.forums[133] = &forumRepresentation{
		name:      "The Film Dump",
		shortName: "cd-dump",
	}
	s.forums[150] = &forumRepresentation{
		name:      "No Music Discussion",
		shortName: "nmd",
	}
	s.forums[104] = &forumRepresentation{
		name:      "Musician's Lounge",
		shortName: "nmd-lounge",
	}
	s.forums[215] = &forumRepresentation{
		name:      "PHIZ",
		shortName: "phiz",
	}
	s.forums[31] = &forumRepresentation{
		name:      "Creative Convention",
		shortName: "cc",
	}
	s.forums[247] = &forumRepresentation{
		name:      "The Dorkroom",
		shortName: "cc-dorkroom",
	}
	s.forums[182] = &forumRepresentation{
		name:      "The Book Barn",
		shortName: "tbb",
	}
	s.forums[688] = &forumRepresentation{
		name:      "The Scholastic Book Fair",
		shortName: "tbb-fair",
	}
	s.forums[130] = &forumRepresentation{
		name:      "TV IV",
		shortName: "tviv",
	}
	s.forums[255] = &forumRepresentation{
		name:      "Rapidly Going Deaf",
		shortName: "rgd",
	}
	s.forums[144] = &forumRepresentation{
		name:      "BSS: Bisexual Super Son",
		shortName: "bss",
	}
	s.forums[27] = &forumRepresentation{
		name:      "VEGETA",
		shortName: "adtrw",
	}

	s.forums[61] = &forumRepresentation{
		name:      "SA-Mart",
		shortName: "samart",
	}
	s.forums[77] = &forumRepresentation{
		name:      "SA-Mart Feedback & Discussion",
		shortName: "samart-feedback",
	}
	s.forums[85] = &forumRepresentation{
		name:      "Coupons & Deals",
		shortName: "samart-deals",
	}
	s.forums[241] = &forumRepresentation{
		name:      "LAN: Your City Sucks - Regional Discussion",
		shortName: "lan",
	}
	s.forums[43] = &forumRepresentation{
		name:      "Goon Meets",
		shortName: "lan-meets",
	}
	s.forums[686] = &forumRepresentation{
		name:      "Something Awful Discussion",
		shortName: "sad",
	}
	s.forums[687] = &forumRepresentation{
		name:      "Resolved, Closed, or Duplicate SAD Threads",
		shortName: "sad-closed",
	}
	s.forums[676] = &forumRepresentation{
		name:      "Technical Enquiries Contemplated Here",
		shortName: "tech",
	}
	s.forums[689] = &forumRepresentation{
		name:      "Goon Rush",
		shortName: "tech-goon-rush",
	}
	s.forums[677] = &forumRepresentation{
		name:      "Resolved Technical Forum Missives",
		shortName: "tech-resolved",
	}

	s.forums[21] = &forumRepresentation{
		name:      "Comedy Goldmine",
		shortName: "goldmine",
	}
	s.forums[680] = &forumRepresentation{
		name:      "Imp Zone: Player's Choice",
		shortName: "goldmine-iz",
	}
	s.forums[690] = &forumRepresentation{
		name:      "The Haunted Clubhouse",
		shortName: "goldmine-haunted",
	}
	s.forums[692] = &forumRepresentation{
		name:      "25 Years of Something Awful",
		shortName: "goldmine-25",
	}
	s.forums[264] = &forumRepresentation{
		name:      "The Goodmine",
		shortName: "goldmine-goodmine",
	}
	s.forums[115] = &forumRepresentation{
		name:      "FYAD: Criterion Collection",
		shortName: "goldmine-fyad",
	}
	s.forums[222] = &forumRepresentation{
		name:      "LF Goldmine",
		shortName: "goldmine-lf",
	}
	s.forums[176] = &forumRepresentation{
		name:      "BYOB Goldmine: Gold Mango",
		shortName: "goldmine-byob",
	}
	s.forums[25] = &forumRepresentation{
		name:      "Toxic Comedy Gas Waste Chamber Dump",
		shortName: "gas",
	}
	s.forums[1] = &forumRepresentation{
		name:      "General Bullshit - ARCHIVES",
		shortName: "archive-gbs",
	}
	s.forums[675] = &forumRepresentation{
		name:      "QCS: Questions, Comments, Suggestions",
		shortName: "archive-qcs",
	}
	s.forums[188] = &forumRepresentation{
		name:      "QCS Success Stories",
		shortName: "archive-qcs-success",
	}

	for id, v := range s.forums {
		v.forumid = int(id)
		s.forumNames[v.shortName] = v
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
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(time.Minute):
			}
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
		forums:              make(map[string]*forumRepresentation),
		seenPrivateMessages: make(map[int64]struct{}),
	}

	go sc.run()

	s.lock.Lock()
	s.connections[sc] = struct{}{}
	s.lock.Unlock()

	return nil
}
