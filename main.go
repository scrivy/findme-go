package main

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	xyzRegex         *regexp.Regexp
	getIdChan        chan string
	idToConnMap      map[string]*client
	idToConnMapMutex sync.RWMutex
	upgrader         = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
)

func main() {
	xyzRegex = regexp.MustCompile(`^\/tiles\/(\d+)\/(\d+)\/(\d+)\.png$`)
	idToConnMap = make(map[string]*client)
	go connIdGen()

	http.HandleFunc("/tiles/", getTiles)
	http.HandleFunc("/ws", wsHandler)
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
	conn          *websocket.Conn
	location      location
	locationMutex sync.RWMutex
	id            string
	writingMutex  sync.Mutex
	queue         []location
	queueLock     sync.Mutex
	closeChan     chan bool
}

func (c *client) send(m *[]byte) {
	c.writingMutex.Lock()
	defer c.writingMutex.Unlock()
	err := c.conn.SetWriteDeadline(time.Now().Add(time.Duration(time.Second)))
	if err != nil {
		logErr(err)
	}
	err = c.conn.WriteMessage(websocket.TextMessage, *m)
	if err != nil {
		if strings.Contains(err.Error(), "i/o timeout") {
			log.Println(err)
		} else if strings.Contains(err.Error(), "write: broken pipe") {
			log.Println(err)
		} else {
			logErr(err)
		}
		close(c.closeChan)
		return
	}
	err = c.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		logErr(err)
	}
}

func (c *client) flushQueue() {
	c.queueLock.Lock()
	var err error
	var jsonBytes []byte
	if len(c.queue) > 0 {
		jsonBytes, err = json.Marshal(message{
			Action: "allLocations",
			Data:   c.queue,
			Date:   time.Now(),
		})
		c.queue = make([]location, 0)
	}
	c.queueLock.Unlock()
	if err != nil {
		logErr(err)
		return
	}
	if binary.Size(jsonBytes) > 0 {
		c.send(&jsonBytes)
	}
}

func (c *client) enqueue(l location) {
	c.queueLock.Lock()
	c.queue = append(c.queue, l)
	c.queueLock.Unlock()
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logErr(err)
		return
	}

	err = websocket.WriteJSON(conn, getAllLocations())
	if err != nil {
		logErr(err)
	}

	id := <-getIdChan
	c := client{
		conn:      conn,
		location:  location{Id: id},
		id:        id,
		closeChan: make(chan bool),
	}
	idToConnMapMutex.Lock()
	idToConnMap[id] = &c
	log.Printf("%d open websockets", len(idToConnMap))
	idToConnMapMutex.Unlock()

	go func(c *client) {
		ticker := time.NewTicker(time.Second)
		for {
			select {
			case <-ticker.C:
				c.flushQueue()
			case <-c.closeChan:
				log.Println("closing chan")
				return
			}
		}
	}(&c)

	var m message
	var jsonBytes []byte
	for {
		messageType, r, err := conn.NextReader()
		if err != nil {
			switch err {
			//			case io.EOF:
			default:
				switch err.Error() {
				case "websocket: close 1006 (abnormal closure): unexpected EOF":
				default:
					log.Println("conn.NextReader err:", err)
				}
			}
			break
		}

		switch messageType {
		case websocket.TextMessage:
			jsonBytes, err = ioutil.ReadAll(r)
			if err != nil {
				logErr(err)
				continue
			}

			json.Unmarshal(jsonBytes, &m)

			switch m.Action {
			case "updateLocation":
				data := m.Data.(map[string]interface{})
				latlng, ok := data["latlng"].([]interface{})
				if !ok || len(latlng) != 2 {
					continue
				}
				lat, ok := latlng[0].(float64)
				if !ok {
					continue
				}
				lng, ok := latlng[1].(float64)
				if !ok {
					continue
				}
				accuracy, ok := data["accuracy"].(float64)
				if !ok {
					continue
				}

				l := location{
					Id:       id,
					Accuracy: accuracy,
					Latlng:   []float64{lat, lng},
				}
				c.locationMutex.Lock()
				c.location = l
				c.locationMutex.Unlock()
				sendLocationToAll(l, &id)
			default:
				log.Printf("m.Data: %+v\n", m.Data)
			}
		default:
			log.Printf("messageType: %#v\n", messageType)
		}

	}

	idToConnMapMutex.Lock()
	delete(idToConnMap, id)
	log.Printf("Disconnected: %d open websockets", len(idToConnMap))
	idToConnMapMutex.Unlock()
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
		Date:   time.Now(),
	}
}

func sendLocationToAll(l location, except *string) {
	var clients []*client
	idToConnMapMutex.RLock()
	for id, c := range idToConnMap {
		if except != nil && id != *except {
			clients = append(clients, c)
		}
	}
	idToConnMapMutex.RUnlock()

	for _, c := range clients {
		c.enqueue(l)
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
			logErr(err)
			return
		}
		defer resp.Body.Close()
		err = os.MkdirAll(xyzPath, 0755)
		if err != nil {
			logErr(err)
			return
		}
		out, err := os.Create(xyzFile)
		if err != nil {
			logErr(err)
			return
		}
		io.Copy(out, resp.Body)
		out.Close()
		log.Println("downloaded " + xyzFile)
	}

	w.Header().Set("Cache-Control", "max-age=86400")
	http.ServeFile(w, r, xyzFile)
}

func logErr(err error) {
	log.Println(err)
	debug.PrintStack()
}
