package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"storj.io/uplink"
)

var (
	colors []string = []string{
		"#00E366",
		"#FF458B",
		"#FFC600",
		"#FFFFFF",
		"#0149FF",
		"#9B4FFF",
		"#00BFEA",
		"#FF7E2E",
		"#EBEEF1",
	}
	uploadInterval time.Duration = 6 * time.Second
	//go:embed public/*
	files embed.FS
)

type embedFileSystem struct {
	http.FileSystem
}

func (e embedFileSystem) Open(name string) (http.File, error) {
	fmt.Printf("Opening file: %s\n", name) // Add logging for debugging
	return e.FileSystem.Open(name)
}

type Server struct {
	mux *mux.Router

	socketMu sync.Mutex
	sockets  []*websocket.Conn
	// end socketMu

	serverMu           sync.Mutex
	shouldUpload       bool
	messages           []*Message
	srcDestPacketCount map[string]int64
	queuedForFrontend  map[string]bool
	colorIndex         int
	// end serverMu

	uploadTicker *time.Ticker
	project      *uplink.Project
	bucket       string
	batchSize    int
	debug        bool
}

func NewServer(frontendDir string, project *uplink.Project, bucket string, batchSize int, debug bool) *Server {
	server := &Server{
		mux:                mux.NewRouter(),
		colorIndex:         0,
		uploadTicker:       time.NewTicker(uploadInterval),
		project:            project,
		bucket:             bucket,
		debug:              debug,
		srcDestPacketCount: make(map[string]int64),
		queuedForFrontend:  make(map[string]bool),
	}
	server.mux.HandleFunc("/ws", server.socketHandler)
	var dir http.FileSystem
	if frontendDir == "" {
		fsys, err := fs.Sub(files, "public")
		if err != nil {
			panic(err)
		}
		dir = embedFileSystem{http.FS(fsys)}
	} else {
		dir = http.Dir(frontendDir)
	}

	server.mux.PathPrefix("/").Handler(http.FileServer(dir))
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
	Src       LatLng `json:"src"`
	Dst       LatLng `json:"dst"`
	Direction string `json:"direction"`
	Count     int    `json:"count"`
	Name      string `json:"name"`
	Color     string `json:"color"`
}

type BatchMessage struct {
	Messages []*Message `json:"messages"`
}

func (s *Server) StartBroadcasts() {
	for {
		select {
		case <-s.uploadTicker.C:
			if s.project != nil {
				s.uplinkUpload()
			}
			s.Broadcast()
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
	if s.debug {
		log.Println("upload started")
	}

	upload, err := s.project.UploadObject(ctx, s.bucket, "test.txt", &uplink.UploadOptions{
		Expires: time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		log.Println("UploadObject error:", err)
	}

	// random bytes to test upload
	buffer := make([]byte, 8024)
	_, err = rand.Read(buffer)
	if err != nil {
		log.Fatalf("Failed to generate random bytes: %v", err)
	}

	// Create a bytes.Reader from the buffer
	randomBytes := bytes.NewReader(buffer)

	_, err = io.Copy(upload, randomBytes)
	if err != nil {
		log.Println("UploadObject io.Copy error:", err)
	}

	err = upload.Commit()
	if err != nil {
		log.Println("upload.Commit error:", err)
	}
	if s.debug {
		log.Println("upload finished")
	}
	s.setShouldUpload(false)
	s.Broadcast()
}

func (s *Server) socketHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade our raw HTTP connection to a websocket based one
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			// Check the origin here and return true if it's valid
			// For example, allow all origins:
			return true
		},
	}

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
		msg := string(message)
		if msg == "start" {
			s.setShouldUpload(true)
		}
		if msg == "routes_done" {
			s.setShouldUpload(true)
		}
	}
}

func (s *Server) Queue(msg *Message) {

	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	srcKey := fmt.Sprintf("%f,%f:%f,%f", msg.Src.Lat, msg.Src.Lng, msg.Dst.Lat, msg.Dst.Lng)
	s.srcDestPacketCount[srcKey] += 1

	msg.Color = colors[s.colorIndex%(len(colors)-1)]
	msg.Count = int(s.srcDestPacketCount[srcKey])

	if !s.queuedForFrontend[srcKey] {
		s.messages = append(s.messages, msg)
	}
	s.queuedForFrontend[srcKey] = true
}

// broadcasts to all sockets
func (s *Server) Broadcast() {

	var messages []*Message
	s.serverMu.Lock()
	messages = s.messages
	s.messages = nil
	s.queuedForFrontend = make(map[string]bool)
	s.serverMu.Unlock()

	//for latLng, value := range s.srcDestPacketCount {
	//	fmt.Printf("%s: %d\n", latLng, value)
	//}

	if len(messages) < s.batchSize || len(s.sockets) < 1 {
		return
	}

	msg := BatchMessage{
		Messages: messages,
	}

	// Hacky way to build a new list of open sockets, assuming if it fails to write, it's not open.
	openSockets := make([]*websocket.Conn, 0)
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
