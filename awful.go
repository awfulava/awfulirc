package awfulirc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// AwfulClient accesses and parses somethingawful.com.
type AwfulClient struct {
	client *http.Client
}

// NewAwfulClient returns an empty client that has not been logged in yet. Some
// operations should work without logging in, but that flow has not been tested
// much, so you probably want to Login immediately after construction.
func NewAwfulClient() (*AwfulClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("unable to create cookie jar: %w", err)
	}
	return &AwfulClient{
		client: &http.Client{
			Jar: jar,
		},
	}, nil
}

// Login logs in with the given credentials.
func (a *AwfulClient) Login(ctx context.Context, username, password string) error {
	data := url.Values{
		"action":   {"login"},
		"username": {username},
		"password": {password},
		"next":     {"/index.php?json=1"},
	}

	req, err := http.NewRequestWithContext(
		ctx, "POST",
		"https://forums.somethingawful.com/account.php?json=1",
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return fmt.Errorf("could not create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	loginResponse, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("unable to send login request: %w", err)
	}
	defer loginResponse.Body.Close()
	// TODO: Looks like we may need to parse the output since we always get
	// 200. Figure this out later.
	if loginResponse.StatusCode != http.StatusOK {
		io.Copy(os.Stdout, loginResponse.Body)
		return fmt.Errorf("login failed for username %q", username)
	}
	return nil
}

// ThreadMetadata represents a thread without its posts.
type ThreadMetadata struct {
	// ID is the somethingawful.com thread ID.
	ID int64

	// Title is the thread title, which may change regularly.
	Title string

	// Replies is the number of replies in the thread.
	Replies int64

	// Updated is the last thread update time.
	Updated time.Time
}

// LastPostURL returns the URL to the thread's last post page.
func (t ThreadMetadata) LastPostURL() string {
	return fmt.Sprintf("https://forums.somethingawful.com/showthread.php?threadid=%d&goto=lastpost", t.ID)
}

// UnreadPostURL returns the URL to the thread's last unread post.
func (t ThreadMetadata) UnreadPostURL() string {
	return fmt.Sprintf("https://forums.somethingawful.com/showthread.php?threadid=%d&goto=newpost", t.ID)
}

// PageURL returns the URL to the specific page of the thread.
func (t ThreadMetadata) PageURL(p int) string {
	return fmt.Sprintf("https://forums.somethingawful.com/showthread.php?threadid=%d&pagenumber=%d", t.ID, p)
}

// ReplyURL returns the URL to the reply page of the thread.
func (t ThreadMetadata) ReplyURL() string {
	return fmt.Sprintf("https://forums.somethingawful.com/newreply.php?action=newreply&threadid=%d", t.ID)
}

type parsedThreads struct {
	Threads    []ThreadMetadata
	TotalPages int64
}

// Post contains the raw post.
type Post struct {
	// ID is the unique post ID from somethingawul.com.
	ID int64

	// Author is the raw author name.
	Author string

	// Body is the raw body text.
	Body string
}

// IRCAuthor converts the author to a standardized IRC-compatible
// representation.
func (p Post) IRCAuthor() string {
	author := strings.ReplaceAll(p.Author, " ", "")
	author = strings.ReplaceAll(author, "_", "__")
	author = strings.ReplaceAll(author, ":", "_")
	return author
}

// IRCLines formats the post for display in the target channel by truncating
// into lines of appropriate length along sensible boundaries.
func (p Post) IRCLines(ch string) []string {
	body := p.Body
	author := p.IRCAuthor()
	prefix := fmt.Sprintf(":%s PRIVMSG %s :", author, ch)
	messageLen := 512 - len(prefix) - 2

	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	var b strings.Builder
	var lines []string
	for line := range strings.SplitSeq(body, "\n") {
		if len(line) <= messageLen {
			lines = append(lines, prefix+line)
		} else {
			// In the bad case, the individual line is too
			// long. Truncate it into separate lines.
			for word := range strings.SplitSeq(line, " ") {
				// Possibly flush the previous word.
				if b.Len() != 0 && b.Len()+1+len(word) > messageLen {
					lines = append(lines, prefix+b.String())
					b.Reset()
				}

				// If the word by itself is too long, truncate it down
				// to a reasonable length. Note that if this is true,
				// the above will also be true so we start from a
				// fresh string.
				for len(word) > messageLen {
					lines = append(lines, prefix+string(word[:messageLen]))
					word = string(word[messageLen:])
				}

				if b.Len() == 0 {
					// Guaranteed to be small enough after above truncation.
					b.WriteString(word)
				} else if b.Len()+1+len(word) > messageLen {
					// Flush and append the word.
					lines = append(lines, prefix+b.String())
					b.Reset()
					b.WriteString(word)
				} else {
					b.WriteString(" ")
					b.WriteString(word)
				}
			}
			if b.Len() > 0 {
				lines = append(lines, prefix+b.String())
				b.Reset()
			}
		}
	}
	return lines
}

// Posts contains all posts on a thread page.
type Posts struct {
	Posts       []Post
	CurrentPage int64
	TotalPages  int64
}

// ReplyToThread replies to the given thread with the given message.
func (a *AwfulClient) ReplyToThread(ctx context.Context, thread ThreadMetadata, message string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", thread.ReplyURL(), nil)
	if err != nil {
		return err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	doc, err := html.Parse(res.Body)
	if err != nil {
		return err
	}

	var (
		action     string
		threadid   string
		formkey    string
		formCookie string
	)

	parseForm := func(form *html.Node) {
		for n := range form.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Input {
				var (
					name  string
					value string
				)
				for _, attr := range n.Attr {
					switch attr.Key {
					case "name":
						name = attr.Val
					case "value":
						value = attr.Val
					}
				}
				switch name {
				case "action":
					action = value
				case "threadid":
					threadid = value
				case "formkey":
					formkey = value
				case "form_cookie":
					formCookie = value
				}
			}
		}
	}

	parseContent := func(content *html.Node) {
		for n := range content.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Form {
				for _, attr := range n.Attr {
					if attr.Key == "action" && attr.Val == "newreply.php" {
						parseForm(n)
						return
					}
				}
			}
		}
	}

	parseContainer := func(container *html.Node) {
		for n := range container.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "id" && attr.Val == "content" {
						parseContent(n)
						return
					}
				}
			}
		}
	}

	parseBody := func(body *html.Node) {
		for n := range body.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "id" && attr.Val == "container" {
						parseContainer(n)
						return
					}
				}
			}
		}
	}

	for n := range doc.ChildNodes() {
		if n.Type == html.ElementNode && n.DataAtom == atom.Html {
			for body := range n.ChildNodes() {
				if body.Type == html.ElementNode && body.DataAtom == atom.Body {
					parseBody(body)
				}
			}
		}
	}

	if action == "" || threadid == "" || formkey == "" || formCookie == "" {
		return errors.New("unable to parse new reply page")
	}

	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	fw, err := w.CreateFormField("action")
	fw.Write([]byte(action))
	fw, _ = w.CreateFormField("threadid")
	fw.Write([]byte(threadid))
	fw, _ = w.CreateFormField("formkey")
	fw.Write([]byte(formkey))
	fw, _ = w.CreateFormField("form_cookie")
	fw.Write([]byte(formCookie))
	fw, _ = w.CreateFormField("message")
	fw.Write([]byte(message))
	w.CreateFormFile("attachment", "")
	fw, _ = w.CreateFormField("parseurl")
	fw.Write([]byte("yes"))
	fw, _ = w.CreateFormField("bookmark")
	fw.Write([]byte("yes"))
	fw, _ = w.CreateFormField("MAX_FILE_SIZE")
	fw.Write([]byte("2097152"))
	fw, _ = w.CreateFormField("submit")
	fw.Write([]byte("Submit Reply"))
	w.Close()

	req, err = http.NewRequestWithContext(ctx, "POST", "https://forums.somethingawful.com/newreply.php", &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	res, err = a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(os.Stdout, res.Body)
		return fmt.Errorf("post failed with status: %v", res.StatusCode)
	}
	return nil
}

