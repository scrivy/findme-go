package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

var xyzRegex *regexp.Regexp
var getIdChan chan string
var idToConnMap map[string]*client
var idToConnMapMutex sync.RWMutex

func main() {
	xyzRegex = regexp.MustCompile(`^\/tiles\/(\d+)\/(\d+)\/(\d+)\.png$`)
	idToConnMap = make(map[string]*client)
	go connIdGen()

	http.HandleFunc("/tiles/", getTiles)
	http.Handle("/ws", websocket.Handler(wsHandler))
	http.Handle("/", http.FileServer(http.Dir("frontend/public")))

	log.Fatal(http.ListenAndServe(":5000", nil))
}

func connIdGen() {
	getIdChan = make(chan string)
	var id string
	count := 0
	for {
		md5Sum := md5.Sum([]byte(strconv.Itoa(count)))
		id = hex.EncodeToString(md5Sum[:])
		getIdChan <- id
		count++
	}
}

type message struct {
	Action string      `json:"action"`
	Data   interface{} `json:"data"`
	Date   time.Time
}

type location struct {
	Id       string    `json:"id"`
	Latlng   []float64 `json:"latlng"`
	Accuracy float64   `json:"accuracy"`
}

type client struct {
	conn     *websocket.Conn
	location location
	id       string
}

func wsHandler(ws *websocket.Conn) {
	id := <-getIdChan
	c := client{
		conn:     ws,
		location: location{Id: id},
		id:       id,
	}
	idToConnMapMutex.Lock()
	idToConnMap[id] = &c
	idToConnMapMutex.Unlock()
	log.Printf("%d open websockets", len(idToConnMap))

	websocket.JSON.Send(ws, getAllLocations())

	var m message
	var err error
	for {
		err = websocket.JSON.Receive(ws, &m)
		if err != nil {
			switch err {
			case io.EOF:

			default:
				log.Println("err = websocket.JSON.Receive(ws, &m):", err)
			}
			break
		}

		// log.Println("Received message:", m.Action)
		//		log.Printf("%+v\n", m.Data)

		switch m.Action {
		case "updateLocation":
			data := m.Data.(map[string]interface{})
			l := location{
				Id:       id,
				Accuracy: data["accuracy"].(float64),
			}
			for _, num := range data["latlng"].([]interface{}) {
				l.Latlng = append(l.Latlng, num.(float64))
			}
			c.location = l
			sendMessageToAll(
				message{
					"updateLocation",
					l,
					time.Now()},
				&id)
		default:
			log.Printf("%+v\n", m.Data)
		}
	}

	idToConnMapMutex.Lock()
	delete(idToConnMap, id)
	idToConnMapMutex.Unlock()
	log.Printf("Disconnected: %d open websockets", len(idToConnMap))
}

func getAllLocations() message {
	locations := make([]location, 0)
	idToConnMapMutex.RLock()
	for _, c := range idToConnMap {
		if c.location.Latlng != nil {
			locations = append(locations, c.location)
		}
	}
	idToConnMapMutex.RUnlock()

	return message{
		Action: "allLocations",
		Data:   &locations,
	}
}

func sendMessageToAll(m message, except *string) {
	var clients []*websocket.Conn
	idToConnMapMutex.RLock()
	for id, c := range idToConnMap {
		if except != nil && id != *except {
			clients = append(clients, c.conn)
		}
	}
	idToConnMapMutex.RUnlock()

	jsonData, err := json.Marshal(m)
	if err != nil {
		log.Println(err)
		return
	}

	for _, c := range clients {
		c.Write(jsonData)
	}
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

	w.Header().Set("Cache-Control", "max-age=86400")
	http.ServeFile(w, r, xyzFile)
}
