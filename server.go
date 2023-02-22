package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"storj.io/uplink"
)

const (
	batchSize = 50
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
		"#800020", // Burgundy
		"#CC5500", // Burnt orange
		"#BD33A4", // Byzantine
		"#702963", // Byzantium
		"#5F9EA0", // Cadet blue
		"#91A3B0", // Cadet grey
		"#006B3C", // Cadmium green
		"#ED872D", // Cadmium orange
		"#A67B5B", // Café au lait
		"#4B3621", // Café noir
		"#A3C1AD", // Cambridge blue
		"#C19A6B", // Camel
		"#EFBBCC", // Cameo pink
		"#FFFF99", // Canary
		"#FFEF00", // Canary yellow
		"#E4717A", // Candy pink
		"#C41E3A", // Cardinal
		"#960018", // Carmine
		"#D70040", // Carmine (M&P)
		"#FFA6C9", // Carnation pink
		"#B31B1B", // Carnelian
		"#56A0D3", // Carolina blue
		"#ED9121", // Carrot orange
		"#703642", // Catawba
		"#C95A49", // Cedar Chest
		"#ACE1AF", // Celadon
		"#B2FFFF", // Celeste
		"#DE3163", // Cerise
		"#007BA7", // Cerulean
		"#2A52BE", // Cerulean blue
		"#6D9BC3", // Cerulean frost
		"#1DACD6", // Cerulean (Crayola)
	}
	uploadInterval time.Duration = 6 * time.Second
)

type Server struct {
	mux *mux.Router

	socketMu sync.Mutex
	sockets  []*websocket.Conn
	// end socketMu

	serverMu     sync.Mutex
	shouldUpload bool
	messages     []*Message
	colorIndex   int
	// end serverMu

	uploadTicker *time.Ticker
	project      *uplink.Project
}

func NewServer(frontendDir string, project *uplink.Project) *Server {
	server := &Server{
		mux:          mux.NewRouter(),
		colorIndex:   0,
		uploadTicker: time.NewTicker(uploadInterval),
		project:      project,
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
		case <-s.uploadTicker.C:
			s.uplinkUpload()
		}
	}
}

func (s *Server) setShouldUpload(upload bool) {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	s.shouldUpload = upload
}

func (s *Server) uplinkUpload() {
	ctx := context.TODO()

	upload, err := s.project.UploadObject(ctx, "files", "test.txt", &uplink.UploadOptions{
		Expires: time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		log.Println("UploadObject error:", err)
	}

	f, err := os.Open("./test.txt")
	if err != nil {
		log.Fatal(err)
	}

	_, err = io.Copy(upload, f)
	if err != nil {
		log.Println("UploadObject io.Copy error:", err)
	}

	err = upload.Commit()
	if err != nil {
		log.Println("upload.Commit error:", err)
	}
	s.setShouldUpload(false)
	s.Broadcast()
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
		if string(message) == "start" {
			s.setShouldUpload(true)
		}
		if string(message) == "routes_done" {
			s.setShouldUpload(true)
		}
		/*
			err = conn.WriteMessage(websocket.TextMessage, []byte("HI"))
			if err != nil {
				log.Println("Error during message writing:", err)
			}
		*/
	}
}

func (s *Server) Queue(msg *Message) {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	msg.Color = colors[s.colorIndex%(len(colors)-1)]

	s.messages = append(s.messages, msg)
}

// broadcasts to all sockets
func (s *Server) Broadcast() {

	var messages []*Message
	s.serverMu.Lock()
	messages = s.messages
	s.messages = nil
	s.serverMu.Unlock()

	if len(messages) < 50 || len(s.sockets) < 1 {
		return
	}

	msg := BatchMessage{
		Messages: messages,
	}

	// Hacky way to build a new list of open sockets, assuming if it fails to write, it's not open.
	openSockets := make([]*websocket.Conn, 0)
	log.Println("Broadcasting")
	for _, conn := range s.sockets {

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

	s.serverMu.Lock()
	s.colorIndex++
	s.serverMu.Unlock()
}

func (s *Server) cleanup(id string) {
	// TODO need to clean up
}
