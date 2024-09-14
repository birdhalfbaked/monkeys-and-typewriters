package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var HOSTNAME = os.Getenv("SERVER_HOSTNAME")

type ServerResp struct {
	WSServer string
}

type KeyPress struct {
	Key byte `json:"key"`
}

var ALLOWED_CHARS = "abcdefghijklmnopqrstuvwxyz,.!?;:\"'-"
var PHRASE_ONLY_CHARS = "nay,answerme:stand,andunfoldyourself.who'sthere?"

var joiner = sync.WaitGroup{}

func connect() (conn *websocket.Conn) {
	resp, err := http.Get("http://" + HOSTNAME + ":8000/wsServer")
	if err != nil {
		slog.Error(err.Error())
		return
	}
	allData, _ := io.ReadAll(resp.Body)
	var serverRep ServerResp
	json.Unmarshal(allData, &serverRep)

	headers := http.Header{}
	headers.Add("Origin", "http://"+HOSTNAME+":8000")
	conn, _, err = websocket.DefaultDialer.Dial(serverRep.WSServer, headers)
	if err != nil {
		slog.Error(err.Error())
		joiner.Done()
		return nil
	}
	go stressDaHellOuttaIt(conn, 1000, 10000, true)
	return
}

func stressDaHellOuttaIt(conn *websocket.Conn, totalPresses, waitMs int, randomWait bool) {
	for i := 0; i < totalPresses; i++ {
		var waitJitter = 0
		if randomWait {
			waitJitter = rand.Intn(1000)
		}
		time.Sleep(time.Duration(waitMs+waitJitter) * time.Millisecond)
		keyIndex := rand.Intn(len(PHRASE_ONLY_CHARS))
		var key byte = PHRASE_ONLY_CHARS[keyIndex]
		conn.WriteJSON(key)
	}
	joiner.Done()
}

func main() {
	runtime.GOMAXPROCS(1)
	for i := 0; i < 10000; i++ {
		go connect()
		joiner.Add(1)
	}
	joiner.Wait()
}
