package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"regexp"

	"golang.org/x/net/websocket"
)

var xyzRegex *regexp.Regexp

func main() {
	xyzRegex = regexp.MustCompile(`^\/tiles\/(\d+)\/(\d+)\/(\d+)\.png$`)

	http.HandleFunc("/tiles/", getTiles)
	http.Handle("/ws", websocket.Handler(wsHandler))
	http.Handle("/", http.FileServer(http.Dir("public")))

	err := http.ListenAndServe(":5000", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func wsHandler(ws *websocket.Conn) {
	io.Copy(ws, ws)
}

func getTiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	if !xyzRegex.MatchString(r.URL.Path) {
		http.Error(w, "wrong format", 400)
		return
	}
	xyz := xyzRegex.FindStringSubmatch(r.URL.Path)
	xyzPath := "tiles/" + xyz[1] + "/" + xyz[2] + "/"
	xyzFile := xyzPath + xyz[3] + ".png"

	if _, err := os.Stat(xyzFile); os.IsNotExist(err) {
		resp, err := http.Get("http://a.tile.thunderforest.com/transport/" + xyz[1] + "/" + xyz[2] + "/" + xyz[3] + ".png")
		if err != nil {
			log.Println(err)
			return
		}
		defer resp.Body.Close()
		err = os.MkdirAll(xyzPath, 0755)
		if err != nil {
			log.Println(err)
			return
		}
		out, err := os.Create(xyzFile)
		if err != nil {
			log.Println(err)
			return
		}
		io.Copy(out, resp.Body)
		out.Close()
		log.Println("downloaded " + xyzFile)
	}

	http.ServeFile(w, r, xyzFile)
}
