package awfulirc

import (
	"sync"
)

// threadRepresentation is the server-side representation of a thread and its
// posts.
type threadRepresentation struct {
	meta      ThreadMetadata
	shortName string

	seen      map[int64]struct{}
	authors   map[string]struct{}
	lock      sync.Mutex
	listening bool
	posts     []Post

	subscribers map[*serverConnection]struct{}
}

type forumRepresentation struct {
	forumid   int
	name      string
	shortName string

	lock        sync.Mutex
	threads     map[int64]ThreadMetadata
	seen        map[int64]struct{}
	authors     map[string]struct{}
	subscribers map[*serverConnection]struct{}
	listening   bool
}
