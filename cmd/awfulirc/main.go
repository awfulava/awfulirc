/*
	Runs the awfulirc bridge. Defaults to listening on 127.0.0.1:6667.

	Only run this on private machines. This bridge has no security
	whatsoever.

Example:

		go run . --username "username" --password "password"
	    go run . --authfile "some-token-file"
*/
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/awfulava/awfulirc"
)

var (
	port     = flag.Int("port", 6667, "port to listen to")
	name     = flag.String("name", "awfulirc", "server name")
	username = flag.String("username", "", "login")
	password = flag.String("password", "", "password")
	authFile = flag.String("authfile", "", "auth file. see README.md")
)

func main() {
	flag.Parse()
	ctx := context.Background()
	addr := fmt.Sprintf("127.0.0.1:%d", *port)

	ac, err := awfulirc.NewAwfulClient()
	if err != nil {
		log.Fatal(err)
	}
	if *authFile != "" {
		func() {
			r, err := os.Open(*authFile)
			if err != nil {
				log.Fatal(err)
			}
			defer r.Close()
			if err := ac.SetAuthCookies(r); err != nil {
				log.Fatal(err)
			}
		}()
	} else if err := ac.Login(ctx, *username, *password); err != nil {
		log.Fatal(err)
	}

	_, err = awfulirc.Listen(ctx, ac, *name, addr)
	log.Printf("Listening on irc://%s", addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		<-time.After(time.Minute)
		log.Printf("Still listening on irc://%s", addr)
	}
}
