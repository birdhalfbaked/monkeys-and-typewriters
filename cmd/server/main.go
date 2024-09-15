package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
)

type config struct {
	SendWaitTimeMs      int
	StaticCacheTimeoutS int
}

// HOT CONFIGURABLES
var CONFIGURATION = config{
	SendWaitTimeMs:      16,
	StaticCacheTimeoutS: 60 * 5,
}

func loadHotConfigurables() {
	file, err := os.Open("./configuration.json")
	if err != nil {
		slog.Error("Could not open hotload configuration", "error", err)
	}
	data, _ := io.ReadAll(file)
	var newConfig = config{}
	err = json.Unmarshal(data, &newConfig)
	if err != nil {
		slog.Error("could not process hotload configuration", "error", err)
	}
	if newConfig.SendWaitTimeMs == CONFIGURATION.SendWaitTimeMs && newConfig.StaticCacheTimeoutS == CONFIGURATION.StaticCacheTimeoutS {
		// don't bother
		return
	}
	// double check some reasonable values
	if newConfig.SendWaitTimeMs > 10 && newConfig.StaticCacheTimeoutS < 20*60 {
		CONFIGURATION = newConfig
		slog.Info("new hotload configuration loaded", "values", CONFIGURATION)
	} else {
		slog.Warn("ignoring hotload configuration with unreasonable values", "values", newConfig)
	}
}

// NOT-SO-HOT CONFIGURABLES
var HOSTNAME = os.Getenv("SERVER_HOSTNAME")
var TLS_CERT_PATH = os.Getenv("SERVER_TLS_CERT_PATH")
var TLS_KEY_PATH = os.Getenv("SERVER_TLS_KEY_PATH")
var MAX_CONNS_IP = 25

// These made me take a long hard look at the mirror and reflect on my life
var NEW_CLIENT_MUTEX = sync.Mutex{}
var NEW_IP_MUTEX = sync.Mutex{}

type StaticCacheData struct {
	bytes     []byte
	cacheTime time.Time
}

var STATIC_RESPONSE_CACHE = make(map[string]StaticCacheData)

func NewUpgrader() websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  8,
		WriteBufferSize: 128,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return strings.Contains(origin, HOSTNAME) || true
		},
	}
}

const (
	CommandPing int = iota
	CommandType
)

type OutDataPacket struct {
	TotalResets uint64
	TotalKeys   uint64
	Part        uint16
	Index       uint16
	Checkpoint  uint16
	Clients     uint32
}

type Client struct {
	conn      *websocket.Conn
	gameRef   *Game
	inChan    chan<- byte
	ID        uuid.UUID
	Successes int
	Failures  int
	LastHash  string
}

var ALLOWED_CHARS = []byte("abcdefghijklmnopqrstuvwxyz,.!?;:\"'-")

func (c *Client) handleInMessages() {
	for {
		var key byte
		err := c.conn.ReadJSON(&key)
		if err != nil {
			c.Close()
			return
		} else if bytes.Contains(ALLOWED_CHARS, []byte{key}) {
			c.inChan <- key
		}
	}
}

func (c *Client) broadcastState() {
	var err error
	var prevMessage *websocket.PreparedMessage
	for {
		if c.gameRef.stateMessage != prevMessage {
			err = c.conn.WritePreparedMessage(c.gameRef.stateMessage)
			if err != nil {
				c.Close()
				return
			}
			prevMessage = c.gameRef.stateMessage
		}
		time.Sleep(time.Duration(CONFIGURATION.SendWaitTimeMs) * time.Millisecond)
	}
}

func (c *Client) Close() {
	host, _, _ := net.SplitHostPort(c.conn.RemoteAddr().String())
	c.conn.Close()
	NEW_IP_MUTEX.Lock()
	c.gameRef.clientsByIP[host]--
	if c.gameRef.clientsByIP[host] == 0 {
		delete(c.gameRef.clientsByIP, host)
	}
	NEW_IP_MUTEX.Unlock()
	c.gameRef.clients--
}

type HamletItem struct {
	Type string
	Text string
}

type Part struct {
	Index int
	Text  string
}

type Game struct {
	sqlConn             *sql.DB
	inChan              chan byte
	clientChan          chan *Client
	AllParts            map[int]Part
	PartText            string
	PartIndex           int
	Index               int
	Length              int
	CheckpointIndex     int
	CurrentPartFailures int
	TotalFailures       int
	TotalKeys           int
	stateMessage        *websocket.PreparedMessage
	clients             uint32
	clientsByIP         map[string]int
	lastTwenty          []uint8
}

