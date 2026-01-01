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