// ParseUnreadPosts returns all posts on the last unread page. Note that the
// client doesn't track previously seen posts, so this may duplicate if called
// twice and the page hasn't changed.
func (a *AwfulClient) ParseUnreadPosts(ctx context.Context, thread ThreadMetadata) (Posts, error) {
	var posts Posts
	req, err := http.NewRequestWithContext(ctx, "GET", thread.UnreadPostURL(), nil)
	if err != nil {
		return posts, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return posts, err
	}
	posts, err = a.parsePosts(ctx, res.Body)
	res.Body.Close()
	if err != nil {
		return posts, err
	}

	for page := posts.CurrentPage + 1; page <= posts.TotalPages; page++ {
		p, err := a.ParsePagePosts(ctx, thread, int(page))
		if err != nil {
			return posts, err
		}
		posts.Posts = append(posts.Posts, p.Posts...)
		posts.TotalPages = p.TotalPages // In case this updated while parsing.
	}

	return posts, nil
}

// ParseLastThreadPosts returns all posts on the last page of the thread.
func (a *AwfulClient) ParseLastThreadPosts(ctx context.Context, thread ThreadMetadata) (Posts, error) {
	var posts Posts
	req, err := http.NewRequestWithContext(ctx, "GET", thread.LastPostURL(), nil)
	if err != nil {
		return posts, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return posts, err
	}
	defer res.Body.Close()
	return a.parsePosts(ctx, res.Body)
}

