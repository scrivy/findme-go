package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	//	"regexp"
)

var addr = flag.String("addr", ":5000", "http service address")

func main() {
	flag.Parse()
	go h.run()
	http.HandleFunc("/tiles/", getTiles)
	http.HandleFunc("/ws", serveWs)
	http.Handle("/", http.FileServer(http.Dir("public")))
	err := http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func getTiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	fmt.Println(r.URL.Path)
}
