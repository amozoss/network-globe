package main

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

var (
	colors []string = []string{"#FFC680", // buff
		"red",
		"orange",
		"#FF1493", // Deep pink
		"white",
		"purple",
		"yellow",
		"pink",
		"#00BFFF", // sky blue
		"#C19A6B", // desert
	}
	updateInterval time.Duration = 1 * time.Second
)

type Server struct {
	mux        *mux.Router
	socketMu   sync.Mutex
	sockets    []*websocket.Conn
	messages   []*Message
	messageMu  sync.Mutex
	colorIndex int
	ticker     *time.Ticker
}

func NewServer(frontendDir string) *Server {
	server := &Server{
		mux:        mux.NewRouter(),
		colorIndex: 0,
		ticker:     time.NewTicker(updateInterval),
	}
	server.mux.HandleFunc("/ws", server.socketHandler)
	server.mux.PathPrefix("/").Handler(http.FileServer(http.Dir(frontendDir)))
	go func() {
	}()
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type LatLng struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type Message struct {
	Src   LatLng `json:"src"`
	Dst   LatLng `json:"dst"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type BatchMessage struct {
	Messages []*Message `json:"messages"`
}

func (s *Server) StartBroadcasts() {
	for {
		select {
		case <-s.ticker.C:
			s.Broadcast()
		}
	}
}

func (s *Server) socketHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade our raw HTTP connection to a websocket based one
	var upgrader = websocket.Upgrader{}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("Error during connection upgradation:", err)
		return
	}
	defer conn.Close()
	s.socketMu.Lock()
	// TODO handle keep-alives and clean-up of the connections
	// a more robust solution https://github.com/gorilla/websocket/blob/master/examples/chat/client.go
	s.sockets = append(s.sockets, conn)
	s.socketMu.Unlock()
	// The event loop
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Error during message reading:", err)
			break
		}
		log.Printf("Received: %s", message)
		/*
			err = conn.WriteMessage(websocket.TextMessage, []byte("HI"))
			if err != nil {
				log.Println("Error during message writing:", err)
			}
		*/
	}
}

func (s *Server) Queue(msg *Message) {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	msg.Color = colors[s.colorIndex%(len(colors)-1)]

	s.messages = append(s.messages, msg)
}

// broadcasts to all sockets
func (s *Server) Broadcast() {
	// Hacky way to build a new list of open sockets, assuming if it fails to write, it's not open.
	openSockets := make([]*websocket.Conn, 0)

	var messages []*Message
	s.messageMu.Lock()
	messages = s.messages
	s.messages = nil
	s.messageMu.Unlock()
	log.Println("len(s.messages)", len(s.messages))
	log.Println("len(messages)", len(messages))

	if len(messages) < 50 {
		return
	}

	msg := BatchMessage{
		Messages: messages,
	}

	for _, conn := range s.sockets {
		log.Printf("Broadcasting: %v", msg)

		err := conn.WriteJSON(msg)
		if err != nil {
			log.Println("Error during message writing:", err)
		} else {
			openSockets = append(openSockets, conn)
		}
	}

	s.socketMu.Lock()
	s.sockets = openSockets
	s.socketMu.Unlock()

	s.messageMu.Lock()
	s.colorIndex++
	s.messageMu.Unlock()
}

func (s *Server) cleanup(id string) {
	// TODO need to clean up
}