// ParsePagePosts returns all posts on the specific page of the thread.
func (a *AwfulClient) ParsePagePosts(ctx context.Context, thread ThreadMetadata, page int) (Posts, error) {
	var posts Posts
	req, err := http.NewRequestWithContext(ctx, "GET", thread.PageURL(page), nil)
	if err != nil {
		return posts, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return posts, err
	}
	defer res.Body.Close()
	return a.parsePosts(ctx, res.Body)
}

func (a *AwfulClient) parsePosts(ctx context.Context, body io.Reader) (Posts, error) {
	var errs []error

	posts := Posts{
		CurrentPage: -1,
		TotalPages:  -1,
	}

	doc, err := html.Parse(body)
	if err != nil {
		return posts, fmt.Errorf("bad html: %w", err)
	}

	parseUserInfo := func(td *html.Node) string {
		for n := range td.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Dl {
				for dt := range n.ChildNodes() {
					if dt.Type == html.ElementNode && dt.DataAtom == atom.Dt {
						for name := range dt.ChildNodes() {
							if name.Type == html.TextNode {
								return name.Data
							}
						}
					}
				}
			}
		}
		return ""
	}

	parsePostBody := func(td *html.Node) string {
		var builder strings.Builder

		for n := range td.ChildNodes() {
			if n.Type == html.TextNode {
				builder.WriteString(n.Data)
			} else if n.Type == html.ElementNode && n.DataAtom == atom.Br {
				builder.WriteString("\n")
			} else if n.Type == html.ElementNode && n.DataAtom == atom.A {
				var (
					href string
					text string
				)
				for _, attr := range n.Attr {
					if attr.Key == "href" {
						href = attr.Val
					}
				}
				for inner := range n.ChildNodes() {
					if inner.Type == html.TextNode {
						text = inner.Data
					}
				}

				switch {
				case href == "":
					builder.WriteString(text)
				case text == "":
					builder.WriteString(href)
				case href == text:
					builder.WriteString(href)
				default:
					builder.WriteString("[")
					builder.WriteString(text)
					builder.WriteString("](")
					builder.WriteString(href)
					builder.WriteString(")")
				}
			}
		}

		return builder.String()
	}

	parsePost := func(tr *html.Node) (string, string) {
		var (
			username string
			body     string
		)
		for td := range tr.ChildNodes() {
			for _, attr := range td.Attr {
				if attr.Key == "class" && attr.Val == "postbody" {
					body = parsePostBody(td)
				} else if attr.Key == "class" && strings.HasPrefix(attr.Val, "userinfo ") {
					username = parseUserInfo(td)
				}
			}
		}

		return username, body
	}

	parseThreadInner := func(inner *html.Node) {
		for n := range inner.ChildNodes() {
			var (
				id       int64
				username string
				postbody string
			)
			if n.Type == html.ElementNode && n.DataAtom == atom.Table {
				for _, attr := range n.Attr {
					if attr.Key == "id" && strings.HasPrefix(attr.Val, "post") {
						parsedid, err := strconv.ParseInt(attr.Val[len("post"):], 10, 64)
						if err != nil {
							errs = append(errs, err)
						} else {
							id = parsedid
							for body := range n.ChildNodes() {
								if body.Type == html.ElementNode && body.DataAtom == atom.Tbody {
									for tr := range body.ChildNodes() {
										if tr.Type == html.ElementNode && tr.DataAtom == atom.Tr {
											username, postbody = parsePost(tr)
											break
										}
									}
								}
							}
						}
						break
					}
				}
			}
			if username != "" && postbody != "" {
				posts.Posts = append(posts.Posts, Post{
					ID:     id,
					Author: username,
					Body:   strings.ReplaceAll(strings.TrimSpace(postbody), "\n\n", "\n"),
				})
			}
		}

	}

	parseBreadcrumbs := func(breadcrumbs *html.Node) {
		for n := range breadcrumbs.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "data-total-pages" {
						pages, err := strconv.ParseInt(attr.Val, 10, 64)
						if err != nil {
							errs = append(errs, fmt.Errorf("unable to parse total pages: %w", err))
						} else {
							posts.TotalPages = pages
						}
					} else if attr.Key == "data-current-page" {
						pages, err := strconv.ParseInt(attr.Val, 10, 64)
						if err != nil {
							errs = append(errs, fmt.Errorf("unable to parse current page: %w", err))
						} else {
							posts.CurrentPage = pages
						}
					}
				}
			}
		}
	}

	parseContent := func(content *html.Node) {
		for n := range content.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "class" && attr.Val == "breadcrumbs" && posts.CurrentPage == -1 {
						parseBreadcrumbs(n)
					} else if attr.Key == "id" && attr.Val == "thread" {
						parseThreadInner(n)
					}
				}
			}
		}
	}

	parseContainer := func(container *html.Node) {
		for n := range container.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "id" && attr.Val == "content" {
						parseContent(n)
					}
				}
			}
		}
	}

	parseUsergroup := func(body *html.Node) {
		for n := range body.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "id" && attr.Val == "container" {
						parseContainer(n)
					}
				}
			}
		}
	}

	parseBody := func(body *html.Node) {
		for n := range body.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "id" && attr.Val == "usergroup" {
						parseUsergroup(n)
					}
				}
			}
		}
	}

	for n := range doc.ChildNodes() {
		if n.Type == html.ElementNode && n.DataAtom == atom.Html {
			for body := range n.ChildNodes() {
				if body.Type == html.ElementNode && body.DataAtom == atom.Body {
					parseBody(body)
				}
			}
		}
	}

	return posts, nil
}

