package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"./common"
	"./tiles"
)

var (
	getIdChan        chan string        = make(chan string)
	idToConnMap      map[string]*client = make(map[string]*client)
	idToConnMapMutex sync.RWMutex
	upgrader         = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	updatesMap   map[string]location = make(map[string]location)
	updatesMutex sync.RWMutex
	getUpdate    chan location = make(chan location)
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

	http.HandleFunc("/tiles/", tiles.GetHandler)
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/update", updateLocation)
	http.Handle("/", http.FileServer(http.Dir("frontend/public")))

	go func() {
		log.Fatal(http.ListenAndServe(":5000", nil))
	}()

	pingTicker := time.NewTicker(time.Duration(5) * time.Second)
	rmOldWsConnsTicker := time.NewTicker(time.Duration(10) * time.Minute)
	updatesTicker := time.NewTicker(time.Second)
	var lastCheck time.Time
	var lastRxMessage time.Time

	queuedUpdates := make(map[string]location)

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
		case <-updatesTicker.C:
			updatesMutex.Lock()
			updatesMap = make(map[string]location)
			for k, v := range queuedUpdates {
				updatesMap[k] = v
			}
			updatesMutex.Unlock()
			queuedUpdates = make(map[string]location)
		case l := <-getUpdate:
			queuedUpdates[l.Id] = l
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
	Accuracy int       `json:"accuracy"`
}

type client struct {
	conn          *websocket.Conn
	wsClosed      bool
	location      location
	mutex         sync.RWMutex
	writingMutex  sync.Mutex
	id            string
	closeChan     chan bool
	lastRxMessage time.Time
}

func (c *client) send(m *[]byte) {
	if !c.wsClosed {
		c.writingMutex.Lock()
		defer c.writingMutex.Unlock()
		err := c.conn.SetWriteDeadline(time.Now().Add(time.Duration(time.Second)))
		if err != nil {
			common.LogErr(err)
			return
		}
		err = c.conn.WriteMessage(websocket.TextMessage, *m)
		if err != nil {
			switch err {
			case websocket.ErrCloseSent:
				log.Println(err)
			default:
				if strings.Contains(err.Error(), "i/o timeout") {
					log.Println(err)
				} else if strings.Contains(err.Error(), "write: broken pipe") {
					log.Println(err)
				} else if strings.Contains(err.Error(), "use of closed network connection") {
					common.LogErr(err)
				}
			}
			c.close()
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
				} else if strings.Contains(err.Error(), "use of closed network connection") {
					log.Println(err)
				} else {
					common.LogErr(err)
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
		common.LogErr(err)
		return
	}

	err = websocket.WriteJSON(conn, getAllLocations())
	if err != nil {
		common.LogErr(err)
	}

	id := <-getIdChan
	err = conn.WriteJSON(message{
		Action: "yourId",
		Data:   id,
	})
	if err != nil {
		common.LogErr(err)
	}

	c := client{
		conn:          conn,
		location:      location{Id: id},
		id:            id,
		closeChan:     make(chan bool),
		lastRxMessage: time.Now(),
	}
	conn.SetPongHandler(connPongHandler(&c))
	idToConnMapMutex.Lock()
	idToConnMap[id] = &c
	log.Printf("%d open websockets, id: %s\n", len(idToConnMap), id)
	idToConnMapMutex.Unlock()

	go func(c *client) {
		ticker := time.NewTicker(time.Second)
		var jsonBytes []byte
		var locations []location
		var err error
		for {
			select {
			case <-ticker.C:
				locations = []location{}
				updatesMutex.RLock()
				if len(updatesMap) > 0 {
					for id, l := range updatesMap {
						if id != c.id {
							locations = append(locations, l)
						}
					}
				}
				updatesMutex.RUnlock()
				if len(locations) > 0 {
					jsonBytes, err = json.Marshal(message{
						Action: "allLocations",
						Data:   locations,
					})
					if err != nil {
						common.LogErr(err)
						continue
					}
					c.send(&jsonBytes)
				}
			case <-c.closeChan:
				ticker.Stop()
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
				common.LogErr(err)
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
				accuracy, ok := data["accuracy"].(int)
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

				getUpdate <- l
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
						common.LogErr(err)
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

func sendMessageToAll(bytes *[]byte, except *string) {
	for _, c := range getAllClients() {
		if except != nil && c.id != *except {
			go c.send(bytes)
		}
	}
}

func getAllClients() []*client {
	clients := make([]*client, 0)
	//	log.Println("idToConnMapMutex.RLock()")
	idToConnMapMutex.RLock()
	for _, c := range idToConnMap {
		clients = append(clients, c)
	}
	idToConnMapMutex.RUnlock()
	//	log.Println("idToConnMapMutex.RUnlock()")
	return clients
}

func updateLocation(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, http.StatusText(405), 405)
		return
	}

	if r.Header.Get("Content-Type") != "application/json" || r.ContentLength == 0 {
		http.Error(w, "The content type must be application/json and contain a body", 400)
		return
	}

	dec := json.NewDecoder(r.Body)
	var loc location
	err := dec.Decode(&loc)
	if err != nil {
		http.Error(w, "Error parsing json", 400)
		return
	}

	idToConnMapMutex.RLock()
	c, ok := idToConnMap[loc.Id]
	idToConnMapMutex.RUnlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	c.mutex.Lock()
	c.location = loc
	c.mutex.Unlock()

	getUpdate <- loc
}
