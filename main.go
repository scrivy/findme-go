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
	xyzRegex         *regexp.Regexp     = regexp.MustCompile(`^\/tiles\/(\d+)\/(\d+)\/(\d+)\.png$`)
	getIdChan        chan string        = make(chan string)
	idToConnMap      map[string]*client = make(map[string]*client)
	idToConnMapMutex sync.RWMutex
	upgrader         = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
)

func main() {
	go func() { // connIdGen
		var id string
		count := 0
		for {
			md5Sum := md5.Sum([]byte(strconv.Itoa(count)))
			id = hex.EncodeToString(md5Sum[:])
			getIdChan <- id
			count++
		}
	}()

	http.HandleFunc("/tiles/", getTiles)
	http.HandleFunc("/ws", wsHandler)
	http.Handle("/", http.FileServer(http.Dir("frontend/public")))

	go func() {
		log.Fatal(http.ListenAndServe(":5000", nil))
	}()

	pingTicker := time.NewTicker(time.Duration(5) * time.Second)
	rmOldWsConnsTicker := time.NewTicker(time.Duration(10) * time.Minute)
	var lastCheck time.Time
	var lastRxMessage time.Time
	for {
		select {
		case <-pingTicker.C:
			for _, c := range getAllClients() {
				go c.ping()
			}
		case <-rmOldWsConnsTicker.C:
			for _, c := range getAllClients() {
				c.mutex.RLock()
				lastRxMessage = c.lastRxMessage
				c.mutex.RUnlock()
				if lastRxMessage.Before(lastCheck) {
					idToConnMapMutex.Lock()
					delete(idToConnMap, c.id)
					idToConnMapMutex.Unlock()
					go c.close()
					log.Println("removed stale client: ", c.id)
					log.Printf("%v clients connected\n", len(idToConnMap))
				}
			}
			lastCheck = time.Now()
		}
	}
}

type message struct {
	Action string      `json:"action"`
	Data   interface{} `json:"data"`
}

type location struct {
	Id       string    `json:"id"`
	Latlng   []float64 `json:"latlng"`
	Accuracy float64   `json:"accuracy"`
}

type client struct {
	conn          *websocket.Conn
	wsClosed      bool
	location      location
	mutex         sync.RWMutex
	writingMutex  sync.Mutex
	id            string
	queue         map[string]location
	enqueue       chan location
	closeChan     chan bool
	lastRxMessage time.Time
}

func (c *client) send(m *[]byte) {
	if !c.wsClosed {
		c.writingMutex.Lock()
		defer c.writingMutex.Unlock()
		err := c.conn.SetWriteDeadline(time.Now().Add(time.Duration(time.Second)))
		if err != nil {
			logErr(err)
			return
		}
		err = c.conn.WriteMessage(websocket.TextMessage, *m)
		if err != nil {
			switch err {
			case websocket.ErrCloseSent:
				c.close()
			default:
				if strings.Contains(err.Error(), "i/o timeout") {
					log.Println(err)
				} else if strings.Contains(err.Error(), "write: broken pipe") {
					log.Println(err)
				} else {
					logErr(err)
				}
			}
		}
	}
}

func (c *client) ping() {
	c.mutex.RLock()
	wsClosed := c.wsClosed
	c.mutex.RUnlock()
	if !wsClosed {
		err := c.conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(time.Second))
		if err != nil {
			switch err {
			case websocket.ErrCloseSent:
			default:
				if strings.Contains(err.Error(), "write: broken pipe") {
					log.Println(err)
				} else {
					logErr(err)
				}
			}
			c.close()
		}
	}
}

func (c *client) close() {
	c.mutex.RLock()
	wsClosed := c.wsClosed
	c.mutex.RUnlock()
	if !wsClosed {
		c.mutex.Lock()
		c.wsClosed = true
		c.mutex.Unlock()
		close(c.closeChan)
		c.conn.Close()
	}
}

func (c *client) flushQueue() {
	c.mutex.Lock()
	if !c.wsClosed {
		var err error
		var jsonBytes []byte
		if len(c.queue) > 0 {
			locations := []location{}
			for _, l := range c.queue {
				locations = append(locations, l)
			}
			jsonBytes, err = json.Marshal(message{
				Action: "allLocations",
				Data:   locations,
			})
			c.queue = make(map[string]location)
		}
		if err != nil {
			logErr(err)
			return
		}
		if binary.Size(jsonBytes) > 0 {
			c.send(&jsonBytes)
		}
	}
	c.mutex.Unlock()
}

func connPongHandler(c *client) func(string) error {
	return func(appData string) error {
		c.mutex.Lock()
		c.lastRxMessage = time.Now()
		c.mutex.Unlock()
		return nil
	}
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
	err = conn.WriteJSON(message{
		Action: "yourId",
		Data:   id,
	})
	if err != nil {
		logErr(err)
	}

	c := client{
		conn:          conn,
		location:      location{Id: id},
		id:            id,
		closeChan:     make(chan bool),
		enqueue:       make(chan location),
		queue:         make(map[string]location),
		lastRxMessage: time.Now(),
	}
	conn.SetPongHandler(connPongHandler(&c))
	idToConnMapMutex.Lock()
	idToConnMap[id] = &c
	log.Printf("%d open websockets", len(idToConnMap))
	idToConnMapMutex.Unlock()

	go func(c *client) {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.flushQueue()
			case l := <-c.enqueue:
				c.queue[l.Id] = l
			case <-c.closeChan:
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
			conn.Close()
			break
		}

		c.lastRxMessage = time.Now()

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
				c.mutex.Lock()
				c.location = l
				c.mutex.Unlock()
				sendLocationToAll(l, &id)
			case "oldId":
				oldId, ok := m.Data.(string)
				if !ok {
					continue
				}

				idToConnMapMutex.Lock()
				oldConn, ok := idToConnMap[oldId]
				delete(idToConnMap, oldId)
				idToConnMapMutex.Unlock()

				if ok {
					oldIdMessage := message{
						Action: "oldId",
						Data: map[string]string{
							"OldId": oldId,
							"NewId": id,
						},
					}
					jsonBytes, err = json.Marshal(oldIdMessage)
					if err != nil {
						logErr(err)
						continue
					}
					sendMessageToAll(&jsonBytes, &id)
					oldConn.close()
					log.Println("deleted oldId")
				}
			default:
				log.Printf("%s: %+v\n", m.Action, m.Data)
			}
		default:
			log.Printf("messageType: %#v\n", messageType)
		}
	}
}

func getAllLocations() message {
	locations := make([]location, 0)
	for _, c := range getAllClients() {
		c.mutex.RLock()
		if c.location.Latlng != nil {
			locations = append(locations, c.location)
		}
		c.mutex.RUnlock()
	}

	return message{
		Action: "allLocations",
		Data:   &locations,
	}
}

func sendLocationToAll(l location, except *string) {
	for _, c := range getAllClients() {
		if except != nil && c.id != *except {
			c.enqueue <- l
		}
	}
}

func sendMessageToAll(bytes *[]byte, except *string) {
	for _, c := range getAllClients() {
		if except != nil && c.id != *except {
			go c.send(bytes)
		}
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

func getAllClients() []*client {
	clients := make([]*client, 0)
	log.Println("idToConnMapMutex.RLock()")
	idToConnMapMutex.RLock()
	for _, c := range idToConnMap {
		clients = append(clients, c)
	}
	idToConnMapMutex.RUnlock()
	log.Println("idToConnMapMutex.RUnlock()")
	return clients
}