func bookmarksPageURL(page int) string {
	return fmt.Sprintf("https://forums.somethingawful.com/bookmarkthreads.php?action=view&perpage=40&pagenumber=%d&sortorder=desc&sortfield=lastpost", page)
}

// ParseAllBookmarks iterates all bookmarked threads and returns them.
func (a *AwfulClient) ParseAllBookmarks(ctx context.Context) ([]ThreadMetadata, error) {
	var (
		threads []ThreadMetadata
		errs    []error
	)
	totalPages := 1
	for page := 1; page <= totalPages; page++ {
		url := bookmarksPageURL(page)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			errs = append(errs, err)
			return threads, errors.Join(err)
		}

		res, err := a.client.Do(req)
		if err != nil {
			errs = append(errs, err)
			return threads, errors.Join(err)
		}

		th, err := a.parseThreadsFromResponse(res)
		threads = append(threads, th.Threads...)
		totalPages = int(th.TotalPages)
		errs = append(errs, err)

		// Sleep a bit to avoid too many requests. Users should have relatively
		// few pages of bookmarks.
		select {
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return threads, errors.Join(errs...)
		case <-time.After(250 * time.Millisecond):
		}
	}
	return threads, errors.Join(errs...)
}

// ParseRecentBookmarks parses recently updated bookmarked threads and returns them.
func (a *AwfulClient) ParseRecentBookmarks(ctx context.Context) ([]ThreadMetadata, error) {
	url := bookmarksPageURL(1)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}

	th, err := a.parseThreadsFromResponse(res)
	return th.Threads, err
}