func initDB() (db *sql.DB, err error) {
	db, err = sql.Open("sqlite3", "./data/sql/primary.db")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS "state" ("part" int, "index" int, "checkpoint_index" int, "failures" bigint, "typed_keys" bigint);
	`)
	return
}

func newGame() (game *Game) {
	file, err := os.Open("./data/hamlet.json")
	if err != nil {
		panic(err)
	}
	allData, err := io.ReadAll(file)
	if err != nil {
		panic(err)
	}
	var hamletItems = make([]HamletItem, 0)
	err = json.Unmarshal(allData, &hamletItems)
	if err != nil {
		panic(err)
	}
	var parts = make(map[int]Part, 0)
	partCount := 0
	for _, part := range hamletItems {
		if part.Type == "part" {
			parts[partCount] = Part{Index: partCount, Text: part.Text}
			partCount++
		}
	}
	inChan := make(chan byte, 2*1024*1024)

	sqlConn, err := initDB()
	if err != nil {
		panic(err)
	}
	game = &Game{
		sqlConn:             sqlConn,
		inChan:              inChan,
		clientChan:          make(chan *Client, 25000),
		AllParts:            parts,
		PartText:            strings.ToLower(parts[0].Text),
		PartIndex:           0,
		CheckpointIndex:     0,
		Index:               0,
		Length:              len(parts[0].Text),
		CurrentPartFailures: 0,
		stateMessage:        nil,
		clients:             0,
		clientsByIP:         make(map[string]int),
		lastTwenty:          make([]uint8, 20),
	}
	err = game.loadState()
	if err != nil {
		panic(err)
	}
	game.stateMessage = game.newPreparedMessage(make([]byte, 46))
	go game.saveStateRoutine()
	go game.handleInMessages()
	return
}

// Technically we can use this to hotswap in the OH SO HOT Configurables
func (g *Game) saveStateRoutine() {
	for {
		loadHotConfigurables()
		_, err := g.sqlConn.Exec(
			`UPDATE state SET "part"=?, "index"=?, "checkpoint_index"=?, "failures"=?, "typed_keys"=?;`,
			g.PartIndex,
			g.Index,
			g.CheckpointIndex,
			g.TotalFailures,
			g.TotalKeys,
		)
		if err != nil {
			slog.Error(err.Error())
		}
		time.Sleep(5 * time.Second)
	}
}

func (g *Game) loadState() (err error) {
	row := g.sqlConn.QueryRow(`SELECT "part", "index", "checkpoint_index", "failures", "typed_keys" FROM "state";`)
	err = row.Scan(&g.PartIndex, &g.Index, &g.CheckpointIndex, &g.TotalFailures, &g.TotalKeys)
	if err == sql.ErrNoRows {
		err = nil
		g.sqlConn.Exec(`INSERT INTO "state" VALUES (0, 0, 0, 0, 0);`)
	}
	g.PartText = strings.ToLower(g.AllParts[g.PartIndex].Text)
	g.Length = len(g.PartText)
	return
}

func (g *Game) newPreparedMessage(buffer []byte) *websocket.PreparedMessage {
	var packet = OutDataPacket{
		TotalResets: uint64(g.TotalFailures),
		TotalKeys:   uint64(g.TotalKeys),
		Part:        uint16(g.PartIndex),
		Index:       uint16(g.Index),
		Checkpoint:  uint16(g.CheckpointIndex),
		Clients:     g.clients,
	}
	var err error
	binary.Encode(buffer, binary.LittleEndian, packet)
	buffer = append(buffer[:26], g.lastTwenty...)
	stateMessage, err := websocket.NewPreparedMessage(websocket.BinaryMessage, buffer)
	if err != nil {
		slog.Error(err.Error())
	}
	return stateMessage
}

func (g *Game) handleInMessages() {
	msgData := make([]byte, 8*2+2*3+4*1+20)
	for char := range g.inChan {
		g.lastTwenty = append(g.lastTwenty[1:], char)
		g.TotalKeys++
		if char == g.PartText[g.Index] {
			for {
				g.Index++
				if g.Index == g.Length || g.PartText[g.Index] != ' ' {
					break
				}
				g.CheckpointIndex = g.Index + 1
			}
			if g.Index == g.Length {
				g.PartIndex++
				part := g.AllParts[g.PartIndex]
				g.Index = 0
				g.Length = len(part.Text)
				g.PartText = strings.ToLower(part.Text)
				g.CurrentPartFailures = 0
				g.CheckpointIndex = 0
			}
		} else {
			g.Index = g.CheckpointIndex
			g.CurrentPartFailures++
			g.TotalFailures++
		}
		message := g.newPreparedMessage(msgData)
		if message != nil {
			g.stateMessage = message
		}
	}
}

func (g *Game) createClient(uuid uuid.UUID, conn *websocket.Conn) {
	var client = &Client{
		conn:    conn,
		ID:      uuid,
		gameRef: g,
		inChan:  g.inChan,
	}
	go client.handleInMessages()
	go client.broadcastState()
	g.clients++
}

type Server struct {
	pageListener   net.Listener
	wsListener     net.Listener
	availablePorts []string
	game           *Game
}

func createServerAndListen() {
	server := &Server{
		availablePorts: make([]string, 0),
		game:           newGame(),
	}
	http.HandleFunc("/", server.index)
	http.HandleFunc("/hamlet.js", server.hamlet)
	http.HandleFunc("/wsServer", server.getServer)

	pagePort := ":80"
	if TLS_CERT_PATH != "" {
		pagePort = ":443"
	}
	pageListener, err := net.Listen("tcp", pagePort)
	if err != nil {
		panic(err)
	}
	server.pageListener = pageListener
	wsMux := http.NewServeMux()
	wsMux.HandleFunc("/connect", server.wsUpgradeHandler(NewUpgrader()))
	server.wsListener, err = net.Listen("tcp", ":8001")
	if err != nil {
		slog.Error(err.Error())
	}
	if TLS_CERT_PATH == "" {
		go http.Serve(server.wsListener, wsMux)
	} else {
		go http.ServeTLS(server.wsListener, wsMux, TLS_CERT_PATH, TLS_KEY_PATH)
	}

	if TLS_CERT_PATH == "" {
		http.Serve(pageListener, nil)
	} else {
		slog.Info("Serving with TLS")
		http.ServeTLS(pageListener, nil, TLS_CERT_PATH, TLS_KEY_PATH)
	}
}

func (s *Server) wsUpgradeHandler(upgrader websocket.Upgrader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		NEW_IP_MUTEX.Lock()
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if cnt, ok := s.game.clientsByIP[host]; !ok {
			s.game.clientsByIP[host] = 0
		} else if cnt > MAX_CONNS_IP-1 {
			w.WriteHeader(429)
			slog.Info("Rejecting Conn due to many active connections")
			NEW_IP_MUTEX.Unlock()
			return
		}
		s.game.clientsByIP[host]++
		NEW_IP_MUTEX.Unlock()
		uuid, _ := uuid.NewRandom()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Error(err.Error())
			return
		}
		go s.game.createClient(uuid, conn)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	var data StaticCacheData
	data, ok := STATIC_RESPONSE_CACHE["index"]
	if !ok || time.Since(data.cacheTime) > (time.Duration(CONFIGURATION.StaticCacheTimeoutS)*time.Millisecond) {
		data = StaticCacheData{}
		file, _ := os.Open("./html/index.html")
		data.bytes, _ = io.ReadAll(file)
		data.cacheTime = time.Now()
		STATIC_RESPONSE_CACHE["index"] = data
		file.Close() // who needs defer
	}
	w.Write(data.bytes)
}

func (s *Server) hamlet(w http.ResponseWriter, r *http.Request) {
	var data StaticCacheData
	data, ok := STATIC_RESPONSE_CACHE["hamlet"]
	if !ok || time.Since(data.cacheTime) > (time.Duration(CONFIGURATION.StaticCacheTimeoutS)*time.Second) {
		data = StaticCacheData{}
		file, _ := os.Open("./html/hamlet.js")
		data.bytes, _ = io.ReadAll(file)
		data.cacheTime = time.Now()
		STATIC_RESPONSE_CACHE["hamlet"] = data
		file.Close() // who needs defer
	}
	w.Write(data.bytes)
}

type ServerDiscoResponse struct {
	WSServer string
}

func (s *Server) getServer(w http.ResponseWriter, r *http.Request) {
	// leaving this disco stuff in case we might actually need it
	var address string
	var port = "8001"
	if TLS_CERT_PATH == "" {
		address = fmt.Sprintf("ws://%s:%s/connect", HOSTNAME, port)
	} else {
		address = fmt.Sprintf("wss://%s:%s/connect", HOSTNAME, port)
	}
	enc := json.NewEncoder(w)
	enc.Encode(ServerDiscoResponse{WSServer: address})
}

func main() {
	if HOSTNAME == "" {
		HOSTNAME = "localhost"
	}
	createServerAndListen()

}
