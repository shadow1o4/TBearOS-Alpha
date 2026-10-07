// Command mock-graph runs an in-memory fake of the Microsoft Graph OneDrive
// and login endpoints, for trying onedrive-fuse without a Microsoft account.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/shadow1o4/TBearOS-Alpha/drivers/onedrive-fuse/internal/graph/graphtest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8765", "listen address")
	seed := flag.Bool("seed", true, "create a few sample files")
	flag.Parse()
	s, err := graphtest.NewAt(*addr)
	if err != nil {
		log.Fatal(err)
	}
	if *seed {
		s.Put("Documents/welcome.txt", []byte("Hello from the fake OneDrive!\n"))
		s.Put("Documents/notes/todo.md", []byte("- try onedrive-fuse\n"))
		s.Put("Pictures/readme.txt", []byte("no pictures yet\n"))
	}
	fmt.Printf("fake Graph API listening on %s\n\n", s.URL)
	fmt.Printf("export ONEDRIVE_FUSE_GRAPH_URL=%s\n", s.GraphURL())
	fmt.Printf("export ONEDRIVE_FUSE_AUTH_URL=%s\n", s.AuthURL())
	fmt.Printf("export ONEDRIVE_FUSE_CLIENT_ID=fake-client\n")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	s.Close()
}
