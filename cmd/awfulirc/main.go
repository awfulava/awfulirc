/*
	Runs the awfulirc bridge. Defaults to listening on 127.0.0.1:6667.

	Only run this on private machines. This bridge has no security
	whatsoever.

Example:

	go run . --username "username" --password "password"
*/
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/awfulava/awfulirc"
)

var (
	port     = flag.Int("port", 6667, "port to listen to")
	name     = flag.String("name", "awfulirc", "server name")
	username = flag.String("username", "", "login")
	password = flag.String("password", "", "password")
)

func main() {
	flag.Parse()
	ctx := context.Background()
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	_, err := awfulirc.Listen(ctx, *username, *password, *name, addr)
	log.Printf("Listening on irc://%s", addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		<-time.After(time.Minute)
		log.Printf("Still listening on irc://%s", addr)
	}
}