func (a *AwfulClient) parseThreadsFromResponse(res *http.Response) (parsedThreads, error) {
	var errs []error
	threads := parsedThreads{
		TotalPages: 1,
	}

	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return threads, fmt.Errorf("bad http status %v", res.StatusCode)
	}

	doc, err := html.Parse(res.Body)
	if err != nil {
		return threads, fmt.Errorf("bad html: %w", err)
	}

	parseReplies := func(replies *html.Node) int64 {
		for n := range replies.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.A {
				for inner := range n.ChildNodes() {
					if inner.Type == html.TextNode {
						parsed, err := strconv.ParseInt(inner.Data, 10, 64)
						if err != nil {
							errs = append(errs, err)
							continue
						}
						return parsed
					}
				}
			}
		}

		return -1
	}

	parseTitleInnerInfo := func(title *html.Node) string {
		for n := range title.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.A {
				for _, attr := range n.Attr {
					if attr.Key == "class" && attr.Val == "thread_title" {
						for inner := range n.ChildNodes() {
							if inner.Type == html.TextNode {
								return inner.Data
							}
						}
					}
				}
			}
		}

		return ""
	}

	parseTitleInner := func(title *html.Node) string {
		for n := range title.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "class" && attr.Val == "info" {
						return parseTitleInnerInfo(n)
					}
				}
			}
		}

		return ""
	}

	parseTitle := func(title *html.Node) string {
		for n := range title.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "class" && attr.Val == "title_inner" {
						return parseTitleInner(n)
					}
				}
			}
		}

		return ""
	}

	parseLastPost := func(lastpost *html.Node) time.Time {
		for n := range lastpost.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "class" && attr.Val == "date" {
						for inner := range n.ChildNodes() {
							if inner.Type == html.TextNode {
								for _, format := range []string{"15:04 Jan _2, 2006", "3:04 PM Jan _2, 2006"} {
									parsed, err := time.Parse(format, strings.TrimSpace(inner.Data))
									if err == nil {
										return parsed
									}
								}
							}
						}
					}
				}
			}
		}
		return time.Time{}
	}

	parseThread := func(thread *html.Node) {
		var (
			threadID       string
			threadTitle    string
			threadReplies  int64 = -1
			threadLastPost time.Time
		)
		for _, attr := range thread.Attr {
			if attr.Key == "id" {
				threadID = attr.Val
			}
		}

		for n := range thread.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Td {
				var class string
				for _, attr := range n.Attr {
					if attr.Key == "class" {
						class = attr.Val
						break
					}
				}

				switch class {
				case "title":
					threadTitle = parseTitle(n)
				case "replies":
					threadReplies = parseReplies(n)
				case "lastpost":
					threadLastPost = parseLastPost(n)
				}
			}
		}

		if threadID != "" && threadTitle != "" && threadReplies != -1 && !threadLastPost.IsZero() {
			if strings.HasPrefix(threadID, "thread") {
				threadNumber, err := strconv.ParseInt(threadID[len("thread"):], 10, 64)
				if err != nil {
					errs = append(errs, fmt.Errorf("unable to parse thread: %v", err))
					return
				}
				repr := ThreadMetadata{
					ID:      threadNumber,
					Title:   threadTitle,
					Replies: threadReplies,
					Updated: threadLastPost,
				}

				threads.Threads = append(threads.Threads, repr)
			}
		}
	}

	parseForumBody := func(body *html.Node) {
		for n := range body.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Tr {
				parseThread(n)
			}
		}
	}

	parseForum := func(forum *html.Node) {
		for n := range forum.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Tbody {
				parseForumBody(n)
			}
		}
	}

	parseForm := func(form *html.Node) {
		for n := range form.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "data-total-pages" {
						pages, err := strconv.ParseInt(attr.Val, 10, 64)
						if err != nil {
							errs = append(errs, fmt.Errorf("unable to parse total pages: %w", err))
						} else {
							threads.TotalPages = pages
						}
					}
				}
			} else if n.Type == html.ElementNode && n.DataAtom == atom.Table {
				for _, attr := range n.Attr {
					if attr.Key == "id" && attr.Val == "forum" {
						parseForum(n)
					}
				}
			}
		}
	}

	parseContent := func(content *html.Node) {
		for n := range content.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Form {
				parseForm(n)
			}
		}
	}

	parseContainer := func(container *html.Node) {
		for n := range container.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "id" && attr.Val == "content" {
						parseContent(n)
					}
				}
			}
		}
	}

	parseBody := func(body *html.Node) {
		for n := range body.ChildNodes() {
			if n.Type == html.ElementNode && n.DataAtom == atom.Div {
				for _, attr := range n.Attr {
					if attr.Key == "id" && attr.Val == "container" {
						parseContainer(n)
					}
				}
			}
		}
	}

	for n := range doc.ChildNodes() {
		if n.Type == html.ElementNode && n.DataAtom == atom.Html {
			for body := range n.ChildNodes() {
				if body.Type == html.ElementNode && body.DataAtom == atom.Body {
					parseBody(body)
				}
			}
		}
	}

	return threads, errors.Join(errs...)
}
