package main

import (
	"log"
	"net/http"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	game "github.com/jilio/tomb_mates"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// // Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	id   string
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// readPump pumps messages from the websocket connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) readPump(world *game.World) {
	defer func() {
		event := &game.Event{
			Type: game.Event_type_exit,
			Data: &game.Event_Exit{
				&game.EventExit{PlayerId: c.id},
			},
		}
		message, err := proto.Marshal(event)
		if err != nil {
			log.Println(err)
		}
		world.HandleEvent(event)
		c.hub.broadcast <- message

		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}
		c.hub.broadcast <- message // ?
		event := &game.Event{}
		err = proto.Unmarshal(message, event)
		if err != nil {
			log.Println(err)
		}
		world.HandleEvent(event)
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.BinaryMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Add queued chat messages to the current websocket message.
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// serveWs handles websocket requests from the peer.
func serveWs(hub *Hub, world *game.World, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	id := world.AddPlayer()
	client := &Client{id: id, hub: hub, conn: conn, send: make(chan []byte, 256)}
	client.hub.register <- client

	event := &game.Event{
		Type: game.Event_type_init,
		Data: &game.Event_Init{
			&game.EventInit{
				PlayerId: id,
				Units:    world.Units,
			},
		},
	}
	message, err := proto.Marshal(event)
	if err != nil {
		//todo: remove unit
		log.Println(err)
	}
	conn.WriteMessage(websocket.BinaryMessage, message)

	unit := world.Units[id]
	event = &game.Event{
		Type: game.Event_type_connect,
		Data: &game.Event_Connect{
			&game.EventConnect{Unit: unit},
		},
	}
	message, err = proto.Marshal(event)
	if err != nil {
		//todo: remove unit
		log.Println(err)
	}
	hub.broadcast <- message

	// Allow collection of memory referenced by the caller by doing all work
	// in new goroutines.
	go client.writePump()
	go client.readPump(world)
}
